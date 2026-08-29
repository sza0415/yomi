package providers

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// ---- 流式（SSE）专用 wire 结构 ----
//
// OpenAI 流式响应里，每个 SSE event 形如：
//
//	data: {"choices":[{"delta":{"content":"你"},"finish_reason":null}]}
//	data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_x",
//	        "function":{"name":"grep","arguments":"{\"pat"}}]}}]}
//	data: [DONE]
//
// 关键差异：正文和工具调用都是"增量"。tool_calls 按 index 分片：
// id / name 一般只在该 index 的第一片出现，arguments 逐片追加拼接。
type openAIStreamChunk struct {
	Choices []struct {
		Delta struct {
			Content   *string                `json:"content"`
			ToolCalls []openAIStreamToolCall `json:"tool_calls"`
			// 推理增量。DeepSeek 等用 reasoning_content，部分实现用 reasoning。
			ReasoningContent *string `json:"reasoning_content"`
			Reasoning        *string `json:"reasoning"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
		Code    string `json:"code"`
	} `json:"error,omitempty"`
}

type openAIStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// toolCallAccumulator 按 index 累积分片的 tool_calls。
type toolCallAccumulator struct {
	order []int // 保持首次出现顺序
	byIdx map[int]*ToolCall
	args  map[int]*strings.Builder
}

func newToolCallAccumulator() *toolCallAccumulator {
	return &toolCallAccumulator{
		byIdx: make(map[int]*ToolCall),
		args:  make(map[int]*strings.Builder),
	}
}

func (a *toolCallAccumulator) add(delta openAIStreamToolCall) {
	call, ok := a.byIdx[delta.Index]
	if !ok {
		call = &ToolCall{}
		a.byIdx[delta.Index] = call
		a.args[delta.Index] = &strings.Builder{}
		a.order = append(a.order, delta.Index)
	}
	if delta.ID != "" {
		call.ID = delta.ID
	}
	if delta.Function.Name != "" {
		call.Name = delta.Function.Name
	}
	if delta.Function.Arguments != "" {
		a.args[delta.Index].WriteString(delta.Function.Arguments)
	}
}

func (a *toolCallAccumulator) finish() []ToolCall {
	if len(a.order) == 0 {
		return nil
	}
	result := make([]ToolCall, 0, len(a.order))
	for _, idx := range a.order {
		call := a.byIdx[idx]
		call.Arguments = json.RawMessage(a.args[idx].String())
		result = append(result, *call)
	}
	return result
}

// ChatStream 发起一次流式 chat completions 调用。
//
// 行为约定：
//   - 每收到一段正文增量，回调一次 StreamChunk{ContentDelta: ...}；
//   - 收到 [DONE] 或流结束时，回调一次 StreamChunk{Done: true, ...}，
//     若期间累积到 tool_calls，则一并放在这条结束 chunk 的 ToolCalls 里；
//   - 无论中途回调多少次，最终都返回一个累积好的完整 ChatResponse。
func (p *OpenAICompatibleProvider) ChatStream(
	ctx context.Context,
	req ChatRequest,
	onChunk func(StreamChunk) error,
) (ChatResponse, error) {
	if p.BaseURL == "" {
		return ChatResponse{}, errors.New("provider: BaseURL is empty")
	}
	if p.APIKey == "" {
		return ChatResponse{}, errors.New("provider: APIKey is empty")
	}
	if req.Model == "" {
		return ChatResponse{}, errors.New("provider: model is empty")
	}

	wireMsgs := make([]openAIChatMessage, 0, len(req.Messages))
	for _, message := range req.Messages {
		wireMsgs = append(wireMsgs, toOpenAIMessage(message))
	}
	streamOptions := &struct {
		IncludeUsage bool `json:"include_usage"`
	}{IncludeUsage: true}
	body, err := json.Marshal(openAIChatRequest{
		Model:         req.Model,
		Messages:      wireMsgs,
		Tools:         toOpenAIToolDefinitions(req.Tools),
		Stream:        true,
		StreamOptions: streamOptions,
	})
	if err != nil {
		return ChatResponse{}, fmt.Errorf("provider: marshal request: %w", err)
	}

	url := p.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResponse{}, fmt.Errorf("provider: new request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.APIKey)
	httpReq.Header.Set("Accept", "text/event-stream")

	client := p.HTTPClient
	if client == nil {
		// 流式连接可能长时间保持，不设短超时；由 ctx 控制生命周期。
		client = &http.Client{Timeout: 0}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		class := ErrorRetryable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			class = ErrorCancelled
		}
		return ChatResponse{}, NewProviderError(class, fmt.Errorf("provider: do request: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := readSnippet(resp.Body, 500)
		class := ErrorNonRetryable
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			class = ErrorRetryable
		}
		return ChatResponse{}, &ProviderError{Class: class, StatusCode: resp.StatusCode, Err: fmt.Errorf("provider: http %d: %s", resp.StatusCode, snippet)}
	}

	var content strings.Builder
	var reasoning strings.Builder
	tools := newToolCallAccumulator()
	finishReason := ""
	var usage Usage

	scanner := bufio.NewScanner(resp.Body)
	// SSE 单行可能较长（尤其 tool_calls arguments），放宽上限到 1MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue // 事件之间的空行
		}
		// 只关心 data: 行，忽略 event:/id:/: 注释等。
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)
		if data == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return ChatResponse{}, fmt.Errorf("provider: parse stream chunk: %w; data=%s",
				err, truncate(data, 300))
		}
		if chunk.Error != nil {
			return ChatResponse{}, &ProviderError{Class: ErrorNonRetryable, Code: chunk.Error.Code, Err: fmt.Errorf("provider: api error: %s (%s)", chunk.Error.Message, chunk.Error.Code)}
		}
		if chunk.Usage != nil {
			usage = chunk.Usage.internal()
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]

		// 推理增量先于正文到达（推理型模型会先"想"再"说"）。
		reasoningDelta := choice.Delta.ReasoningContent
		if reasoningDelta == nil {
			reasoningDelta = choice.Delta.Reasoning
		}
		if reasoningDelta != nil && *reasoningDelta != "" {
			delta := *reasoningDelta
			reasoning.WriteString(delta)
			if err := onChunk(StreamChunk{ReasoningDelta: delta}); err != nil {
				return ChatResponse{}, err
			}
		}

		if choice.Delta.Content != nil && *choice.Delta.Content != "" {
			delta := *choice.Delta.Content
			content.WriteString(delta)
			if err := onChunk(StreamChunk{ContentDelta: delta}); err != nil {
				return ChatResponse{}, err
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			tools.add(tc)
		}
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			finishReason = *choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResponse{}, fmt.Errorf("provider: read stream: %w", err)
	}

	toolCalls := tools.finish()
	for _, call := range toolCalls {
		if call.ID == "" {
			return ChatResponse{}, errors.New("provider: streamed tool call is missing an ID")
		}
		if call.Name == "" {
			return ChatResponse{}, errors.New("provider: streamed tool call is missing a function name")
		}
	}

	// 结束标记：把累积到的 tool_calls 一并交出，便于上层在一处处理。
	if err := onChunk(StreamChunk{Done: true, ToolCalls: toolCalls, FinishReason: finishReason}); err != nil {
		return ChatResponse{}, err
	}

	return ChatResponse{
		Content:      content.String(),
		Reasoning:    reasoning.String(),
		ToolCalls:    toolCalls,
		FinishReason: finishReason,
		Usage:        usage,
	}, nil
}

// readSnippet 读取 body 的前 n 字节用于错误信息（不 ReadAll 整个流）。
func readSnippet(r interface{ Read([]byte) (int, error) }, n int) string {
	buf := make([]byte, n)
	read, _ := r.Read(buf)
	return truncate(string(buf[:read]), n)
}

// 编译期断言：OpenAICompatibleProvider 必须满足 StreamingProvider。
var _ StreamingProvider = (*OpenAICompatibleProvider)(nil)
