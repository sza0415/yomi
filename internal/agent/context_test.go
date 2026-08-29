package agent

import (
	"context"
	"testing"

	"github.com/ziangsun/szabot/internal/providers"
)

type summaryProvider struct{ calls int }

func (p *summaryProvider) Name() string { return "summary-test" }
func (p *summaryProvider) Chat(_ context.Context, req providers.ChatRequest) (providers.ChatResponse, error) {
	p.calls++
	if len(req.Messages) > 0 && req.Messages[0].Role == providers.RoleSystem && len(req.Messages[0].Content) > 0 {
		return providers.ChatResponse{Content: "用户曾讨论项目约束；保留最近任务。"}, nil
	}
	return providers.ChatResponse{Content: "ok"}, nil
}

func TestContextManagerCompactsAndPersistsSummary(t *testing.T) {
	store, err := NewSessionStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append("s", providers.Message{Role: providers.RoleUser, Content: "old fact"}, providers.Message{Role: providers.RoleAssistant, Content: "old answer"}, providers.Message{Role: providers.RoleUser, Content: "recent"}); err != nil {
		t.Fatal(err)
	}
	p := &summaryProvider{}
	m := &ContextManager{Store: store, Provider: p, MaxContextTokens: 6, RecentMessages: 1}
	got, err := m.Build(context.Background(), "s", "SYS", providers.Message{Role: providers.RoleUser, Content: "now"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Compacted {
		t.Fatal("expected compaction")
	}
	if len(got.Messages) != 3 || got.Messages[1].Content == "" || got.Messages[2].Content != "now" {
		t.Fatalf("messages = %#v", got.Messages)
	}
	if p.calls != 1 {
		t.Fatalf("summary calls = %d, want 1", p.calls)
	}

	reopened, err := NewSessionStore(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	summary, covered, err := reopened.LoadSummary("s")
	if err != nil || summary == "" || covered != 2 {
		t.Fatalf("summary=%q covered=%d err=%v", summary, covered, err)
	}
	archives, err := reopened.LoadArchives("s")
	if err != nil || len(archives) != 1 {
		t.Fatalf("archives=%#v err=%v", archives, err)
	}
	if archives[0].CoveredFrom != 0 || archives[0].CoveredTo != 2 || archives[0].Summary == "" {
		t.Fatalf("archive=%#v", archives[0])
	}
}
