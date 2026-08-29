package providers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// sseServer 起一个假的 OpenAI 兼容流式服务，按传入的原始 SSE 文本吐出。
func sseServer(t *testing.T, sse string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
}

// TestChatStreamContent 验证正文增量按序回调、并累积成完整 ChatResponse。
func TestChatStreamContent(t *testing.T) {
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"你\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"好\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"呀\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	server := sseServer(t, sse)
	defer server.Close()

	p := &OpenAICompatibleProvider{
		ProviderName: "test",
		BaseURL:      server.URL,
		APIKey:       "k",
	}

	var deltas []string
	var doneSeen bool
	resp, err := p.ChatStream(context.Background(),
		ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}},
		func(c StreamChunk) error {
			if c.ContentDelta != "" {
				deltas = append(deltas, c.ContentDelta)
			}
			if c.Done {
				doneSeen = true
			}
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if got := len(deltas); got != 3 {
		t.Fatalf("delta count = %d, want 3: %v", got, deltas)
	}
	if deltas[0] != "你" || deltas[1] != "好" || deltas[2] != "呀" {
		t.Fatalf("deltas = %v, want [你 好 呀]", deltas)
	}
	if !doneSeen {
		t.Fatal("expected a Done chunk")
	}
	if resp.Content != "你好呀" {
		t.Fatalf("accumulated content = %q, want 你好呀", resp.Content)
	}
	if resp.FinishReason != "stop" {
		t.Fatalf("finish reason = %q, want stop", resp.FinishReason)
	}
}

// TestChatStreamToolCallsAcrossChunks 验证分片的 tool_calls 能按 index 正确拼装。
func TestChatStreamToolCallsAcrossChunks(t *testing.T) {
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"grep\",\"arguments\":\"{\\\"pat\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"tern\\\":\\\"foo\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	server := sseServer(t, sse)
	defer server.Close()

	p := &OpenAICompatibleProvider{ProviderName: "test", BaseURL: server.URL, APIKey: "k"}

	resp, err := p.ChatStream(context.Background(),
		ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "search"}}},
		func(StreamChunk) error { return nil })
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
	call := resp.ToolCalls[0]
	if call.ID != "call_1" || call.Name != "grep" {
		t.Fatalf("tool call = %+v, want id=call_1 name=grep", call)
	}
	if string(call.Arguments) != `{"pattern":"foo"}` {
		t.Fatalf("tool call arguments = %s, want {\"pattern\":\"foo\"}", call.Arguments)
	}
	if resp.FinishReason != "tool_calls" {
		t.Fatalf("finish reason = %q, want tool_calls", resp.FinishReason)
	}
}

// TestChatStreamReasoning 验证：流式 reasoning_content 增量被单独回调为
// ReasoningDelta，并累积进 ChatResponse.Reasoning，与正文互不混淆。
func TestChatStreamReasoning(t *testing.T) {
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"想\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"法\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"答\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"案\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"

	server := sseServer(t, sse)
	defer server.Close()

	p := &OpenAICompatibleProvider{ProviderName: "test", BaseURL: server.URL, APIKey: "k"}

	var content, reasoning string
	resp, err := p.ChatStream(context.Background(),
		ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}},
		func(c StreamChunk) error {
			content += c.ContentDelta
			reasoning += c.ReasoningDelta
			return nil
		})
	if err != nil {
		t.Fatalf("ChatStream() error = %v", err)
	}
	if content != "答案" {
		t.Fatalf("streamed content = %q, want 答案", content)
	}
	if reasoning != "想法" {
		t.Fatalf("streamed reasoning = %q, want 想法", reasoning)
	}
	if resp.Content != "答案" || resp.Reasoning != "想法" {
		t.Fatalf("accumulated resp = %#v", resp)
	}
}

func TestChatStreamUsage(t *testing.T) {
	sse := "" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"

	server := sseServer(t, sse)
	defer server.Close()
	p := &OpenAICompatibleProvider{ProviderName: "test", BaseURL: server.URL, APIKey: "k"}
	resp, err := p.ChatStream(context.Background(), ChatRequest{Model: "m"}, func(StreamChunk) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Usage.Reported || resp.Usage.TotalTokens != 9 || resp.Usage.InputTokens != 7 || resp.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

// TestChatStreamHTTPError 验证非 2xx 时返回带状态码的错误。
func TestChatStreamHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key"}}`))
	}))
	defer server.Close()

	p := &OpenAICompatibleProvider{ProviderName: "test", BaseURL: server.URL, APIKey: "k"}
	_, err := p.ChatStream(context.Background(),
		ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}},
		func(StreamChunk) error { return nil })
	if err == nil {
		t.Fatal("expected error on 401, got nil")
	}
}
