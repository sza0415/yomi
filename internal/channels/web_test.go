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
		ID:               "web",
		Bus:              b,
		subscribers:      make(map[string]map[*subscriber]struct{}),
		pendingQuestions: make(map[string]bus.OutboundMessage),
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
		if in.UserID != "local" {
			t.Errorf("UserID = %q, want local", in.UserID)
		}
		if in.Text != "你好" {
			t.Errorf("Text = %q, want 你好", in.Text)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for inbound message")
	}
}

func TestHandleSendUsesConfiguredUserAcrossSessions(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)
	w.UserID = "my-user"
	ctx := context.Background()

	for _, session := range []string{"web:first", "web:second"} {
		body, _ := json.Marshal(sendRequest{Session: session, Text: "记住我的家乡"})
		req := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewReader(body))
		rec := httptest.NewRecorder()
		w.handleSend(ctx)(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("session %s status = %d; body=%s", session, rec.Code, rec.Body.String())
		}
		select {
		case in := <-b.Inbound():
			if in.SessionID != session {
				t.Errorf("SessionID = %q, want %q", in.SessionID, session)
			}
			if in.UserID != "my-user" {
				t.Errorf("UserID = %q, want my-user", in.UserID)
			}
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for inbound message for %s", session)
		}
	}
}

func TestIdentityReturnsConfiguredUser(t *testing.T) {
	w := newTestWeb(bus.New(16))
	w.UserID = "my-user"
	rec := httptest.NewRecorder()
	w.handleIdentity(rec, httptest.NewRequest(http.MethodGet, "/api/identity", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"user_id":"my-user"`) {
		t.Fatalf("identity response = %d %s", rec.Code, rec.Body.String())
	}
}

func TestIdentityCanBeChangedAtRuntime(t *testing.T) {
	w := newTestWeb(bus.New(16))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/identity", strings.NewReader(`{"user_id":"alice"}`))
	req.Header.Set("Content-Type", "application/json")
	w.handleIdentity(rec, req)
	if rec.Code != http.StatusOK || w.userID() != "alice" {
		t.Fatalf("identity update = %d %s, user=%q", rec.Code, rec.Body.String(), w.userID())
	}
}

func TestDebugResetInvokesCallback(t *testing.T) {
	w := newTestWeb(bus.New(16))
	called := false
	w.DebugReset = func(context.Context) error { called = true; return nil }
	rec := httptest.NewRecorder()
	w.handleDebugReset(rec, httptest.NewRequest(http.MethodPost, "/api/debug/reset", nil))
	if rec.Code != http.StatusOK || !called || !strings.Contains(rec.Body.String(), `"reset":true`) {
		t.Fatalf("reset response = %d %s called=%v", rec.Code, rec.Body.String(), called)
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

func TestNewSessionIDIsWindowsSafe(t *testing.T) {
	id := newSessionID()
	if !strings.HasPrefix(id, "web-") {
		t.Fatalf("newSessionID() = %q, want web- prefix", id)
	}
	if strings.ContainsAny(id, `<>:"/\|?*`) {
		t.Fatalf("newSessionID() = %q, contains a Windows-invalid filename character", id)
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

func TestHandleConfigReturnsSanitizedView(t *testing.T) {
	w := newTestWeb(bus.New(16))
	w.Config = ConfigView{Sections: []ConfigSection{{ID: "start", Title: "首次运行", Items: []ConfigItem{{Key: "provider", Value: "echo", Sensitive: true}}}}}
	req := httptest.NewRequest(http.MethodGet, "/api/config", nil)
	rec := httptest.NewRecorder()
	w.handleConfig(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "首次运行") || !strings.Contains(rec.Body.String(), "echo") {
		t.Fatalf("config response = %s", rec.Body.String())
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

func TestBuildSessionTimelineRestoresAgentActivities(t *testing.T) {
	now := time.Now()
	events := []tracing.Event{
		{Sequence: 1, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventInputReceived, Data: map[string]any{"content": "检查文件"}},
		{Sequence: 2, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventAssistantCompleted, Data: map[string]any{"message": providers.Message{
			Role: providers.RoleAssistant, Reasoning: "先读取 artifact", ToolCalls: []providers.ToolCall{{ID: "call-1", Name: "artifact_read", Arguments: json.RawMessage(`{"artifact_id":"a1"}`)}},
		}}},
		{Sequence: 3, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventToolExecutionStarted, Status: "running", Data: map[string]any{"tool_call_id": "call-1", "tool_name": "artifact_read", "arguments": `{"artifact_id":"a1"}`}},
		{Sequence: 4, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventUserQuestionAsked, Status: "waiting_user", Data: map[string]any{"question": "Allow tool?", "options": []string{"Allow once", "Allow always", "Deny"}}},
	}

	turns := buildSessionTimeline(events)
	if len(turns) != 1 || turns[0].User != "检查文件" || len(turns[0].Activities) != 3 {
		t.Fatalf("timeline = %#v", turns)
	}
	if turns[0].Activities[0].Kind != "reasoning" || turns[0].Activities[0].Content != "先读取 artifact" {
		t.Fatalf("reasoning activity = %#v", turns[0].Activities[0])
	}
	if turns[0].Activities[1].Kind != "tool" || turns[0].Activities[1].Title != "artifact_read" {
		t.Fatalf("tool activity = %#v", turns[0].Activities[1])
	}
	approval := turns[0].Activities[2]
	if approval.Kind != "approval" || approval.Status != "waiting" || len(approval.Options) != 3 {
		t.Fatalf("approval activity = %#v", approval)
	}

	events = append(events,
		tracing.Event{Sequence: 5, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventUserQuestionAnswered, Data: map[string]any{"answer": "Allow once"}},
		tracing.Event{Sequence: 6, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventToolExecutionFinished, Status: "completed", Data: map[string]any{"tool_call_id": "call-1", "tool_name": "artifact_read", "result": "artifact body"}},
		tracing.Event{Sequence: 7, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventAssistantCompleted, Data: map[string]any{"message": providers.Message{Role: providers.RoleAssistant, Content: "读取完成"}}},
		tracing.Event{Sequence: 8, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventRunFinished, Status: "completed"},
	)
	turns = buildSessionTimeline(events)
	if turns[0].Activities[1].Status != "completed" || turns[0].Activities[1].Result != "artifact body" {
		t.Fatalf("completed tool activity = %#v", turns[0].Activities[1])
	}
	if turns[0].Activities[2].Status != "completed" || turns[0].Activities[2].Answer != "Allow once" {
		t.Fatalf("answered approval activity = %#v", turns[0].Activities[2])
	}
	if turns[0].Assistant != "读取完成" || turns[0].Status != "completed" {
		t.Fatalf("completed turn = %#v", turns[0])
	}
}

func TestBuildSessionTimelineClosesApprovalWhenRunTerminates(t *testing.T) {
	now := time.Now()
	turns := buildSessionTimeline([]tracing.Event{
		{Sequence: 1, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventInputReceived, Data: map[string]any{"content": "搜索"}},
		{Sequence: 2, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventUserQuestionAsked, Status: "waiting_user", Data: map[string]any{"question": "Allow web search?", "options": []string{"Allow once", "Deny"}}},
		{Sequence: 3, Timestamp: now, SessionID: "S", RunID: "R", Type: tracing.EventRunFinished, Status: "timed_out"},
	})
	if len(turns) != 1 || len(turns[0].Activities) != 1 || turns[0].Activities[0].Status != "failed" {
		t.Fatalf("terminal approval timeline = %#v", turns)
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

func TestDeliverKeepsQuestionAndDoneWhenSubscriberQueueIsFull(t *testing.T) {
	w := newTestWeb(bus.New(16))
	sub := &subscriber{sessionID: "S", events: make(chan bus.OutboundMessage, 1)}
	w.addSubscriber(sub)

	// 模拟断点恢复后代理尚未及时消费：队列里已经积压一条普通增量。
	sub.events <- bus.OutboundMessage{SessionID: "S", Text: "stale", Delta: true}
	question := bus.OutboundMessage{SessionID: "S", RunID: "run-1", Kind: bus.KindQuestion, Text: "Allow tool?"}
	w.deliver(question)
	if got := <-sub.events; got.Kind != bus.KindQuestion {
		t.Fatalf("full queue kept %q, want permission question", got.Kind)
	}

	sub.events <- bus.OutboundMessage{SessionID: "S", Text: "stale", Delta: true}
	w.deliver(bus.OutboundMessage{SessionID: "S", RunID: "run-1", Done: true})
	if got := <-sub.events; !got.Done {
		t.Fatalf("full queue kept %#v, want run done", got)
	}
}

func TestPendingQuestionSurvivesDisconnectUntilAnswer(t *testing.T) {
	b := bus.New(16)
	w := newTestWeb(b)
	ctx := context.Background()

	question := bus.OutboundMessage{
		ChannelID: "web", SessionID: "S", RunID: "run-approval",
		Kind: bus.KindQuestion, Text: "Allow tool?",
		Meta: map[string]any{
			"question": "Allow tool?",
			"options":  []string{"Allow once", "Allow always", "Deny"},
		},
	}
	w.deliver(question)

	pending, ok := w.pendingQuestion("S")
	if !ok || pending.RunID != "run-approval" || pending.Text != "Allow tool?" {
		t.Fatalf("pending question = %#v, %v", pending, ok)
	}

	body, _ := json.Marshal(sendRequest{Session: "S", Text: "Allow once"})
	req := httptest.NewRequest(http.MethodPost, "/api/send", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	w.handleSend(ctx)(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("answer status = %d body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := w.pendingQuestion("S"); ok {
		t.Fatal("pending question was not cleared after the answer was published")
	}
}

func TestStreamReplaysPendingQuestion(t *testing.T) {
	w := newTestWeb(bus.New(16))
	w.deliver(bus.OutboundMessage{
		ChannelID: "web", SessionID: "S", RunID: "run-approval",
		Kind: bus.KindQuestion, Text: "Allow tool?",
		Meta: map[string]any{
			"question": "Allow tool?",
			"options":  []string{"Allow once", "Allow always", "Deny"},
		},
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
	line := waitForLine(t, reader, `"type":"CUSTOM"`)
	if !strings.Contains(line, `"ASK_USER_QUESTION"`) || !strings.Contains(line, `"Allow once"`) {
		t.Fatalf("replayed question payload = %s", line)
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
