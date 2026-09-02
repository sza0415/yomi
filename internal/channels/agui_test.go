package channels

import (
	"testing"

	"github.com/ziangsun/szabot/internal/bus"
)

// recordingEmitter 把翻译出的 AG-UI 事件收集到内存，供测试断言序列。
type recordingEmitter struct {
	events []recordedEvent
}

type recordedEvent struct {
	Type    string
	Payload map[string]any
}

func (e *recordingEmitter) send(eventType string, payload map[string]any) error {
	e.events = append(e.events, recordedEvent{Type: eventType, Payload: payload})
	return nil
}

func (e *recordingEmitter) types() []string {
	out := make([]string, 0, len(e.events))
	for _, ev := range e.events {
		out = append(out, ev.Type)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestAGUITranslatorTextRun 验证纯文本一轮的事件序列：
// SESSION → RUN_STARTED → TEXT_MESSAGE_START → CONTENT(×N) → END → RUN_FINISHED。
func TestAGUITranslatorTextRun(t *testing.T) {
	em := &recordingEmitter{}
	tr := newAGUITranslator("S", em)

	_ = tr.start()
	_ = tr.handle(bus.OutboundMessage{RunID: "core-run", Kind: bus.KindAnswer, Text: "你", Delta: true})
	_ = tr.handle(bus.OutboundMessage{RunID: "core-run", Kind: bus.KindAnswer, Text: "好", Delta: true})
	_ = tr.handle(bus.OutboundMessage{RunID: "core-run", Done: true})

	want := []string{
		"SESSION", "RUN_STARTED",
		"TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_CONTENT", "TEXT_MESSAGE_END",
		"RUN_FINISHED",
	}
	if !equalStrings(em.types(), want) {
		t.Fatalf("event sequence = %v, want %v", em.types(), want)
	}
	if em.events[1].Payload["runId"] != "core-run" || em.events[len(em.events)-1].Payload["runId"] != "core-run" {
		t.Fatalf("core runId was not preserved: start=%v finish=%v", em.events[1].Payload["runId"], em.events[len(em.events)-1].Payload["runId"])
	}

	// 两条 CONTENT 应共享同一 messageId。
	startID := em.events[2].Payload["messageId"]
	c1 := em.events[3].Payload["messageId"]
	c2 := em.events[4].Payload["messageId"]
	endID := em.events[5].Payload["messageId"]
	if startID == "" || startID != c1 || c1 != c2 || c2 != endID {
		t.Fatalf("messageId mismatch: start=%v c1=%v c2=%v end=%v", startID, c1, c2, endID)
	}
}

// TestAGUITranslatorToolThenText 验证"文本一半 → 工具调用 → 工具结果 → 继续文本"
// 场景：切到工具前，正在进行的文本消息应被 TEXT_MESSAGE_END 收尾；工具事件带
// toolCallId 配对；之后的文本是一条**新的** TEXT 消息（不同 messageId）。
func TestAGUITranslatorToolThenText(t *testing.T) {
	em := &recordingEmitter{}
	tr := newAGUITranslator("S", em)

	_ = tr.start()
	_ = tr.handle(bus.OutboundMessage{Kind: bus.KindAnswer, Text: "让我查", Delta: true})
	_ = tr.handle(bus.OutboundMessage{
		Kind: bus.KindToolCall, Delta: true,
		ToolCallID: "call_1", ToolName: "web_search", Arguments: `{"q":"x"}`,
	})
	_ = tr.handle(bus.OutboundMessage{
		Kind: bus.KindToolResult, Delta: true,
		ToolCallID: "call_1", Text: "result-text",
	})
	_ = tr.handle(bus.OutboundMessage{Kind: bus.KindAnswer, Text: "查到了", Delta: true})
	_ = tr.handle(bus.OutboundMessage{Done: true})

	want := []string{
		"SESSION", "RUN_STARTED",
		"TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT",
		"TEXT_MESSAGE_END", // 切到工具前收尾第一条文本
		"TOOL_CALL_START", "TOOL_CALL_ARGS", "TOOL_CALL_END",
		"TOOL_CALL_RESULT",
		"TEXT_MESSAGE_START", "TEXT_MESSAGE_CONTENT", // 工具后的文本是新消息
		"TEXT_MESSAGE_END", // Done 收尾第二条文本
		"RUN_FINISHED",
	}
	if !equalStrings(em.types(), want) {
		t.Fatalf("event sequence = %v, want %v", em.types(), want)
	}

	// 工具事件的 toolCallId 应一致。
	if em.events[5].Payload["toolCallId"] != "call_1" ||
		em.events[6].Payload["toolCallId"] != "call_1" ||
		em.events[7].Payload["toolCallId"] != "call_1" ||
		em.events[8].Payload["toolCallId"] != "call_1" {
		t.Fatalf("toolCallId mismatch across tool events")
	}
	if em.events[6].Payload["delta"] != `{"q":"x"}` {
		t.Fatalf("TOOL_CALL_ARGS delta = %v", em.events[6].Payload["delta"])
	}
	if em.events[8].Payload["content"] != "result-text" || em.events[8].Payload["role"] != "tool" {
		t.Fatalf("TOOL_CALL_RESULT payload = %v", em.events[8].Payload)
	}

	// 两条文本消息的 messageId 应不同。
	firstText := em.events[2].Payload["messageId"]
	secondText := em.events[9].Payload["messageId"]
	if firstText == secondText {
		t.Fatalf("expected different messageId for the two text messages, both=%v", firstText)
	}
}

// TestAGUITranslatorReasoning 验证推理增量翻译成 REASONING_MESSAGE_* 事件。
func TestAGUITranslatorReasoning(t *testing.T) {
	em := &recordingEmitter{}
	tr := newAGUITranslator("S", em)

	_ = tr.start()
	_ = tr.handle(bus.OutboundMessage{Kind: bus.KindReasoning, Text: "思考中", Delta: true})
	_ = tr.handle(bus.OutboundMessage{Done: true})

	want := []string{
		"SESSION", "RUN_STARTED",
		"REASONING_MESSAGE_START", "REASONING_MESSAGE_CONTENT", "REASONING_MESSAGE_END",
		"RUN_FINISHED",
	}
	if !equalStrings(em.types(), want) {
		t.Fatalf("event sequence = %v, want %v", em.types(), want)
	}
}

// TestAGUITranslatorCancelledRunThenNextRun 验证取消 Run 也发送 Done 后，
// 同一条 SSE 连接上的下一轮会重新建立 RUN_STARTED，且不会复用旧 runId。
func TestAGUITranslatorCancelledRunThenNextRun(t *testing.T) {
	em := &recordingEmitter{}
	tr := newAGUITranslator("S", em)

	_ = tr.start()
	_ = tr.handle(bus.OutboundMessage{RunID: "cancelled-run", Kind: bus.KindAnswer, Text: "半截", Delta: true})
	_ = tr.handle(bus.OutboundMessage{RunID: "cancelled-run", Done: true})
	_ = tr.handle(bus.OutboundMessage{RunID: "next-run", Kind: bus.KindAnswer, Text: "新回复", Delta: true})
	_ = tr.handle(bus.OutboundMessage{RunID: "next-run", Done: true})

	starts := []string{}
	finishes := []string{}
	for _, event := range em.events {
		switch event.Type {
		case "RUN_STARTED":
			starts = append(starts, event.Payload["runId"].(string))
		case "RUN_FINISHED":
			finishes = append(finishes, event.Payload["runId"].(string))
		}
	}
	if !equalStrings(starts, []string{"cancelled-run", "next-run"}) {
		t.Fatalf("RUN_STARTED ids = %v", starts)
	}
	if !equalStrings(finishes, []string{"cancelled-run", "next-run"}) {
		t.Fatalf("RUN_FINISHED ids = %v", finishes)
	}
}

// TestAGUITranslatorQuestion 验证 ask_user_question 翻译成 CUSTOM 事件。
func TestAGUITranslatorQuestion(t *testing.T) {
	em := &recordingEmitter{}
	tr := newAGUITranslator("S", em)

	_ = tr.start()
	_ = tr.handle(bus.OutboundMessage{
		RunID: "run-question", Kind: bus.KindQuestion,
		Text: "选哪个？\n可选项：\n  1. A\n  2. B",
		Meta: map[string]any{"question": "选哪个？", "options": []string{"A", "B"}},
	})

	// 应有一条 CUSTOM 事件，name=ASK_USER_QUESTION，携带 question 与 options。
	var found *recordedEvent
	for i := range em.events {
		if em.events[i].Type == "CUSTOM" {
			found = &em.events[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("expected a CUSTOM event, got %v", em.types())
	}
	if found.Payload["name"] != "ASK_USER_QUESTION" {
		t.Fatalf("CUSTOM name = %v", found.Payload["name"])
	}
	if found.Payload["runId"] != "run-question" {
		t.Fatalf("CUSTOM runId = %v", found.Payload["runId"])
	}
	value, ok := found.Payload["value"].(map[string]any)
	if !ok || value["question"] != "选哪个？" {
		t.Fatalf("CUSTOM value = %v", found.Payload["value"])
	}
}
