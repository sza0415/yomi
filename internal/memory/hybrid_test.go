package memory

import (
	"context"
	"errors"
	"testing"
)

type hybridStoreStub struct {
	items    map[string]Memory
	lexical  []Memory
	searches int
}

func (s *hybridStoreStub) Search(context.Context, Query) ([]Memory, error) {
	s.searches++
	return append([]Memory(nil), s.lexical...), nil
}
func (s *hybridStoreStub) Upsert(context.Context, Memory) error { return nil }
func (s *hybridStoreStub) Get(_ context.Context, userID, id string) (Memory, error) {
	item, ok := s.items[id]
	if !ok || item.UserID != userID {
		return Memory{}, errors.New("not found")
	}
	return item, nil
}
func (s *hybridStoreStub) List(context.Context, string) ([]Memory, error)       { return nil, nil }
func (s *hybridStoreStub) Delete(context.Context, string, string, string) error { return nil }
func (s *hybridStoreStub) Rebuild(context.Context, string) error                { return nil }

type hybridEmbedder struct{ err error }

func (e hybridEmbedder) Model() string   { return "test" }
func (e hybridEmbedder) Version() string { return "v1" }
func (e hybridEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return [][]float32{{0.1, 0.2}}, nil
}

type hybridSemantic struct{ hits []SemanticHit }

func (s hybridSemantic) Search(context.Context, []float32, Query) ([]SemanticHit, error) {
	return s.hits, nil
}

type reverseReranker struct{}

func (reverseReranker) Rerank(_ context.Context, _ string, items []Memory) ([]Memory, error) {
	result := append([]Memory(nil), items...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result, nil
}

func TestHybridStoreFusesLexicalAndSemanticRanks(t *testing.T) {
	store := &hybridStoreStub{items: map[string]Memory{
		"mem-1": {ID: "mem-1", UserID: "alice", Status: StatusActive, Importance: 0.9},
		"mem-2": {ID: "mem-2", UserID: "alice", Status: StatusActive, Importance: 0.1},
		"mem-3": {ID: "mem-3", UserID: "alice", Status: StatusActive},
	}, lexical: []Memory{{ID: "mem-1", UserID: "alice", Status: StatusActive}, {ID: "mem-2", UserID: "alice", Status: StatusActive}}}
	hybrid := NewHybridStore(store, hybridEmbedder{}, hybridSemantic{hits: []SemanticHit{{ID: "mem-2", Score: 0.99}, {ID: "mem-3", Score: 0.8}}})
	got, err := hybrid.Search(context.Background(), Query{UserID: "alice", Text: "中文", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != "mem-2" || got[1].ID != "mem-1" || got[2].ID != "mem-3" {
		t.Fatalf("fused memories = %#v", got)
	}
}

func TestHybridStoreFiltersConflictsAndFallsBackOnEmbeddingFailure(t *testing.T) {
	store := &hybridStoreStub{items: map[string]Memory{
		"mem-active":   {ID: "mem-active", UserID: "alice", Status: StatusActive},
		"mem-conflict": {ID: "mem-conflict", UserID: "alice", Status: StatusConflict},
	}, lexical: []Memory{{ID: "mem-active", UserID: "alice", Status: StatusActive}}}
	hybrid := NewHybridStore(store, hybridEmbedder{err: errors.New("embedding unavailable")}, hybridSemantic{hits: []SemanticHit{{ID: "mem-conflict", Score: 1}}})
	got, err := hybrid.Search(context.Background(), Query{UserID: "alice", Text: "query", Limit: 8})
	if err != nil || len(got) != 1 || got[0].ID != "mem-active" {
		t.Fatalf("fallback memories = %#v, err=%v", got, err)
	}
}

func TestHybridStoreCanIncludeConflictsFromSemanticResults(t *testing.T) {
	store := &hybridStoreStub{items: map[string]Memory{
		"mem-conflict": {ID: "mem-conflict", UserID: "alice", Status: StatusConflict},
	}, lexical: nil}
	hybrid := NewHybridStore(store, hybridEmbedder{}, hybridSemantic{hits: []SemanticHit{{ID: "mem-conflict", Score: 1}}})
	got, err := hybrid.Search(context.Background(), Query{UserID: "alice", Text: "query", Limit: 8, IncludeConflicts: true})
	if err != nil || len(got) != 1 || got[0].ID != "mem-conflict" {
		t.Fatalf("included conflict memories = %#v, err=%v", got, err)
	}
}

func TestHybridStoreReportsStatsAndUsesOptionalReranker(t *testing.T) {
	store := &hybridStoreStub{items: map[string]Memory{
		"mem-1": {ID: "mem-1", UserID: "alice", Status: StatusActive},
		"mem-2": {ID: "mem-2", UserID: "alice", Status: StatusActive},
	}, lexical: []Memory{{ID: "mem-1", UserID: "alice", Status: StatusActive}, {ID: "mem-2", UserID: "alice", Status: StatusActive}}}
	hybrid := NewHybridStore(store, hybridEmbedder{}, hybridSemantic{hits: []SemanticHit{{ID: "mem-2", Score: 0.9}}})
	hybrid.Reranker = reverseReranker{}
	result, err := hybrid.SearchDetailed(context.Background(), Query{UserID: "alice", Text: "query", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stats.LexicalCount != 2 || result.Stats.SemanticCount != 1 || result.Stats.FusedCount != 2 || !result.Stats.RerankAttempted {
		t.Fatalf("stats = %#v", result.Stats)
	}
	if len(result.Memories) != 2 || result.Memories[0].ID != "mem-1" {
		t.Fatalf("reranked memories = %#v", result.Memories)
	}
}
