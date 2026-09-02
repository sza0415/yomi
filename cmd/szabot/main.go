// Command szabot 是 CLI 入口。
//
// 这个 main 文件做的事情非常简单——它只做"装配"：
//  1. 创建一条 MessageBus；
//  2. 根据环境变量选择 Provider（echo / deepseek）；
//  3. 用 bus + runner 创建 AgentLoop 并启动；
//  4. 二选一起交互 channel：SZABOT_WEB 起 HTTP/SSE API，否则起 CLI；
//  5. 等系统信号退出。
//
// 没有任何业务逻辑，所有逻辑都被关在了对应的 package 里——
// 这就是 nanobot 设计宪法的第一条："Core stays small; extend at the edges"。
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ziangsun/szabot/internal/agent"
	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/channels"
	"github.com/ziangsun/szabot/internal/configstore"
	"github.com/ziangsun/szabot/internal/memory"
	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/skills"
	"github.com/ziangsun/szabot/internal/tools"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

func main() {
	configOnly := flag.Bool("config", false, "start the configuration wizard without starting the agent")
	flag.Parse()
	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve workspace: %v\n", err)
		os.Exit(1)
	}
	configPath := envOr("YOMI_CONFIG_FILE", filepath.Join(workspace, ".yomi", "config.json"))
	settings, err := configstore.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		os.Exit(1)
	}
	if *configOnly {
		settings.ApplyToEnv()
		runConfigWizard(workspace, configPath, settings)
		return
	}
	settings.ApplyToEnv()

	// 监听 Ctrl+C / SIGTERM，触发优雅退出。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// 1. 消息总线。
	b := bus.New(64)

	// 2. 根据环境变量选 Provider。
	provider, model := buildProvider()

	registry := tools.NewRegistry()
	todoTool := registerTools(registry, workspace)

	runner := &agent.Runner{
		Provider:       provider,
		Model:          model,
		Tools:          registry,
		PermissionGate: tools.NewPolicyGate(permissionMode()),
		// Agent 状态栏：让 Runner 每轮把 todo_write 的任务清单与进度
		// 作为一条 user 消息注入到上下文末尾，供模型自我感知计划与进度。
		Status: todoTool,
	}

	// 技能系统：扫描 workspace/skills 生成 L1 摘要，拼进 system prompt。
	// agent 触发某个技能时，用现成的 read_file 读摘要里给出的路径即可（L2），
	// 无需专门的 skill 工具。
	systemPrompt := buildSystemPrompt(workspace)

	// Conversation 只保存用户与最终回答；内部推理和工具轨迹单独进入 traces。
	rootDir := sessionDir(workspace)
	contextMaxTokens := envIntDefault("SZABOT_MAX_CONTEXT_TOKENS", 6000)
	contextRecentMessages := envIntDefault("SZABOT_CONTEXT_RECENT_MESSAGES", 8)
	toolResultMaxTokens := envIntDefault("SZABOT_TOOL_RESULT_MAX_TOKENS", 1200)
	outputReserveTokens := envIntDefault("SZABOT_CONTEXT_OUTPUT_RESERVE_TOKENS", 1024)
	contextBudget := &agent.ContextBudget{
		MaxContextTokens:    contextMaxTokens,
		WarningRatio:        0.8,
		OutputReserveTokens: outputReserveTokens,
	}
	summaryTimeout := 30 * time.Second
	runTimeout := envDuration("SZABOT_RUN_TIMEOUT", 3*time.Minute)
	budget := agent.RunBudget{
		MaxInputTokens:  envInt("SZABOT_MAX_INPUT_TOKENS"),
		MaxOutputTokens: envInt("SZABOT_MAX_OUTPUT_TOKENS"),
		MaxTotalTokens:  envInt("SZABOT_MAX_TOTAL_TOKENS"),
		MaxModelCalls:   envInt("SZABOT_MAX_MODEL_CALLS"),
		MaxToolCalls:    envInt("SZABOT_MAX_TOOL_CALLS"),
	}
	store, err := agent.NewSessionStore(filepath.Join(rootDir, "conversations"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init session store: %v\n", err)
		os.Exit(1)
	}
	memoryStore, err := memory.NewSQLiteStore(filepath.Join(rootDir, "memories", "memory.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init memory store: %v\n", err)
		os.Exit(1)
	}
	defer memoryStore.Close()
	var memoryExtractor memory.Extractor
	if memoryExtractionEnabled(provider) {
		memoryExtractor = &memory.LLMExtractor{Provider: provider, Model: model}
	}
	var memoryEmbedder memory.EmbeddingProvider
	var memoryIndexer memory.Indexer
	qdrantEnabled := qdrantEnabledByConfig()
	qdrantURL := strings.TrimSpace(os.Getenv("SZABOT_QDRANT_URL"))
	if qdrantEnabled && qdrantURL == "" {
		qdrantURL = "http://127.0.0.1:6333"
	}
	qdrantCollection := envOr("SZABOT_QDRANT_COLLECTION", "yomi_memories")
	embeddingBaseURL := envOr("SZABOT_EMBEDDING_BASE_URL", os.Getenv("DEEPSEEK_BASE_URL"))
	embeddingAPIKey := envOr("SZABOT_EMBEDDING_API_KEY", os.Getenv("DEEPSEEK_API_KEY"))
	embeddingModel := strings.TrimSpace(os.Getenv("SZABOT_EMBEDDING_MODEL"))
	qdrantReady := false
	var qdrantCleanup func(context.Context) error
	if qdrantEnabled && qdrantURL != "" {
		qdrantReady = true
		if qdrantAutoStartEnabled(qdrantURL) {
			qdrantCtx, qdrantCancel := context.WithTimeout(ctx, 30*time.Second)
			cleanup, err := memory.EnsureLocalQdrantManaged(qdrantCtx, qdrantURL)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: Qdrant auto-start failed; semantic memory indexing disabled: %v\n", err)
				qdrantReady = false
			} else {
				qdrantCleanup = cleanup
			}
			qdrantCancel()
		}
		if embeddingBaseURL == "" || embeddingAPIKey == "" || embeddingModel == "" {
			fmt.Fprintln(os.Stderr, "warning: Qdrant configured but embedding settings are incomplete; semantic memory indexing disabled")
		} else if !qdrantReady {
			// Keep the SQLite memory path available after a local Docker failure.
		} else if indexer, indexErr := memory.NewQdrantIndexer(memory.QdrantConfig{BaseURL: qdrantURL, Collection: qdrantCollection}); indexErr != nil {
			fmt.Fprintf(os.Stderr, "warning: disable Qdrant memory index: %v\n", indexErr)
		} else {
			memoryEmbedder = &memory.OpenAIEmbeddingProvider{BaseURL: embeddingBaseURL, APIKey: embeddingAPIKey, ModelName: embeddingModel}
			memoryIndexer = indexer
		}
	}
	rerankerEnabled := rerankerEnabledByConfig()
	rerankerModel := strings.TrimSpace(os.Getenv("SZABOT_RERANKER_MODEL"))
	rerankerBaseURL := strings.TrimSpace(os.Getenv("SZABOT_RERANKER_BASE_URL"))
	if rerankerBaseURL == "" {
		rerankerBaseURL = embeddingBaseURL
	}
	rerankerAPIKey := envOr("SZABOT_RERANKER_API_KEY", embeddingAPIKey)
	var reranker memory.Reranker
	if rerankerEnabled && rerankerBaseURL != "" && rerankerAPIKey != "" && rerankerModel != "" {
		reranker = &memory.HTTPReranker{
			BaseURL: rerankerBaseURL, APIKey: rerankerAPIKey, ModelName: rerankerModel,
			TopN: envIntDefault("SZABOT_RERANKER_TOP_N", 20),
		}
	} else if rerankerEnabled {
		fmt.Fprintln(os.Stderr, "warning: reranker enabled but base URL, API key, or model is missing; reranker disabled")
	}
	var contextMemory memory.Store = memoryStore
	hybridSearchReason := "sqlite_fts_only"
	if memoryEmbedder != nil {
		if semanticSearcher, ok := memoryIndexer.(memory.SemanticSearcher); ok {
			hybrid := memory.NewHybridStore(memoryStore, memoryEmbedder, semanticSearcher)
			hybrid.Reranker = reranker
			contextMemory = hybrid
			hybridSearchReason = "fts_bm25_plus_qdrant_rrf"
		} else {
			hybridSearchReason = "qdrant_searcher_unavailable"
		}
	} else if qdrantEnabled {
		hybridSearchReason = "embedding_or_qdrant_index_disabled"
	} else {
		hybridSearchReason = "qdrant_disabled"
	}
	if reranker != nil && hybridSearchReason != "fts_bm25_plus_qdrant_rrf" {
		fmt.Fprintf(os.Stderr, "warning: reranker configured but inactive because hybrid retrieval is disabled (reason=%s)\n", hybridSearchReason)
	}
	resetRuntimeData := func(resetCtx context.Context) error {
		if resetter, ok := memoryIndexer.(memory.Resetter); ok {
			if err := resetter.Reset(resetCtx); err != nil {
				return err
			}
		}
		if err := memoryStore.ClearAll(resetCtx); err != nil {
			return err
		}
		if err := store.ClearAll(); err != nil {
			return err
		}
		for _, name := range []string{"archives", "traces", "runs", "artifacts"} {
			path := filepath.Join(rootDir, name)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("clear %s: %w", name, err)
			}
			if err := os.MkdirAll(path, 0o700); err != nil {
				return fmt.Errorf("recreate %s: %w", name, err)
			}
		}
		if err := os.Remove(filepath.Join(rootDir, ".DS_Store")); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("clear session root metadata: %w", err)
		}
		return nil
	}
	qdrantIndexReason := "qdrant_disabled"
	if qdrantEnabled {
		switch {
		case !qdrantReady:
			qdrantIndexReason = "qdrant_not_ready"
		case embeddingBaseURL == "" || embeddingAPIKey == "" || embeddingModel == "":
			qdrantIndexReason = "embedding_config_incomplete"
		case memoryIndexer == nil || memoryEmbedder == nil:
			qdrantIndexReason = "indexer_init_failed"
		default:
			qdrantIndexReason = "enabled"
		}
	}
	artifactStore, err := tools.NewArtifactStore(filepath.Join(rootDir, "artifacts"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init artifact store: %v\n", err)
		os.Exit(1)
	}
	artifactRead, err := tools.NewArtifactReadTool(artifactStore)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create artifact_read tool: %v\n", err)
		os.Exit(1)
	}
	if err := registry.Register(artifactRead); err != nil {
		fmt.Fprintf(os.Stderr, "error: register artifact_read tool: %v\n", err)
		os.Exit(1)
	}
	runner.Artifacts = artifactStore
	runner.ContextBudget = contextBudget
	runner.MaxContextTokens = contextMaxTokens
	runner.ToolResultMaxTokens = toolResultMaxTokens
	runner.OutputReserveTokens = outputReserveTokens
	traceSink, err := tracing.NewJSONLSink(filepath.Join(rootDir, "traces"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init trace store: %v\n", err)
		os.Exit(1)
	}
	runSnapshots, err := agent.NewJSONRunSnapshotStore(filepath.Join(rootDir, "runs"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init run snapshot store: %v\n", err)
		os.Exit(1)
	}

	// 3. AgentLoop：同 Session 串行、不同 Session 并行；每条请求拥有独立 Run。
	loop := &agent.Loop{
		Bus:          b,
		Runner:       runner,
		Store:        store,
		Trace:        traceSink,
		Snapshots:    runSnapshots,
		SystemPrompt: systemPrompt,
		Context: &agent.ContextManager{
			Store:            store,
			Provider:         provider,
			Model:            model,
			MaxContextTokens: contextMaxTokens,
			RecentMessages:   contextRecentMessages,
			SummaryTimeout:   summaryTimeout,
			ContextBudget:    contextBudget,
			Tools:            registry,
			Memory:           contextMemory,
		},
		Memory:          memoryStore,
		MemoryExtractor: memoryExtractor,
		MemoryEmbedder:  memoryEmbedder,
		MemoryIndexer:   memoryIndexer,
		MemoryTimeout:   envDuration("SZABOT_MEMORY_TIMEOUT", 30*time.Second),
		RunTimeout:      runTimeout,
		Budget:          budget,
	}
	loop.Start(ctx)
	printRuntimeConfig(provider, model, workspace, rootDir, contextMaxTokens, contextRecentMessages, summaryTimeout, runTimeout, budget, memoryRuntimeConfig{
		DBPath:              filepath.Join(rootDir, "memories", "memory.db"),
		ExtractionEnabled:   memoryExtractor != nil,
		ExtractionTimeout:   envDuration("SZABOT_MEMORY_TIMEOUT", 30*time.Second),
		QdrantURL:           qdrantURL,
		QdrantEnabled:       qdrantEnabled,
		QdrantCollection:    qdrantCollection,
		QdrantAutoStart:     qdrantAutoStartEnabled(qdrantURL),
		QdrantReady:         qdrantReady,
		QdrantIndexEnabled:  memoryIndexer != nil && memoryEmbedder != nil,
		QdrantIndexReason:   qdrantIndexReason,
		EmbeddingBaseURL:    embeddingBaseURL,
		EmbeddingModel:      embeddingModel,
		EmbeddingKeyPresent: embeddingAPIKey != "",
		HybridSearchEnabled: hybridSearchReason == "fts_bm25_plus_qdrant_rrf",
		HybridSearchReason:  hybridSearchReason,
		RerankerRequested:   rerankerEnabled,
		RerankerEnabled:     reranker != nil && hybridSearchReason == "fts_bm25_plus_qdrant_rrf",
		RerankerBaseURL:     rerankerBaseURL,
		RerankerModel:       rerankerModel,
		RerankerKeyPresent:  rerankerAPIKey != "",
		RerankerTopN:        envIntDefault("SZABOT_RERANKER_TOP_N", 20),
	})

	// 4. 选择交互 channel。
	//
	// CLI 与 Web 都监听同一条 bus.Outbound()，两者一起跑会互相抢消息，
	// 因此这里二选一：设置了 SZABOT_WEB 就起 Web，否则维持原来的 CLI 行为。
	//   - SZABOT_WEB=1            启用前端所需的 HTTP/SSE API（默认监听 :8080）
	//   - SZABOT_WEB_ADDR=:9000   自定义监听地址
	if os.Getenv("SZABOT_WEB") != "" {
		addr := envOr("SZABOT_WEB_ADDR", ":8080")
		web := &channels.WebChannel{
			ID:        "web",
			UserID:    envOr("SZABOT_USER_ID", "local"),
			Bus:       b,
			Trace:     traceSink,
			Snapshots: runSnapshots,
			Sessions:  store,
			Addr:      addr,
			// 只有 Web 客户端显式点击取消时，才停止该会话正在运行的
			// Runner/LLM 请求。SSE 断连本身不影响任务生命周期。
			OnCancel:    loop.CancelSession,
			Config:      buildConfigView(provider.Name(), model, workspace, rootDir, contextMaxTokens, contextRecentMessages, toolResultMaxTokens, outputReserveTokens, runTimeout, memoryExtractor != nil, qdrantEnabled, qdrantURL, qdrantCollection, qdrantAutoStartEnabled(qdrantURL), memoryIndexer != nil && memoryEmbedder != nil, embeddingBaseURL, embeddingModel, memoryConfigKeyPresent(embeddingAPIKey), rerankerEnabled, reranker != nil, rerankerBaseURL, rerankerModel, memoryConfigKeyPresent(rerankerAPIKey)),
			ConfigStore: settings,
			DebugReset:  resetRuntimeData,
		}
		if err := web.Start(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "error: start web channel: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("yomi started. provider=%s model=%s. web API listening on %s; start the frontend with: cd web && npm run dev. Ctrl+C to quit.\n",
			provider.Name(), model, addr)
	} else {
		// CLIChannel：stdin → bus 入站；bus 出站 → stdout。
		cli := &channels.CLIChannel{
			ID:  "cli",
			Bus: b,
		}
		cli.Start(ctx)

		fmt.Printf("yomi started. provider=%s model=%s. type something and press Enter. Ctrl+C to quit.\n",
			provider.Name(), model)
	}

	// 5. 等退出信号。
	<-ctx.Done()
	if qdrantCleanup != nil {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := qdrantCleanup(cleanupCtx); err != nil {
			fmt.Fprintf(os.Stderr, "warning: Qdrant shutdown cleanup failed: %v\n", err)
		} else {
			fmt.Println("qdrant container stopped (owned by yomi)")
		}
		cleanupCancel()
	}
	fmt.Println("\nyomi stopped.")
}

func buildConfigView(provider, model, workspace, sessionRoot string, maxContext, recent, toolMax, outputReserve int, runTimeout time.Duration, extraction, qdrant bool, qdrantURL, qdrantCollection string, qdrantAutoStart, qdrantIndex bool, embeddingURL, embeddingModel string, embeddingKey bool, rerankerRequested, reranker bool, rerankerURL, rerankerModel string, rerankerKey bool) channels.ConfigView {
	value := func(v string) string {
		if strings.TrimSpace(v) == "" {
			return "未设置"
		}
		return v
	}
	status := func(on bool) string {
		if on {
			return "已启用"
		}
		return "未启用"
	}
	embeddingKeyValue := envOr("SZABOT_EMBEDDING_API_KEY", os.Getenv("DEEPSEEK_API_KEY"))
	rerankerKeyValue := envOr("SZABOT_RERANKER_API_KEY", embeddingKeyValue)
	return channels.ConfigView{Sections: []channels.ConfigSection{
		{ID: "start", Title: "首次运行", Description: "最小配置只需要启动并选择一个模型提供商。其余能力都可以保持默认。", Items: []channels.ConfigItem{
			{Key: "provider", Env: "SZABOT_PROVIDER", Value: provider, Default: "echo", Description: "模型提供商。echo 不需要网络或密钥，适合首次验证；deepseek 需要 DEEPSEEK_API_KEY。", Status: "当前生效", RestartRequired: true},
			{Key: "model", Env: "DEEPSEEK_MODEL", Value: model, Default: "deepseek-chat", Description: "发送请求时使用的模型名称；仅在使用 deepseek 或兼容 Provider 时生效。", Status: "当前生效", RestartRequired: true},
			{Key: "provider_api_key", Env: "DEEPSEEK_API_KEY", Value: os.Getenv("DEEPSEEK_API_KEY"), Default: "echo 模式无需配置", Description: "真实模型服务的访问密钥。配置页会显示当前进程拿到的完整值。", Sensitive: true, RestartRequired: true},
			{Key: "provider_base_url", Env: "DEEPSEEK_BASE_URL", Value: envOr("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1"), Default: "https://api.deepseek.com/v1", Description: "OpenAI-compatible API 地址；使用官方 DeepSeek 时通常无需修改。", RestartRequired: true},
			{Key: "workspace", Value: workspace, Description: "文件工具允许访问的工作区根目录。", Status: "当前生效", RestartRequired: true},
		}},
		{ID: "context", Title: "上下文与运行", Description: "控制历史消息、工具结果和单次任务的资源上限。数值越大越灵活，但会增加模型消耗。", Items: []channels.ConfigItem{
			{Key: "max_context_tokens", Env: "SZABOT_MAX_CONTEXT_TOKENS", Value: strconv.Itoa(maxContext), Default: "6000", Description: "单次模型请求的上下文预算；接近上限时会压缩较早历史。", RestartRequired: true},
			{Key: "recent_messages", Env: "SZABOT_CONTEXT_RECENT_MESSAGES", Value: strconv.Itoa(recent), Default: "8", Description: "压缩历史时保留的最近消息条数。", RestartRequired: true},
			{Key: "tool_result_max_tokens", Env: "SZABOT_TOOL_RESULT_MAX_TOKENS", Value: strconv.Itoa(toolMax), Default: "1200", Description: "工具结果超过该近似 token 数时外置到 artifacts，模型按需读取。", RestartRequired: true},
			{Key: "output_reserve_tokens", Env: "SZABOT_CONTEXT_OUTPUT_RESERVE_TOKENS", Value: strconv.Itoa(outputReserve), Default: "1024", Description: "为模型输出预留的上下文空间，不是输出长度上限。", RestartRequired: true},
			{Key: "run_timeout", Env: "SZABOT_RUN_TIMEOUT", Value: runTimeout.String(), Default: "3m", Description: "单次任务最长运行时间，超时会结束 Run。", RestartRequired: true},
			{Key: "max_input_tokens", Env: "SZABOT_MAX_INPUT_TOKENS", Value: limitText(envInt("SZABOT_MAX_INPUT_TOKENS")), Default: "不限", Description: "单次 Run 允许发送给模型的输入 token 上限。", RestartRequired: true},
			{Key: "max_output_tokens", Env: "SZABOT_MAX_OUTPUT_TOKENS", Value: limitText(envInt("SZABOT_MAX_OUTPUT_TOKENS")), Default: "不限", Description: "单次 Run 允许模型生成的输出 token 上限。", RestartRequired: true},
			{Key: "max_total_tokens", Env: "SZABOT_MAX_TOTAL_TOKENS", Value: limitText(envInt("SZABOT_MAX_TOTAL_TOKENS")), Default: "不限", Description: "单次 Run 输入和输出 token 的合计上限。", RestartRequired: true},
			{Key: "max_model_calls", Env: "SZABOT_MAX_MODEL_CALLS", Value: limitText(envInt("SZABOT_MAX_MODEL_CALLS")), Default: "不限", Description: "单次 Run 最多调用模型的次数。", RestartRequired: true},
			{Key: "max_tool_calls", Env: "SZABOT_MAX_TOOL_CALLS", Value: limitText(envInt("SZABOT_MAX_TOOL_CALLS")), Default: "不限", Description: "单次 Run 最多执行工具的次数。", RestartRequired: true},
		}},
		{ID: "memory", Title: "长期记忆", Description: "SQLite 记忆默认可用；向量检索和重排属于可选增强，需要额外服务与模型。", Items: []channels.ConfigItem{
			{Key: "extraction", Env: "SZABOT_MEMORY_EXTRACTION", Value: status(extraction), Default: "真实 Provider 默认启用，echo 默认关闭", Description: "任务完成后从对话中提取事实、偏好和经历。", RestartRequired: true},
			{Key: "session_dir", Env: "SZABOT_SESSION_DIR", Value: sessionRoot, Default: "工作区/sessionlogs", Description: "会话、Trace、artifacts 和 memory.db 的存储根目录。", RestartRequired: true},
			{Key: "memory_timeout", Env: "SZABOT_MEMORY_TIMEOUT", Value: envDuration("SZABOT_MEMORY_TIMEOUT", 30*time.Second).String(), Default: "30s", Description: "记忆提取、Embedding 和索引任务的最长执行时间。", RestartRequired: true},
			{Key: "qdrant", Env: "SZABOT_QDRANT_ENABLED", Value: status(qdrant), Default: "启用", Description: "启用 Qdrant 向量索引。首次运行不需要它，关闭也不影响 SQLite 关键词记忆。", RestartRequired: true},
			{Key: "qdrant_url", Env: "SZABOT_QDRANT_URL", Value: value(qdrantURL), Default: "http://127.0.0.1:6333", Description: "Qdrant 服务地址；本机地址会尝试自动管理 Docker 容器。", RestartRequired: true},
			{Key: "qdrant_collection", Env: "SZABOT_QDRANT_COLLECTION", Value: qdrantCollection, Default: "yomi_memories", Description: "保存记忆向量的 Collection 名称。", RestartRequired: true},
			{Key: "qdrant_auto_start", Env: "SZABOT_QDRANT_AUTO_START", Value: status(qdrantAutoStart), Default: "本机地址默认启用", Description: "Qdrant 地址为本机时，是否自动启动或复用 Docker 容器。", RestartRequired: true},
			{Key: "qdrant_index", Value: status(qdrantIndex), Description: "当前是否已经成功建立 Embedding + Qdrant 索引链路。", Status: "运行状态", RestartRequired: true},
			{Key: "embedding_base_url", Env: "SZABOT_EMBEDDING_BASE_URL", Value: value(embeddingURL), Default: "继承 DEEPSEEK_BASE_URL", Description: "Embedding 服务的 OpenAI-compatible 地址。", RestartRequired: true},
			{Key: "embedding_model", Env: "SZABOT_EMBEDDING_MODEL", Value: value(embeddingModel), Default: "未设置", Description: "把记忆转换成向量的模型名称。", RestartRequired: true},
			{Key: "embedding_api_key", Env: "SZABOT_EMBEDDING_API_KEY", Value: embeddingKeyValue, Default: "继承 DEEPSEEK_API_KEY", Description: "Embedding 服务密钥。配置页会显示当前进程拿到的完整值。", Sensitive: true, RestartRequired: true},
			{Key: "reranker", Env: "SZABOT_RERANKER_ENABLED", Value: status(reranker), Default: "配置模型后自动启用", Description: "对混合召回结果进行更精确的排序。只有 Qdrant + Embedding 已启用时才会执行。", Status: func() string {
				if rerankerRequested && !reranker {
					return "配置不完整"
				}
				return ""
			}(), RestartRequired: true},
			{Key: "reranker_model", Env: "SZABOT_RERANKER_MODEL", Value: value(rerankerModel), Default: "未设置", Description: "Cross-Encoder 重排模型名称，不能用 Embedding 模型替代。", RestartRequired: true},
			{Key: "reranker_base_url", Env: "SZABOT_RERANKER_BASE_URL", Value: value(rerankerURL), Default: "继承 Embedding 地址", Description: "Reranker 的 OpenAI-compatible 地址。", RestartRequired: true},
			{Key: "reranker_api_key", Env: "SZABOT_RERANKER_API_KEY", Value: rerankerKeyValue, Default: "继承 Embedding API key", Description: "Reranker 服务密钥。配置页会显示当前进程拿到的完整值。", Sensitive: true, RestartRequired: true},
			{Key: "reranker_top_n", Env: "SZABOT_RERANKER_TOP_N", Value: strconv.Itoa(envIntDefault("SZABOT_RERANKER_TOP_N", 20)), Default: "20", Description: "送入 Reranker 重排的候选数量。", RestartRequired: true},
		}},
		{ID: "tools", Title: "工具与安全", Description: "文件工具默认可用；联网搜索和 Docker 沙盒需要显式准备依赖。", Items: []channels.ConfigItem{
			{Key: "permission_mode", Env: "SZABOT_PERMISSION_MODE", Value: string(permissionMode()), Default: "safe", Description: "工具写入权限：safe 最严格，workspace_write 允许工作区写入，full 权限最大。", RestartRequired: true},
			{Key: "skills", Env: "SZABOT_SKILLS", Value: value(os.Getenv("SZABOT_SKILLS")), Default: "未启用", Description: "加载工作区 skills；可填 auto 或逗号分隔的技能名。", RestartRequired: true},
			{Key: "web_search", Env: "TAVILY_API_KEY", Value: os.Getenv("TAVILY_API_KEY"), Default: "未配置", Description: "配置 Tavily 密钥后注册 web_search 工具；当前值会完整显示。", Sensitive: true, RestartRequired: true},
			{Key: "sandbox", Env: "SZABOT_SANDBOX", Value: status(os.Getenv("SZABOT_SANDBOX") != ""), Default: "关闭", Description: "启用 Docker 隔离的 bash/python 工具；需要本机 Docker。", RestartRequired: true},
			{Key: "sandbox_network", Env: "SZABOT_SANDBOX_NETWORK", Value: status(os.Getenv("SZABOT_SANDBOX_NETWORK") != ""), Default: "关闭", Description: "允许沙盒容器联网，默认断网。", RestartRequired: true},
			{Key: "python_image", Env: "SZABOT_PYTHON_IMAGE", Value: envOr("SZABOT_PYTHON_IMAGE", "python:3.12-slim"), Default: "python:3.12-slim", Description: "Python 沙盒使用的 Docker 镜像。", RestartRequired: true},
			{Key: "bash_image", Env: "SZABOT_BASH_IMAGE", Value: envOr("SZABOT_BASH_IMAGE", "debian:stable-slim"), Default: "debian:stable-slim", Description: "Bash 沙盒使用的 Docker 镜像。", RestartRequired: true},
			{Key: "sandbox_tmp_size", Env: "SZABOT_SANDBOX_TMP_SIZE", Value: envOr("SZABOT_SANDBOX_TMP_SIZE", "64m"), Default: "64m", Description: "沙盒容器 /tmp 的大小限制。", RestartRequired: true},
		}},
		{ID: "web", Title: "Web 界面", Description: "控制是否启动浏览器界面以及监听地址。", Items: []channels.ConfigItem{
			{Key: "web_enabled", Env: "SZABOT_WEB", Value: status(os.Getenv("SZABOT_WEB") != ""), Default: "关闭", Description: "设置任意非空值后启动 Web 界面；否则使用 CLI。", RestartRequired: true},
			{Key: "web_addr", Env: "SZABOT_WEB_ADDR", Value: envOr("SZABOT_WEB_ADDR", ":8080"), Default: ":8080", Description: "Web HTTP 监听地址。", RestartRequired: true},
		}},
	}}
}

func memoryConfigKeyPresent(value string) bool { return strings.TrimSpace(value) != "" }

func runConfigWizard(workspace, configPath string, settings *configstore.Store) {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	view := buildConfigView("echo", "echo", workspace, sessionDir(workspace), 6000, 8, 1200, 1024, 3*time.Minute, memoryExtractionEnabled(providers.EchoProvider{}), qdrantEnabledByConfig(), strings.TrimSpace(os.Getenv("SZABOT_QDRANT_URL")), envOr("SZABOT_QDRANT_COLLECTION", "yomi_memories"), false, false, envOr("SZABOT_EMBEDDING_BASE_URL", os.Getenv("DEEPSEEK_BASE_URL")), strings.TrimSpace(os.Getenv("SZABOT_EMBEDDING_MODEL")), memoryConfigKeyPresent(os.Getenv("SZABOT_EMBEDDING_API_KEY")), rerankerEnabledByConfig(), false, strings.TrimSpace(os.Getenv("SZABOT_RERANKER_BASE_URL")), strings.TrimSpace(os.Getenv("SZABOT_RERANKER_MODEL")), memoryConfigKeyPresent(os.Getenv("SZABOT_RERANKER_API_KEY")))
	view.Editable = true
	view.File = configPath
	web := &channels.WebChannel{ID: "config", Addr: envOr("YOMI_CONFIG_ADDR", ":8080"), Config: view, ConfigStore: settings}
	if err := web.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "error: start config wizard: %v\n", err)
		return
	}
	addr := envOr("YOMI_CONFIG_ADDR", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "localhost" + addr
	}
	fmt.Printf("yomi configuration wizard. open http://%s/?config=1. saved to %s. Ctrl+C to quit.\n", addr, configPath)
	<-ctx.Done()
}

type memoryRuntimeConfig struct {
	DBPath              string
	ExtractionEnabled   bool
	ExtractionTimeout   time.Duration
	QdrantEnabled       bool
	QdrantURL           string
	QdrantCollection    string
	QdrantAutoStart     bool
	QdrantReady         bool
	QdrantIndexEnabled  bool
	QdrantIndexReason   string
	EmbeddingBaseURL    string
	EmbeddingModel      string
	EmbeddingKeyPresent bool
	HybridSearchEnabled bool
	HybridSearchReason  string
	RerankerRequested   bool
	RerankerEnabled     bool
	RerankerBaseURL     string
	RerankerModel       string
	RerankerKeyPresent  bool
	RerankerTopN        int
}

func printRuntimeConfig(provider providers.Provider, model, workspace, sessionRoot string, maxContextTokens, recentMessages int, summaryTimeout, runTimeout time.Duration, budget agent.RunBudget, memoryConfig memoryRuntimeConfig) {
	fmt.Println("yomi configuration:")
	fmt.Printf("  provider=%s model=%s workspace=%s\n", provider.Name(), model, workspace)
	fmt.Printf("  session_dir=%s\n", sessionRoot)
	fmt.Printf("  context.max_tokens=%d context.recent_messages=%d context.summary_timeout=%s\n", maxContextTokens, recentMessages, summaryTimeout)
	fmt.Printf("  context.tool_result_max_tokens=%d context.output_reserve_tokens=%d\n", envIntDefault("SZABOT_TOOL_RESULT_MAX_TOKENS", 1200), envIntDefault("SZABOT_CONTEXT_OUTPUT_RESERVE_TOKENS", 1024))
	fmt.Printf("  run.timeout=%s\n", runTimeout)
	fmt.Printf("  budget.input_tokens=%s output_tokens=%s total_tokens=%s model_calls=%s tool_calls=%s\n",
		limitText(budget.MaxInputTokens), limitText(budget.MaxOutputTokens), limitText(budget.MaxTotalTokens), limitText(budget.MaxModelCalls), limitText(budget.MaxToolCalls))
	fmt.Printf("  permission_mode=%s\n", permissionMode())
	fmt.Printf("  memory.db=%s sqlite_fts5=enabled extraction=%s extraction_timeout=%s\n",
		memoryConfig.DBPath, enabledText(memoryConfig.ExtractionEnabled), memoryConfig.ExtractionTimeout)
	fmt.Printf("  memory.layers=profile(fact,preference):limit=4 episode(episode):limit=4\n")
	fmt.Printf("  memory.embedding.base_url=%s model=%s api_key=%s\n",
		valueText(memoryConfig.EmbeddingBaseURL), valueText(memoryConfig.EmbeddingModel), configuredText(memoryConfig.EmbeddingKeyPresent))
	fmt.Printf("  memory.retrieval=hybrid=%s reason=%s\n",
		enabledText(memoryConfig.HybridSearchEnabled), memoryConfig.HybridSearchReason)
	fmt.Printf("  memory.reranker.base_url=%s model=%s api_key=%s enabled=%s\n",
		valueText(memoryConfig.RerankerBaseURL), valueText(memoryConfig.RerankerModel), configuredText(memoryConfig.RerankerKeyPresent), enabledText(memoryConfig.RerankerEnabled))
	fmt.Printf("  memory.reranker=requested=%s enabled=%s top_n=%d endpoint=/rerank\n",
		enabledText(memoryConfig.RerankerRequested), enabledText(memoryConfig.RerankerEnabled), memoryConfig.RerankerTopN)
	if !memoryConfig.QdrantEnabled {
		fmt.Println("  memory.qdrant=disabled auto_start=disabled ready=disabled index=disabled reason=SZABOT_QDRANT_ENABLED=off")
	} else if memoryConfig.QdrantURL == "" {
		fmt.Println("  memory.qdrant=disabled auto_start=disabled ready=disabled index=disabled reason=configuration_error")
	} else {
		fmt.Printf("  memory.qdrant.url=%s collection=%s auto_start=%s ready=%s index=%s\n",
			memoryConfig.QdrantURL, memoryConfig.QdrantCollection, enabledText(memoryConfig.QdrantAutoStart), enabledText(memoryConfig.QdrantReady), enabledText(memoryConfig.QdrantIndexEnabled))
		fmt.Printf("  memory.qdrant.index_reason=%s\n", memoryConfig.QdrantIndexReason)
	}
}

func enabledText(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func configuredText(configured bool) string {
	if configured {
		return "configured"
	}
	return "missing"
}

func valueText(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unset"
	}
	return value
}

func limitText(value int) string {
	if value <= 0 {
		return "unlimited"
	}
	return strconv.Itoa(value)
}

func permissionMode() tools.PermissionMode {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("SZABOT_PERMISSION_MODE"))) {
	case string(tools.PermissionWorkspaceWrite):
		return tools.PermissionWorkspaceWrite
	case string(tools.PermissionFull):
		return tools.PermissionFull
	default:
		return tools.PermissionSafe
	}
}

// buildSystemPrompt 组装系统提示：基础说明 + 技能系统的 L1 摘要。
//
// 三层渐进式披露里，这里只负责 L1（元数据）与 always 技能正文的注入：
//   - L1 摘要：列出 name / description / 相对路径，量级极小、固定不变；
//   - always 技能正文：少数需要常驻的技能，直接展开在 system prompt 里；
//   - 普通技能的正文（L2）与子资源（L3）都由 agent 后续用 read_file 按需读取。
//
// 之所以固定拼在 system prompt 且启动后不变，是为了命中 KV Cache：
// 动态内容只应追加在对话末尾，绝不插进前缀破坏缓存。
func buildSystemPrompt(workspace string) string {
	var b strings.Builder
	b.WriteString("You are yomi, a helpful AI assistant with local tools.\n")
	mode := strings.TrimSpace(os.Getenv("SZABOT_SKILLS"))
	if strings.EqualFold(mode, "off") {
		return strings.TrimRight(b.String(), "\n")
	}

	var loader *skills.Loader
	if strings.EqualFold(mode, "auto") {
		loader = skills.NewLoader(workspace)
	} else if mode != "" {
		names := strings.Split(mode, ",")
		loader = skills.NewLoader(workspace, skills.WithEnabled(names...))
	} else {
		return strings.TrimRight(b.String(), "\n")
	}

	if bodies := loader.AlwaysBodies(); bodies != "" {
		b.WriteString("\n# Active Skills\n\n")
		b.WriteString(bodies)
		b.WriteString("\n")
	}

	if summary := loader.Summary(); summary != "" {
		b.WriteString("\n# Skills\n\n")
		b.WriteString("The following skills extend your capabilities. ")
		b.WriteString("To use a skill, read its SKILL.md file (the path in backticks) with the read_file tool, ")
		b.WriteString("then follow its instructions. ")
		b.WriteString("Skills marked (unavailable: ...) need their dependencies installed first.\n\n")
		b.WriteString(summary)
		b.WriteString("\n")
	}

	return strings.TrimRight(b.String(), "\n")
}

// registerTools 把工作区内的本地工具装进 registry，并返回 todo_write 工具引用。
//
// 每个工具都被限制在 workspace 内（沙盒边界），任何创建/注册失败都视为致命错误：
// 工具集是 agent 的能力清单，缺失会让行为不可预测，宁可启动即失败。
//
// 返回 *tools.TodoWriteTool 是为了把它同时接到 Runner.Status 上：todo_write 既是
// 一个普通工具（模型可调用它写清单），也是「Agent 状态栏」的数据源（Runner 每轮
// 读它的进度注入上下文），二者共用同一个实例才能保证读到的就是刚写入的清单。
func registerTools(registry *tools.Registry, workspace string) *tools.TodoWriteTool {
	todoTool, err := tools.NewTodoWrite()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: create todo_write tool: %v\n", err)
		os.Exit(1)
	}

	type factory struct {
		name  string
		build func(string) (tools.Tool, error)
	}
	factories := []factory{
		{"read_file", func(ws string) (tools.Tool, error) { return tools.NewReadFile(ws) }},
		{"write_file", func(ws string) (tools.Tool, error) { return tools.NewWriteFile(ws) }},
		{"edit_file", func(ws string) (tools.Tool, error) { return tools.NewEditFile(ws) }},
		{"list_dir", func(ws string) (tools.Tool, error) { return tools.NewListDir(ws) }},
		{"glob", func(ws string) (tools.Tool, error) { return tools.NewGlob(ws) }},
		{"grep", func(ws string) (tools.Tool, error) { return tools.NewGrep(ws) }},
		// 以下工具与工作区无关，忽略 ws 参数即可，签名保持一致便于统一注册。
		{"web_fetch", func(string) (tools.Tool, error) { return tools.NewWebFetch() }},
		{"todo_write", func(string) (tools.Tool, error) { return todoTool, nil }},
		{"ask_user_question", func(string) (tools.Tool, error) { return tools.NewAskUserQuestion() }},
	}

	for _, f := range factories {
		tool, err := f.build(workspace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: create %s tool: %v\n", f.name, err)
			os.Exit(1)
		}
		if err := registry.Register(tool); err != nil {
			fmt.Fprintf(os.Stderr, "error: register %s tool: %v\n", f.name, err)
			os.Exit(1)
		}
	}

	registerWebSearch(registry)
	registerSandboxTools(registry, workspace)
	return todoTool
}

// registerWebSearch 注册需要 Tavily API key 的 web_search 工具。
//
// 设计取舍：跟 bash/python 一样属于"能力增强"而非"核心必需"。
// 未设置 TAVILY_API_KEY 时只打印提示并跳过，不让程序启动失败——
// 没有 key 的用户仍能用其余工具。
//
// 获取 key：https://tavily.com 免费注册，然后 export TAVILY_API_KEY=tvly-xxxx
func registerWebSearch(registry *tools.Registry) {
	apiKey := os.Getenv("TAVILY_API_KEY")
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "note: TAVILY_API_KEY not set, skipping web_search")
		return
	}
	tool, err := tools.NewWebSearch(apiKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: create web_search tool: %v\n", err)
		return
	}
	if err := registry.Register(tool); err != nil {
		fmt.Fprintf(os.Stderr, "warn: register web_search tool: %v\n", err)
		return
	}
	fmt.Println("web_search enabled (Tavily)")
}

// registerSandboxTools 注册需要 Docker 沙盒的执行类工具（bash / python）。
//
// 设计取舍：这两个工具依赖本机 Docker，属于"能力增强"而非"核心必需"。
// 因此当 SZABOT_SANDBOX 未开启，或 Docker 不可用时，只打印提示并跳过，
// 而不是让整个程序启动失败——没装 Docker 的用户仍能用文件类工具。
//
// 开启方式：
//   - export SZABOT_SANDBOX=1            启用 bash + python
//   - export SZABOT_SANDBOX_NETWORK=1    额外允许容器联网（默认断网）
//   - export SZABOT_PYTHON_IMAGE=...     python 镜像，默认 python:3.12-slim
//   - export SZABOT_BASH_IMAGE=...       bash 镜像，默认 debian:stable-slim
//   - export SZABOT_SANDBOX_TMP_SIZE=... /tmp 大小，默认 64m
func registerSandboxTools(registry *tools.Registry, workspace string) {
	if os.Getenv("SZABOT_SANDBOX") == "" {
		return
	}

	network := os.Getenv("SZABOT_SANDBOX_NETWORK") != ""
	pythonImage := envOr("SZABOT_PYTHON_IMAGE", "python:3.12-slim")
	bashImage := envOr("SZABOT_BASH_IMAGE", "debian:stable-slim")
	tmpSize := envOr("SZABOT_SANDBOX_TMP_SIZE", "64m")

	bashSandbox, err := tools.NewSandbox(tools.SandboxConfig{
		Image:     bashImage,
		Workspace: workspace,
		TmpSize:   tmpSize,
		Network:   network,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: sandbox unavailable, skipping bash/python: %v\n", err)
		return
	}
	pythonSandbox, err := tools.NewSandbox(tools.SandboxConfig{
		Image:     pythonImage,
		Workspace: workspace,
		TmpSize:   tmpSize,
		Network:   network,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: sandbox unavailable, skipping bash/python: %v\n", err)
		return
	}
	fmt.Println("docker daemon available; sandbox execution checks passed")

	bash, err := tools.NewBash(bashSandbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: create bash tool: %v\n", err)
		return
	}
	python, err := tools.NewPython(pythonSandbox)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: create python tool: %v\n", err)
		return
	}

	for name, tool := range map[string]tools.Tool{"bash": bash, "python": python} {
		if err := registry.Register(tool); err != nil {
			fmt.Fprintf(os.Stderr, "warn: register %s tool: %v\n", name, err)
		}
	}
	fmt.Printf("sandbox tools enabled: bash(%s) python(%s) tmp=%s network=%v\n", bashImage, pythonImage, tmpSize, network)
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func rerankerEnabledByConfig() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("SZABOT_RERANKER_ENABLED")))
	if mode == "off" || mode == "0" || mode == "false" {
		return false
	}
	if mode == "on" || mode == "1" || mode == "true" {
		return true
	}
	return strings.TrimSpace(os.Getenv("SZABOT_RERANKER_MODEL")) != ""
}

func memoryExtractionEnabled(provider providers.Provider) bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("SZABOT_MEMORY_EXTRACTION")))
	if mode == "off" || mode == "0" || mode == "false" {
		return false
	}
	if mode == "on" || mode == "1" || mode == "true" {
		return true
	}
	return provider != nil && provider.Name() != "echo"
}

func qdrantAutoStartEnabled(endpoint string) bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("SZABOT_QDRANT_AUTO_START")))
	if mode == "off" || mode == "0" || mode == "false" {
		return false
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	local := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if !local {
		return false
	}
	return true
}

func qdrantEnabledByConfig() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("SZABOT_QDRANT_ENABLED")))
	return mode != "off" && mode != "0" && mode != "false"
}

// sessionDir 决定会话历史（jsonl）的落盘目录。
//
//   - 显式设置 SZABOT_SESSION_DIR 时用它（可为绝对或相对路径）；
//   - 否则默认落在 工作区/sessionlogs，即 szabot 启动目录下的 sessionlogs 子目录，
//     便于随项目查看/清理会话历史。
func sessionDir(workspace string) string {
	if dir := os.Getenv("SZABOT_SESSION_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(workspace, "sessionlogs")
}

func envInt(key string) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		fmt.Fprintf(os.Stderr, "warning: ignore invalid %s=%q\n", key, value)
		return 0
	}
	return parsed
}

func envIntDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		fmt.Fprintf(os.Stderr, "warning: ignore invalid %s=%q\n", key, value)
		return fallback
	}
	return parsed
}

func envDuration(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		fmt.Fprintf(os.Stderr, "warning: ignore invalid %s=%q\n", key, value)
		return fallback
	}
	return parsed
}

// buildProvider 根据环境变量决定用哪个 Provider。
//
// 切换方式（只看 SZABOT_PROVIDER）：
//   - 不设置 / 设为 "echo"  → 用 EchoProvider（默认，零依赖）
//   - 设为 "deepseek"        → 用 DeepSeek（OpenAI 兼容）
//
// DeepSeek 需要的环境变量：
//   - DEEPSEEK_API_KEY  必填
//   - DEEPSEEK_MODEL    可选，默认 "deepseek-chat"
//   - DEEPSEEK_BASE_URL 可选，默认 "https://api.deepseek.com/v1"
func buildProvider() (providers.Provider, string) {
	providerEnv := os.Getenv("SZABOT_PROVIDER")
	fmt.Printf("DEBUG: SZABOT_PROVIDER=%q\n", providerEnv)

	switch providerEnv {
	case "deepseek":
		key := os.Getenv("DEEPSEEK_API_KEY")
		if key == "" {
			fmt.Fprintln(os.Stderr, "error: DEEPSEEK_API_KEY is not set")
			os.Exit(1)
		}
		baseURL := os.Getenv("DEEPSEEK_BASE_URL")
		if baseURL == "" {
			baseURL = "https://api.deepseek.com/v1"
		}
		model := os.Getenv("DEEPSEEK_MODEL")
		if model == "" {
			model = "deepseek-chat"
		}
		fmt.Printf("DEBUG: Using deepseek provider with model=%s\n", model)
		return &providers.OpenAICompatibleProvider{
			ProviderName: "deepseek",
			BaseURL:      baseURL,
			APIKey:       key,
		}, model

	default:
		fmt.Printf("DEBUG: Using default echo provider\n")
		return providers.EchoProvider{}, "echo"
	}
}
