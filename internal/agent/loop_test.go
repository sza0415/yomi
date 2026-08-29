package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

type recordingTraceSink struct {
	events []tracing.Event
}

func (s *recordingTraceSink) Record(_ context.Context, event tracing.Event) error {
	s.events = append(s.events, event)
	return nil
}

// TestLoopAskUserQuestionRoundTrip 验证 ask_user_question 的 bus 双向通道：
//   - 第一条用户消息触发模型请求调用 ask_user_question；
//   - Loop 把问题作为出站消息发出（channel 会看到）；
//   - 用户的下一条入站消息被识别为"回答"，喂回等待中的工具；
//   - 工具拿到回答后模型给出最终答复。
func TestLoopAskUserQuestionRoundTrip(t *testing.T) {
	registry := tools.NewRegistry()
	askTool, err := tools.NewAskUserQuestion()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(askTool); err != nil {
		t.Fatal(err)
	}

	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{ // 第一轮：请求问用户
			ToolCalls: []providers.ToolCall{{
				ID:        "call_ask",
				Name:      "ask_user_question",
				Arguments: json.RawMessage(`{"question":"你想要哪种颜色？","options":["红","蓝"]}`),
			}},
		},
		{Content: "已记录：蓝色"}, // 第二轮：拿到回答后收尾
	}}

	b := bus.New(8)
	runner := &Runner{Provider: provider, Model: "test", Tools: registry}
	loop := &Loop{Bus: b, Runner: runner}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)

	// 1. 用户第一条消息：触发提问。
	if err := b.PublishInbound(ctx, bus.InboundMessage{
		ChannelID: "cli", SessionID: "s1", Text: "帮我选颜色", Time: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 2. Loop 应发出一条 KindQuestion 出站消息（ask_user_question 的问题）。
	//    合并后的架构会先发出工具调用等 Delta 分片，这里只捕获问题消息——
	//    同时这也保证我们在 Ask 已注册 pending 之后才发送回答，避免竞态。
	waitQuestion := func() bus.OutboundMessage {
		for {
			select {
			case out := <-b.Outbound():
				if out.Kind != bus.KindQuestion {
					continue // 跳过流式分片、工具事件等
				}
				return out
			case <-ctx.Done():
				t.Fatal("timed out waiting for the question to be sent")
				return bus.OutboundMessage{}
			}
		}
	}
	question := waitQuestion()
	if question.SessionID != "s1" || question.ChannelID != "cli" {
		t.Fatalf("question routed wrong: %#v", question)
	}
	if question.Text == "" {
		t.Fatalf("question text is empty")
	}
	// 问题应携带结构化候选项，供 Web 前端渲染成按钮。
	if opts, ok := question.Meta["options"].([]string); !ok || len(opts) != 2 {
		t.Fatalf("question options = %#v, want 2 items", question.Meta["options"])
	}

	// 3. 用户回答：这条不应被当成新问题，而是喂给等待中的工具。
	if err := b.PublishInbound(ctx, bus.InboundMessage{
		ChannelID: "cli", SessionID: "s1", Text: "蓝", Time: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 4. 最终答复：合并后的架构以 KindAnswer 正文增量（Delta=true）发出，
	//    最后跟一条 Done=true 收尾。这里累加正文增量直到收到 Done。
	var answer string
collect:
	for {
		select {
		case out := <-b.Outbound():
			if out.Done {
				break collect
			}
			if out.Kind == bus.KindAnswer && out.Delta {
				answer += out.Text
			}
		case <-ctx.Done():
			t.Fatal("timed out waiting for the final reply")
		}
	}
	if answer != "已记录：蓝色" {
		t.Fatalf("final reply = %q, want 已记录：蓝色", answer)
	}

	// 校验工具确实把用户回答交给了模型。
	if len(provider.requests) != 2 {
		t.Fatalf("Chat calls = %d, want 2", len(provider.requests))
	}
	toolMsg := provider.requests[1].Messages[len(provider.requests[1].Messages)-1]
	if toolMsg.Role != providers.RoleTool || toolMsg.ToolCallID != "call_ask" || toolMsg.Content != "蓝" {
		t.Fatalf("tool result message = %#v, want answer 蓝", toolMsg)
	}
}

func TestLoopDuplicateAnswerDoesNotBecomeNewRun(t *testing.T) {
	loop := &Loop{pending: map[string]*pendingAsk{}}
	wait := &pendingAsk{answer: make(chan string, 1)}
	loop.pending["s1"] = wait

	if !loop.deliverAnswer("s1", "Allow once") {
		t.Fatal("first answer was not routed")
	}
	if !loop.deliverAnswer("s1", "Allow once") {
		t.Fatal("duplicate answer should remain associated with pending run")
	}
	if got := <-wait.answer; got != "Allow once" {
		t.Fatalf("answer = %q", got)
	}
	if _, ok := loop.pending["s1"]; !ok {
		t.Fatal("pending answer removed before Ask consumed it")
	}
}

func TestLoopSeparatesStreamDeltasFromTrace(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.ChatResponse{{
		Content:   "完整答案",
		Reasoning: "完整推理",
	}}}
	traceSink := &recordingTraceSink{}
	loop := &Loop{
		Bus:    bus.New(8),
		Runner: &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()},
		Trace:  traceSink,
	}

	loop.handle(context.Background(), bus.InboundMessage{
		ChannelID: "web", SessionID: "trace-test", Text: "问题", Time: time.Now(),
	})

	for _, event := range traceSink.events {
		if event.Type == "delta" || event.Type == "reasoning" {
			t.Fatalf("stream event %q should not be persisted in Trace: %#v", event.Type, event)
		}
	}
	var assistant tracing.Event
	for _, event := range traceSink.events {
		if event.Type == tracing.EventAssistantCompleted {
			assistant = event
			break
		}
	}
	if assistant.Type == "" {
		t.Fatal("missing assistant.message.completed Trace event")
	}
	message, ok := assistant.Data["message"].(providers.Message)
	if !ok {
		t.Fatalf("assistant message type = %T, want providers.Message", assistant.Data["message"])
	}
	if message.Content != "完整答案" || message.Reasoning != "完整推理" {
		t.Fatalf("assistant message = %#v, want complete content and reasoning", message)
	}
}

func TestLoopRecordsContextStrategySummary(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.ChatResponse{{Content: "answer"}}}
	traceSink := &recordingTraceSink{}
	loop := &Loop{
		Bus:    bus.New(8),
		Runner: &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()},
		Trace:  traceSink,
	}
	loop.handle(context.Background(), bus.InboundMessage{ChannelID: "cli", SessionID: "strategy-test", Text: "question"})

	var strategy tracing.Event
	for _, event := range traceSink.events {
		if event.Type == tracing.EventContextStrategyApplied && event.Data["stage"] == "pre_model" {
			strategy = event
			break
		}
	}
	if strategy.Type == "" {
		t.Fatal("missing context.strategy.applied event")
	}
	if strategy.Data["policy"] != "layered" || strategy.Data["selected_layer"] != "none" {
		t.Fatalf("strategy data = %#v", strategy.Data)
	}
	if strategy.Data["context_id"] == "" {
		t.Fatalf("strategy context_id = %#v", strategy.Data["context_id"])
	}
}
