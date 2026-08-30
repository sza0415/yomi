package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/memory"
	"github.com/ziangsun/szabot/internal/providers"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

// Loop 是"对外"的那一层：把 bus 入站消息接住，调用 Runner，再把结果推回 bus。
//
// 设计要点：
//   - Loop 不知道 LLM 怎么调（那是 Runner 的事）；
//   - Loop 不知道消息从哪个平台来（那是 Channel 的事）；
//   - Loop 唯一的职责是"协调上下文 + 路由回复"。
//
// 上下文构造（system 恒在最前）：
//
//	system prompt(固定) + 历史(从 Store 按 SessionID 加载) + 本轮 user
//
// 一轮结束后，把"本轮 user"和"assistant 回复"追加回 Store，
// 从而同一 session 的后续请求都能带上此前的对话历史。
//
// ask_user_question 支持（bus 双向通道）：
//   - 工具需要"问用户并等回答"时，不直接碰 stdin，而是通过 Loop 实现的 Asker：
//     Loop 把问题当成一条 OutboundMessage 发给 channel（用户看到问题），
//     并按 SessionID 登记一个等待中的回答通道；
//   - 用户的下一条入站消息进来时，Loop 发现该 session 正在等回答，
//     就不送去 LLM，而是把文本喂给等待中的工具，唤醒它继续。
//   - 这样 CLIChannel 一行不用改，将来任何 channel 都天然复用。
type Loop struct {
	Bus    *bus.MessageBus
	Runner *Runner
	// Store 只按 SessionID 持久化用户可见的主对话（user + final assistant）。
	Store *SessionStore
	// Trace 保存 Run 的内部执行轨迹，不参与下一轮模型上下文。
	Trace tracing.Sink
	// Snapshots 可选地持久化 Run 状态，用于启动时识别上次进程遗留的未完成任务。
	Snapshots RunSnapshotStore
	// RunTimeout 为每个 Run 的总截止时间；零值表示不额外限制。
	RunTimeout time.Duration
	// Budget 是每个新 Run 使用的资源上限；字段为零表示不限。
	Budget RunBudget
	// SystemPrompt 是一段固定的系统提示，作为每轮对话的首条 system 消息。
	SystemPrompt string
	// Context 可选地为长会话启用预算控制和 rolling summary。
	Context *ContextManager
	// Memory is the optional cross-session user memory store. ContextManager
	// performs the scoped lookup; Loop only supplies the inbound UserID.
	Memory          memory.Store
	MemoryExtractor memory.Extractor
	MemoryPolicy    memory.Policy
	MemoryEmbedder  memory.EmbeddingProvider
	MemoryIndexer   memory.Indexer
	MemoryTimeout   time.Duration

	mu       sync.Mutex
	pending  map[string]*pendingAsk
	running  map[string]*runHandle
	queues   map[string][]queuedRun
	draining map[string]bool
}

// runHandle 是一次正在运行的 Run 的取消句柄。
type runHandle struct {
	run    *Run
	cancel context.CancelFunc
}

type queuedRun struct {
	in  bus.InboundMessage
	run *Run
}

// pendingAsk 是一次挂起中的提问：answer 用于把用户回答回传给等待的工具。
type pendingAsk struct {
	answer chan string
}

// Start 起一个 goroutine 持续消费入站消息。
// ctx 取消时退出。
func (l *Loop) Start(ctx context.Context) {
	l.mu.Lock()
	if l.pending == nil {
		l.pending = make(map[string]*pendingAsk)
	}
	if l.running == nil {
		l.running = make(map[string]*runHandle)
	}
	if l.queues == nil {
		l.queues = make(map[string][]queuedRun)
	}
	if l.draining == nil {
		l.draining = make(map[string]bool)
	}
	l.mu.Unlock()
	if l.Snapshots != nil {
		if interrupted, err := l.Snapshots.MarkInterrupted(); err != nil {
			log.Printf("[loop] mark interrupted runs error: %v", err)
		} else if len(interrupted) > 0 {
			log.Printf("[loop] marked %d interrupted run(s) after restart", len(interrupted))
		}
	}
	// 把 Loop 自己作为 Asker 注入给 Runner，供 ask_user_question 工具使用。
	if l.Runner != nil {
		l.Runner.Asker = l
	}
	go l.run(ctx)
}

func (l *Loop) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case in, ok := <-l.Bus.Inbound():
			if !ok {
				return
			}
			// 若该 session 正在等用户回答，这条消息就是"答案"，
			// 直接喂给等待中的工具，不当成新问题送去 LLM。
			if l.deliverAnswer(in.SessionID, in.Text) {
				continue
			}
			// ask_user_question 必须由本循环继续消费答案；普通请求则进入
			// Session FIFO。同 Session 严格串行，不同 Session 仍可并行。
			l.enqueue(ctx, in)
		}
	}
}

func (l *Loop) enqueue(ctx context.Context, in bus.InboundMessage) {
	run := NewRun(in.SessionID, l.Budget)
	item := queuedRun{in: in, run: run}

	l.mu.Lock()
	if l.queues == nil {
		l.queues = make(map[string][]queuedRun)
	}
	if l.draining == nil {
		l.draining = make(map[string]bool)
	}
	l.queues[in.SessionID] = append(l.queues[in.SessionID], item)
	start := !l.draining[in.SessionID]
	if start {
		l.draining[in.SessionID] = true
	}
	l.mu.Unlock()

	l.record(ctx, run, tracing.EventRunQueued, string(RunQueued), map[string]any{
		"input": in.Text, "channel_id": in.ChannelID,
	})
	l.persistSnapshot(run)
	if start {
		go l.drainSession(ctx, in.SessionID)
	}
}

func (l *Loop) drainSession(ctx context.Context, sessionID string) {
	for {
		l.mu.Lock()
		queue := l.queues[sessionID]
		if len(queue) == 0 {
			delete(l.queues, sessionID)
			delete(l.draining, sessionID)
			l.mu.Unlock()
			return
		}
		item := queue[0]
		l.queues[sessionID] = queue[1:]
		l.mu.Unlock()

		l.handleRun(ctx, item.in, item.run)
	}
}

// handle 保留为同步兼容入口，供测试和嵌入调用使用。
func (l *Loop) handle(ctx context.Context, in bus.InboundMessage) {
	l.handleRun(ctx, in, NewRun(in.SessionID, l.Budget))
}

func (l *Loop) transitionRun(ctx context.Context, run *Run, to RunStatus, reason string) error {
	if err := run.Transition(to, reason); err != nil {
		return err
	}
	l.persistSnapshot(run)
	l.record(ctx, run, tracing.EventRunStatusChanged, string(to), map[string]any{
		"reason": reason,
	})
	return nil
}

func (l *Loop) persistSnapshot(run *Run) {
	if l.Snapshots == nil {
		return
	}
	if err := l.Snapshots.Save(run.Snapshot()); err != nil {
		log.Printf("[loop] save run snapshot run=%s error: %v", run.ID, err)
	}
}

func (l *Loop) handleRun(ctx context.Context, in bus.InboundMessage, run *Run) {
	if err := l.transitionRun(ctx, run, RunRunning, "run started"); err != nil {
		log.Printf("[loop] run=%s start transition failed: %v", run.ID, err)
		return
	}
	l.record(ctx, run, tracing.EventRunStarted, string(RunRunning), nil)

	var cancel context.CancelFunc
	// Run context 会在显式取消/超时后立刻变为 Done。终止事件仍必须送到
	// Web/CLI，才能让出站侧收尾当前流并为下一轮重置状态；因此收尾发送
	// 使用一个短超时、脱离 Run 取消信号的 context。
	if l.RunTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, l.RunTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()
	ctx = withRun(ctx, run)

	// 在加载上下文前就登记取消句柄，让显式取消也能中断耗时的历史加载、
	// 上下文压缩或其内部的模型请求。
	handle := &runHandle{run: run, cancel: cancel}
	l.registerRun(in.SessionID, handle)
	defer l.unregisterRun(in.SessionID, handle)

	userMsg := providers.Message{Role: providers.RoleUser, Content: in.Text}

	// 1. 通过 ContextManager 加载并按预算构造本轮上下文。
	var messages []providers.Message
	historyCount := 0
	if l.Context != nil {
		contextManager := l.Context
		if contextManager.Memory == nil && l.Memory != nil {
			configured := *contextManager
			configured.Memory = l.Memory
			contextManager = &configured
		}
		memoryConfigured := contextManager.Memory != nil && strings.TrimSpace(in.UserID) != ""
		if memoryConfigured {
			l.record(ctx, run, tracing.EventMemoryRetrievalStarted, "started", map[string]any{
				"user_id_hash": hashScope(in.UserID), "query_hash": hashScope(in.Text),
			})
		}
		built, err := contextManager.BuildForUser(ctx, in.UserID, in.SessionID, l.SystemPrompt, userMsg)
		if err != nil {
			log.Printf("[loop] build context session=%s error: %v", in.SessionID, err)
			status := RunFailed
			if errors.Is(err, context.DeadlineExceeded) {
				status = RunTimedOut
			} else if errors.Is(err, context.Canceled) {
				status = RunCancelled
			}
			if transitionErr := l.transitionRun(ctx, run, status, err.Error()); transitionErr != nil {
				log.Printf("[loop] run=%s context error transition failed: %v", run.ID, transitionErr)
			}
			run.setError(err)
			l.persistSnapshot(run)
			l.record(ctx, run, tracing.EventRunFinished, string(status), map[string]any{"error": err.Error()})
			l.publishRunDone(ctx, in, run)
			return
		}
		messages, historyCount = built.Messages, built.HistoryCount
		if memoryConfigured {
			if built.MemoryError != "" {
				l.record(ctx, run, tracing.EventMemoryRetrievalFailed, "failed", map[string]any{
					"user_id_hash": hashScope(in.UserID), "query_hash": hashScope(in.Text), "error": built.MemoryError,
				})
			} else {
				l.record(ctx, run, tracing.EventMemoryRetrievalFinished, "completed", map[string]any{
					"user_id_hash": hashScope(in.UserID), "query_hash": hashScope(in.Text), "memory_count": built.MemoryCount, "memory_ids": built.MemoryIDs,
				})
				if built.MemoryCount > 0 {
					l.record(ctx, run, tracing.EventMemoryContextInjected, "completed", map[string]any{
						"user_id_hash": hashScope(in.UserID), "query_hash": hashScope(in.Text),
						"memory_count": built.MemoryCount, "memory_ids": built.MemoryIDs, "estimated_tokens": built.MemoryTokens,
					})
				}
			}
		}
		if built.Compaction != nil {
			c := built.Compaction
			l.record(ctx, run, tracing.EventContextCompacted, "completed", map[string]any{
				"covered_count_before": c.CoveredBefore, "covered_count_after": c.CoveredAfter,
				"estimated_tokens_before": c.BeforeTokens, "estimated_tokens_after": c.AfterTokens,
				"recent_message_count": c.RecentMessages, "summary": c.Summary, "archive_id": c.ArchiveID,
				"summary_duration_ms": c.Duration.Milliseconds(),
			})
			if c.ArchiveID != "" {
				l.record(ctx, run, tracing.EventContextArchived, "completed", map[string]any{
					"archive_id": c.ArchiveID, "covered_count_before": c.CoveredBefore,
					"covered_count_after": c.CoveredAfter, "summary": c.Summary,
				})
			}
			l.record(ctx, run, tracing.EventContextStrategyApplied, "completed", map[string]any{
				"context_id": contextID(ctx, 0), "model_step": 0, "stage": "history",
				"policy": "layered", "trigger": "history_compaction",
				"attempted_layers": []string{"budget_guard", "full_compaction"},
				"selected_layer":   "full_compaction", "action": "summarize",
				"source_ids": []string{}, "archive_ids": nonEmptyStrings(c.ArchiveID),
				"tokens_before": c.BeforeTokens, "tokens_after": c.AfterTokens,
				"reversible": true, "reason": "history exceeded the context budget",
			})
		}
	} else {
		var history []providers.Message
		if l.Store != nil {
			loaded, err := l.Store.Load(in.SessionID)
			if err != nil {
				log.Printf("[loop] load session=%s error: %v", in.SessionID, err)
			} else {
				history = loaded
			}
		}
		if l.SystemPrompt != "" {
			messages = append(messages, providers.Message{Role: providers.RoleSystem, Content: l.SystemPrompt})
			l.record(ctx, run, tracing.EventSystemMessage, "", map[string]any{
				"role": "system", "content": l.SystemPrompt,
			})
		}
		messages = append(messages, history...)
		messages = append(messages, userMsg)
		historyCount = len(history)
	}
	// system 不进 Store，仅在发送前拼接，保证前缀稳定。
	if l.Context != nil && l.SystemPrompt != "" {
		l.record(ctx, run, tracing.EventSystemMessage, "", map[string]any{"role": "system", "content": l.SystemPrompt})
	}
	l.record(ctx, run, tracing.EventInputReceived, "", map[string]any{
		"role": "user", "content": in.Text, "channel_id": in.ChannelID,
		"history_message_count": historyCount,
	})

	// 把回信地址挂到当前 Run Context，供工具进行用户交互。
	runCtx := withRoute(ctx, in.SessionID, in.ChannelID)

	// 出站分片的统一发送器：按 Kind 区分正文 / 推理 / 工具调用 / 工具结果。
	// 都以 Delta=true 的分片形式流过 bus，channel 可据 Kind 分区渲染。
	//
	// decorate 是可选的字段装饰器：工具相关事件用它把 ToolCallID/ToolName/
	// Arguments 等结构化信息填进 OutboundMessage，供出站侧（如 Web 的 AG-UI
	// 翻译）发出合规的 TOOL_CALL_* 事件。正文/推理事件不传 decorate。
	emit := func(kind bus.OutboundKind, text string, decorate func(*bus.OutboundMessage)) {
		// 工具结果即使为空也要发出，否则前端只能看到调用，看不到执行完成。
		if text == "" && kind != bus.KindToolResult {
			return
		}
		sequence := run.nextSequence()
		out := bus.OutboundMessage{
			SessionID: in.SessionID,
			ChannelID: in.ChannelID,
			RunID:     run.ID,
			AgentID:   run.AgentID,
			Sequence:  sequence,
			Text:      text,
			Kind:      kind,
			Delta:     true,
			Time:      time.Now(),
		}
		if decorate != nil {
			decorate(&out)
		}
		// 正文和 reasoning 的 delta 只用于实时传输；完整 assistant message
		// 会在模型步骤结束时通过 assistant.message.completed 写入 Trace。
		// 工具调用/结果是结构化执行事实，需要保留在 Trace 中。
		if kind == bus.KindToolCall || kind == bus.KindToolResult {
			l.recordEvent(ctx, tracing.Event{
				Sequence: sequence, Timestamp: out.Time, SessionID: run.SessionID,
				RunID: run.ID, AgentID: run.AgentID, Type: kindLabel(kind),
				Data: map[string]any{"text": text, "tool_call_id": out.ToolCallID, "tool_name": out.ToolName, "arguments": out.Arguments},
			})
		}
		if err := l.Bus.PublishOutbound(ctx, out); err != nil {
			log.Printf("[loop] publish %s error: %v", kindLabel(kind), err)
		}
	}

	// Runner 把实时事件继续发给 channel；结构化事件只进入 Trace，供历史轨迹 UI 使用。
	sink := newLoopSink(emit)
	sink.OnContext = func(event ContextEvent) {
		l.record(runCtx, run, tracing.EventContextInjected, "", map[string]any{
			"model_step": event.Step, "context_type": event.Type, "content": event.Content,
		})
	}
	sink.OnModelCallStarted = func(event ModelCallEvent) {
		l.record(runCtx, run, tracing.EventModelRequestStarted, string(event.Status), map[string]any{
			"context_id": event.ContextID, "model_step": event.Step, "provider": event.Provider, "model": event.Model,
			"streaming": event.Streaming, "message_count": len(event.Messages),
			"tool_definition_count": event.ToolCount, "tool_definitions": event.Tools,
			"context_budget": event.Budget,
			"messages":       event.Messages,
		})
		l.record(runCtx, run, tracing.EventContextStrategyApplied, "completed", map[string]any{
			"context_id": event.ContextID, "model_step": event.Step, "stage": "pre_model",
			"policy": "layered", "trigger": "none", "attempted_layers": []string{"budget_guard"},
			"selected_layer": "none", "action": "keep", "tokens_before": event.Budget.TotalTokens,
			"tokens_after": event.Budget.TotalTokens, "budget": event.Budget,
		})
	}
	sink.OnModelCallStatus = func(event ModelCallEvent) {
		l.record(runCtx, run, tracing.EventModelStatusChanged, string(event.Status), map[string]any{
			"model_step": event.Step, "provider": event.Provider, "model": event.Model,
			"streaming": event.Streaming, "status_transition": true,
		})
	}
	sink.OnModelCallFinished = func(event ModelCallEvent) {
		eventType, status := tracing.EventModelResponseFinished, string(event.Status)
		data := map[string]any{
			"model_step": event.Step, "provider": event.Provider, "model": event.Model,
			"streaming": event.Streaming, "finish_reason": event.Response.FinishReason,
			"usage": event.Response.Usage, "time_to_first_token_ms": event.FirstToken.Milliseconds(),
			"first_token_reported": event.FirstTokenSet,
		}
		if event.Err != nil {
			eventType, status = tracing.EventModelRequestFailed, string(event.Status)
			data["error"] = event.Err.Error()
		} else if event.Status != ModelFinished {
			status = string(event.Status)
		}
		l.recordDuration(runCtx, run, eventType, status, event.Duration, data)
	}
	sink.OnAssistantMessage = func(event AssistantMessageEvent) {
		l.record(runCtx, run, tracing.EventAssistantCompleted, "completed", map[string]any{
			"model_step": event.Step, "message": event.Message,
		})
	}
	sink.OnToolStarted = func(event ToolExecutionEvent) {
		l.record(runCtx, run, tracing.EventToolExecutionStarted, string(event.Status), map[string]any{
			"tool_call_id": event.Call.ID, "tool_name": event.Call.Name,
			"arguments": string(event.Call.Arguments), "arguments_valid_json": json.Valid(event.Call.Arguments),
		})
	}
	sink.OnToolStatus = func(event ToolExecutionEvent) {
		l.record(runCtx, run, tracing.EventToolStatusChanged, string(event.Status), map[string]any{
			"tool_call_id": event.Call.ID, "tool_name": event.Call.Name,
		})
	}
	sink.OnToolFinished = func(event ToolExecutionEvent) {
		eventType, status := tracing.EventToolExecutionFinished, string(event.Status)
		data := map[string]any{
			"context_id": event.ContextID, "model_step": event.Step,
			"tool_call_id": event.Call.ID, "tool_name": event.Call.Name,
			"arguments": string(event.Call.Arguments), "arguments_valid_json": json.Valid(event.Call.Arguments),
			"result": event.Result, "result_size": len(event.Result),
		}
		if event.Err != nil {
			eventType, status = tracing.EventToolExecutionFailed, "failed"
			if event.Status == ToolTimedOut {
				status = string(ToolTimedOut)
			}
			data["error"] = event.Err.Error()
		}
		l.recordDuration(runCtx, run, eventType, status, event.Duration, data)
		if decision := event.ContextDecision; decision != nil && decision.Layer != "none" {
			strategyData := map[string]any{
				"context_id": event.ContextID, "model_step": event.Step, "stage": decision.Stage,
				"layer": decision.Layer, "action": decision.Action, "source_id": decision.SourceID,
				"trigger": decision.Trigger, "attempted_layers": decision.AttemptedLayers,
				"reason": decision.Reason, "artifact_id": decision.ArtifactID,
				"original_bytes": decision.OriginalBytes, "context_bytes": decision.ContextBytes,
				"tokens_before": decision.TokensBefore, "tokens_after": decision.TokensAfter,
				"reversible": decision.Reversible,
			}
			if decision.ArtifactError != "" {
				strategyData["artifact_error"] = decision.ArtifactError
			}
			// 单条策略事件同时记录上下文决策和实际应用结果。
			strategyData["policy"] = "layered"
			strategyData["source_ids"] = []string{event.Call.ID}
			strategyData["artifact_ids"] = nonEmptyStrings(decision.ArtifactID)
			l.record(runCtx, run, tracing.EventContextStrategyApplied, "completed", strategyData)
			if decision.ArtifactID != "" {
				l.record(runCtx, run, tracing.EventArtifactCreated, "completed", map[string]any{
					"artifact_id": decision.ArtifactID, "tool_call_id": event.Call.ID,
					"tool_name": event.Call.Name, "original_bytes": decision.OriginalBytes,
					"preview_bytes": decision.ContextBytes,
				})
			}
		}
	}

	result, err := l.Runner.RunCollect(runCtx, messages, sink)
	run.setUsage(result.Usage)
	if err != nil {
		status := RunFailed
		switch {
		case errors.Is(err, ErrBudgetExceeded), errors.Is(err, ErrContextBudgetExceeded):
			status = RunBudgetExceeded
		case errors.Is(err, context.DeadlineExceeded):
			status = RunTimedOut
		case errors.Is(err, context.Canceled):
			status = RunCancelled
		}
		if transitionErr := l.transitionRun(ctx, run, status, err.Error()); transitionErr != nil {
			log.Printf("[loop] run=%s terminal transition failed: %v", run.ID, transitionErr)
		}
		run.setError(err)
		l.persistSnapshot(run)
		l.record(ctx, run, tracing.EventRunFinished, string(status), map[string]any{"error": err.Error(), "usage": result.Usage})
		l.publishRunDone(ctx, in, run)
		if status == RunCancelled {
			log.Printf("[loop] session=%s run=%s canceled", in.SessionID, run.ID)
		} else {
			log.Printf("[loop] runner error session=%s run=%s status=%s: %v", in.SessionID, run.ID, status, err)
		}
		return
	}

	// Conversation 只保存用户可见主线；内部推理、工具调用和结果已进入 Trace。
	if l.Store != nil {
		if err := l.Store.Append(in.SessionID,
			userMsg,
			providers.Message{Role: providers.RoleAssistant, Content: result.Answer},
		); err != nil {
			log.Printf("[loop] append session=%s error: %v", in.SessionID, err)
		}
	}

	if err := l.transitionRun(ctx, run, RunCompleted, "answer completed"); err != nil {
		log.Printf("[loop] run=%s completion transition failed: %v", run.ID, err)
		return
	}
	l.startMemoryExtraction(ctx, run, in, result.Answer)
	l.record(ctx, run, tracing.EventRunFinished, string(RunCompleted), map[string]any{"usage": result.Usage, "answer": result.Answer, "memory": run.Snapshot().Memory})

	l.publishRunDone(ctx, in, run)
}

// publishRunDone 发送一轮 Run 的出站收尾标记。它不能使用已被取消的
// Run Context，否则显式取消后 Done 会被 PublishOutbound 直接丢弃，导致
// Web 的 AG-UI 翻译器一直持有旧 Run 状态，下一轮消息无法正确分界。
func (l *Loop) publishRunDone(runCtx context.Context, in bus.InboundMessage, run *Run) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), time.Second)
	defer cancel()
	done := bus.OutboundMessage{
		SessionID: in.SessionID,
		ChannelID: in.ChannelID,
		RunID:     run.ID,
		AgentID:   run.AgentID,
		Sequence:  run.nextSequence(),
		Done:      true,
		Time:      time.Now(),
	}
	if err := l.Bus.PublishOutbound(ctx, done); err != nil {
		log.Printf("[loop] publish done error run=%s: %v", run.ID, err)
	}
}

// newLoopSink 把统一的 emit 发送器适配成 Runner 需要的 StreamSink：
// 每类事件各走一条对应 Kind 的出站分片。工具调用/结果除了格式化成简短可读
// 的 Text（供 CLI 等直接展示），还通过 decorate 把结构化字段（ToolCallID/
// ToolName/Arguments）填进 OutboundMessage，供 Web 出站侧翻译成 AG-UI 的
// TOOL_CALL_* 事件（这些事件以 toolCallId 为核心配对）。
func newLoopSink(emit func(bus.OutboundKind, string, func(*bus.OutboundMessage))) StreamSink {
	return StreamSink{
		OnContentDelta: func(delta string) { emit(bus.KindAnswer, delta, nil) },
		OnReasoningDelta: func(delta string) {
			emit(bus.KindReasoning, delta, nil)
		},
		OnToolCall: func(call providers.ToolCall) {
			emit(bus.KindToolCall, formatToolCall(call), func(o *bus.OutboundMessage) {
				o.ToolCallID = call.ID
				o.ToolName = call.Name
				o.Arguments = string(call.Arguments)
			})
		},
		OnToolResult: func(call providers.ToolCall, result string) {
			emit(bus.KindToolResult, formatToolResult(call, result), func(o *bus.OutboundMessage) {
				o.ToolCallID = call.ID
				o.ToolName = call.Name
			})
		},
	}
}

// formatToolCall 把一次工具调用渲染成 "name(arguments)" 形式。
func formatToolCall(call providers.ToolCall) string {
	args := string(call.Arguments)
	if args == "" || args == "null" {
		return call.Name + "()"
	}
	return call.Name + "(" + args + ")"
}

// formatToolResult 把一次工具结果渲染成 "name -> result" 形式，
// 过长时截断，避免刷屏。
func formatToolResult(call providers.ToolCall, result string) string {
	const maxResultRunes = 500
	trimmed := result
	if r := []rune(result); len(r) > maxResultRunes {
		trimmed = string(r[:maxResultRunes]) + "…(truncated)"
	}
	return call.Name + " -> " + trimmed
}

// kindLabel 给日志用的可读标签。
func kindLabel(kind bus.OutboundKind) string {
	switch kind {
	case bus.KindReasoning:
		return "reasoning"
	case bus.KindToolCall:
		return "tool_call"
	case bus.KindToolResult:
		return "tool_result"
	default:
		return "delta"
	}
}

func nonEmptyStrings(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	return []string{value}
}

// Ask 实现 tools.Asker：把问题发给 channel，然后阻塞等用户回答。
//
// route（SessionID/ChannelID）从 ctx 里取——它由 handle 在进入 Runner 前塞入。
//
// 出站的问题消息带 bus.KindQuestion 标记，并把结构化 options 放进 Meta，
// 好让 channel 分别处理：CLI 直接显示拼好的文本，Web 可据 options 渲染
// 可点击的选项按钮，并提示用户输入回答（而非把本轮当作已结束）。
func (l *Loop) Ask(ctx context.Context, question string, options []string) (string, error) {
	sessionID, channelID, ok := routeFrom(ctx)
	if !ok || sessionID == "" {
		return "", fmt.Errorf("ask_user_question: no session route in context")
	}

	wait := &pendingAsk{answer: make(chan string, 1)}

	l.mu.Lock()
	if _, busy := l.pending[sessionID]; busy {
		l.mu.Unlock()
		return "", fmt.Errorf("ask_user_question: session %q already has a pending question", sessionID)
	}
	l.pending[sessionID] = wait
	l.mu.Unlock()

	// 无论如何退出都要清理登记，避免 session 永久卡在"等回答"。
	defer func() {
		l.mu.Lock()
		if l.pending[sessionID] == wait {
			delete(l.pending, sessionID)
		}
		l.mu.Unlock()
	}()

	// 1. 把问题作为一条出站消息发给 channel（用户就看到了）。
	//    Text 含拼好的选项文本（CLI 用）；Meta.options 为结构化数组（Web 用）。
	out := bus.OutboundMessage{
		SessionID: sessionID,
		ChannelID: channelID,
		Text:      renderQuestion(question, options),
		Kind:      bus.KindQuestion,
		Time:      time.Now(),
	}
	if run, ok := runFrom(ctx); ok {
		out.RunID = run.ID
		out.AgentID = run.AgentID
		out.Sequence = run.nextSequence()
	}
	if len(options) > 0 {
		out.Meta = map[string]any{"options": options}
	}
	if err := l.Bus.PublishOutbound(ctx, out); err != nil {
		if run, ok := runFrom(ctx); ok {
			_ = l.transitionRun(ctx, run, RunRunning, "question delivery failed")
		}
		return "", fmt.Errorf("ask_user_question: send question: %w", err)
	}
	if run, ok := runFrom(ctx); ok {
		if err := l.transitionRun(ctx, run, RunWaitingUser, "waiting for user answer"); err != nil {
			return "", err
		}
		l.recordEvent(ctx, tracing.Event{
			Sequence: out.Sequence, Timestamp: out.Time, SessionID: run.SessionID,
			RunID: run.ID, AgentID: run.AgentID, Type: tracing.EventUserQuestionAsked, Status: string(RunWaitingUser),
			Data: map[string]any{"question": question, "options": options},
		})
	}

	// 2. 阻塞等用户的下一条消息被 run() 路由过来。
	waitStarted := time.Now()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case answer := <-wait.answer:
		if run, ok := runFrom(ctx); ok {
			if err := l.transitionRun(ctx, run, RunRunning, "user answer received"); err != nil {
				return "", err
			}
			l.recordDuration(ctx, run, tracing.EventUserQuestionAnswered, string(RunRunning), time.Since(waitStarted), map[string]any{
				"answer": answer,
			})
		}
		return answer, nil
	}
}

// renderQuestion 把问题与可选项拼成一段纯文本，供不支持结构化选项的 channel
// （如 CLI）直接展示。
func renderQuestion(question string, options []string) string {
	if len(options) == 0 {
		return question
	}
	var b strings.Builder
	b.WriteString(question)
	b.WriteString("\n可选项：")
	for i, opt := range options {
		fmt.Fprintf(&b, "\n  %d. %s", i+1, opt)
	}
	b.WriteString("\n（可直接回复选项序号或内容）")
	return b.String()
}

// deliverAnswer 若 sessionID 正在等回答，则把 text 交给等待的工具并返回 true；
// 否则返回 false，表示这是一条正常的新消息。
func (l *Loop) deliverAnswer(sessionID, text string) bool {
	l.mu.Lock()
	wait, ok := l.pending[sessionID]
	l.mu.Unlock()

	if !ok {
		return false
	}
	// 保持 pending 登记直到 Ask 消费回答并执行 defer 清理。这样在
	// 前端重复提交/重连时，后续文本不会被误当成一个新的 Run。
	select {
	case wait.answer <- text:
	default:
		// 已经有回答在等待消费，丢弃重复提交但仍视为已路由。
	}
	return true
}

// registerRun 登记一次正在运行的 handle 的取消句柄。
func (l *Loop) registerRun(sessionID string, h *runHandle) {
	l.mu.Lock()
	if l.running == nil {
		l.running = make(map[string]*runHandle)
	}
	l.running[sessionID] = h
	l.mu.Unlock()
}

// unregisterRun 在 handle 结束时注销其取消句柄。
// 校验当前登记的是否仍是自己，避免误删同 session 后来任务登记的句柄。
func (l *Loop) unregisterRun(sessionID string, h *runHandle) {
	l.mu.Lock()
	if l.running[sessionID] == h {
		delete(l.running, sessionID)
	}
	l.mu.Unlock()
}

// CancelSession 取消某个 session 当前运行的任务（中断下游 Runner/LLM）。
// 这是显式的用户取消入口；连接断开不会调用它。找不到运行中的任务
// （已结束/从未开始）时静默返回。
func (l *Loop) CancelSession(sessionID string) {
	l.mu.Lock()
	h := l.running[sessionID]
	l.mu.Unlock()

	if h != nil {
		h.cancel()
	}
}

func (l *Loop) record(ctx context.Context, run *Run, eventType, status string, data map[string]any) {
	l.recordDuration(ctx, run, eventType, status, 0, data)
}

func (l *Loop) recordDuration(ctx context.Context, run *Run, eventType, status string, duration time.Duration, data map[string]any) {
	if run == nil {
		return
	}
	l.recordEvent(ctx, tracing.Event{
		Sequence: run.nextSequence(), Timestamp: time.Now(), SessionID: run.SessionID,
		RunID: run.ID, AgentID: run.AgentID, Type: eventType, Status: status,
		DurationMS: duration.Milliseconds(), Data: data,
	})
}

func (l *Loop) recordEvent(ctx context.Context, event tracing.Event) {
	if l.Trace == nil {
		return
	}
	if err := l.Trace.Record(ctx, event); err != nil {
		log.Printf("[trace] run=%s sequence=%d type=%s error: %v", event.RunID, event.Sequence, event.Type, err)
	}
}
