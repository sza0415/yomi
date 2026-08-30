package channels

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ziangsun/szabot/internal/agent"
	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/providers"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

// newTestWeb 造一个已初始化 subscribers 的 WebChannel（不真正监听端口），
// 方便直接调 handler 与 deliver 做单元测试。
func newTestWeb(b *bus.MessageBus) *WebChannel {
	return &WebChannel{
		ID:          "web",
		Bus:         b,
		subscribers: make(map[string]map[*subscriber]struct{}),
	}
}

// TestHandleSendPublishesInbound 验证 POST /api/send 会把消息翻译成
// InboundMessage 推进 bus，且带上正确的 session/channel。
func TestHandleSendPublishesInbound(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body, _ := json.Marshal(sendRequest{Session: "web:abc", Text: "你好"})
	req := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	w.handleSend(ctx)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	select {
	case in := <-b.Inbound():
		if in.ChannelID != "web" {
			t.Errorf("ChannelID = %q, want web", in.ChannelID)
		}
		if in.SessionID != "web:abc" {
			t.Errorf("SessionID = %q, want web:abc", in.SessionID)
		}
		if in.Text != "你好" {
			t.Errorf("Text = %q, want 你好", in.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for inbound message")
	}
}

// TestHandleSendRejectsEmpty 验证空文本被拒。
func TestHandleSendRejectsEmpty(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)
	ctx := context.Background()

	body, _ := json.Marshal(sendRequest{Session: "s", Text: ""})
	req := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()

	w.handleSend(ctx)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestHandleCancelCallsOnCancel(t *testing.T) {
	w := newTestWeb(bus.New(16))
	got := ""
	w.OnCancel = func(sessionID string) { got = sessionID }

	body, _ := json.Marshal(cancelRequest{Session: "web:cancel"})
	req := httptest.NewRequest(http.MethodPost, "/api/cancel", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	w.handleCancel(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got != "web:cancel" {
		t.Fatalf("OnCancel got %q, want web:cancel", got)
	}
	if !strings.Contains(rec.Body.String(), `"cancelled":true`) {
		t.Fatalf("cancel response = %s", rec.Body.String())
	}
}

func TestHandleCancelRequiresSession(t *testing.T) {
	w := newTestWeb(bus.New(16))
	req := httptest.NewRequest(http.MethodPost, "/api/cancel", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	w.handleCancel(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestInspectTraceIdentifiesCompleteAndIncompleteRuns(t *testing.T) {
	now := time.Now()
	complete := inspectTrace([]tracing.Event{
		{Sequence: 1, Timestamp: now, SessionID: "s", RunID: "r", Type: tracing.EventRunQueued, Data: map[string]any{"input": "hello"}},
		{Sequence: 3, Timestamp: now.Add(time.Millisecond), SessionID: "s", RunID: "r", Type: tracing.EventRunStarted},
		{Sequence: 8, Timestamp: now.Add(2 * time.Millisecond), SessionID: "s", RunID: "r", Type: tracing.EventRunFinished},
	})
	if !complete.Complete || complete.InputText != "hello" || complete.FirstSequence != 1 {
		t.Fatalf("complete metadata = %#v", complete)
	}

	incomplete := inspectTrace([]tracing.Event{
		{Sequence: 39, Timestamp: now, SessionID: "s", RunID: "r2", Type: tracing.EventModelRequestStarted},
		{Sequence: 45, Timestamp: now.Add(time.Millisecond), SessionID: "s", RunID: "r2", Type: tracing.EventRunFinished},
	})
	if incomplete.Complete || !strings.Contains(incomplete.Warning, "starts at sequence 39") || !strings.Contains(incomplete.Warning, "missing run.started") {
		t.Fatalf("incomplete metadata = %#v", incomplete)
	}
}

func TestTraceAPIReadsSessionAndRun(t *testing.T) {
	b := bus.New(16)
	sink, err := tracing.NewJSONLSink(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for sequence, event := range []tracing.Event{
		{Sequence: 1, Timestamp: now, SessionID: "web:trace", RunID: "run-1", AgentID: "default", Type: tracing.EventRunStarted, Status: "running"},
		{Sequence: 2, Timestamp: now.Add(time.Millisecond), SessionID: "web:trace", RunID: "run-1", AgentID: "default", Type: tracing.EventRunFinished, Status: "completed", Data: map[string]any{"answer": "ok"}},
	} {
		event.Sequence = uint64(sequence + 1)
		if err := sink.Record(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	w := newTestWeb(b)
	w.Trace = sink

	sessionReq := httptest.NewRequest(http.MethodGet, "/api/traces?session=web:trace", nil)
	sessionRec := httptest.NewRecorder()
	w.handleTraces(sessionRec, sessionReq)
	if sessionRec.Code != http.StatusOK || !strings.Contains(sessionRec.Body.String(), "run-1") {
		t.Fatalf("session trace response = %d %s", sessionRec.Code, sessionRec.Body.String())
	}

	runReq := httptest.NewRequest(http.MethodGet, "/api/traces/run?run_id=run-1", nil)
	runRec := httptest.NewRecorder()
	w.handleTraceRun(runRec, runReq)
	if runRec.Code != http.StatusOK || !strings.Contains(runRec.Body.String(), "completed") {
		t.Fatalf("run trace response = %d %s", runRec.Code, runRec.Body.String())
	}

	missingReq := httptest.NewRequest(http.MethodGet, "/api/traces/run?run_id=missing", nil)
	missingRec := httptest.NewRecorder()
	w.handleTraceRun(missingRec, missingReq)
	if missingRec.Code != http.StatusNotFound {
		t.Fatalf("missing trace status = %d, want 404", missingRec.Code)
	}
}

func TestRunAPIListsSnapshotsAndFiltersStatus(t *testing.T) {
	store, err := agent.NewJSONRunSnapshotStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	completed := agent.NewRun("web:runs", agent.RunBudget{})
	if err := completed.Transition(agent.RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := completed.Transition(agent.RunCompleted, "done"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(completed.Snapshot()); err != nil {
		t.Fatal(err)
	}
	cancelled := agent.NewRun("web:runs", agent.RunBudget{})
	if err := cancelled.Transition(agent.RunRunning, "started"); err != nil {
		t.Fatal(err)
	}
	if err := cancelled.Transition(agent.RunCancelled, "user requested cancellation"); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(cancelled.Snapshot()); err != nil {
		t.Fatal(err)
	}

	w := newTestWeb(bus.New(16))
	w.Snapshots = store
	req := httptest.NewRequest(http.MethodGet, "/api/runs?session=web:runs&status=cancelled", nil)
	rec := httptest.NewRecorder()
	w.handleRuns(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), string(agent.RunCancelled)) || strings.Contains(rec.Body.String(), string(agent.RunCompleted)) {
		t.Fatalf("run response = %d %s", rec.Code, rec.Body.String())
	}

	detailReq := httptest.NewRequest(http.MethodGet, "/api/traces/run?run_id="+cancelled.ID, nil)
	detailRec := httptest.NewRecorder()
	w.handleTraceRun(detailRec, detailReq)
	if detailRec.Code != http.StatusOK || !strings.Contains(detailRec.Body.String(), "user requested cancellation") {
		t.Fatalf("snapshot detail response = %d %s", detailRec.Code, detailRec.Body.String())
	}
}

func TestSessionAPIListsAndLoadsHistory(t *testing.T) {
	store, err := agent.NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("web:history", providers.Message{Role: providers.RoleUser, Content: "第一条问题"}, providers.Message{Role: providers.RoleAssistant, Content: "第一条回答"}); err != nil {
		t.Fatal(err)
	}
	w := newTestWeb(bus.New(16))
	w.Sessions = store

	listRec := httptest.NewRecorder()
	w.handleSessions(listRec, httptest.NewRequest(http.MethodGet, "/api/sessions", nil))
	if listRec.Code != http.StatusOK || !strings.Contains(listRec.Body.String(), "web:history") || !strings.Contains(listRec.Body.String(), "第一条问题") {
		t.Fatalf("session list response = %d %s", listRec.Code, listRec.Body.String())
	}

	historyRec := httptest.NewRecorder()
	w.handleSessionMessages(historyRec, httptest.NewRequest(http.MethodGet, "/api/session/messages?session=web:history", nil))
	if historyRec.Code != http.StatusOK || !strings.Contains(historyRec.Body.String(), "第一条回答") {
		t.Fatalf("session history response = %d %s", historyRec.Code, historyRec.Body.String())
	}
}

// TestDeliverRoutesBySession 验证出站消息只投给匹配 SessionID 的订阅者，
// 其他 session 的订阅者收不到——这是 Web 多连接场景的核心正确性。
func TestDeliverRoutesBySession(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	subA := &subscriber{sessionID: "A", events: make(chan bus.OutboundMessage, 4)}
	subB := &subscriber{sessionID: "B", events: make(chan bus.OutboundMessage, 4)}
	w.addSubscriber(subA)
	w.addSubscriber(subB)

	w.deliver(bus.OutboundMessage{ChannelID: "web", SessionID: "A", Text: "for-a", Delta: true})

	select {
	case out := <-subA.events:
		if out.Text != "for-a" {
			t.Errorf("subA got %q, want for-a", out.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("subA should have received the message")
	}

	select {
	case out := <-subB.events:
		t.Fatalf("subB should NOT receive session A's message, got %q", out.Text)
	case <-time.After(50 * time.Millisecond):
		// 正确：B 收不到。
	}
}

// TestRemoveSubscriber 验证注销后不再投递。
func TestRemoveSubscriber(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	sub := &subscriber{sessionID: "A", events: make(chan bus.OutboundMessage, 4)}
	w.addSubscriber(sub)
	w.removeSubscriber(sub)

	w.deliver(bus.OutboundMessage{ChannelID: "web", SessionID: "A", Text: "x", Delta: true})

	select {
	case <-sub.events:
		t.Fatal("removed subscriber should not receive messages")
	case <-time.After(50 * time.Millisecond):
		// 正确。
	}
}

// TestDispatchStreamsToSSE 端到端验证：dispatch 从 bus 读到属于 web 的出站消息，
// 经由 SSE handler 翻译成 AG-UI 事件推给"浏览器"。用 httptest server 起真实 HTTP。
func TestDispatchStreamsToSSE(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.dispatch(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", w.handleStream)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 起 SSE 连接。
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream?session=S", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request error: %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)

	// 连接建立先发 SESSION 事件，确认订阅者已注册。
	waitForLine(t, reader, `"type":"SESSION"`)

	// 通过 bus 推一条正文 Delta，应翻译成 TEXT_MESSAGE_START + TEXT_MESSAGE_CONTENT。
	_ = b.PublishOutbound(ctx, bus.OutboundMessage{
		ChannelID: "web", SessionID: "S", Text: "hello-sse", Delta: true,
	})

	line := waitForLine(t, reader, "hello-sse")
	if !strings.HasPrefix(line, "data: ") {
		t.Fatalf("expected data line, got %q", line)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
		t.Fatalf("bad json payload %q: %v", line, err)
	}
	if payload["type"] != "TEXT_MESSAGE_CONTENT" || payload["delta"] != "hello-sse" {
		t.Fatalf("unexpected payload: %v", payload)
	}
	if payload["messageId"] == nil || payload["messageId"] == "" {
		t.Fatalf("TEXT_MESSAGE_CONTENT must carry messageId: %v", payload)
	}
}

func TestDispatchStreamsToolKindsToSSE(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go w.dispatch(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", w.handleStream)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/stream?session=S", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream request error: %v", err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	waitForLine(t, reader, `"type":"SESSION"`)

	// 工具调用应翻译成 TOOL_CALL_START（含 toolCallId/toolCallName）。
	_ = b.PublishOutbound(ctx, bus.OutboundMessage{
		ChannelID: "web", SessionID: "S", Kind: bus.KindToolCall,
		Text: "read_file(README.md)", Delta: true,
		ToolCallID: "call_1", ToolName: "read_file", Arguments: `{"path":"README.md"}`,
	})
	line := waitForLine(t, reader, `"type":"TOOL_CALL_START"`)
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &payload); err != nil {
		t.Fatalf("bad json payload %q: %v", line, err)
	}
	if payload["toolCallId"] != "call_1" || payload["toolCallName"] != "read_file" {
		t.Fatalf("unexpected tool payload: %v", payload)
	}

	// 紧接着应有 TOOL_CALL_ARGS 携带参数。
	argsLine := waitForLine(t, reader, `"type":"TOOL_CALL_ARGS"`)
	var argsPayload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(argsLine, "data: ")), &argsPayload); err != nil {
		t.Fatalf("bad args payload %q: %v", argsLine, err)
	}
	if argsPayload["toolCallId"] != "call_1" || argsPayload["delta"] != `{"path":"README.md"}` {
		t.Fatalf("unexpected args payload: %v", argsPayload)
	}
}

// waitForLine 从 SSE 流里逐行读，直到某行包含 want 或超时。
func waitForLine(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream error: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		if strings.Contains(line, want) {
			return line
		}
	}
	t.Fatalf("timeout waiting for line containing %q", want)
	return ""
}

// TestDisconnectDoesNotCancel 验证：SSE 断开只移除订阅者，不会取消 Run。
func TestDisconnectDoesNotCancel(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	got := make(chan string, 1)
	w.OnCancel = func(sessionID string) { got <- sessionID }

	sub := &subscriber{sessionID: "S", events: make(chan bus.OutboundMessage, 4)}
	w.addSubscriber(sub)
	w.removeSubscriber(sub)

	select {
	case s := <-got:
		t.Fatalf("disconnect should not cancel, got %q", s)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestMultiTabDisconnectOnlyRemovesOne 验证：一个 session 开多标签页时，
// 断开其中一个连接只移除当前订阅者。
func TestMultiTabDisconnectOnlyRemovesOne(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)

	tab1 := &subscriber{sessionID: "S", events: make(chan bus.OutboundMessage, 4)}
	tab2 := &subscriber{sessionID: "S", events: make(chan bus.OutboundMessage, 4)}
	w.addSubscriber(tab1)
	w.addSubscriber(tab2)

	w.removeSubscriber(tab1)
	w.mu.RLock()
	remaining := len(w.subscribers["S"])
	w.mu.RUnlock()
	if remaining != 1 {
		t.Fatalf("remaining subscribers = %d, want 1", remaining)
	}
}
