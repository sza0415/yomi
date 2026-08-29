package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
)

// blockingProvider 在 Chat 里阻塞，直到 ctx 被取消才返回 ctx.Err()。
// 用它来模拟"正在烧算力的 LLM 请求"，从而验证 CancelSession 能中断下游。
type blockingProvider struct {
	started chan struct{} // Chat 一进入就关闭，通知测试"任务已开始跑"
}

func (p *blockingProvider) Name() string { return "blocking" }

func (p *blockingProvider) Chat(ctx context.Context, _ providers.ChatRequest) (providers.ChatResponse, error) {
	select {
	case <-p.started:
		// 已通知过（防止重复 close）
	default:
		close(p.started)
	}
	<-ctx.Done()
	return providers.ChatResponse{}, ctx.Err()
}

// TestCancelSessionInterruptsRunner 验证：CancelSession 能取消正在运行的
// handle，使下游 Provider.Chat（挂在 runCtx 上）随之收到 ctx 取消并返回。
func TestCancelSessionInterruptsRunner(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{})}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}

	b := bus.New(8)
	loop := &Loop{Bus: b, Runner: runner}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)

	if err := b.PublishInbound(ctx, bus.InboundMessage{
		ChannelID: "web", SessionID: "S", Text: "跑一个长任务", Time: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// 等任务真正开始（Provider.Chat 已进入阻塞）。
	select {
	case <-provider.started:
	case <-time.After(2 * time.Second):
		t.Fatal("runner did not start in time")
	}

	// 此刻 running 注册表里应有该 session 的取消句柄。
	loop.mu.Lock()
	_, ok := loop.running["S"]
	loop.mu.Unlock()
	if !ok {
		t.Fatal("expected session S to be registered as running")
	}

	// 模拟 Web 客户端显式点击取消。
	loop.CancelSession("S")

	// 取消后，handle 应结束并从 running 注册表注销。
	deadline := time.Now().Add(2 * time.Second)
	for {
		loop.mu.Lock()
		_, still := loop.running["S"]
		loop.mu.Unlock()
		if !still {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("session S was not unregistered after cancel")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// 即使 Run Context 已取消，也必须发出 Done，让 Web/AG-UI 收尾本轮。
	select {
	case out := <-b.Outbound():
		if !out.Done || out.SessionID != "S" || out.ChannelID != "web" {
			t.Fatalf("cancelled run done event = %#v", out)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled run did not publish done event")
	}
}

// TestCancelSessionCancelsPending 验证：显式取消也会中止正在等待用户回答的 Run。
func TestCancelSessionCancelsPending(t *testing.T) {
	loop := &Loop{}
	loop.pending = make(map[string]*pendingAsk)
	loop.running = make(map[string]*runHandle)

	cancelled := false
	loop.running["S"] = &runHandle{cancel: func() { cancelled = true }}
	// 标记该 session 正在等回答。
	loop.pending["S"] = &pendingAsk{answer: make(chan string, 1)}

	loop.CancelSession("S")

	if !cancelled {
		t.Fatal("CancelSession should cancel a pending Run when explicitly requested")
	}
}

// TestCancelSessionUnknownIsNoop 验证：对没有运行中任务的 session 调用
// CancelSession 是安全的静默操作。
func TestCancelSessionUnknownIsNoop(t *testing.T) {
	loop := &Loop{}
	loop.pending = make(map[string]*pendingAsk)
	loop.running = make(map[string]*runHandle)
	// 不 panic 即通过。
	loop.CancelSession("does-not-exist")
}

// TestCancelSessionErrorIsContextCanceled 附带确认：取消导致的错误确实是
// context.Canceled（loop 据此降级日志）。
func TestCancelSessionErrorIsContextCanceled(t *testing.T) {
	provider := &blockingProvider{started: make(chan struct{})}
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}

	ctx, cancel := context.WithCancel(context.Background())
	runCtx, runCancel := context.WithCancel(ctx)

	done := make(chan error, 1)
	go func() {
		_, err := runner.RunCollect(runCtx, []providers.Message{{Role: providers.RoleUser, Content: "hi"}}, StreamSink{})
		done <- err
	}()

	<-provider.started
	runCancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunCollect did not return after cancel")
	}
	cancel()
}
