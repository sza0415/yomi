package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/ziangsun/szabot/internal/memory"
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

type contextMemoryStore struct{}

func (contextMemoryStore) Search(context.Context, memory.Query) ([]memory.Memory, error) {
	return []memory.Memory{{ID: "mem-1", UserID: "alice", Kind: memory.KindPreference, Content: "用户偏好中文回答", Confidence: 0.93, SourceRunID: "run-1"}}, nil
}
func (contextMemoryStore) Upsert(context.Context, memory.Memory) error { return nil }
func (contextMemoryStore) Get(context.Context, string, string) (memory.Memory, error) {
	return memory.Memory{}, nil
}
func (contextMemoryStore) List(context.Context, string) ([]memory.Memory, error) { return nil, nil }
func (contextMemoryStore) Delete(context.Context, string, string, string) error  { return nil }
func (contextMemoryStore) Rebuild(context.Context, string) error                 { return nil }

func TestContextManagerInjectsScopedMemory(t *testing.T) {
	m := &ContextManager{Memory: contextMemoryStore{}}
	got, err := m.BuildForUser(context.Background(), "alice", "session-1", "SYS", providers.Message{Role: providers.RoleUser, Content: "怎么回答？"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryCount != 1 || len(got.MemoryIDs) != 1 || got.MemoryIDs[0] != "mem-1" || got.MemoryTokens == 0 || len(got.Messages) != 3 {
		t.Fatalf("memory context = %#v count=%d", got.Messages, got.MemoryCount)
	}
	if got.Messages[1].Role != providers.RoleSystem || !strings.Contains(got.Messages[1].Content, "user_memory") {
		t.Fatalf("memory message = %#v", got.Messages[1])
	}
	if !strings.Contains(got.Messages[1].Content, "仅供参考") || !strings.Contains(got.Messages[1].Content, "用户偏好中文回答") {
		t.Fatalf("memory message content = %q", got.Messages[1].Content)
	}

	withoutUser, err := m.BuildForUser(context.Background(), "", "session-2", "SYS", providers.Message{Role: providers.RoleUser, Content: "怎么回答？"})
	if err != nil {
		t.Fatal(err)
	}
	if withoutUser.MemoryCount != 0 || len(withoutUser.Messages) != 2 {
		t.Fatalf("unscoped memory = %#v count=%d", withoutUser.Messages, withoutUser.MemoryCount)
	}
}

type layeredMemoryStore struct{}

func (layeredMemoryStore) Search(_ context.Context, query memory.Query) ([]memory.Memory, error) {
	if len(query.Kinds) == 1 && query.Kinds[0] == memory.KindEpisode {
		return []memory.Memory{{ID: "episode-1", UserID: query.UserID, Kind: memory.KindEpisode, Content: "用户昨天确认了东京行程", Confidence: 0.8}}, nil
	}
	return []memory.Memory{{ID: "profile-1", UserID: query.UserID, Kind: memory.KindPreference, Content: "用户偏好中文回答", Confidence: 0.9}}, nil
}
func (layeredMemoryStore) Upsert(context.Context, memory.Memory) error { return nil }
func (layeredMemoryStore) Get(context.Context, string, string) (memory.Memory, error) {
	return memory.Memory{}, nil
}
func (layeredMemoryStore) List(context.Context, string) ([]memory.Memory, error) { return nil, nil }
func (layeredMemoryStore) Delete(context.Context, string, string, string) error  { return nil }
func (layeredMemoryStore) Rebuild(context.Context, string) error                 { return nil }

func TestContextManagerRetrievesProfileBeforeEpisode(t *testing.T) {
	m := &ContextManager{Memory: layeredMemoryStore{}}
	got, err := m.BuildForUser(context.Background(), "alice", "session-1", "SYS", providers.Message{Role: providers.RoleUser, Content: "东京行程用什么语言记录？"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MemoryProfileCount != 1 || got.MemoryEpisodeCount != 1 || len(got.MemoryIDs) != 2 {
		t.Fatalf("layer counts = profile:%d episode:%d ids:%v", got.MemoryProfileCount, got.MemoryEpisodeCount, got.MemoryIDs)
	}
	if !strings.Contains(got.Messages[1].Content, "profile") || !strings.Contains(got.Messages[1].Content, "episode") {
		t.Fatalf("layered memory context = %q", got.Messages[1].Content)
	}
}
