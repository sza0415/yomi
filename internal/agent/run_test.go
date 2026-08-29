package agent

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
)

type orderedBlockingProvider struct {
	mu       sync.Mutex
	requests []providers.ChatRequest
	firstHit chan struct{}
	release  chan struct{}
}

func (p *orderedBlockingProvider) Name() string { return "ordered-blocking" }

func (p *orderedBlockingProvider) Chat(ctx context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.mu.Lock()
	index := len(p.requests)
	p.requests = append(p.requests, req)
	p.mu.Unlock()
	if index == 0 {
		close(p.firstHit)
		select {
		case <-ctx.Done():
			return providers.ChatResponse{}, ctx.Err()
		case <-p.release:
		}
	}
	return providers.ChatResponse{Content: "reply"}, nil
}

func TestLoopSerializesSameSession(t *testing.T) {
	provider := &orderedBlockingProvider{firstHit: make(chan struct{}), release: make(chan struct{})}
	b := bus.New(32)
	loop := &Loop{Bus: b, Runner: &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	loop.Start(ctx)

	_ = b.PublishInbound(ctx, bus.InboundMessage{SessionID: "S", ChannelID: "cli", Text: "first"})
	<-provider.firstHit
	_ = b.PublishInbound(ctx, bus.InboundMessage{SessionID: "S", ChannelID: "cli", Text: "second"})

	time.Sleep(30 * time.Millisecond)
	provider.mu.Lock()
	callsBeforeRelease := len(provider.requests)
	provider.mu.Unlock()
	if callsBeforeRelease != 1 {
		t.Fatalf("same-session requests ran concurrently: calls=%d", callsBeforeRelease)
	}
	close(provider.release)

	deadline := time.Now().Add(time.Second)
	for {
		provider.mu.Lock()
		calls := len(provider.requests)
		provider.mu.Unlock()
		if calls == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second same-session run never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunnerAggregatesUsageAndEnforcesBudget(t *testing.T) {
	provider := &scriptedProvider{responses: []providers.ChatResponse{{
		Content: "too expensive",
		Usage:   providers.Usage{InputTokens: 8, OutputTokens: 5, TotalTokens: 13, Reported: true},
	}}}
	run := NewRun("S", RunBudget{MaxTotalTokens: 10})
	ctx := withRun(context.Background(), run)
	runner := &Runner{Provider: provider, Model: "test", Tools: tools.NewRegistry()}

	result, err := runner.RunCollect(ctx, []providers.Message{{Role: providers.RoleUser, Content: "hi"}}, StreamSink{})
	if err == nil || !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("err = %v, want ErrBudgetExceeded", err)
	}
	if result.Usage.TotalTokens != 13 || result.Usage.ModelCalls != 1 {
		t.Fatalf("usage = %#v", result.Usage)
	}
}
