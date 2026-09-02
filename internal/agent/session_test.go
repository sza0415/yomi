package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
)

// recordingProvider 记录每次收到的请求，并按脚本返回固定回复。
type recordingProvider struct {
	requests []providers.ChatRequest
	replies  []string
	idx      int
}

func (p *recordingProvider) Name() string { return "recording" }

func (p *recordingProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.requests = append(p.requests, req)
	reply := "ok"
	if p.idx < len(p.replies) {
		reply = p.replies[p.idx]
	}
	p.idx++
	return providers.ChatResponse{Content: reply}, nil
}

// TestSessionStoreRoundTrip 验证 jsonl 存储的 Append/Load round-trip 与跨实例读取。
func TestSessionStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore() error = %v", err)
	}

	if got, err := store.Load("s1"); err != nil || len(got) != 0 {
		t.Fatalf("Load new session = %v, %v; want empty, nil", got, err)
	}

	if err := store.Append("s1",
		providers.Message{Role: providers.RoleUser, Content: "hi"},
		providers.Message{Role: providers.RoleAssistant, Content: "hello"},
	); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	// 新建一个 store 实例，强制从磁盘重新读取，验证真的落盘了。
	reopened, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("reopen NewSessionStore() error = %v", err)
	}
	got, err := reopened.Load("s1")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if len(got) != 2 || got[0].Content != "hi" || got[1].Content != "hello" {
		t.Fatalf("Load() = %#v, want [hi, hello]", got)
	}

	// session 隔离：另一个 session 应为空。
	if other, err := reopened.Load("s2"); err != nil || len(other) != 0 {
		t.Fatalf("Load s2 = %v, %v; want empty, nil", other, err)
	}

	// 确认文件名按 SessionID 落在目录下。
	if _, err := reopened.Load("s1"); err != nil {
		t.Fatalf("second Load() error = %v", err)
	}
	if p := filepath.Join(dir, "s1.jsonl"); p == "" {
		t.Fatal("unexpected empty path")
	}
}

func TestSessionStoreWindowsUnsafeIDRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	const sessionID = "web:1700000000000:abc123"
	if err := store.Append(sessionID, providers.Message{Role: providers.RoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ":") {
			t.Fatalf("persisted filename %q is not Windows-safe", entry.Name())
		}
	}

	sessions, err := store.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || sessions[0].ID != sessionID {
		t.Fatalf("ListSessions() = %#v, want original session ID %q", sessions, sessionID)
	}
}

func TestSessionArchiveRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.AppendArchive("s1", ArchiveRecord{
		CoveredFrom: 2,
		CoveredTo:   6,
		RunID:       "run-1",
		Summary:     "FACTS:\nA fact\nDECISIONS:\nA decision",
		Sections:    map[string]string{"facts": "A fact", "decisions": "A decision"},
	})
	if err != nil || id == "" {
		t.Fatalf("AppendArchive() id=%q err=%v", id, err)
	}
	reopened, err := NewSessionStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	archives, err := reopened.LoadArchives("s1")
	if err != nil || len(archives) != 1 {
		t.Fatalf("LoadArchives() = %#v err=%v", archives, err)
	}
	if archives[0].ID != id || archives[0].CoveredFrom != 2 || archives[0].CoveredTo != 6 || archives[0].MessageCount != 4 {
		t.Fatalf("archive = %#v", archives[0])
	}
	if archives[0].Sections["facts"] != "A fact" {
		t.Fatalf("archive sections = %#v", archives[0].Sections)
	}
}

// TestLoopCarriesHistoryAcrossRequests 是本次修复的核心回归测试：
// 同一 session 的第二次请求，必须携带第一次的 user + assistant 历史。
func TestLoopCarriesHistoryAcrossRequests(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStore() error = %v", err)
	}

	provider := &recordingProvider{replies: []string{"first-reply", "second-reply"}}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}
	loop := &Loop{
		Bus:          bus.New(8),
		Runner:       runner,
		Store:        store,
		SystemPrompt: "SYS",
	}

	ctx := context.Background()
	loop.handle(ctx, bus.InboundMessage{SessionID: "sess", ChannelID: "cli", Text: "first"})
	loop.handle(ctx, bus.InboundMessage{SessionID: "sess", ChannelID: "cli", Text: "second"})

	if len(provider.requests) != 2 {
		t.Fatalf("Chat calls = %d, want 2", len(provider.requests))
	}

	// 第一次请求：system + user(first)。
	first := provider.requests[0].Messages
	if len(first) != 2 ||
		first[0].Role != providers.RoleSystem || first[0].Content != "SYS" ||
		first[1].Role != providers.RoleUser || first[1].Content != "first" {
		t.Fatalf("first request messages = %#v", first)
	}

	// 第二次请求：system + user(first) + assistant(first-reply) + user(second)。
	// 这正是历史被带上的证据。
	second := provider.requests[1].Messages
	if len(second) != 4 {
		t.Fatalf("second request messages = %d, want 4: %#v", len(second), second)
	}
	if second[0].Role != providers.RoleSystem || second[0].Content != "SYS" {
		t.Fatalf("second[0] = %#v, want system SYS", second[0])
	}
	if second[1].Role != providers.RoleUser || second[1].Content != "first" {
		t.Fatalf("second[1] = %#v, want user first", second[1])
	}
	if second[2].Role != providers.RoleAssistant || second[2].Content != "first-reply" {
		t.Fatalf("second[2] = %#v, want assistant first-reply", second[2])
	}
	if second[3].Role != providers.RoleUser || second[3].Content != "second" {
		t.Fatalf("second[3] = %#v, want user second", second[3])
	}
}

// TestLoopSessionsAreIsolated 验证不同 SessionID 之间历史互不串扰。
func TestLoopSessionsAreIsolated(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewSessionStore() error = %v", err)
	}
	provider := &recordingProvider{replies: []string{"a1", "b1"}}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}
	loop := &Loop{Bus: bus.New(8), Runner: runner, Store: store}

	ctx := context.Background()
	loop.handle(ctx, bus.InboundMessage{SessionID: "A", ChannelID: "cli", Text: "a-first"})
	loop.handle(ctx, bus.InboundMessage{SessionID: "B", ChannelID: "cli", Text: "b-first"})

	// 第二次请求属于 session B，不应看到 session A 的历史。
	second := provider.requests[1].Messages
	if len(second) != 1 || second[0].Content != "b-first" {
		t.Fatalf("session B request = %#v, want only [b-first]", second)
	}
}

// TestLoopPersistsOnlyConversation 验证内部推理、工具调用和工具结果不再污染
// session 上下文；Conversation 只持久化 user 与最终 assistant 正文。
func TestLoopPersistsOnlyConversation(t *testing.T) {
	dir := t.TempDir()
	store, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("NewSessionStore() error = %v", err)
	}
	registry := tools.NewRegistry()
	if err := registry.Register(echoTool{}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{
			Reasoning: "需要先调用 echo",
			ToolCalls: []providers.ToolCall{{
				ID:        "call_1",
				Name:      "echo_tool",
				Arguments: json.RawMessage(`{"value":"hello"}`),
			}},
		},
		{Content: "done", Reasoning: "总结完成"},
	}}
	runner := &Runner{Provider: provider, Model: "test", Tools: registry}
	loop := &Loop{Bus: bus.New(16), Runner: runner, Store: store}

	loop.handle(context.Background(), bus.InboundMessage{
		SessionID: "sess", ChannelID: "cli", Text: "use tool",
	})

	reopened, err := NewSessionStore(dir)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	got, err := reopened.Load("sess")
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("persisted messages = %d, want 2: %#v", len(got), got)
	}
	if got[0].Role != providers.RoleUser || got[0].Content != "use tool" {
		t.Fatalf("got[0] = %#v, want user", got[0])
	}
	if got[1].Role != providers.RoleAssistant || got[1].Content != "done" {
		t.Fatalf("got[1] = %#v, want final assistant", got[1])
	}
	if got[1].Reasoning != "" || len(got[1].ToolCalls) != 0 {
		t.Fatalf("internal trace leaked into conversation: %#v", got[1])
	}
}
