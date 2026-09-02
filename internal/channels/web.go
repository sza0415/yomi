package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ziangsun/szabot/internal/agent"
	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/providers"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

// WebChannel 是基于 HTTP 的 channel：浏览器通过它跟 agent 对话。
//
// 为什么用 SSE（Server-Sent Events）而不是 WebSocket？
//   - go.mod 不引第三方依赖，标准库没有 WebSocket，SSE 用 net/http 就能做；
//   - agent 的输出本就是"服务端单向流式推送"，SSE 天生贴合这个形态；
//   - 用户发消息走普通 POST 即可，不需要双向长连接。
//
// 关键难点——出站消息的分发（fan-out）：
//
//	bus.Outbound() 是一条被所有 channel 共享的 channel，多个消费者一起读会
//	互相抢消息。CLIChannel 独占终端时无所谓，但 Web 会同时有多个浏览器连接
//	（每个浏览器 = 一个 SessionID）。因此 WebChannel 用"单读多分发"模型：
//	  - 只有 dispatch() 这一个 goroutine 读 bus.Outbound()；
//	  - 它按 OutboundMessage.SessionID 找到对应的订阅者，把消息投递过去；
//	  - 每个 SSE 连接在建立时注册一个订阅者，断开时注销。
type WebChannel struct {
	// ID 就是 ChannelID，出站消息靠它区分归属。默认 "web"。
	ID string

	// UserID 是长期记忆的统一用户作用域。它与 SessionID 分离：一个用户
	// 可以创建多个 Web 会话，但这些会话共享同一组长期记忆。
	UserID string
	userMu sync.RWMutex

	// Bus 是消息总线引用。
	Bus *bus.MessageBus

	// Trace 是只读的 Run 轨迹查询器，供 Web Trace 工作台使用。
	Trace tracing.Reader
	// Snapshots 提供跨 Run 的任务摘要查询，包含没有完整 Trace 的中断 Run。
	Snapshots RunSnapshotReader
	// Sessions 提供 Web 会话列表和历史回放。对话写入仍由 Agent Loop 负责。
	Sessions *agent.SessionStore
	// Config is the sanitized startup configuration shown in the Web settings view.
	Config      ConfigView
	ConfigStore ConfigStore
	// DebugReset clears all local runtime data when explicitly requested by the
	// user through the debug endpoint.
	DebugReset func(context.Context) error

	// Addr 是 HTTP 监听地址，如 ":8080"。默认 ":8080"。
	Addr string

	// OnCancel 是 Web 客户端显式请求取消任务时的回调。SSE 断开不会调用它；
	// 连接生命周期和 Run 生命周期彼此独立。
	OnCancel func(sessionID string)

	// mu 保护 subscribers。
	mu sync.RWMutex
	// subscribers 按 SessionID 记录当前在线的 SSE 连接。
	// 一个 SessionID 理论上可能有多个连接（同一会话开了多个标签页），
	// 所以 value 是一个集合。
	subscribers map[string]map[*subscriber]struct{}
	// pendingQuestions 保存尚未得到用户回答的问题。权限 Hook 会在工具执行
	// 中途阻塞；如果浏览器刷新或 SSE 短暂断线，新连接必须能重新拿到确认卡，
	// 否则 Run 会永远停在 waiting_user。
	pendingQuestions map[string]bus.OutboundMessage
}

// subscriber 代表一个在线的 SSE 连接。
// events 是投递该连接的出站消息队列；dispatch 往里写，SSE handler 往外读。
type subscriber struct {
	sessionID string
	events    chan bus.OutboundMessage
}

// webSessionCookie 是给浏览器分配会话的 cookie 名。
const webSessionCookie = "szabot_session"

// sendRequest 是 POST /api/send 的请求体。
type sendRequest struct {
	Session string `json:"session"`
	Text    string `json:"text"`
}

type cancelRequest struct {
	Session string `json:"session"`
}

// Start 起 HTTP 服务与出站分发 goroutine。
//
// 注意 ctx 取消时会优雅关停 HTTP server，避免端口泄漏。
func (w *WebChannel) Start(ctx context.Context) error {
	if w.ID == "" {
		w.ID = "web"
	}
	if w.Addr == "" {
		w.Addr = ":8080"
	}
	w.subscribers = make(map[string]map[*subscriber]struct{})
	w.pendingQuestions = make(map[string]bus.OutboundMessage)

	// 出站分发：全局唯一的 goroutine 读 bus，按 SessionID 投递。
	if w.Bus != nil {
		go w.dispatch(ctx)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", w.handleAPIIndex)
	mux.HandleFunc("/api/send", w.handleSend(ctx))
	mux.HandleFunc("/api/cancel", w.handleCancel)
	mux.HandleFunc("/api/stream", w.handleStream)
	mux.HandleFunc("/api/traces", w.handleTraces)
	mux.HandleFunc("/api/traces/run", w.handleTraceRun)
	mux.HandleFunc("/api/runs", w.handleRuns)
	mux.HandleFunc("/api/sessions", w.handleSessions)
	mux.HandleFunc("/api/session/messages", w.handleSessionMessages)
	mux.HandleFunc("/api/config", w.handleConfig)
	mux.HandleFunc("/api/identity", w.handleIdentity)
	mux.HandleFunc("/api/debug/reset", w.handleDebugReset)

	server := &http.Server{Addr: w.Addr, Handler: mux}

	// ctx 取消 → 关 server。用一个短超时的独立 ctx 做 Shutdown。
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	// 在独立 goroutine 里跑，避免阻塞调用方（跟 CLIChannel.Start 语义一致）。
	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[web] server error: %v", err)
		}
	}()

	return nil
}

func (w *WebChannel) handleConfig(rw http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		view := w.Config
		if w.ConfigStore != nil {
			view.Editable = true
			view.Values = w.ConfigStore.Snapshot()
		}
		writeJSON(rw, view)
		return
	}
	if r.Method != http.MethodPut || w.ConfigStore == nil {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var values map[string]string
	if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
		http.Error(rw, "invalid config", http.StatusBadRequest)
		return
	}
	if err := w.ConfigStore.Update(values); err != nil {
		http.Error(rw, "save config failed", http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"saved": true})
}

// dispatch 是唯一读 bus.Outbound() 的 goroutine：按 SessionID 把出站消息
// 投递给对应的所有订阅者。找不到订阅者（连接已断/尚未建立）时直接丢弃。
func (w *WebChannel) dispatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case out, ok := <-w.Bus.Outbound():
			if !ok {
				return
			}
			// 只处理属于自己的消息。
			if out.ChannelID != w.ID {
				continue
			}
			w.deliver(out)
		}
	}
}

// deliver 把一条出站消息投递给指定 session 的全部订阅者。
func (w *WebChannel) deliver(out bus.OutboundMessage) {
	w.mu.Lock()
	if w.pendingQuestions == nil {
		w.pendingQuestions = make(map[string]bus.OutboundMessage)
	}
	if out.Kind == bus.KindQuestion {
		w.pendingQuestions[out.SessionID] = cloneQuestion(out)
	} else if out.Done {
		delete(w.pendingQuestions, out.SessionID)
	}
	subs := w.subscribers[out.SessionID]
	targets := make([]*subscriber, 0, len(subs))
	for s := range subs {
		targets = append(targets, s)
	}
	w.mu.Unlock()

	for _, s := range targets {
		select {
		case s.events <- out:
		default:
			// 思考/正文增量允许由 History/Trace 恢复，但等待用户确认和 Run
			// 收尾不能丢，否则前端会永久停在“运行中”。队列满时淘汰一条
			// 较旧事件，给关键控制事件留出位置，同时保证 dispatch 不阻塞。
			if out.Kind == bus.KindQuestion || out.Done {
				select {
				case <-s.events:
				default:
				}
				select {
				case s.events <- out:
				default:
				}
			}
		}
	}
}

func cloneQuestion(out bus.OutboundMessage) bus.OutboundMessage {
	clone := out
	if out.Meta != nil {
		clone.Meta = make(map[string]any, len(out.Meta))
		for key, value := range out.Meta {
			if options, ok := value.([]string); ok {
				clone.Meta[key] = append([]string(nil), options...)
				continue
			}
			clone.Meta[key] = value
		}
	}
	return clone
}

func (w *WebChannel) pendingQuestion(sessionID string) (bus.OutboundMessage, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()
	out, ok := w.pendingQuestions[sessionID]
	return cloneQuestion(out), ok
}

func (w *WebChannel) clearPendingQuestion(sessionID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.pendingQuestions, sessionID)
}

// addSubscriber / removeSubscriber 维护 session → 连接集合的映射。
func (w *WebChannel) addSubscriber(s *subscriber) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.subscribers[s.sessionID] == nil {
		w.subscribers[s.sessionID] = make(map[*subscriber]struct{})
	}
	w.subscribers[s.sessionID][s] = struct{}{}

}

func (w *WebChannel) removeSubscriber(s *subscriber) {
	w.mu.Lock()
	defer w.mu.Unlock()
	set := w.subscribers[s.sessionID]
	if set == nil {
		return
	}
	delete(set, s)
	if len(set) > 0 {
		// 该 session 还有别的活连接（如多标签页），只移除当前连接。
		return
	}
	delete(w.subscribers, s.sessionID)
}

// handleAPIIndex 只描述当前 HTTP 服务。Vue 前端已独立到仓库根目录 web/，
// 开发和发布均由该工程负责；Go channel 不再携带或托管静态资源。
func (w *WebChannel) handleAPIIndex(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	writeJSON(rw, map[string]any{
		"service": "yomi-web-api",
		"status":  "ok",
		"frontend": map[string]string{
			"directory": "web",
			"command":   "npm run dev",
		},
	})
}

type traceRunView struct {
	RunID         string          `json:"run_id"`
	SessionID     string          `json:"session_id"`
	AgentID       string          `json:"agent_id"`
	Status        string          `json:"status,omitempty"`
	InputText     string          `json:"input_text,omitempty"`
	FirstSequence uint64          `json:"first_sequence,omitempty"`
	LastSequence  uint64          `json:"last_sequence,omitempty"`
	TraceComplete bool            `json:"trace_complete"`
	TraceWarning  string          `json:"trace_warning,omitempty"`
	StartedAt     time.Time       `json:"started_at,omitempty"`
	FinishedAt    time.Time       `json:"finished_at,omitempty"`
	EventCount    int             `json:"event_count"`
	Events        []tracing.Event `json:"events,omitempty"`
}

type RunSnapshotReader interface {
	List(sessionID string) ([]agent.RunSnapshot, error)
	Load(runID string) (agent.RunSnapshot, error)
}

type runSummaryView struct {
	RunID         string            `json:"run_id"`
	SessionID     string            `json:"session_id"`
	AgentID       string            `json:"agent_id"`
	Status        agent.RunStatus   `json:"status"`
	StatusReason  string            `json:"status_reason,omitempty"`
	Error         string            `json:"error,omitempty"`
	InputText     string            `json:"input_text,omitempty"`
	FirstSequence uint64            `json:"first_sequence,omitempty"`
	LastSequence  uint64            `json:"last_sequence,omitempty"`
	TraceComplete bool              `json:"trace_complete"`
	TraceWarning  string            `json:"trace_warning,omitempty"`
	QueuedAt      time.Time         `json:"queued_at"`
	StartedAt     time.Time         `json:"started_at,omitempty"`
	FinishedAt    time.Time         `json:"finished_at,omitempty"`
	UpdatedAt     time.Time         `json:"updated_at"`
	ModelStatus   string            `json:"model_status,omitempty"`
	ToolStatuses  map[string]string `json:"tool_statuses,omitempty"`
	ModelCalls    int               `json:"model_calls"`
	ToolCalls     int               `json:"tool_calls"`
	EventCount    int               `json:"event_count"`
	Events        []tracing.Event   `json:"events,omitempty"`
}

func writeJSON(rw http.ResponseWriter, value any) {
	rw.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(rw).Encode(value)
}

// handleSessions lists persisted Web/CLI conversations. Session IDs are
// intentionally returned as opaque values; the browser sends them back when
// selecting a conversation.
func (w *WebChannel) handleSessions(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if w.Sessions == nil {
		http.Error(rw, "session store unavailable", http.StatusServiceUnavailable)
		return
	}
	userID := w.userID()
	var sessions []agent.SessionInfo
	var err error
	if scoped, ok := interface{}(w.Sessions).(interface {
		ListSessionsForUser(string) ([]agent.SessionInfo, error)
	}); ok {
		sessions, err = scoped.ListSessionsForUser(userID)
	} else {
		sessions, err = w.Sessions.ListSessions()
	}
	if err != nil {
		http.Error(rw, "list sessions failed", http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"user_id": userID, "sessions": sessions})
}

func (w *WebChannel) handleIdentity(rw http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var request struct {
			UserID string `json:"user_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(rw, "invalid user id", http.StatusBadRequest)
			return
		}
		if err := w.setUserID(request.UserID); err != nil {
			http.Error(rw, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(rw, map[string]string{"user_id": w.userID()})
		return
	}
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(rw, map[string]string{"user_id": w.userID()})
}

func (w *WebChannel) handleDebugReset(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if w.DebugReset == nil {
		http.Error(rw, "debug reset unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := w.DebugReset(r.Context()); err != nil {
		http.Error(rw, "reset failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"reset": true, "user_id": w.userID()})
}

func (w *WebChannel) userID() string {
	w.userMu.RLock()
	defer w.userMu.RUnlock()
	userID := strings.TrimSpace(w.UserID)
	if userID == "" {
		return "local"
	}
	return userID
}

func (w *WebChannel) setUserID(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("user id is required")
	}
	w.userMu.Lock()
	w.UserID = value
	w.userMu.Unlock()
	return nil
}

type sessionActivityView struct {
	ID      string   `json:"id"`
	Kind    string   `json:"kind"`
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Result  string   `json:"result,omitempty"`
	Status  string   `json:"status"`
	RunID   string   `json:"runId,omitempty"`
	Options []string `json:"options,omitempty"`
	Answer  string   `json:"answer,omitempty"`
}

type sessionTurnView struct {
	RunID      string                `json:"runId"`
	User       string                `json:"user"`
	Assistant  string                `json:"assistant"`
	Status     string                `json:"status,omitempty"`
	Activities []sessionActivityView `json:"activities"`
}

// handleSessionMessages returns the provider-safe Conversation plus a
// display-only timeline reconstructed from Trace. The timeline lets the Web UI
// restore reasoning, tool execution/results, and pending approvals after a
// refresh without feeding those internal events back into the model context.
func (w *WebChannel) handleSessionMessages(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if w.Sessions == nil {
		http.Error(rw, "session store unavailable", http.StatusServiceUnavailable)
		return
	}
	sessionID := strings.TrimSpace(r.URL.Query().Get("session"))
	if sessionID == "" {
		http.Error(rw, "missing session", http.StatusBadRequest)
		return
	}
	userID := w.userID()
	var history []providers.Message
	var err error
	if scoped, ok := interface{}(w.Sessions).(interface {
		LoadForUser(string, string) ([]providers.Message, error)
	}); ok {
		history, err = scoped.LoadForUser(userID, sessionID)
	} else {
		history, err = w.Sessions.Load(sessionID)
	}
	if err != nil {
		http.Error(rw, "session unavailable", http.StatusNotFound)
		return
	}
	turns := []sessionTurnView{}
	if w.Trace != nil {
		if events, traceErr := w.Trace.ReadSession(sessionID); traceErr == nil {
			turns = buildSessionTimeline(events)
		}
	}
	writeJSON(rw, map[string]any{"session_id": sessionID, "messages": history, "turns": turns})
}

func buildSessionTimeline(events []tracing.Event) []sessionTurnView {
	turns := make([]sessionTurnView, 0)
	turnIndex := make(map[string]int)
	toolIndex := make(map[string]map[string]int)

	ensureTurn := func(runID string) *sessionTurnView {
		if index, ok := turnIndex[runID]; ok {
			return &turns[index]
		}
		turnIndex[runID] = len(turns)
		toolIndex[runID] = make(map[string]int)
		turns = append(turns, sessionTurnView{RunID: runID, Activities: []sessionActivityView{}})
		return &turns[len(turns)-1]
	}

	for _, event := range events {
		if event.RunID == "" {
			continue
		}
		turn := ensureTurn(event.RunID)
		switch event.Type {
		case tracing.EventInputReceived:
			turn.User, _ = event.Data["content"].(string)
		case tracing.EventAssistantCompleted:
			var message providers.Message
			if raw, err := json.Marshal(event.Data["message"]); err == nil {
				_ = json.Unmarshal(raw, &message)
			}
			if message.Reasoning != "" {
				turn.Activities = append(turn.Activities, sessionActivityView{
					ID: fmt.Sprintf("%s-reasoning-%d", event.RunID, event.Sequence), Kind: "reasoning",
					Title: "思考过程", Content: message.Reasoning, Status: "completed", RunID: event.RunID,
				})
			}
			for _, call := range message.ToolCalls {
				if _, exists := toolIndex[event.RunID][call.ID]; exists {
					continue
				}
				toolIndex[event.RunID][call.ID] = len(turn.Activities)
				turn.Activities = append(turn.Activities, sessionActivityView{
					ID: call.ID, Kind: "tool", Title: call.Name, Content: string(call.Arguments),
					Status: "running", RunID: event.RunID,
				})
			}
			if strings.TrimSpace(message.Content) != "" {
				if turn.Assistant != "" {
					turn.Assistant += "\n\n"
				}
				turn.Assistant += message.Content
			}
		case tracing.EventToolExecutionStarted:
			toolID, _ := event.Data["tool_call_id"].(string)
			toolName, _ := event.Data["tool_name"].(string)
			arguments, _ := event.Data["arguments"].(string)
			index, exists := toolIndex[event.RunID][toolID]
			if !exists {
				index = len(turn.Activities)
				toolIndex[event.RunID][toolID] = index
				turn.Activities = append(turn.Activities, sessionActivityView{ID: toolID, Kind: "tool", RunID: event.RunID})
			}
			activity := &turn.Activities[index]
			activity.Title, activity.Content, activity.Status = toolName, arguments, "running"
		case tracing.EventToolExecutionFinished, tracing.EventToolExecutionFailed:
			toolID, _ := event.Data["tool_call_id"].(string)
			index, exists := toolIndex[event.RunID][toolID]
			if !exists {
				index = len(turn.Activities)
				toolIndex[event.RunID][toolID] = index
				toolName, _ := event.Data["tool_name"].(string)
				turn.Activities = append(turn.Activities, sessionActivityView{ID: toolID, Kind: "tool", Title: toolName, RunID: event.RunID})
			}
			activity := &turn.Activities[index]
			activity.Result, _ = event.Data["result"].(string)
			if failure, ok := event.Data["error"].(string); ok && failure != "" {
				activity.Result = failure
				activity.Status = "failed"
			} else {
				activity.Status = "completed"
			}
		case tracing.EventUserQuestionAsked:
			question, _ := event.Data["question"].(string)
			turn.Activities = append(turn.Activities, sessionActivityView{
				ID: fmt.Sprintf("%s-approval-%d", event.RunID, event.Sequence), Kind: "approval",
				Title: approvalTitle(stringSlice(event.Data["options"])), Content: question,
				Status: "waiting", RunID: event.RunID, Options: stringSlice(event.Data["options"]),
			})
		case tracing.EventUserQuestionAnswered:
			for index := len(turn.Activities) - 1; index >= 0; index-- {
				activity := &turn.Activities[index]
				if activity.Kind == "approval" && activity.Status == "waiting" {
					activity.Answer, _ = event.Data["answer"].(string)
					activity.Status = "completed"
					break
				}
			}
		case tracing.EventRunFinished:
			turn.Status = event.Status
			// Run 已结束后不应再把历史授权卡恢复成可操作的 waiting 状态。
			// 断点/超时经常发生在等待授权期间；若缺少 answered 事件，就把
			// 该卡标成失败，避免页面重连后向一个已结束的 Run 继续提交答案。
			for index := range turn.Activities {
				activity := &turn.Activities[index]
				if activity.Kind == "approval" && activity.Status == "waiting" {
					activity.Status = "failed"
				} else if activity.Status == "running" {
					if event.Status == string(agent.RunCompleted) {
						activity.Status = "completed"
					} else {
						activity.Status = "failed"
					}
				}
			}
		}
	}

	return turns
}

func stringSlice(value any) []string {
	switch options := value.(type) {
	case []string:
		return append([]string(nil), options...)
	case []any:
		result := make([]string, 0, len(options))
		for _, option := range options {
			if text, ok := option.(string); ok {
				result = append(result, text)
			}
		}
		return result
	default:
		return nil
	}
}

func approvalTitle(options []string) string {
	for _, option := range options {
		if option == "Allow once" {
			return "工具权限确认"
		}
	}
	return "需要你的回答"
}

// handleTraces 返回一个 Session 下按 Run 分组的 Trace 摘要。
func (w *WebChannel) handleTraces(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID == "" {
		http.Error(rw, "missing session", http.StatusBadRequest)
		return
	}
	if !w.sessionAllowed(sessionID) {
		http.Error(rw, "session unavailable", http.StatusNotFound)
		return
	}
	if w.Trace == nil {
		http.Error(rw, "trace reader unavailable", http.StatusServiceUnavailable)
		return
	}
	events, err := w.Trace.ReadSession(sessionID)
	if err != nil {
		http.Error(rw, "read trace failed", http.StatusInternalServerError)
		return
	}
	writeJSON(rw, map[string]any{"session_id": sessionID, "runs": groupTraceRuns(events, false)})
}

// handleRuns returns snapshot-backed run summaries. Unlike /api/traces, this
// also includes interrupted runs that may not have a complete trace file.
func (w *WebChannel) handleRuns(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if w.Snapshots == nil {
		http.Error(rw, "run snapshot reader unavailable", http.StatusServiceUnavailable)
		return
	}
	sessionID := r.URL.Query().Get("session")
	if sessionID != "" && !w.sessionAllowed(sessionID) {
		http.Error(rw, "session unavailable", http.StatusNotFound)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	snapshots, err := w.Snapshots.List(sessionID)
	if err != nil {
		http.Error(rw, "list runs failed", http.StatusInternalServerError)
		return
	}
	runs := make([]runSummaryView, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if statusFilter != "" && string(snapshot.Status) != statusFilter {
			continue
		}
		runs = append(runs, w.summaryFromSnapshot(snapshot, false))
	}
	writeJSON(rw, map[string]any{"session_id": sessionID, "runs": runs})
}

// handleTraceRun 返回一个 Run 的完整事件，供右侧详情面板读取。
func (w *WebChannel) handleTraceRun(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runID := r.URL.Query().Get("run_id")
	if runID == "" {
		http.Error(rw, "missing run_id", http.StatusBadRequest)
		return
	}
	if w.Snapshots != nil {
		if snapshot, err := w.Snapshots.Load(runID); err == nil {
			if !w.sessionAllowed(snapshot.SessionID) {
				http.Error(rw, "run unavailable", http.StatusNotFound)
				return
			}
			writeJSON(rw, w.summaryFromSnapshot(snapshot, true))
			return
		}
	}
	if w.Trace != nil {
		events, err := w.Trace.ReadRun(runID)
		if err != nil {
			http.Error(rw, "read trace failed", http.StatusInternalServerError)
			return
		}
		if len(events) > 0 {
			if !w.sessionAllowed(events[0].SessionID) {
				http.Error(rw, "run unavailable", http.StatusNotFound)
				return
			}
			runs := groupTraceRuns(events, true)
			writeJSON(rw, runs[0])
			return
		}
	}
	http.NotFound(rw, r)
}

func (w *WebChannel) sessionAllowed(sessionID string) bool {
	if w.Sessions == nil {
		return true
	}
	userID := w.userID()
	_, err := w.Sessions.LoadForUser(userID, strings.TrimSpace(sessionID))
	return err == nil
}

func (w *WebChannel) summaryFromSnapshot(snapshot agent.RunSnapshot, includeEvents bool) runSummaryView {
	view := runSummaryView{
		RunID: snapshot.ID, SessionID: snapshot.SessionID, AgentID: snapshot.AgentID,
		Status: snapshot.Status, StatusReason: snapshot.StatusReason, Error: snapshot.Error,
		QueuedAt: snapshot.QueuedAt, StartedAt: snapshot.StartedAt, FinishedAt: snapshot.FinishedAt,
		UpdatedAt: snapshot.UpdatedAt, ToolStatuses: make(map[string]string),
		ModelCalls: snapshot.Usage.ModelCalls, ToolCalls: snapshot.Usage.ToolCalls,
	}
	if w.Trace == nil {
		view.TraceWarning = "trace unavailable"
		return view
	}
	events, err := w.Trace.ReadRun(snapshot.ID)
	if err != nil {
		view.TraceWarning = "trace unavailable"
		return view
	}
	meta := inspectTrace(events)
	view.InputText = meta.InputText
	view.FirstSequence = meta.FirstSequence
	view.LastSequence = meta.LastSequence
	view.TraceComplete = meta.Complete
	view.TraceWarning = meta.Warning
	view.EventCount = len(events)
	for _, event := range events {
		switch event.Type {
		case tracing.EventModelRequestStarted, tracing.EventModelStatusChanged:
			view.ModelStatus = event.Status
		case tracing.EventModelResponseFinished, tracing.EventModelRequestFailed:
			view.ModelStatus = event.Status
		case tracing.EventToolStatusChanged:
			if id, ok := event.Data["tool_call_id"].(string); ok && id != "" {
				view.ToolStatuses[id] = event.Status
			}
		case tracing.EventToolExecutionStarted:
			if id, ok := event.Data["tool_call_id"].(string); ok && id != "" {
				if _, exists := view.ToolStatuses[id]; !exists {
					view.ToolStatuses[id] = event.Status
				}
			}
		}
	}
	if !includeEvents {
		return view
	}
	view.Events = events
	return view
}

type traceMetadata struct {
	InputText     string
	FirstSequence uint64
	LastSequence  uint64
	Complete      bool
	Warning       string
}

// inspectTrace identifies the Run boundary without requiring every sequence
// number to be contiguous. Outbound stream events share the Run sequence
// counter but are intentionally not persisted, so gaps after the first event
// are expected.
func inspectTrace(events []tracing.Event) traceMetadata {
	meta := traceMetadata{}
	if len(events) == 0 {
		meta.Warning = "trace is empty"
		return meta
	}
	meta.FirstSequence = events[0].Sequence
	meta.LastSequence = events[len(events)-1].Sequence
	hasQueued, hasStarted, hasFinished := false, false, false
	for _, event := range events {
		switch event.Type {
		case tracing.EventRunQueued:
			hasQueued = true
			if meta.InputText == "" {
				if input, ok := event.Data["input"].(string); ok {
					meta.InputText = input
				}
			}
		case tracing.EventInputReceived:
			if meta.InputText == "" {
				if input, ok := event.Data["content"].(string); ok {
					meta.InputText = input
				}
			}
		case tracing.EventRunStarted:
			hasStarted = true
		case tracing.EventRunFinished:
			hasFinished = true
		}
	}
	warnings := make([]string, 0, 2)
	if meta.FirstSequence != 1 {
		warnings = append(warnings, fmt.Sprintf("starts at sequence %d", meta.FirstSequence))
	}
	if !hasQueued {
		warnings = append(warnings, "missing run.queued")
	}
	if !hasStarted {
		warnings = append(warnings, "missing run.started")
	}
	if !hasFinished {
		warnings = append(warnings, "missing run.finished")
	}
	meta.Complete = len(warnings) == 0
	if !meta.Complete {
		meta.Warning = "trace incomplete: " + strings.Join(warnings, "; ")
	}
	return meta
}

func groupTraceRuns(events []tracing.Event, includeEvents bool) []traceRunView {
	byRun := make(map[string]*traceRunView)
	order := make([]string, 0)
	for _, event := range events {
		run := byRun[event.RunID]
		if run == nil {
			run = &traceRunView{RunID: event.RunID, SessionID: event.SessionID, AgentID: event.AgentID}
			byRun[event.RunID] = run
			order = append(order, event.RunID)
		}
		run.EventCount++
		if event.Status != "" {
			run.Status = event.Status
		}
		if event.Type == tracing.EventRunStarted && run.StartedAt.IsZero() {
			run.StartedAt = event.Timestamp
		}
		if event.Type == tracing.EventRunFinished {
			run.FinishedAt = event.Timestamp
		}
		run.Events = append(run.Events, event)
	}
	for _, run := range byRun {
		meta := inspectTrace(run.Events)
		run.InputText = meta.InputText
		run.FirstSequence = meta.FirstSequence
		run.LastSequence = meta.LastSequence
		run.TraceComplete = meta.Complete
		run.TraceWarning = meta.Warning
		if !includeEvents {
			run.Events = nil
		}
	}
	result := make([]traceRunView, 0, len(order))
	for i := len(order) - 1; i >= 0; i-- {
		result = append(result, *byRun[order[i]])
	}
	return result
}

// handleSend 接收浏览器发来的用户消息，翻译成 InboundMessage 推进 bus。
//
// 用闭包捕获 Start 的 ctx：请求处理需要在系统关停时能被取消，避免卡在
// PublishInbound 上。
func (w *WebChannel) handleSend(ctx context.Context) http.HandlerFunc {
	return func(rw http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req sendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(rw, "bad request", http.StatusBadRequest)
			return
		}
		if req.Text == "" {
			http.Error(rw, "empty text", http.StatusBadRequest)
			return
		}

		// session 优先取请求体，其次取 cookie，最后兜底分配一个。
		session := req.Session
		if session == "" {
			if c, err := r.Cookie(webSessionCookie); err == nil {
				session = c.Value
			}
		}
		if session == "" {
			session = newSessionID()
		}

		in := bus.InboundMessage{
			ChannelID: w.ID,
			SessionID: session,
			UserID:    w.userID(),
			Text:      req.Text,
			Time:      time.Now(),
		}
		if in.UserID == "" {
			in.UserID = "local"
		}
		if w.Sessions != nil {
			if err := w.Sessions.SetSessionUser(session, in.UserID); err != nil {
				http.Error(rw, "session belongs to another user", http.StatusForbidden)
				return
			}
		}
		if err := w.Bus.PublishInbound(ctx, in); err != nil {
			http.Error(rw, "publish failed", http.StatusServiceUnavailable)
			return
		}
		// pending question 的唯一合法推进方式就是同一 session 的下一条输入。
		// Publish 成功后再清理，失败时保留确认卡供用户重试。
		w.clearPendingQuestion(session)

		rw.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(rw).Encode(map[string]string{"session": session})
	}
}

// handleCancel 处理 Web 客户端的显式取消请求。请求断开、SSE 重连和页面
// 刷新都不会触发取消；只有前端调用这个接口才会停止对应 Session 的 Run。
func (w *WebChannel) handleCancel(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(rw, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req cancelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(rw, "bad request", http.StatusBadRequest)
		return
	}
	session := req.Session
	if session == "" {
		if c, err := r.Cookie(webSessionCookie); err == nil {
			session = c.Value
		}
	}
	if session == "" {
		http.Error(rw, "missing session", http.StatusBadRequest)
		return
	}
	if w.OnCancel == nil {
		http.Error(rw, "cancellation unavailable", http.StatusServiceUnavailable)
		return
	}
	w.OnCancel(session)
	w.clearPendingQuestion(session)

	rw.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(rw).Encode(map[string]any{"session": session, "cancelled": true})
}

// handleStream 建立 SSE 长连接，把该 session 的出站消息实时推给浏览器。
//
// 出站事件对齐 AG-UI 协议：每条事件形如
//
//	id: <TYPE>_<ts>
//	data: {"type":"<TYPE>",...}
//
// 内部扁平的 OutboundMessage 分片由 aguiTranslator 在此翻译成带生命周期的
// AG-UI 事件（TEXT_MESSAGE_*/TOOL_CALL_*/REASONING_MESSAGE_*/RUN_* 等）。
func (w *WebChannel) handleStream(rw http.ResponseWriter, r *http.Request) {
	session := r.URL.Query().Get("session")
	if session == "" {
		http.Error(rw, "missing session", http.StatusBadRequest)
		return
	}

	flusher, ok := rw.(http.Flusher)
	if !ok {
		http.Error(rw, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	rw.Header().Set("Content-Type", "text/event-stream")
	rw.Header().Set("Cache-Control", "no-cache")
	rw.Header().Set("Connection", "keep-alive")
	rw.Header().Set("X-Accel-Buffering", "no")

	sub := &subscriber{
		sessionID: session,
		// 缓冲给足，突发的流式增量不至于因为瞬时消费慢而被 deliver 丢弃。
		events: make(chan bus.OutboundMessage, 2048),
	}
	w.addSubscriber(sub)
	defer w.removeSubscriber(sub)

	// AG-UI 翻译器：每连接一个，负责把分片重建成带边界的 AG-UI 事件。
	translator := newAGUITranslator(session, &sseEmitter{w: rw, flusher: flusher})
	// 先发 SESSION 事件，让前端确认连接已就绪（替代旧的 event: ready）。
	if err := translator.start(); err != nil {
		return
	}
	// 权限确认和 ask_user_question 都可能跨页面刷新/SSE 重连等待。
	// 重放当前 pending 问题，避免后端等待而前端没有可操作入口。
	if pending, ok := w.pendingQuestion(session); ok {
		if err := translator.handle(pending); err != nil {
			return
		}
	}

	// 心跳：定期发注释行，避免中间代理把空闲连接掐断。
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			// 浏览器断开连接。
			return
		case <-heartbeat.C:
			fmt.Fprint(rw, ": keep-alive\n\n")
			flusher.Flush()
		case out := <-sub.events:
			if err := translator.handle(out); err != nil {
				// 写失败通常意味着连接已断，退出让 defer 注销订阅者。
				return
			}
		}
	}
}

// newSessionID 生成一个基于时间戳的会话 ID。
// Web 场景对唯一性要求不高（本地单机为主），时间戳纳秒足够区分。
func newSessionID() string {
	// 会话 ID 会参与生成持久化文件和 artifact 目录名。
	// Windows 不允许文件名包含冒号，因此只使用跨平台安全的分隔符。
	return fmt.Sprintf("web-%d", time.Now().UnixNano())
}
