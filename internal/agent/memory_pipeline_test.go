package agent

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/memory"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

type pipelineExtractor struct {
	candidates []memory.Candidate
}

func (e pipelineExtractor) Extract(context.Context, memory.ExtractionInput) ([]memory.Candidate, error) {
	return e.candidates, nil
}

type pipelineEmbedder struct{}

func (pipelineEmbedder) Model() string   { return "test-embed" }
func (pipelineEmbedder) Version() string { return "v1" }
func (pipelineEmbedder) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{0.1, 0.2, 0.3}}, nil
}

type pipelineIndexer struct {
	records []memory.VectorRecord
}

func (i *pipelineIndexer) Upsert(_ context.Context, records []memory.VectorRecord) error {
	i.records = append(i.records, records...)
	return nil
}
func (i *pipelineIndexer) Delete(context.Context, []string) error { return nil }

func TestMemoryPipelineExtractsWritesAndIndexes(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	indexer := &pipelineIndexer{}
	traceSink := &recordingTraceSink{}
	loop := &Loop{
		Memory:          store,
		MemoryExtractor: pipelineExtractor{candidates: []memory.Candidate{{Kind: memory.KindPreference, Content: "用户偏好中文回答", Confidence: 0.95, Importance: 0.9}}},
		MemoryEmbedder:  pipelineEmbedder{},
		MemoryIndexer:   indexer,
		Trace:           traceSink,
	}
	run := NewRun("session-1", RunBudget{})
	input := bus.InboundMessage{UserID: "alice", SessionID: "session-1", Text: "请记住我喜欢中文", ChannelID: "test"}
	loop.extractAndStoreMemory(context.Background(), run, input, "好的")
	got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "中文", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "用户偏好中文回答" || got[0].SourceRunID != run.ID {
		t.Fatalf("memories = %#v", got)
	}
	if got[0].IndexStatus != "indexed" || got[0].EmbeddingModel != "test-embed" || got[0].EmbeddingDim != 3 {
		t.Fatalf("index state = %#v", got[0])
	}
	if len(indexer.records) != 1 || indexer.records[0].ID != got[0].ID {
		t.Fatalf("indexed records = %#v", indexer.records)
	}
	seen := map[string]bool{}
	for _, event := range traceSink.events {
		seen[event.Type] = true
	}
	for _, eventType := range []string{tracing.EventMemoryExtractionStarted, tracing.EventMemoryExtractionFinished, tracing.EventMemoryPolicyApplied, tracing.EventMemoryCandidateAccepted, tracing.EventMemoryWriteCompleted, tracing.EventMemoryIndexCompleted} {
		if !seen[eventType] {
			t.Fatalf("missing memory trace event %q in %#v", eventType, traceSink.events)
		}
	}
}

func TestMemoryPipelineDoesNotWriteRejectedCandidate(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loop := &Loop{
		Memory:          store,
		MemoryExtractor: pipelineExtractor{candidates: []memory.Candidate{{Kind: memory.KindFact, Content: "用户密码是 abc123", Confidence: 0.99, Importance: 0.99}}},
		Trace:           tracing.NoopSink{},
	}
	loop.extractAndStoreMemory(context.Background(), NewRun("session-1", RunBudget{}), bus.InboundMessage{UserID: "alice", SessionID: "session-1", Text: "我的密码是 abc123"}, "收到")
	got, err := store.List(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("sensitive memories = %#v", got)
	}
}
