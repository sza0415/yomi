// Package providers 抽象了"和某个 LLM 厂商对话"这件事。
//
// 设计要点：
//   - AgentRunner 只依赖 Provider 接口，不直接调任何 SDK；
//   - 想换 OpenAI / DeepSeek / Anthropic / 本地 Ollama 时，
//     只需要新增一个实现，Runner 一行不用改。
package providers

import (
	"context"
	"encoding/json"
	"errors"
)

type ErrorClass string

const (
	ErrorRetryable    ErrorClass = "retryable"
	ErrorNonRetryable ErrorClass = "non_retryable"
	ErrorCancelled    ErrorClass = "cancelled"
)

// ProviderError preserves the underlying error while making retry semantics explicit.
type ProviderError struct {
	Class      ErrorClass
	StatusCode int
	Code       string
	Err        error
}

func (e *ProviderError) Error() string { return e.Err.Error() }
func (e *ProviderError) Unwrap() error { return e.Err }

func (e *ProviderError) Retryable() bool { return e != nil && e.Class == ErrorRetryable }

func NewProviderError(class ErrorClass, err error) error {
	if err == nil {
		return nil
	}
	return &ProviderError{Class: class, Err: err}
}

func IsRetryable(err error) bool {
	var pe *ProviderError
	return errors.As(err, &pe) && pe.Retryable()
}

// Role 是对话消息的角色，沿用 OpenAI 兼容惯例。
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// ToolDefinition describes one function the model is allowed to request.
type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall is one function request returned by a model.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Message is one item in a model conversation. Assistant messages retain their
// calls and tool messages retain the matching call ID so provider protocols can
// replay the required assistant → tool sequence.
//
// JSON tags are explicit so that the on-disk session log (jsonl) has a stable,
// readable schema independent of Go field names.
//
// Reasoning 保存推理型模型（如 DeepSeek-R1、OpenAI o 系列）在正文之外单独
// 给出的"思考过程"。它只做展示与回放之用，不会作为 content 回传给模型，
// 因此用独立字段承载，避免污染 content。
type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCalls  []ToolCall `json:"toolCalls,omitempty"`
	ToolCallID string     `json:"toolCallId,omitempty"`
}

// ChatRequest is one request to a model provider.
type ChatRequest struct {
	Model    string
	Messages []Message
	Tools    []ToolDefinition
}

// Usage 是一次模型调用报告的 token 消耗。Provider 未报告的字段保持零值，
// Reported 用于区分“真实为 0”和“未知”。
type Usage struct {
	InputTokens     int  `json:"input_tokens"`
	OutputTokens    int  `json:"output_tokens"`
	TotalTokens     int  `json:"total_tokens"`
	CachedTokens    int  `json:"cached_tokens,omitempty"`
	ReasoningTokens int  `json:"reasoning_tokens,omitempty"`
	Reported        bool `json:"reported"`
}

func (u *Usage) Add(other Usage) {
	u.InputTokens += other.InputTokens
	u.OutputTokens += other.OutputTokens
	u.TotalTokens += other.TotalTokens
	u.CachedTokens += other.CachedTokens
	u.ReasoningTokens += other.ReasoningTokens
	u.Reported = u.Reported || other.Reported
}

// ChatResponse is a model reply. A non-empty ToolCalls list asks the Runner to
// execute local tools and submit their results in a follow-up request.
type ChatResponse struct {
	Content string
	// Reasoning 是推理型模型单独返回的思考过程（reasoning_content），
	// 可能为空。它与 Content 相互独立。
	Reasoning    string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// Provider 是 LLM 厂商的统一接口。
type Provider interface {
	// Name 仅用于日志/调试。
	Name() string
	// Chat 发起一次对话调用（非流式，一次性拿到完整回复）。
	Chat(ctx context.Context, req ChatRequest) (ChatResponse, error)
}

// StreamChunk 是流式响应里的一个增量片段。
//
// 一次流式调用会回调多次：
//   - 文本增量：ContentDelta 非空（拼起来就是完整回复正文）；
//   - 推理增量：ReasoningDelta 非空（拼起来就是完整思考过程，
//     推理型模型专有，普通模型永远为空）；
//   - 工具调用：provider 把分片拼装完成后，通过 ToolCalls 一次性给出；
//   - 结束标记：Done=true，并带上 FinishReason。
type StreamChunk struct {
	ContentDelta   string
	ReasoningDelta string
	ToolCalls      []ToolCall
	Done           bool
	FinishReason   string
}

// StreamingProvider 是"可选能力"：实现了它的 Provider 支持流式输出。
//
// 之所以做成独立接口而非塞进 Provider，是为了不强迫所有实现都支持流式
// （比如 EchoProvider 没必要）。调用方用类型断言探测：
//
//	if sp, ok := provider.(StreamingProvider); ok { 走流式 } else { 回退 Chat }
type StreamingProvider interface {
	Provider
	// ChatStream 发起一次流式对话。每收到一个增量就调用 onChunk 回调；
	// onChunk 返回错误则中止流式并把该错误返回。
	// 无论是否流式，最终都要返回一个累积好的完整 ChatResponse，
	// 供上层记录历史、判断 tool_calls 与停止条件。
	ChatStream(ctx context.Context, req ChatRequest, onChunk func(StreamChunk) error) (ChatResponse, error)
}
