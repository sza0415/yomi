// Package agent 实现 szabot 的核心循环。
//
// 这里有两个角色：
//   - Runner：对内（朝 LLM）。负责跟 Provider 来回打交道、（将来）执行 tool call、判断停止条件。
//   - Loop：对外（朝 channel）。负责消费 bus 入站消息、加载/保存 session、把回复推回 bus。
//
// 第一阶段的 Runner 极度简化：单轮调用，不做工具、不做多轮。
// 等接入真实 LLM 和 tool 之后，这里会演进为真正的"循环"。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
)

const defaultMaxToolTurns = 12

type ModelStatus string

const (
	ModelIdle       ModelStatus = "idle"
	ModelRequesting ModelStatus = "requesting"
	ModelStreaming  ModelStatus = "streaming"
	ModelFinished   ModelStatus = "finished"
	ModelErrored    ModelStatus = "errored"
)

type ToolStatus string

const (
	ToolPending   ToolStatus = "pending"
	ToolRunning   ToolStatus = "running"
	ToolSucceeded ToolStatus = "succeeded"
	ToolFailed    ToolStatus = "failed"
	ToolTimedOut  ToolStatus = "timed_out"
)

var (
	ErrInvalidModelTransition = errors.New("agent: invalid model status transition")
	ErrInvalidToolTransition  = errors.New("agent: invalid tool status transition")
)

func validModelTransition(from, to ModelStatus) bool {
	switch from {
	case ModelIdle:
		return to == ModelRequesting
	case ModelRequesting:
		return to == ModelStreaming || to == ModelFinished || to == ModelErrored
	case ModelStreaming:
		return to == ModelFinished || to == ModelErrored
	default:
		return false
	}
}

func validToolTransition(from, to ToolStatus) bool {
	switch from {
	case ToolPending:
		return to == ToolRunning
	case ToolRunning:
		return to == ToolSucceeded || to == ToolFailed || to == ToolTimedOut
	default:
		return false
	}
}

func transitionToolStatus(current *ToolStatus, next ToolStatus) error {
	if *current == next {
		return nil
	}
	if !validToolTransition(*current, next) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidToolTransition, *current, next)
	}
	*current = next
	return nil
}

// StatusProvider 是「Agent 状态栏」的数据源：按 SessionID 返回一段要注入到
// 上下文末尾的元信息文本（当前由 todo_write 提供任务清单与进度）。
//
// 返回空串表示该会话暂无可注入的状态，Runner 据此跳过注入，保持上下文前缀
// 稳定、对 KV Cache 友好。做成接口而非直接依赖 *tools.TodoWriteTool，是为了
// 让核心循环不绑死具体工具——将来任何"想在每轮上下文里露个脸"的能力都能实现它。
type StatusProvider interface {
	StatusLine(sessionID string) string
}

// Runner coordinates a model conversation and the explicit local tool allowlist.
type Runner struct {
	Provider     providers.Provider
	Model        string
	Tools        *tools.Registry
	MaxToolTurns int
	// Artifacts stores oversized tool results outside the model context. When
	// nil, oversized results fall back to a bounded local preview.
	Artifacts *tools.ArtifactStore
	// MaxContextTokens bounds the in-run conversation after tool results are
	// appended. ContextManager remains responsible for cross-run history.
	ContextBudget       *ContextBudget
	MaxContextTokens    int
	ToolResultMaxTokens int
	OutputReserveTokens int

	// Asker lets tools such as ask_user_question pause the loop and ask the
	// user a question through the channel. Injected by Loop.Start; may be nil
	// when no interactive channel is wired (e.g. some tests).
	Asker tools.Asker

	// Status 提供每轮注入到上下文末尾的「Agent 状态栏」（任务清单/进度等）。
	// 为 nil 时不注入任何状态栏（退化为无状态栏行为）。
	Status         StatusProvider
	Retry          RetryPolicy
	ToolRetry      RetryPolicy
	PermissionGate tools.PermissionGate
}

type RetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
}

func (p RetryPolicy) attempts() int {
	if p.MaxAttempts < 0 {
		return 1
	}
	if p.MaxAttempts == 0 {
		return 3
	}
	return p.MaxAttempts
}

func (p RetryPolicy) delay(attempt int) time.Duration {
	initial := p.InitialDelay
	if initial <= 0 && p.MaxAttempts == 0 {
		initial = 200 * time.Millisecond
	}
	if initial <= 0 {
		return 0
	}
	d := initial
	for i := 1; i < attempt; i++ {
		if p.MaxDelay > 0 && d >= p.MaxDelay/2 {
			return p.MaxDelay
		}
		d *= 2
	}
	if p.MaxDelay > 0 && d > p.MaxDelay {
		return p.MaxDelay
	}
	return d
}

// ModelCallEvent 描述一次模型请求及其结果，Step 从 1 开始。
type ModelCallEvent struct {
	Step          int
	ContextID     string
	Status        ModelStatus
	Provider      string
	Model         string
	Streaming     bool
	Messages      []providers.Message
	Tools         []providers.ToolDefinition
	ToolCount     int
	Budget        BudgetSnapshot
	Response      providers.ChatResponse
	Duration      time.Duration
	FirstToken    time.Duration
	FirstTokenSet bool
	Err           error
}

// ToolExecutionEvent 描述一次工具执行的完整结果。
type ToolExecutionEvent struct {
	Step            int
	ContextID       string
	Call            providers.ToolCall
	Status          ToolStatus
	Result          string
	ContextResult   string
	ContextDecision *ToolContextDecision
	Duration        time.Duration
	Err             error
}

// AssistantMessageEvent 是一次模型步骤产生的完整 assistant 消息。
type AssistantMessageEvent struct {
	Step    int
	Message providers.Message
}

// ContextEvent 描述框架在模型调用前临时注入的上下文。
type ContextEvent struct {
	Step    int
	Type    string
	Content string
}

// StreamSink 是 Runner 向上层实时汇报“运行中发生了什么”的回调集合。
// 所有回调均可为 nil；旧调用方只使用增量和工具回调即可保持原行为。
type StreamSink struct {
	OnContentDelta   func(string)
	OnReasoningDelta func(string)
	OnToolCall       func(providers.ToolCall)
	OnToolResult     func(call providers.ToolCall, result string)

	OnModelCallStarted  func(ModelCallEvent)
	OnModelCallStatus   func(ModelCallEvent)
	OnModelCallFinished func(ModelCallEvent)
	OnAssistantMessage  func(AssistantMessageEvent)
	OnContext           func(ContextEvent)
	OnToolStarted       func(ToolExecutionEvent)
	OnToolStatus        func(ToolExecutionEvent)
	OnToolFinished      func(ToolExecutionEvent)
}

// RunResult 汇总一轮 Run 的产物。
//
//   - Answer 是面向用户的最终正文（与旧 Run 的返回字符串语义一致）；
//   - Messages 是本轮**新增**的全部对话消息，按发生顺序排列：
//     每个 tool-call 轮的 assistant 消息（含 Reasoning 与 ToolCalls）、
//     对应的 tool 结果消息，以及最终那条 assistant 正文消息。
//     Loop 应把 Messages 整体追加进 session，从而推理过程与工具调用
//     都被完整持久化，而不再只剩一条最终正文。
type RunResult struct {
	Answer   string
	Messages []providers.Message
	Usage    RunUsage
}

// Run 是非流式入口，等价于 RunStream(ctx, messages, nil)。
func (r *Runner) Run(ctx context.Context, messages []providers.Message) (string, error) {
	return r.RunStream(ctx, messages, nil)
}

// RunStream 与 Run 相同，但会把模型正文的增量通过 onDelta 实时回调出去。
//
// 它是 RunCollect 的兼容封装：只关心正文增量、只返回最终答案字符串，
// 供既有调用方/测试沿用旧签名。需要推理过程、工具调用事件或完整消息
// 序列的调用方（如 Loop）应改用 RunCollect。
func (r *Runner) RunStream(
	ctx context.Context,
	messages []providers.Message,
	onDelta func(string),
) (string, error) {
	result, err := r.RunCollect(ctx, messages, StreamSink{OnContentDelta: onDelta})
	if err != nil {
		return "", err
	}
	return result.Answer, nil
}

// RunCollect 是真正的对话引擎：持续调用 Provider，执行工具，直到模型给出
// 一条不含 tool_calls 的正常回复，或达到工具轮数上限。
//
// 与旧实现的关键区别：它不再"用完即弃"中间过程，而是把本轮新增的每一条
// 消息（含推理过程、工具调用、工具结果）都收进 RunResult.Messages，并通过
// sink 把推理增量、工具调用/结果实时汇报出去。这样上层既能完整落盘，也能
// 分类渲染。
//
// 流式约定与旧版一致：
//   - 若 Provider 实现 StreamingProvider 且 sink 有正文/推理回调，走真正的 SSE；
//   - 否则回退到一次性 Chat，再把完整正文/推理当作单个增量回调出去。
func (r *Runner) RunCollect(
	ctx context.Context,
	messages []providers.Message,
	sink StreamSink,
) (RunResult, error) {
	if r.Provider == nil {
		return RunResult{}, fmt.Errorf("agent: provider is nil")
	}

	conversation := append([]providers.Message(nil), messages...)
	// produced 只收集“本轮新增”的消息，供上层写入 Trace。
	produced := make([]providers.Message, 0, 4)
	var usage RunUsage
	budget := RunBudget{}
	if run, ok := runFrom(ctx); ok {
		budget = run.Budget
	}

	maxTurns := r.MaxToolTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxToolTurns
	}

	// Make the Asker reachable to tools that need to question the user
	// (e.g. ask_user_question) without changing the Tool.Execute signature.
	if r.Asker != nil {
		ctx = tools.WithAsker(ctx, r.Asker)
	}
	// Expose the session id so session-scoped tools (e.g. todo_write) can keep
	// per-conversation state.
	if sessionID, _, ok := routeFrom(ctx); ok && sessionID != "" {
		ctx = tools.WithSession(ctx, sessionID)
	}

	for turn := 0; turn < maxTurns; turn++ {
		step := turn + 1
		response, err, attempts := r.chatWithRetry(ctx, conversation, step, sink)
		usage.ModelCalls += attempts
		if err != nil {
			return RunResult{Messages: produced, Usage: usage}, err
		}
		usage.Usage.Add(response.Usage)

		assistant := providers.Message{
			Role:      providers.RoleAssistant,
			Content:   response.Content,
			Reasoning: response.Reasoning,
			ToolCalls: response.ToolCalls,
		}
		if sink.OnAssistantMessage != nil {
			sink.OnAssistantMessage(AssistantMessageEvent{Step: step, Message: assistant})
		}
		if err := checkBudget(budget, usage); err != nil {
			produced = append(produced, assistant)
			return RunResult{Messages: produced, Usage: usage}, err
		}

		// 无工具调用 = 本轮的最终答案。记录这条 assistant 正文（含推理）后收尾。
		if len(response.ToolCalls) == 0 {
			produced = append(produced, assistant)
			return RunResult{Answer: response.Content, Messages: produced, Usage: usage}, nil
		}

		// 有工具调用：先记录这条 assistant 消息（可能带思考过程 + tool_calls），
		// 再逐个执行工具、记录结果。这些都会进入 produced 从而被持久化。
		conversation = append(conversation, assistant)
		produced = append(produced, assistant)

		for _, call := range response.ToolCalls {
			if strings.TrimSpace(call.ID) == "" {
				return RunResult{Messages: produced, Usage: usage}, fmt.Errorf("agent: provider returned a tool call without an ID")
			}
			usage.ToolCalls++
			if err := checkBudget(budget, usage); err != nil {
				return RunResult{Messages: produced, Usage: usage}, err
			}
			if sink.OnToolCall != nil {
				sink.OnToolCall(call)
			}
			if sink.OnToolStatus != nil {
				sink.OnToolStatus(ToolExecutionEvent{Step: step, ContextID: contextID(ctx, step), Call: call, Status: ToolPending})
			}
			toolStatus := ToolPending
			if err := transitionToolStatus(&toolStatus, ToolRunning); err != nil {
				return RunResult{Messages: produced, Usage: usage}, err
			}
			if sink.OnToolStarted != nil {
				sink.OnToolStarted(ToolExecutionEvent{Step: step, ContextID: contextID(ctx, step), Call: call, Status: toolStatus})
			}
			if sink.OnToolStatus != nil {
				sink.OnToolStatus(ToolExecutionEvent{Step: step, ContextID: contextID(ctx, step), Call: call, Status: toolStatus})
			}

			started := time.Now()
			result, toolErr := r.executeToolWithRetry(ctx, call.Name, call.Arguments)
			duration := time.Since(started)
			if toolErr != nil {
				result = "Error: " + toolErr.Error()
			}
			if sink.OnToolResult != nil {
				sink.OnToolResult(call, result)
			}
			contextResult, contextDecision := r.prepareToolResult(ctx, conversation, call, result)
			terminalStatus := ToolSucceeded
			if toolErr != nil {
				terminalStatus = ToolFailed
				if errors.Is(toolErr, context.DeadlineExceeded) {
					terminalStatus = ToolTimedOut
				}
			}
			if err := transitionToolStatus(&toolStatus, terminalStatus); err != nil {
				return RunResult{Messages: produced, Usage: usage}, err
			}
			if sink.OnToolFinished != nil {
				sink.OnToolFinished(ToolExecutionEvent{Step: step, ContextID: contextID(ctx, step), Call: call, Status: toolStatus, Result: result, ContextResult: contextResult, ContextDecision: contextDecision, Duration: duration, Err: toolErr})
			}
			if sink.OnToolStatus != nil {
				sink.OnToolStatus(ToolExecutionEvent{Step: step, ContextID: contextID(ctx, step), Call: call, Status: toolStatus, Result: result, ContextResult: contextResult, ContextDecision: contextDecision, Duration: duration, Err: toolErr})
			}

			toolMsg := providers.Message{
				Role:       providers.RoleTool,
				ToolCallID: call.ID,
				Content:    contextResult,
			}
			conversation = append(conversation, toolMsg)
			produced = append(produced, toolMsg)
		}
	}

	return RunResult{Messages: produced, Usage: usage}, fmt.Errorf("agent: exceeded maximum tool turns (%d)", maxTurns)
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) executeToolWithRetry(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if r.PermissionGate != nil {
		if err := r.PermissionGate.Check(ctx, tools.PermissionRequest{Tool: name, Arguments: args, Reason: permissionReason(name)}); err != nil {
			return "", err
		}
	}
	tool, ok := r.Tools.Lookup(name)
	if !ok {
		return r.Tools.Execute(ctx, name, args)
	}
	classifier, optedIn := tool.(tools.RetryClassifier)
	max := r.ToolRetry.attempts()
	for attempt := 1; attempt <= max; attempt++ {
		result, err := tool.Execute(ctx, args)
		if err == nil || !optedIn || !classifier.Retryable(err) || attempt == max {
			return result, err
		}
		if err := waitRetry(ctx, r.ToolRetry.delay(attempt)); err != nil {
			return "", err
		}
	}
	return "", context.Canceled
}

func permissionReason(name string) string {
	switch name {
	case "bash", "python":
		return "this tool can execute arbitrary code and may modify files"
	case "write_file", "edit_file":
		return "this tool modifies workspace files"
	case "web_fetch", "web_search":
		return "this tool accesses external network resources"
	default:
		return "this tool is not read-only"
	}
}

func (r *Runner) chatWithRetry(ctx context.Context, conversation []providers.Message, step int, sink StreamSink) (providers.ChatResponse, error, int) {
	max := r.Retry.attempts()
	for attempt := 1; attempt <= max; attempt++ {
		producedToken := false
		wrapped := sink
		previousFinished := sink.OnModelCallFinished
		wrapped.OnModelCallFinished = func(event ModelCallEvent) {
			producedToken = event.FirstTokenSet
			if previousFinished != nil {
				previousFinished(event)
			}
		}
		response, err := r.chatOnce(ctx, conversation, step, wrapped)
		if err == nil {
			return response, nil, attempt
		}
		if !providers.IsRetryable(err) || producedToken || attempt == max {
			return providers.ChatResponse{}, err, attempt
		}
		if err := waitRetry(ctx, r.Retry.delay(attempt)); err != nil {
			return providers.ChatResponse{}, err, attempt
		}
	}
	return providers.ChatResponse{}, context.Canceled, max
}

// chatOnce 发起单轮模型调用，并通过结构化回调报告请求快照与完成状态。
func (r *Runner) chatOnce(
	ctx context.Context,
	conversation []providers.Message,
	step int,
	sink StreamSink,
) (providers.ChatResponse, error) {
	requestMessages, contextContent := r.withStatusLine(ctx, conversation)
	requestTools := providerToolDefinitions(r.Tools)
	budget := r.effectiveContextBudget().Evaluate(requestMessages, requestTools)
	if budget.Exceeded {
		return providers.ChatResponse{}, fmt.Errorf("%w: total=%d max=%d", ErrContextBudgetExceeded, budget.TotalTokens, budget.MaxContextTokens)
	}
	request := providers.ChatRequest{
		Model:    r.Model,
		Messages: requestMessages,
		Tools:    requestTools,
	}

	streamer, canStream := r.Provider.(providers.StreamingProvider)
	wantsStream := sink.OnContentDelta != nil || sink.OnReasoningDelta != nil
	streaming := canStream && wantsStream
	event := ModelCallEvent{
		Step:      step,
		ContextID: contextID(ctx, step),
		Status:    ModelRequesting,
		Provider:  r.Provider.Name(),
		Model:     r.Model,
		Streaming: streaming,
		Messages:  append([]providers.Message(nil), request.Messages...),
		Tools:     append([]providers.ToolDefinition(nil), request.Tools...),
		ToolCount: len(request.Tools),
		Budget:    budget,
	}
	if contextContent != "" && sink.OnContext != nil {
		sink.OnContext(ContextEvent{Step: step, Type: "agent_status", Content: contextContent})
	}
	if sink.OnModelCallStarted != nil {
		sink.OnModelCallStarted(event)
	}

	started := time.Now()
	markFirstToken := func() {
		if !event.FirstTokenSet {
			event.FirstTokenSet = true
			event.FirstToken = time.Since(started)
		}
	}
	var response providers.ChatResponse
	var err error
	modelStatus := ModelIdle
	setModelStatus := func(next ModelStatus) error {
		if modelStatus == next {
			return nil
		}
		if !validModelTransition(modelStatus, next) {
			return fmt.Errorf("%w: %s -> %s", ErrInvalidModelTransition, modelStatus, next)
		}
		modelStatus = next
		return nil
	}
	if err := setModelStatus(ModelRequesting); err != nil {
		return providers.ChatResponse{}, err
	}
	event.Status = modelStatus
	if streaming {
		response, err = streamer.ChatStream(ctx, request, func(chunk providers.StreamChunk) error {
			if chunk.ReasoningDelta != "" || chunk.ContentDelta != "" || len(chunk.ToolCalls) > 0 {
				markFirstToken()
				if modelStatus == ModelRequesting {
					if statusErr := setModelStatus(ModelStreaming); statusErr != nil {
						return statusErr
					}
					event.Status = modelStatus
					if sink.OnModelCallStatus != nil {
						sink.OnModelCallStatus(event)
					}
				}
			}
			if chunk.ReasoningDelta != "" {
				if sink.OnReasoningDelta != nil {
					sink.OnReasoningDelta(chunk.ReasoningDelta)
				}
			}
			if chunk.ContentDelta != "" {
				if sink.OnContentDelta != nil {
					sink.OnContentDelta(chunk.ContentDelta)
				}
			}
			return nil
		})
	} else {
		response, err = r.Provider.Chat(ctx, request)
		if err == nil {
			if response.Reasoning != "" && sink.OnReasoningDelta != nil {
				sink.OnReasoningDelta(response.Reasoning)
			}
			if len(response.ToolCalls) == 0 && response.Content != "" && sink.OnContentDelta != nil {
				sink.OnContentDelta(response.Content)
			}
		}
	}

	event.Response = response
	event.Duration = time.Since(started)
	event.Err = err
	if err != nil {
		_ = setModelStatus(ModelErrored)
		event.Status = ModelErrored
	} else {
		_ = setModelStatus(ModelFinished)
		event.Status = ModelFinished
	}
	if sink.OnModelCallFinished != nil {
		sink.OnModelCallFinished(event)
	}
	if err != nil {
		return providers.ChatResponse{}, err
	}
	return response, nil
}

// withStatusLine 在发送给 Provider 之前，把「Agent 状态栏」作为一条 user 消息
// 临时追加到 conversation 末尾，并返回这份**新切片**（绝不改动传入的
// conversation，也不写回 produced/Store）。
//
// 为什么这样做：
//   - 每轮 chatOnce 都重新读取一次最新状态（如 todo_write 刚更新的进度），
//     因此状态栏永远反映当下——这正是"每个 turn 重新生成状态"的语义；
//   - 只临时挂在末尾、不落盘，system 前缀与历史前缀保持字节稳定，
//     不破坏 KV Cache（这是"用 user 槽位挂系统状态"而非"改 system"的关键取舍）；
//   - 借用 user 角色只是 API 协议层面的技术选择，内容由框架生成、并非真实用户输入。
func (r *Runner) withStatusLine(ctx context.Context, conversation []providers.Message) ([]providers.Message, string) {
	if r.Status == nil {
		return conversation, ""
	}
	sessionID, _, _ := routeFrom(ctx)
	line := r.Status.StatusLine(sessionID)
	if strings.TrimSpace(line) == "" {
		return conversation, ""
	}

	statusMsg := providers.Message{
		Role: providers.RoleUser,
		Content: "[Agent 状态栏 — 仅供你自我感知的元信息，不是用户的新指令]\n" +
			line +
			"\n[/Agent 状态栏]",
	}
	// 复制一份再追加，避免 append 复用底层数组污染调用方持有的 conversation。
	out := make([]providers.Message, 0, len(conversation)+1)
	out = append(out, conversation...)
	out = append(out, statusMsg)
	return out, line
}

func providerToolDefinitions(registry *tools.Registry) []providers.ToolDefinition {
	definitions := registry.Definitions()
	if len(definitions) == 0 {
		return nil
	}

	result := make([]providers.ToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, providers.ToolDefinition{
			Name:        definition.Name,
			Description: definition.Description,
			Parameters:  definition.Parameters,
		})
	}
	return result
}

func (r *Runner) effectiveContextBudget() ContextBudget {
	if r.ContextBudget != nil {
		return *r.ContextBudget
	}
	return ContextBudget{
		MaxContextTokens:    r.MaxContextTokens,
		OutputReserveTokens: r.outputReserveTokens(),
		WarningRatio:        defaultContextWarningRatio,
	}
}

func (r *Runner) outputReserveTokens() int {
	if r.OutputReserveTokens > 0 {
		return r.OutputReserveTokens
	}
	if r.MaxContextTokens > 0 {
		return defaultOutputReserveTokens
	}
	return 0
}

func contextID(ctx context.Context, step int) string {
	if run, ok := runFrom(ctx); ok && run != nil {
		return fmt.Sprintf("%s:%d", run.ID, step)
	}
	return fmt.Sprintf("step:%d", step)
}
