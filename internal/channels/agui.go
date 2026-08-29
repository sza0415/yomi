package channels

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
)

// AG-UI 事件翻译层。
//
// 背景：szabot 内部出站流是"扁平分片"——每条 OutboundMessage 带 Kind
// (answer/reasoning/tool_call/tool_result/question) + Delta/Done。而 AG-UI
// 是一套标准化的、带生命周期的强类型事件协议（TEXT_MESSAGE_START/CONTENT/END、
// TOOL_CALL_START/ARGS/END/RESULT、REASONING_MESSAGE_START/CONTENT/END、
// RUN_STARTED/FINISHED 等）。二者的核心差异是"三段式生命周期" vs "分片+done"。
//
// aguiTranslator 是一个**每连接**的状态机，负责在出站侧把分片重建成带边界的
// AG-UI 事件：它记住"当前是否有一条文本/推理消息正在进行"，从而在合适的时机
// 补发 START / END —— 例如从 answer 切到 tool_call，就意味着上一条文本消息结束。
//
// 设计取舍：翻译只发生在 Web 出站侧，核心链路（bus/loop/runner）不感知 AG-UI。
// 这样对齐 AG-UI 生态的成本被收敛在一个文件里。

// aguiEmitter 是翻译器写事件的目标（SSE 连接）。抽成接口便于测试。
type aguiEmitter interface {
	// send 写一条 AG-UI 事件：line 形如 "id: <TYPE>_<ts>\ndata: {json}\n\n"。
	send(eventType string, payload map[string]any) error
}

// sseEmitter 把 AG-UI 事件按 SSE 线格式写入 http 响应。
//
// 线格式对齐参考样例：
//
//	id: TEXT_MESSAGE_CONTENT_1787045056476
//	data: {"type":"TEXT_MESSAGE_CONTENT","timestamp":...,"messageId":...,"delta":"以下"}
//
// data 里带 type 字段，前端用 EventSource.onmessage 统一接、按 type 分发。
type sseEmitter struct {
	w       io.Writer
	flusher interface{ Flush() }
}

func (e *sseEmitter) send(eventType string, payload map[string]any) error {
	payload["type"] = eventType
	if _, ok := payload["timestamp"]; !ok {
		payload["timestamp"] = time.Now().UnixMilli()
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(e.w, "id: %s_%d\ndata: %s\n\n", eventType, time.Now().UnixMilli(), data); err != nil {
		return err
	}
	e.flusher.Flush()
	return nil
}

// aguiTranslator 把某个连接上的 OutboundMessage 序列翻译成 AG-UI 事件流。
type aguiTranslator struct {
	sessionID string
	emit      aguiEmitter

	threadID string
	runID    string

	runStarted bool // 本轮是否已发过 RUN_STARTED

	// 当前正在进行的文本消息 / 推理消息的 messageId（空表示没有正在进行的）。
	textMsgID      string
	reasoningMsgID string
}

func newAGUITranslator(sessionID string, emit aguiEmitter) *aguiTranslator {
	return &aguiTranslator{
		sessionID: sessionID,
		emit:      emit,
		// 对齐样例的 threadId 形式：agent:main:<session>。
		threadID: "agent:main:" + sessionID,
	}
}

// start 在连接建立时调用：发 SESSION 事件，告知前端会话已就绪。
func (t *aguiTranslator) start() error {
	return t.emit.send("SESSION", map[string]any{
		"sessionId": t.sessionID,
	})
}

// ensureRunStarted 在本轮第一条业务事件到达前补发 RUN_STARTED。
// 优先使用核心事件携带的 RunID；旧事件没有 RunID 时才本地兜底生成。
func (t *aguiTranslator) ensureRunStarted(runID string) error {
	if t.runStarted {
		return nil
	}
	if runID == "" {
		runID = newID()
	}
	t.runID = runID
	t.runStarted = true
	return t.emit.send("RUN_STARTED", map[string]any{
		"threadId": t.threadID,
		"runId":    t.runID,
	})
}

// handle 翻译一条出站消息。
func (t *aguiTranslator) handle(out bus.OutboundMessage) error {
	// done 标记：结束本轮 —— 关掉未收尾的文本/推理消息，再发 RUN_FINISHED。
	if out.Done {
		return t.finishRun(out.RunID)
	}

	if err := t.ensureRunStarted(out.RunID); err != nil {
		return err
	}

	switch out.Kind {
	case bus.KindAnswer:
		// KindAnswer 的零值即 ""，涵盖了未显式设置 Kind 的正文分片。
		return t.handleText(out.Text)
	case bus.KindReasoning:
		return t.handleReasoning(out.Text)
	case bus.KindToolCall:
		return t.handleToolCall(out)
	case bus.KindToolResult:
		return t.handleToolResult(out)
	case bus.KindQuestion:
		return t.handleQuestion(out)
	default:
		return nil
	}
}

// handleText 处理正文增量：首片补 TEXT_MESSAGE_START，之后逐片 CONTENT。
// 一旦有别的类型事件插入（工具/推理），closeOpenMessages 会补 END。
func (t *aguiTranslator) handleText(delta string) error {
	if delta == "" {
		return nil
	}
	// 正文与推理互斥：开始正文前先收掉正在进行的推理消息。
	if err := t.closeReasoning(); err != nil {
		return err
	}
	if t.textMsgID == "" {
		t.textMsgID = newID()
		if err := t.emit.send("TEXT_MESSAGE_START", map[string]any{
			"messageId": t.textMsgID,
			"role":      "assistant",
		}); err != nil {
			return err
		}
	}
	return t.emit.send("TEXT_MESSAGE_CONTENT", map[string]any{
		"messageId": t.textMsgID,
		"delta":     delta,
	})
}

// handleReasoning 处理推理增量：REASONING_MESSAGE_START/CONTENT。
func (t *aguiTranslator) handleReasoning(delta string) error {
	if delta == "" {
		return nil
	}
	// 推理与正文互斥：开始推理前先收掉正在进行的正文消息。
	if err := t.closeText(); err != nil {
		return err
	}
	if t.reasoningMsgID == "" {
		t.reasoningMsgID = newID()
		if err := t.emit.send("REASONING_MESSAGE_START", map[string]any{
			"messageId": t.reasoningMsgID,
			"role":      "reasoning",
		}); err != nil {
			return err
		}
	}
	return t.emit.send("REASONING_MESSAGE_CONTENT", map[string]any{
		"messageId": t.reasoningMsgID,
		"delta":     delta,
	})
}

// handleToolCall 处理工具调用：先收掉正在进行的文本/推理消息，再发
// TOOL_CALL_START → TOOL_CALL_ARGS → TOOL_CALL_END（provider 已把参数攒完整，
// 故 ARGS 一次性发出）。
func (t *aguiTranslator) handleToolCall(out bus.OutboundMessage) error {
	if err := t.closeOpenMessages(); err != nil {
		return err
	}
	if err := t.emit.send("TOOL_CALL_START", map[string]any{
		"toolCallId":   out.ToolCallID,
		"toolCallName": out.ToolName,
	}); err != nil {
		return err
	}
	if out.Arguments != "" && out.Arguments != "null" {
		if err := t.emit.send("TOOL_CALL_ARGS", map[string]any{
			"toolCallId": out.ToolCallID,
			"delta":      out.Arguments,
		}); err != nil {
			return err
		}
	}
	return t.emit.send("TOOL_CALL_END", map[string]any{
		"toolCallId": out.ToolCallID,
	})
}

// handleToolResult 处理工具结果：TOOL_CALL_RESULT，用 toolCallId 与调用配对。
func (t *aguiTranslator) handleToolResult(out bus.OutboundMessage) error {
	return t.emit.send("TOOL_CALL_RESULT", map[string]any{
		"messageId":  newID(),
		"toolCallId": out.ToolCallID,
		"content":    out.Text,
		"role":       "tool",
	})
}

// handleQuestion 处理 ask_user_question。它不属于 AG-UI 标准事件，用 CUSTOM
// 事件承载，把问题文本与结构化候选项透出，前端据此渲染可点击选项。
func (t *aguiTranslator) handleQuestion(out bus.OutboundMessage) error {
	if err := t.closeOpenMessages(); err != nil {
		return err
	}
	value := map[string]any{"question": out.Text}
	if opts, ok := out.Meta["options"].([]string); ok && len(opts) > 0 {
		value["options"] = opts
	}
	return t.emit.send("CUSTOM", map[string]any{
		"name":  "ASK_USER_QUESTION",
		"value": value,
	})
}

// closeText / closeReasoning 收尾当前正在进行的文本/推理消息（补 END）。
func (t *aguiTranslator) closeText() error {
	if t.textMsgID == "" {
		return nil
	}
	id := t.textMsgID
	t.textMsgID = ""
	return t.emit.send("TEXT_MESSAGE_END", map[string]any{"messageId": id})
}

func (t *aguiTranslator) closeReasoning() error {
	if t.reasoningMsgID == "" {
		return nil
	}
	id := t.reasoningMsgID
	t.reasoningMsgID = ""
	return t.emit.send("REASONING_MESSAGE_END", map[string]any{"messageId": id})
}

func (t *aguiTranslator) closeOpenMessages() error {
	if err := t.closeText(); err != nil {
		return err
	}
	return t.closeReasoning()
}

// finishRun 收尾本轮：关掉未结束的文本/推理消息，发 RUN_FINISHED，并重置
// 状态，为同一连接上的下一轮做准备。
func (t *aguiTranslator) finishRun(runID string) error {
	if err := t.closeOpenMessages(); err != nil {
		return err
	}
	// 未开始过 run 就收到 done（异常/空轮），也补一个 RUN_STARTED 保证成对。
	if !t.runStarted {
		if err := t.ensureRunStarted(runID); err != nil {
			return err
		}
	}
	if err := t.emit.send("RUN_FINISHED", map[string]any{
		"threadId": t.threadID,
		"runId":    t.runID,
	}); err != nil {
		return err
	}
	// 重置，等待下一轮。
	t.runStarted = false
	t.runID = ""
	return nil
}

// newID 生成一个 16 字节的十六进制随机 ID，用作 messageId / toolCallId(生成场景)
// / runId。用标准库 crypto/rand，保持 szabot"零第三方依赖"的设计。
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极罕见：熵源不可用时退化为时间戳，保证不 panic、仍大体唯一。
		return fmt.Sprintf("id-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
