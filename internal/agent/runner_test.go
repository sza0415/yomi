package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
)

type scriptedProvider struct {
	responses []providers.ChatResponse
	requests  []providers.ChatRequest
}

func (p *scriptedProvider) Name() string { return "scripted" }

func (p *scriptedProvider) Chat(_ context.Context, request providers.ChatRequest) (providers.ChatResponse, error) {
	p.requests = append(p.requests, request)
	response := p.responses[0]
	p.responses = p.responses[1:]
	return response, nil
}

// streamingScriptedProvider 在 scriptedProvider 基础上实现 StreamingProvider，
// 把脚本回复的正文按 rune 逐段回调，用于验证 RunStream 的增量透传。
type streamingScriptedProvider struct {
	scriptedProvider
}

func (p *streamingScriptedProvider) ChatStream(
	_ context.Context,
	request providers.ChatRequest,
	onChunk func(providers.StreamChunk) error,
) (providers.ChatResponse, error) {
	p.requests = append(p.requests, request)
	response := p.responses[0]
	p.responses = p.responses[1:]

	for _, r := range response.Content {
		if err := onChunk(providers.StreamChunk{ContentDelta: string(r)}); err != nil {
			return providers.ChatResponse{}, err
		}
	}
	if err := onChunk(providers.StreamChunk{
		Done:         true,
		ToolCalls:    response.ToolCalls,
		FinishReason: response.FinishReason,
	}); err != nil {
		return providers.ChatResponse{}, err
	}
	return response, nil
}

type echoTool struct{}

func (echoTool) Name() string        { return "echo_tool" }
func (echoTool) Description() string { return "Echo one value." }
func (echoTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"]}`)
}
func (echoTool) Execute(_ context.Context, arguments json.RawMessage) (string, error) {
	var input struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(arguments, &input); err != nil {
		return "", err
	}
	return "tool result: " + input.Value, nil
}

type largeTool struct{}

func (largeTool) Name() string        { return "large_tool" }
func (largeTool) Description() string { return "Return a large deterministic result." }
func (largeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (largeTool) Execute(context.Context, json.RawMessage) (string, error) {
	return strings.Repeat("0123456789abcdef", 400), nil
}

type retryProvider struct {
	attempts int
	err      error
}

func (p *retryProvider) Name() string { return "retry-provider" }
func (p *retryProvider) Chat(context.Context, providers.ChatRequest) (providers.ChatResponse, error) {
	p.attempts++
	if p.attempts == 1 && p.err != nil {
		return providers.ChatResponse{}, p.err
	}
	return providers.ChatResponse{Content: "ok"}, nil
}

type retryTool struct{ attempts int }

func (t *retryTool) Name() string                { return "retry_tool" }
func (t *retryTool) Description() string         { return "retry tool" }
func (t *retryTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *retryTool) Retryable(error) bool        { return true }
func (t *retryTool) Execute(context.Context, json.RawMessage) (string, error) {
	t.attempts++
	if t.attempts == 1 {
		return "", errors.New("temporary")
	}
	return "done", nil
}

func TestRunnerRetriesRetryableProviderError(t *testing.T) {
	provider := &retryProvider{err: providers.NewProviderError(providers.ErrorRetryable, errors.New("temporary"))}
	runner := &Runner{Provider: provider, Model: "test", Retry: RetryPolicy{MaxAttempts: 2}}
	result, err := runner.RunCollect(context.Background(), nil, StreamSink{})
	if err != nil || result.Answer != "ok" || provider.attempts != 2 || result.Usage.ModelCalls != 2 {
		t.Fatalf("result=%#v err=%v attempts=%d", result, err, provider.attempts)
	}
}

func TestRunnerDoesNotRetryNonRetryableProviderError(t *testing.T) {
	provider := &retryProvider{err: providers.NewProviderError(providers.ErrorNonRetryable, errors.New("bad request"))}
	runner := &Runner{Provider: provider, Model: "test", Retry: RetryPolicy{MaxAttempts: 3}}
	_, err := runner.RunCollect(context.Background(), nil, StreamSink{})
	if err == nil || provider.attempts != 1 {
		t.Fatalf("err=%v attempts=%d", err, provider.attempts)
	}
}

func TestRunnerRetriesOptedInTool(t *testing.T) {
	tool := &retryTool{}
	registry := tools.NewRegistry()
	if err := registry.Register(tool); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{ToolCalls: []providers.ToolCall{{ID: "call_1", Name: "retry_tool", Arguments: json.RawMessage(`{}`)}}},
		{Content: "finished"},
	}}
	runner := &Runner{Provider: provider, Model: "test", Tools: registry, ToolRetry: RetryPolicy{MaxAttempts: 2}}
	answer, err := runner.Run(context.Background(), nil)
	if err != nil || answer != "finished" || tool.attempts != 2 {
		t.Fatalf("answer=%q err=%v attempts=%d", answer, err, tool.attempts)
	}
}

func TestRunnerExecutesToolAndContinuesConversation(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{
			ToolCalls: []providers.ToolCall{{
				ID:        "call_1",
				Name:      "echo_tool",
				Arguments: json.RawMessage(`{"value":"hello"}`),
			}},
		},
		{Content: "final answer"},
	}}
	runner := &Runner{Provider: provider, Model: "test", Tools: registry}

	answer, err := runner.Run(context.Background(), []providers.Message{{
		Role: providers.RoleUser, Content: "use the tool",
	}})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if answer != "final answer" {
		t.Fatalf("Run() answer = %q, want final answer", answer)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("Chat calls = %d, want 2", len(provider.requests))
	}
	if len(provider.requests[0].Tools) != 1 || provider.requests[0].Tools[0].Name != "echo_tool" {
		t.Fatalf("first request tools = %#v, want echo_tool", provider.requests[0].Tools)
	}

	messages := provider.requests[1].Messages
	if len(messages) != 3 {
		t.Fatalf("second request messages = %d, want 3", len(messages))
	}
	if len(messages[1].ToolCalls) != 1 || messages[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("assistant tool calls = %#v, want call_1", messages[1].ToolCalls)
	}
	if messages[2].Role != providers.RoleTool || messages[2].ToolCallID != "call_1" || messages[2].Content != "tool result: hello" {
		t.Fatalf("tool result message = %#v", messages[2])
	}
}

func TestRunnerExternalizesLargeToolResult(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(largeTool{}); err != nil {
		t.Fatal(err)
	}
	store, err := tools.NewArtifactStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{ToolCalls: []providers.ToolCall{{ID: "large_1", Name: "large_tool", Arguments: json.RawMessage(`{}`)}}},
		{Content: "artifact handled"},
	}}
	runner := &Runner{
		Provider:            provider,
		Model:               "test",
		Tools:               registry,
		Artifacts:           store,
		ToolResultMaxTokens: 8,
		MaxContextTokens:    200,
		OutputReserveTokens: 20,
	}
	var decision *ToolContextDecision
	run := NewRun("s", RunBudget{})
	ctx := withRun(withRoute(context.Background(), "s", "test"), run)
	result, err := runner.RunCollect(ctx, []providers.Message{{Role: providers.RoleUser, Content: "use large tool"}}, StreamSink{
		OnToolFinished: func(event ToolExecutionEvent) { decision = event.ContextDecision },
	})
	if err != nil || result.Answer != "artifact handled" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if decision == nil || decision.Layer != "artifact" || decision.ArtifactID == "" {
		t.Fatalf("decision = %#v", decision)
	}
	if len(provider.requests) != 2 {
		t.Fatalf("requests = %d, want 2", len(provider.requests))
	}
	toolMessage := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if !strings.Contains(toolMessage.Content, decision.ArtifactID) {
		t.Fatalf("tool message = %q, want artifact id %q", toolMessage.Content, decision.ArtifactID)
	}
	if strings.Contains(toolMessage.Content, strings.Repeat("0123456789abcdef", 20)) {
		t.Fatal("second request should not contain the complete tool result")
	}
	full, err := store.Read(context.Background(), "s", decision.ArtifactID, 0, 10000)
	if err != nil || len(full) != 6400 {
		t.Fatalf("artifact full result len=%d err=%v, want 6400", len(full), err)
	}
}

// TestRunStreamDeliversDeltas 验证：provider 支持流式时，RunStream 会把最终
// 答案的正文按增量回调出去，且返回的完整答案与增量拼接一致。
func TestRunStreamDeliversDeltas(t *testing.T) {
	provider := &streamingScriptedProvider{
		scriptedProvider: scriptedProvider{responses: []providers.ChatResponse{
			{Content: "你好世界"},
		}},
	}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}

	var got string
	answer, err := runner.RunStream(context.Background(),
		[]providers.Message{{Role: providers.RoleUser, Content: "hi"}},
		func(delta string) { got += delta })
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	if answer != "你好世界" {
		t.Fatalf("answer = %q, want 你好世界", answer)
	}
	if got != "你好世界" {
		t.Fatalf("streamed deltas = %q, want 你好世界", got)
	}
}

func TestRunStreamReportsModelStatus(t *testing.T) {
	provider := &streamingScriptedProvider{
		scriptedProvider: scriptedProvider{responses: []providers.ChatResponse{{Content: "ok", FinishReason: "stop"}}},
	}
	runner := &Runner{Provider: provider, Model: "test"}
	var started, transitions, finished []ModelStatus
	_, err := runner.RunCollect(context.Background(), nil, StreamSink{
		OnContentDelta: func(string) {},
		OnModelCallStarted: func(event ModelCallEvent) {
			started = append(started, event.Status)
		},
		OnModelCallStatus: func(event ModelCallEvent) {
			transitions = append(transitions, event.Status)
		},
		OnModelCallFinished: func(event ModelCallEvent) {
			finished = append(finished, event.Status)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 1 || started[0] != ModelRequesting {
		t.Fatalf("model start status = %#v", started)
	}
	if len(transitions) != 1 || transitions[0] != ModelStreaming {
		t.Fatalf("model transition status = %#v", transitions)
	}
	if len(finished) != 1 || finished[0] != ModelFinished {
		t.Fatalf("model finish status = %#v", finished)
	}
}

// TestRunStreamFallbackWhenNotStreaming 验证：provider 不支持流式时，
// RunStream 回退到 Chat，并把完整正文当作一个增量回调出去。
func TestRunStreamFallbackWhenNotStreaming(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.ChatResponse{{Content: "plain"}}}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}

	var got string
	answer, err := runner.RunStream(context.Background(),
		[]providers.Message{{Role: providers.RoleUser, Content: "hi"}},
		func(delta string) { got += delta })
	if err != nil {
		t.Fatalf("RunStream() error = %v", err)
	}
	if answer != "plain" || got != "plain" {
		t.Fatalf("answer=%q streamed=%q, want both plain", answer, got)
	}
}

// TestRunCollectCapturesReasoningAndToolMessages 是本次改动的核心测试：
// RunCollect 必须把推理过程、工具调用、工具结果都收进 RunResult.Messages，
// 并通过 StreamSink 把各类事件实时汇报出去，而不再只剩最终正文。
func TestRunCollectCapturesReasoningAndToolMessages(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{
			Reasoning: "先想想需要调用工具",
			ToolCalls: []providers.ToolCall{{
				ID:        "call_1",
				Name:      "echo_tool",
				Arguments: json.RawMessage(`{"value":"hello"}`),
			}},
		},
		{Content: "final answer", Reasoning: "拿到结果后总结"},
	}}
	runner := &Runner{Provider: provider, Model: "test", Tools: registry}

	var (
		reasoning   string
		toolCalls   []string
		toolResults []string
	)
	result, err := runner.RunCollect(context.Background(),
		[]providers.Message{{Role: providers.RoleUser, Content: "use the tool"}},
		StreamSink{
			OnReasoningDelta: func(s string) { reasoning += s },
			OnToolCall:       func(c providers.ToolCall) { toolCalls = append(toolCalls, c.Name) },
			OnToolResult:     func(_ providers.ToolCall, r string) { toolResults = append(toolResults, r) },
		})
	if err != nil {
		t.Fatalf("RunCollect() error = %v", err)
	}

	if result.Answer != "final answer" {
		t.Fatalf("answer = %q, want final answer", result.Answer)
	}

	// 事件回调：两轮的推理都应汇报，工具调用/结果各一次。
	if reasoning != "先想想需要调用工具拿到结果后总结" {
		t.Fatalf("reasoning stream = %q", reasoning)
	}
	if len(toolCalls) != 1 || toolCalls[0] != "echo_tool" {
		t.Fatalf("tool calls = %#v, want [echo_tool]", toolCalls)
	}
	if len(toolResults) != 1 || toolResults[0] != "tool result: hello" {
		t.Fatalf("tool results = %#v", toolResults)
	}

	// 完整消息序列：assistant(带推理+tool_calls) → tool 结果 → assistant(最终答案)。
	if len(result.Messages) != 3 {
		t.Fatalf("result messages = %d, want 3: %#v", len(result.Messages), result.Messages)
	}
	first := result.Messages[0]
	if first.Role != providers.RoleAssistant || first.Reasoning != "先想想需要调用工具" ||
		len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != "call_1" {
		t.Fatalf("messages[0] = %#v", first)
	}
	second := result.Messages[1]
	if second.Role != providers.RoleTool || second.ToolCallID != "call_1" || second.Content != "tool result: hello" {
		t.Fatalf("messages[1] = %#v", second)
	}
	third := result.Messages[2]
	if third.Role != providers.RoleAssistant || third.Content != "final answer" || third.Reasoning != "拿到结果后总结" {
		t.Fatalf("messages[2] = %#v", third)
	}
}

func TestRunCollectEmitsStructuredTraceEvents(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{
			FinishReason: "tool_calls",
			Usage:        providers.Usage{InputTokens: 4, TotalTokens: 4, Reported: true},
			ToolCalls: []providers.ToolCall{{
				ID: "call_1", Name: "echo_tool", Arguments: json.RawMessage(`{"value":"hello"}`),
			}},
		},
		{Content: "done", FinishReason: "stop"},
	}}
	status := &fakeStatus{line: "working"}
	runner := &Runner{Provider: provider, Model: "test-model", Tools: registry, Status: status}

	var started, finished []ModelCallEvent
	var assistants []AssistantMessageEvent
	var contexts []ContextEvent
	var toolStarted, toolFinished []ToolExecutionEvent
	var toolStatuses []ToolStatus
	_, err := runner.RunCollect(withRoute(context.Background(), "s1", "cli"),
		[]providers.Message{{Role: providers.RoleUser, Content: "go"}},
		StreamSink{
			OnModelCallStarted:  func(event ModelCallEvent) { started = append(started, event) },
			OnModelCallFinished: func(event ModelCallEvent) { finished = append(finished, event) },
			OnAssistantMessage:  func(event AssistantMessageEvent) { assistants = append(assistants, event) },
			OnContext:           func(event ContextEvent) { contexts = append(contexts, event) },
			OnToolStarted:       func(event ToolExecutionEvent) { toolStarted = append(toolStarted, event) },
			OnToolStatus:        func(event ToolExecutionEvent) { toolStatuses = append(toolStatuses, event.Status) },
			OnToolFinished:      func(event ToolExecutionEvent) { toolFinished = append(toolFinished, event) },
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(started) != 2 || started[0].Step != 1 || started[1].Step != 2 || started[0].Model != "test-model" {
		t.Fatalf("model started = %#v", started)
	}
	if started[0].Status != ModelRequesting || started[1].Status != ModelRequesting {
		t.Fatalf("model start statuses = %#v", started)
	}
	if len(started[0].Tools) != 1 || started[0].Tools[0].Name != "echo_tool" || len(started[0].Tools[0].Parameters) == 0 {
		t.Fatalf("model tool definitions = %#v", started[0].Tools)
	}
	if len(started[1].Tools) != 1 || started[1].Tools[0].Name != "echo_tool" {
		t.Fatalf("second model tool definitions = %#v", started[1].Tools)
	}
	if len(finished) != 2 || finished[0].Response.FinishReason != "tool_calls" || finished[1].Response.FinishReason != "stop" {
		t.Fatalf("model finished = %#v", finished)
	}
	if finished[0].Status != ModelFinished || finished[1].Status != ModelFinished {
		t.Fatalf("model finish statuses = %#v", finished)
	}
	if len(assistants) != 2 || len(assistants[0].Message.ToolCalls) != 1 || assistants[1].Message.Content != "done" {
		t.Fatalf("assistants = %#v", assistants)
	}
	if len(contexts) != 2 || contexts[0].Type != "agent_status" || contexts[0].Content != "working" {
		t.Fatalf("contexts = %#v", contexts)
	}
	if len(toolStarted) != 1 || len(toolFinished) != 1 || toolFinished[0].Result != "tool result: hello" || toolFinished[0].Err != nil {
		t.Fatalf("tool events = %#v / %#v", toolStarted, toolFinished)
	}
	if toolStarted[0].Status != ToolRunning || toolFinished[0].Status != ToolSucceeded {
		t.Fatalf("tool statuses = %#v / %#v", toolStarted, toolFinished)
	}
	if len(toolStatuses) != 3 || toolStatuses[0] != ToolPending || toolStatuses[1] != ToolRunning || toolStatuses[2] != ToolSucceeded {
		t.Fatalf("tool status transitions = %#v", toolStatuses)
	}
}

// fakeStatus 是一个可控的 StatusProvider 桩，用于单独验证注入行为。
type fakeStatus struct {
	line     string
	sessions []string // 记录每次被查询的 sessionID
}

func (f *fakeStatus) StatusLine(sessionID string) string {
	f.sessions = append(f.sessions, sessionID)
	return f.line
}

// TestRunnerInjectsStatusLineEveryTurn 验证「Agent 状态栏」的核心契约：
//   - Runner.Status 非空且返回文本时，每一轮发给 Provider 的消息末尾都追加
//     一条 user 角色的状态栏消息（借用 user 槽位挂系统状态，不改 system 前缀）；
//   - 状态栏只在发送时临时拼接，绝不进入 RunResult.Messages（不落盘）；
//   - 每一轮都重新查询一次状态源（体现"每个 turn 重新生成状态"）。
func TestRunnerInjectsStatusLineEveryTurn(t *testing.T) {
	registry := tools.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{ToolCalls: []providers.ToolCall{{
			ID:        "call_1",
			Name:      "echo_tool",
			Arguments: json.RawMessage(`{"value":"hi"}`),
		}}},
		{Content: "done"},
	}}
	status := &fakeStatus{line: "任务清单 1/2 已完成\n[x] A\n[~] B"}
	runner := &Runner{Provider: provider, Model: "test", Tools: registry, Status: status}

	ctx := withRoute(context.Background(), "cli:local", "cli")
	result, err := runner.RunCollect(ctx,
		[]providers.Message{{Role: providers.RoleUser, Content: "go"}},
		StreamSink{})
	if err != nil {
		t.Fatalf("RunCollect() error = %v", err)
	}

	// 两轮 chatOnce，两次请求，每次末尾都应带状态栏 user 消息。
	if len(provider.requests) != 2 {
		t.Fatalf("Chat calls = %d, want 2", len(provider.requests))
	}
	for i, req := range provider.requests {
		last := req.Messages[len(req.Messages)-1]
		if last.Role != providers.RoleUser {
			t.Fatalf("request[%d] last message role = %q, want user", i, last.Role)
		}
		if !strings.Contains(last.Content, "任务清单 1/2 已完成") || !strings.Contains(last.Content, "Agent 状态栏") {
			t.Fatalf("request[%d] status line = %q", i, last.Content)
		}
	}

	// 状态源每轮都被查询一次，且用的是路由里的 sessionID。
	if len(status.sessions) != 2 {
		t.Fatalf("status queried %d times, want 2", len(status.sessions))
	}
	for _, s := range status.sessions {
		if s != "cli:local" {
			t.Fatalf("status queried with session %q, want cli:local", s)
		}
	}

	// 状态栏绝不落盘：RunResult.Messages 里不应出现任何状态栏内容。
	for _, m := range result.Messages {
		if strings.Contains(m.Content, "Agent 状态栏") {
			t.Fatalf("status line leaked into persisted messages: %#v", m)
		}
	}
}

// TestRunnerSkipsEmptyStatusLine 验证：状态源返回空串时不注入任何消息，
// 保持上下文前缀稳定（无状态会话不应平白多出一条 user 消息）。
func TestRunnerSkipsEmptyStatusLine(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.ChatResponse{{Content: "ok"}}}
	status := &fakeStatus{line: ""}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry(), Status: status}

	ctx := withRoute(context.Background(), "cli:local", "cli")
	if _, err := runner.RunCollect(ctx,
		[]providers.Message{{Role: providers.RoleUser, Content: "go"}},
		StreamSink{}); err != nil {
		t.Fatalf("RunCollect() error = %v", err)
	}

	msgs := provider.requests[0].Messages
	if len(msgs) != 1 || msgs[0].Content != "go" {
		t.Fatalf("messages = %#v, want only the original user message", msgs)
	}
}

// TestRunnerStatusLineReflectsLatestTodoState 是端到端验证：用真实的
// todo_write 工具作为状态源，模型第一轮调用 todo_write 写入清单后，
// 第二轮的状态栏应立即反映刚写入的进度——证明"每轮重新读取"生效。
func TestRunnerStatusLineReflectsLatestTodoState(t *testing.T) {
	todoTool, err := tools.NewTodoWrite()
	if err != nil {
		t.Fatal(err)
	}
	registry := tools.NewRegistry()
	if err := registry.Register(todoTool); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{ToolCalls: []providers.ToolCall{{
			ID:   "call_1",
			Name: "todo_write",
			Arguments: json.RawMessage(
				`{"todos":[{"id":"1","content":"任务甲","status":"completed"},{"id":"2","content":"任务乙","status":"in_progress"}]}`,
			),
		}}},
		{Content: "done"},
	}}
	runner := &Runner{Provider: provider, Model: "test", Tools: registry, Status: todoTool}

	ctx := withRoute(context.Background(), "cli:local", "cli")
	if _, err := runner.RunCollect(ctx,
		[]providers.Message{{Role: providers.RoleUser, Content: "开始干活"}},
		StreamSink{}); err != nil {
		t.Fatalf("RunCollect() error = %v", err)
	}

	// 第一轮：todo_write 还没执行，清单为空 → 不注入状态栏。
	first := provider.requests[0].Messages
	if strings.Contains(first[len(first)-1].Content, "Agent 状态栏") {
		t.Fatalf("first request should not carry a status line: %#v", first)
	}
	// 第二轮：清单已写入 → 状态栏应反映 1/2 已完成与两条任务。
	second := provider.requests[1].Messages
	last := second[len(second)-1]
	if last.Role != providers.RoleUser || !strings.Contains(last.Content, "任务清单 1/2 已完成") ||
		!strings.Contains(last.Content, "任务甲") || !strings.Contains(last.Content, "任务乙") {
		t.Fatalf("second request status line = %q", last.Content)
	}
}
