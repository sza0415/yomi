package providers

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOpenAICompatibleProviderToolCallRoundTrip(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		var payload openAIChatRequest
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal request: %v", err)
		}

		switch requestCount {
		case 0:
			if len(payload.Tools) != 1 || payload.Tools[0].Function.Name != "read_file" {
				t.Fatalf("tools = %#v, want read_file", payload.Tools)
			}
			response := map[string]any{
				"choices": []any{map[string]any{
					"finish_reason": "tool_calls",
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []any{map[string]any{
							"id":   "call_1",
							"type": "function",
							"function": map[string]string{
								"name":      "read_file",
								"arguments": `{"path":"README.md"}`,
							},
						}},
					},
				}},
			}
			if err := json.NewEncoder(writer).Encode(response); err != nil {
				t.Errorf("write response: %v", err)
			}
		case 1:
			if len(payload.Messages) != 2 {
				t.Fatalf("messages = %d, want 2", len(payload.Messages))
			}
			if payload.Messages[0].Content != nil || len(payload.Messages[0].ToolCalls) != 1 {
				t.Fatalf("assistant replay = %#v", payload.Messages[0])
			}
			if payload.Messages[1].ToolCallID != "call_1" || payload.Messages[1].Content == nil || *payload.Messages[1].Content != "file contents" {
				t.Fatalf("tool replay = %#v", payload.Messages[1])
			}
			_, _ = writer.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"summary"}}]}`))
		}
		requestCount++
	}))
	defer server.Close()

	provider := &OpenAICompatibleProvider{
		BaseURL:    server.URL,
		APIKey:     "test-key",
		HTTPClient: server.Client(),
	}
	definition := ToolDefinition{
		Name:        "read_file",
		Description: "Read a file.",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`),
	}

	first, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "test-model",
		Messages: []Message{{Role: RoleUser, Content: "read README"}},
		Tools:    []ToolDefinition{definition},
	})
	if err != nil {
		t.Fatalf("first Chat() error = %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != "call_1" || string(first.ToolCalls[0].Arguments) != `{"path":"README.md"}` {
		t.Fatalf("first response tool calls = %#v", first.ToolCalls)
	}

	second, err := provider.Chat(context.Background(), ChatRequest{
		Model: "test-model",
		Messages: []Message{
			{Role: RoleAssistant, ToolCalls: first.ToolCalls},
			{Role: RoleTool, ToolCallID: "call_1", Content: "file contents"},
		},
	})
	if err != nil {
		t.Fatalf("second Chat() error = %v", err)
	}
	if second.Content != "summary" || second.FinishReason != "stop" {
		t.Fatalf("second response = %#v", second)
	}
}

func TestChatParsesUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,"prompt_tokens_details":{"cached_tokens":3},"completion_tokens_details":{"reasoning_tokens":2}}}`))
	}))
	defer server.Close()

	provider := &OpenAICompatibleProvider{BaseURL: server.URL, APIKey: "k", HTTPClient: server.Client()}
	resp, err := provider.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Content: "hi"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !resp.Usage.Reported || resp.Usage.InputTokens != 10 || resp.Usage.OutputTokens != 4 ||
		resp.Usage.TotalTokens != 14 || resp.Usage.CachedTokens != 3 || resp.Usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %#v", resp.Usage)
	}
}

// TestChatParsesReasoningContent 验证：推理型模型返回的 reasoning_content
// 会被解析进 ChatResponse.Reasoning，且不污染 Content。
func TestChatParsesReasoningContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"答案","reasoning_content":"我的思考过程"}}]}`))
	}))
	defer server.Close()

	provider := &OpenAICompatibleProvider{
		BaseURL:    server.URL,
		APIKey:     "k",
		HTTPClient: server.Client(),
	}
	resp, err := provider.Chat(context.Background(), ChatRequest{
		Model:    "reasoner",
		Messages: []Message{{Role: RoleUser, Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Chat() error = %v", err)
	}
	if resp.Content != "答案" {
		t.Fatalf("content = %q, want 答案", resp.Content)
	}
	if resp.Reasoning != "我的思考过程" {
		t.Fatalf("reasoning = %q, want 我的思考过程", resp.Reasoning)
	}
}
