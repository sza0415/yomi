package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/skillreview"
)

// llmExtractor 用一次 LLM 调用把 SKILL.md 正文抽成结构化 Path。
//
// 为什么用 LLM：真实 skill 的"工具"形态五花八门（MCP 调用 mcporter call、
// CLI 命令 kbcli kb-search、工具表格里的 MCP 工具名……），正则启发式抓不全。
// 交给模型读语义、直接产出 PathDefinition 更准。
//
// 复用 szabot 已有的 providers 抽象：只依赖 Provider.Chat 接口，
// 想换 DeepSeek / OpenAI / 本地模型都不用改这里。
type llmExtractor struct {
	provider providers.Provider
	model    string
}

// newLLMExtractor 按与 szabot 一致的环境变量约定构造 Provider。
//
//	SZABOT_PROVIDER=deepseek 且 DEEPSEEK_API_KEY 存在 → 用 DeepSeek 抽取；
//	其余情况（未配置 / echo）→ 返回 nil，调用方回退到正则版 derivePath。
//
// 之所以在"echo"时返回 nil：echo provider 只回声、产不出合法 JSON，
// 走 LLM 抽取只会失败，不如直接回退。
func newLLMExtractor() *llmExtractor {
	switch os.Getenv("SZABOT_PROVIDER") {
	case "deepseek":
		key := os.Getenv("DEEPSEEK_API_KEY")
		if key == "" {
			return nil
		}
		baseURL := envOr("DEEPSEEK_BASE_URL", "https://api.deepseek.com/v1")
		model := envOr("DEEPSEEK_MODEL", "deepseek-chat")
		return &llmExtractor{
			provider: &providers.OpenAICompatibleProvider{
				ProviderName: "deepseek", BaseURL: baseURL, APIKey: key,
			},
			model: model,
		}
	case "openai":
		key := os.Getenv("OPENAI_API_KEY")
		if key == "" {
			return nil
		}
		baseURL := envOr("OPENAI_BASE_URL", "https://api.openai.com/v1")
		model := envOr("OPENAI_MODEL", "gpt-4o-mini")
		return &llmExtractor{
			provider: &providers.OpenAICompatibleProvider{
				ProviderName: "openai", BaseURL: baseURL, APIKey: key,
			},
			model: model,
		}
	default:
		return nil
	}
}

// envOr 返回环境变量，缺省时用 fallback。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// pathExtractSystemPrompt 指导模型把 SKILL.md 抽成一条带分叉的 PathDefinition。
// 关键约束：只输出 JSON、节点类型受限枚举、工具节点必须带 tool 名、
// 判断节点必须用 branches 表达分叉（而不是把所有分支拍平成一条线）。
const pathExtractSystemPrompt = `你是 Skill 评审系统的路径抽取器。给你一个 Skill 的 SKILL.md 正文，
请抽取出这个 Skill 的完整执行路径（Path）。一个 Skill 往往会"根据不同意图/条件走不同分支，
不同分支有不同的工具调用和不同的预期结果"——你必须把这种分叉如实表达成一棵树，
不要拍平成一条直线。只输出一个 JSON 对象，不要任何解释文字、不要 markdown 代码块围栏。

JSON 结构（严格遵守字段名）：
{
  "path_id": "path_<skill名，非字母数字换成下划线>",
  "name": "<skill名> 完整路径",
  "entry_conditions": ["触发该 Skill 的条件/关键词"],
  "nodes": [
    {
      "id": "唯一英文小写下划线id",
      "kind": "input|validation|decision|tool|output|fallback",
      "tool": "工具名(仅 kind=tool 时必填)",
      "condition": "该节点做什么/进入条件",
      "required": true,
      "notes": ["注意事项(可选)"],
      "next": ["线性后继节点id(0或1个；分叉节点不用next，用branches)"],
      "branches": [
        {
          "when": "进入该分支的条件",
          "to": "该分支指向的后继节点id",
          "label": "分支简称",
          "expect": {"type": "输出类型", "contains": ["应包含"], "not_contains": ["禁止包含"]}
        }
      ]
    }
  ],
  "exit": "主出口节点id"
}

抽取规则：
- 第一个节点固定 kind=input、id=match_intent。
- 节点之间用 next（线性）或 branches（分叉）显式连接，形成一棵从 match_intent 出发的树。
- 【分叉是重点】凡是正文里出现"若X则走A，若Y则走B""两条线互斥""按品类/模式分别处理"等，
  必须建一个 kind=decision 节点，用 branches 列出每个互斥分支：
    * when：进入该分支的判断条件；
    * to：该分支的下一个节点id（通常是一个 tool 节点）；
    * expect：该分支最终应产生的预期结果（尽量填 type 和关键 contains，用于区分不同分叉的不同预期）。
  不要把互斥的分支平铺成一串 required=false 的节点。
- 工具调用抽成 kind=tool 节点，tool 填真实工具名，识别三类形态：
    * MCP：mcporter call 'server.tool' → tool 填 tool 部分（如 mcp_exec_sql、kb_search）；
    * CLI：如 kbcli kb-search、kbcli sage → tool 填命令；
    * bash 脚本 → tool 填脚本名。
- "必须先读 references/xxx""先读后调" → kind=validation。
- "重要规则/禁止/不得/铁律" → 放进相关节点/分支的 notes。
- 异常处理/重试/兜底 → kind=fallback。
- 每条分支应各自走到一个 kind=output 节点（不同分叉可有不同的 output 节点与不同 expect）。
- required：核心必经节点 true，仅某分支才走的节点 false。

示例（互斥双线）：一个既能"查数"又能"专家问答"的 Skill，match_intent 后接一个
decision 节点，branches 有两支：一支 when="查数场景" to="run_query"，一支
when="专家问答" to="run_expert"，两支各自走到不同的 output 且 expect 不同。

只输出 JSON。`

// extract 调用 LLM 抽取 Path。失败时返回 error，调用方据此回退到正则版。
func (e *llmExtractor) extract(ctx context.Context, name, md string) (skillreview.PathDefinition, error) {
	if e == nil || e.provider == nil {
		return skillreview.PathDefinition{}, fmt.Errorf("llm extractor unavailable")
	}
	// 给足超时，但不无限等。
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	user := fmt.Sprintf("Skill 名：%s\n\nSKILL.md 正文：\n---\n%s\n---", name, md)
	resp, err := e.provider.Chat(ctx, providers.ChatRequest{
		Model: e.model,
		Messages: []providers.Message{
			{Role: providers.RoleSystem, Content: pathExtractSystemPrompt},
			{Role: providers.RoleUser, Content: user},
		},
	})
	if err != nil {
		return skillreview.PathDefinition{}, fmt.Errorf("llm chat: %w", err)
	}
	path, err := parsePathJSON(resp.Content)
	if err != nil {
		return skillreview.PathDefinition{}, fmt.Errorf("parse llm output: %w", err)
	}
	return path, nil
}

// parsePathJSON 从模型输出里提取并解析出 PathDefinition。
// 模型偶尔会包一层 ```json 围栏或带前后缀文字，这里做容错：截取第一个
// '{' 到最后一个 '}' 之间的内容再解析。
func parsePathJSON(raw string) (skillreview.PathDefinition, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return skillreview.PathDefinition{}, fmt.Errorf("empty output")
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start < 0 || end < 0 || end < start {
		return skillreview.PathDefinition{}, fmt.Errorf("no json object found")
	}
	var path skillreview.PathDefinition
	if err := json.Unmarshal([]byte(s[start:end+1]), &path); err != nil {
		return skillreview.PathDefinition{}, err
	}
	if path.PathID == "" || len(path.Nodes) == 0 {
		return skillreview.PathDefinition{}, fmt.Errorf("incomplete path: missing path_id or nodes")
	}
	return path, nil
}
