package providers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// OpenAICompatibleProvider 是一个通用的 OpenAI 兼容 Provider。
//
// 凡是接口形如 POST {BaseURL}/chat/completions、请求/响应遵循 OpenAI
// chat completions 规范的服务，都可以用它。已知兼容：
//   - OpenAI       BaseURL = https://api.openai.com/v1
//   - DeepSeek     BaseURL = https://api.deepseek.com/v1
//   - Moonshot     BaseURL = https://api.moonshot.cn/v1
//   - 本地 Ollama   BaseURL = http://localhost:11434/v1
//
// 设计要点：
//   - 不引入任何官方 SDK，纯 net/http + encoding/json，依赖最少；
//   - 错误信息尽量带上 HTTP 状态码和原始响应体，便于排查；
//   - 请求超时由调用方通过 ctx 控制；HTTPClient.Timeout 作为兜底。
type OpenAICompatibleProvider struct {
	// ProviderName 用于日志展示，例如 "deepseek"、"openai"。
	ProviderName string

	// BaseURL 不要带末尾斜杠，例如 "https://api.deepseek.com/v1"。
	BaseURL string

	// APIKey 走 Authorization: Bearer 头。
	APIKey string

	// HTTPClient 可注入；为空时使用 30s 超时的默认 client。
	HTTPClient *http.Client
}

// Name 返回 provider 名字（日志用）。
func (p *OpenAICompatibleProvider) Name() string {
	if p.ProviderName == "" {
		return "openai-compatible"
	}
	return p.ProviderName
}

// ---- 与 OpenAI 一致的请求/响应结构 ----

type openAIChatMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	// ReasoningContent 是推理型模型（如 DeepSeek-R1）在 content 之外单独返回的
	// 思考过程。部分兼容实现用 reasoning 字段，两个都收，取非空者。
	// 仅用于解析响应；请求侧不回传推理内容，因此始终省略序列化。
	ReasoningContent string `json:"reasoning_content,omitempty"`
	Reasoning        string `json:"reasoning,omitempty"`
}

type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type openAIToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters"`
	} `json:"function"`
}

type openAIChatRequest struct {
	Model         string                 `json:"model"`
	Messages      []openAIChatMessage    `json:"messages"`
	Tools         []openAIToolDefinition `json:"tools,omitempty"`
	Stream        bool                   `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	PromptDetails    struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (u openAIUsage) internal() Usage {
	return Usage{
		InputTokens:     u.PromptTokens,
		OutputTokens:    u.CompletionTokens,
		TotalTokens:     u.TotalTokens,
		CachedTokens:    u.PromptDetails.CachedTokens,
		ReasoningTokens: u.CompletionDetails.ReasoningTokens,
		Reported:        true,
	}
}

type openAIChatResponse struct {
	Choices []struct {
		Message      openAIChatMessage `json:"message"`
		FinishReason string            `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

func toOpenAIMessage(message Message) openAIChatMessage {
	wire := openAIChatMessage{
		Role:       string(message.Role),
		ToolCallID: message.ToolCallID,
	}
	if message.Role != RoleAssistant || len(message.ToolCalls) == 0 || message.Content != "" {
		content := message.Content
		wire.Content = &content
	}
	for _, call := range message.ToolCalls {
		wireCall := openAIToolCall{ID: call.ID, Type: "function"}
		wireCall.Function.Name = call.Name
		wireCall.Function.Arguments = string(call.Arguments)
		wire.ToolCalls = append(wire.ToolCalls, wireCall)
	}
	return wire
}

func toOpenAIToolDefinitions(definitions []ToolDefinition) []openAIToolDefinition {
	if len(definitions) == 0 {
		return nil
	}

	wire := make([]openAIToolDefinition, 0, len(definitions))
	for _, definition := range definitions {
		parameters := definition.Parameters
		if len(parameters) == 0 {
			parameters = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		entry := openAIToolDefinition{Type: "function"}
		entry.Function.Name = definition.Name
		entry.Function.Description = definition.Description
		entry.Function.Parameters = parameters
		wire = append(wire, entry)
	}
	return wire
}

// Chat 发起一次 chat completions 调用。
func (p *OpenAICompatibleProvider) Chat(ctx context.Context, req ChatRequest) (ChatResponse, error) {
	if p.BaseURL == "" {
		return ChatResponse{}, NewProviderError(ErrorNonRetryable, errors.New("provider: BaseURL is empty"))
	}
	if p.APIKey == "" {
		return ChatResponse{}, NewProviderError(ErrorNonRetryable, errors.New("provider: APIKey is empty"))
	}
	if req.Model == "" {
		return ChatResponse{}, NewProviderError(ErrorNonRetryable, errors.New("provider: model is empty"))
	}

	// 1. 把内部会话与工具定义转成 OpenAI wire format。
	wireMsgs := make([]openAIChatMessage, 0, len(req.Messages))
	for _, message := range req.Messages {
		wireMsgs = append(wireMsgs, toOpenAIMessage(message))
	}

	body, err := json.Marshal(openAIChatRequest{
		Model:    req.Model,
		Messages: wireMsgs,
		Tools:    toOpenAIToolDefinitions(req.Tools),
		Stream:   false,
	})
	if err != nil {
		return ChatResponse{}, fmt.Errorf("provider: marshal request: %w", err)
	}

	// 2. 发起 HTTP 请求。
	url := p.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("provider: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		class := ErrorRetryable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			class = ErrorCancelled
		} else if _, ok := err.(net.Error); !ok {
			class = ErrorNonRetryable
		}
		return ChatResponse{}, NewProviderError(class, fmt.Errorf("provider: do request: %w", err))
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return ChatResponse{}, fmt.Errorf("provider: read response: %w", err)
	}

	// 3. 非 2xx 直接返回带原始内容的错误，便于排查 401/429/400 等。
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		class := ErrorNonRetryable
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			class = ErrorRetryable
		}
		return ChatResponse{}, &ProviderError{
			Class: class, StatusCode: resp.StatusCode,
			Err: fmt.Errorf(
				"provider: http %d: %s",
				resp.StatusCode,
				truncate(string(respBody), 500),
			),
		}
	}

	// 4. 解析 JSON。
	var parsed openAIChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return ChatResponse{}, fmt.Errorf("provider: unmarshal response: %w; body=%s",
			err, truncate(string(respBody), 500))
	}
	if parsed.Error != nil {
		return ChatResponse{}, &ProviderError{Class: ErrorNonRetryable, Code: parsed.Error.Code, Err: fmt.Errorf("provider: api error: %s (%s)",
			parsed.Error.Message, parsed.Error.Code)}
	}
	if len(parsed.Choices) == 0 {
		return ChatResponse{}, errors.New("provider: no choices in response")
	}

	choice := parsed.Choices[0]
	content := ""
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}
	reasoning := choice.Message.ReasoningContent
	if reasoning == "" {
		reasoning = choice.Message.Reasoning
	}
	toolCalls := make([]ToolCall, 0, len(choice.Message.ToolCalls))
	for _, call := range choice.Message.ToolCalls {
		if call.ID == "" {
			return ChatResponse{}, errors.New("provider: tool call is missing an ID")
		}
		if call.Function.Name == "" {
			return ChatResponse{}, errors.New("provider: tool call is missing a function name")
		}
		toolCalls = append(toolCalls, ToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: json.RawMessage([]byte(call.Function.Arguments)),
		})
	}

	usage := Usage{}
	if parsed.Usage != nil {
		usage = parsed.Usage.internal()
	}
	return ChatResponse{
		Content:      content,
		Reasoning:    reasoning,
		ToolCalls:    toolCalls,
		FinishReason: choice.FinishReason,
		Usage:        usage,
	}, nil
}

// truncate 截断超长字符串，避免把整段响应糊到错误里。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}
