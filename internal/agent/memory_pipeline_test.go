package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/memory"
	"github.com/ziangsun/szabot/internal/providers"
	"github.com/ziangsun/szabot/internal/tools"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

type pipelineExtractor struct {
	candidates []memory.Candidate
}

type pipelineCurator struct {
	proposals []memory.Proposal
	err       error
}

func (c pipelineCurator) Curate(context.Context, memory.ExtractionInput) ([]memory.Proposal, error) {
	return c.proposals, c.err
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
	records    []memory.VectorRecord
	deletedIDs []string
}

func (i *pipelineIndexer) Upsert(_ context.Context, records []memory.VectorRecord) error {
	i.records = append(i.records, records...)
	return nil
}
func (i *pipelineIndexer) Delete(_ context.Context, ids []string) error {
	i.deletedIDs = append(i.deletedIDs, ids...)
	return nil
}

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

func TestMemoryPipelineDoesNotFallbackAfterCuratorFailure(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loop := &Loop{
		Memory:        store,
		MemoryCurator: pipelineCurator{err: errors.New("curator unavailable")},
		MemoryExtractor: pipelineExtractor{candidates: []memory.Candidate{{
			Kind: memory.KindFact, Subject: "self", Attribute: "home_province",
			Value: "四川", Content: "用户家在四川", Confidence: 0.95, Importance: 0.8,
		}}},
		Trace: tracing.NoopSink{},
	}
	outcome, confirmations := loop.extractAndStoreMemory(context.Background(), NewRun("session-1", RunBudget{}), bus.InboundMessage{
		UserID: "alice", SessionID: "session-1", Text: "我的家在四川",
	}, "好的")
	if outcome.Status != "failed" || !strings.Contains(outcome.Error, "curator unavailable") || len(confirmations) != 0 {
		t.Fatalf("outcome=%#v confirmations=%#v", outcome, confirmations)
	}
	if got, err := store.List(context.Background(), "alice"); err != nil || len(got) != 0 {
		t.Fatalf("memories=%#v err=%v", got, err)
	}
}

func TestMemoryPipelineSupersedesExplicitReplacement(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Upsert(context.Background(), memory.Memory{
		ID: "mem-old", UserID: "alice", Kind: memory.KindFact, Subject: "self",
		Attribute: "home_city", Value: "北京", Content: "用户住在北京", Status: memory.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	indexer := &pipelineIndexer{}
	loop := &Loop{
		Memory: store,
		MemoryExtractor: pipelineExtractor{candidates: []memory.Candidate{{
			Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "上海",
			Content: "用户已搬到上海", ChangeHint: memory.ChangeHintReplace,
			Confidence: 0.95, Importance: 0.9,
		}}},
		MemoryEmbedder: pipelineEmbedder{}, MemoryIndexer: indexer, Trace: tracing.NoopSink{},
	}
	loop.extractAndStoreMemory(context.Background(), NewRun("session-1", RunBudget{}), bus.InboundMessage{UserID: "alice", SessionID: "session-1", Text: "我搬到上海了"}, "好的")
	old, err := store.Get(context.Background(), "alice", "mem-old")
	if err != nil || old.Status != memory.StatusSuperseded {
		t.Fatalf("old memory = %#v, err=%v", old, err)
	}
	got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "上海", Limit: 8})
	if err != nil || len(got) != 1 || got[0].Status != memory.StatusActive {
		t.Fatalf("new memories = %#v, err=%v", got, err)
	}
	if len(indexer.deletedIDs) != 1 || indexer.deletedIDs[0] != "mem-old" {
		t.Fatalf("deleted index IDs = %#v", indexer.deletedIDs)
	}
}

func TestMemoryCuratorReusesExistingAttributeForExplicitReplacement(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Upsert(context.Background(), memory.Memory{
		ID: "mem-old", UserID: "alice", Kind: memory.KindFact, Subject: "self",
		Attribute: "home_city", Value: "云南昭通", Content: "用户家在云南昭通", Status: memory.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	loop := &Loop{
		Memory: store, Trace: tracing.NoopSink{},
		MemoryCurator: pipelineCurator{proposals: []memory.Proposal{{
			Operation: memory.ProposalReplace, TargetIDs: []string{"mem-old"},
			Candidate: memory.Candidate{Kind: memory.KindFact, Subject: "self", Attribute: "home_province",
				Value: "四川", Content: "用户家在四川", Confidence: 0.95, Importance: 0.8},
		}}},
	}
	outcome, confirmations := loop.extractAndStoreMemory(context.Background(), NewRun("session-1", RunBudget{}), bus.InboundMessage{
		UserID: "alice", SessionID: "session-1", ChannelID: "test", Text: "我的家其实在四川",
	}, "好的")
	if len(confirmations) != 0 || outcome.WrittenCount != 1 || outcome.Status != "completed" {
		t.Fatalf("outcome=%#v confirmations=%d", outcome, len(confirmations))
	}
	old, err := store.Get(context.Background(), "alice", "mem-old")
	if err != nil || old.Status != memory.StatusSuperseded {
		t.Fatalf("old=%#v err=%v", old, err)
	}
	result, err := store.Browse(context.Background(), memory.BrowseQuery{
		UserID: "alice", Level: memory.BrowseMemories, Kind: memory.KindFact, Subject: "self", Attribute: "home_city",
	})
	if err != nil || len(result.Memories) != 1 || result.Memories[0].Value != "四川" {
		t.Fatalf("replacement=%#v err=%v", result.Memories, err)
	}
	catalog, err := store.Catalog(context.Background(), "alice", false)
	if err != nil || len(catalog) != 1 || catalog[0].Attribute != "home_city" {
		t.Fatalf("catalog=%#v err=%v", catalog, err)
	}
}

func TestMemoryConfirmationRestoresPendingProposalAfterRestart(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Upsert(ctx, memory.Memory{
		ID: "mem-old", UserID: "alice", Kind: memory.KindFact, Subject: "self",
		Attribute: "home_city", Value: "云南", Content: "用户家在云南", Status: memory.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateProposal(ctx, memory.ProposalRecord{
		UserID: "alice", SourceSessionID: "session-1", SourceRunID: "run-before-restart", ChannelID: "test",
		Operation: memory.ProposalNeedsConfirmation,
		Candidate: memory.Candidate{Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "四川", Content: "用户家在四川", Confidence: 0.9, Importance: 0.8},
		TargetIDs: []string{"mem-old"}, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	messageBus := bus.New(32)
	loop := &Loop{Bus: messageBus, Memory: store, Trace: tracing.NoopSink{}, MemoryConfirmationTimeout: time.Second}
	loopCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	loop.Start(loopCtx)
	question := waitForMemoryQuestion(t, messageBus)
	if err := messageBus.PublishInbound(ctx, bus.InboundMessage{UserID: "alice", SessionID: "session-1", ChannelID: "test", Text: "拒绝替换"}); err != nil {
		t.Fatal(err)
	}
	waitForRunDone(t, messageBus, question.RunID)
	pending, err := store.ListPendingProposals(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	old, err := store.Get(ctx, "alice", "mem-old")
	if err != nil || old.Status != memory.StatusActive {
		t.Fatalf("old=%#v err=%v", old, err)
	}
}

func TestMemoryPipelineConfirmsAmbiguousReplacementInLightweightRun(t *testing.T) {
	loop, store, messageBus, sourceRun, cancel := newMemoryConfirmationFixture(t, time.Second)
	defer cancel()

	loop.startMemoryExtraction(context.Background(), sourceRun, ambiguousReplacementInbound(), "好的")
	question := waitForMemoryQuestion(t, messageBus)
	if question.SessionID != sourceRun.SessionID || question.RunID == "" || question.RunID == sourceRun.ID {
		t.Fatalf("confirmation question = %#v, want a lightweight run in the source session", question)
	}
	old, err := store.Get(context.Background(), "alice", "mem-old")
	if err != nil || old.Status != memory.StatusActive {
		t.Fatalf("old memory changed before confirmation: %#v, err=%v", old, err)
	}
	if got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "上海", Limit: 8}); err != nil || len(got) != 0 {
		t.Fatalf("new memory was written before confirmation: %#v, err=%v", got, err)
	}

	if err := messageBus.PublishInbound(context.Background(), bus.InboundMessage{UserID: "alice", SessionID: sourceRun.SessionID, ChannelID: "test", Text: "确认替换"}); err != nil {
		t.Fatal(err)
	}
	waitForRunDone(t, messageBus, question.RunID)

	old, err = store.Get(context.Background(), "alice", "mem-old")
	if err != nil || old.Status != memory.StatusSuperseded {
		t.Fatalf("old memory = %#v, err=%v, want superseded", old, err)
	}
	if got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "北京", Limit: 8}); err != nil || len(got) != 0 {
		t.Fatalf("superseded memory remained recallable: %#v, err=%v", got, err)
	}
	got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "上海", Limit: 8})
	if err != nil || len(got) != 1 || got[0].Status != memory.StatusActive {
		t.Fatalf("confirmed memory = %#v, err=%v", got, err)
	}
	pending, err := store.ListPendingProposals(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending proposals = %#v, err=%v", pending, err)
	}
	state := sourceRun.Snapshot().Memory
	if sourceRun.Status != RunCompleted || state.Status != "completed" || state.PendingConfirmationCount != 0 || state.ConfirmedCount != 1 || state.WrittenCount != 1 {
		t.Fatalf("source run memory state = %#v, run status=%s", state, sourceRun.Status)
	}
}

func TestMemoryPipelineRejectsAmbiguousReplacement(t *testing.T) {
	loop, store, messageBus, sourceRun, cancel := newMemoryConfirmationFixture(t, time.Second)
	defer cancel()

	loop.startMemoryExtraction(context.Background(), sourceRun, ambiguousReplacementInbound(), "好的")
	question := waitForMemoryQuestion(t, messageBus)
	if err := messageBus.PublishInbound(context.Background(), bus.InboundMessage{UserID: "alice", SessionID: sourceRun.SessionID, ChannelID: "test", Text: "拒绝替换"}); err != nil {
		t.Fatal(err)
	}
	waitForRunDone(t, messageBus, question.RunID)

	assertAmbiguousReplacementDiscarded(t, store, sourceRun)
}

func TestMemoryPipelineDiscardsAmbiguousReplacementOnTimeout(t *testing.T) {
	loop, store, messageBus, sourceRun, cancel := newMemoryConfirmationFixture(t, 30*time.Millisecond)
	defer cancel()

	loop.startMemoryExtraction(context.Background(), sourceRun, ambiguousReplacementInbound(), "好的")
	question := waitForMemoryQuestion(t, messageBus)
	waitForRunDone(t, messageBus, question.RunID)

	assertAmbiguousReplacementDiscarded(t, store, sourceRun)
}

func TestMemoryConfirmationKeepsProposalPendingOnShutdown(t *testing.T) {
	loop, store, messageBus, sourceRun, cancel := newMemoryConfirmationFixture(t, time.Second)

	loop.startMemoryExtraction(context.Background(), sourceRun, ambiguousReplacementInbound(), "好的")
	question := waitForMemoryQuestion(t, messageBus)
	cancel()
	waitForRunDone(t, messageBus, question.RunID)

	pending, err := store.ListPendingProposals(context.Background())
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending proposals = %#v, err=%v, want one proposal preserved for restart", pending, err)
	}
	old, err := store.Get(context.Background(), "alice", "mem-old")
	if err != nil || old.Status != memory.StatusActive {
		t.Fatalf("old memory = %#v, err=%v, want active", old, err)
	}
	if got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "上海", Limit: 8}); err != nil || len(got) != 0 {
		t.Fatalf("candidate was written during shutdown: %#v, err=%v", got, err)
	}
	state := sourceRun.Snapshot().Memory
	if state.Status != "waiting_user" || state.PendingConfirmationCount != 1 || state.WrittenCount != 0 {
		t.Fatalf("source run memory state = %#v", state)
	}
}

func TestMemoryPipelineDiscardsConfirmationWhenRelatedMemoryChanges(t *testing.T) {
	loop, store, messageBus, sourceRun, cancel := newMemoryConfirmationFixture(t, time.Second)
	defer cancel()

	loop.startMemoryExtraction(context.Background(), sourceRun, ambiguousReplacementInbound(), "好的")
	question := waitForMemoryQuestion(t, messageBus)
	if err := store.Upsert(context.Background(), memory.Memory{
		ID: "mem-concurrent", UserID: "alice", Kind: memory.KindFact, Subject: "self",
		Attribute: "home_city", Value: "深圳", Content: "用户住在深圳", Status: memory.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	if err := messageBus.PublishInbound(context.Background(), bus.InboundMessage{UserID: "alice", SessionID: sourceRun.SessionID, ChannelID: "test", Text: "确认替换"}); err != nil {
		t.Fatal(err)
	}
	waitForRunDone(t, messageBus, question.RunID)

	for _, id := range []string{"mem-old", "mem-concurrent"} {
		item, err := store.Get(context.Background(), "alice", id)
		if err != nil || item.Status != memory.StatusActive {
			t.Fatalf("concurrent memory %s = %#v, err=%v, want active", id, item, err)
		}
	}
	if got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "上海", Limit: 8}); err != nil || len(got) != 0 {
		t.Fatalf("stale candidate was stored: %#v, err=%v", got, err)
	}
	state := sourceRun.Snapshot().Memory
	if state.DiscardedCount != 1 || state.WrittenCount != 0 || state.Status != "completed" {
		t.Fatalf("source run memory state = %#v", state)
	}
}

func TestMemoryConfirmationRunsAfterSourceRunDone(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Upsert(context.Background(), memory.Memory{
		ID: "mem-old", UserID: "alice", Kind: memory.KindFact, Subject: "self",
		Attribute: "home_city", Value: "北京", Content: "用户住在北京", Status: memory.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	messageBus := bus.New(32)
	loop := &Loop{
		Bus:    messageBus,
		Runner: &Runner{Provider: &scriptedProvider{responses: []providers.ChatResponse{{Content: "好的"}}}, Model: "test", Tools: tools.NewRegistry()},
		Memory: store,
		MemoryExtractor: pipelineExtractor{candidates: []memory.Candidate{{
			Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "上海",
			Content: "用户住在上海", ChangeHint: memory.ChangeHintReplace,
			Confidence: 0.95, Importance: 0.9,
		}}},
		MemoryConfirmationTimeout: time.Second,
		Trace:                     tracing.NoopSink{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	loop.Start(ctx)
	if err := messageBus.PublishInbound(ctx, ambiguousReplacementInbound()); err != nil {
		t.Fatal(err)
	}

	var sourceRunID string
	deadline := time.After(2 * time.Second)
	for {
		select {
		case out := <-messageBus.Outbound():
			if out.Done && sourceRunID == "" {
				sourceRunID = out.RunID
				continue
			}
			if out.Kind != bus.KindQuestion {
				continue
			}
			if sourceRunID == "" {
				t.Fatal("memory confirmation started before the source run finished")
			}
			if out.RunID == sourceRunID {
				t.Fatal("memory confirmation reused the terminal source run")
			}
			if err := messageBus.PublishInbound(ctx, bus.InboundMessage{UserID: "alice", SessionID: out.SessionID, ChannelID: "test", Text: "拒绝替换"}); err != nil {
				t.Fatal(err)
			}
			waitForRunDone(t, messageBus, out.RunID)
			return
		case <-deadline:
			t.Fatal("timed out waiting for queued memory confirmation")
		}
	}
}

func newMemoryConfirmationFixture(t *testing.T, confirmationTimeout time.Duration) (*Loop, *memory.SQLiteStore, *bus.MessageBus, *Run, context.CancelFunc) {
	t.Helper()
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Upsert(context.Background(), memory.Memory{
		ID: "mem-old", UserID: "alice", Kind: memory.KindFact, Subject: "self",
		Attribute: "home_city", Value: "北京", Content: "用户住在北京", Status: memory.StatusActive,
	}); err != nil {
		t.Fatal(err)
	}
	messageBus := bus.New(32)
	loop := &Loop{
		Bus: messageBus, Memory: store,
		MemoryExtractor: pipelineExtractor{candidates: []memory.Candidate{{
			Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "上海",
			Content: "用户住在上海", ChangeHint: memory.ChangeHintReplace,
			Confidence: 0.95, Importance: 0.9,
		}}},
		MemoryConfirmationTimeout: confirmationTimeout,
		Trace:                     tracing.NoopSink{},
	}
	ctx, cancel := context.WithCancel(context.Background())
	loop.Start(ctx)
	sourceRun := NewRun("session-1", RunBudget{})
	if err := sourceRun.Transition(RunRunning, "run started"); err != nil {
		t.Fatal(err)
	}
	if err := sourceRun.Transition(RunCompleted, "answer completed"); err != nil {
		t.Fatal(err)
	}
	return loop, store, messageBus, sourceRun, cancel
}

func ambiguousReplacementInbound() bus.InboundMessage {
	return bus.InboundMessage{
		UserID: "alice", SessionID: "session-1", ChannelID: "test",
		Text: "我在上海有一套房", Time: time.Now(),
	}
}

func waitForMemoryQuestion(t *testing.T, messageBus *bus.MessageBus) bus.OutboundMessage {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case out := <-messageBus.Outbound():
			if out.Kind == bus.KindQuestion {
				return out
			}
		case <-deadline:
			t.Fatal("timed out waiting for memory confirmation question")
		}
	}
}

func waitForRunDone(t *testing.T, messageBus *bus.MessageBus, runID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case out := <-messageBus.Outbound():
			if out.Done && out.RunID == runID {
				return
			}
		case <-deadline:
			t.Fatalf("timed out waiting for run %s to finish", runID)
		}
	}
}

func assertAmbiguousReplacementDiscarded(t *testing.T, store *memory.SQLiteStore, sourceRun *Run) {
	t.Helper()
	old, err := store.Get(context.Background(), "alice", "mem-old")
	if err != nil || old.Status != memory.StatusActive {
		t.Fatalf("old memory = %#v, err=%v, want active", old, err)
	}
	got, err := store.Search(context.Background(), memory.Query{UserID: "alice", Text: "上海", Limit: 8})
	if err != nil || len(got) != 0 {
		t.Fatalf("discarded candidate was stored: %#v, err=%v", got, err)
	}
	pending, err := store.ListPendingProposals(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending proposals = %#v, err=%v", pending, err)
	}
	state := sourceRun.Snapshot().Memory
	if sourceRun.Status != RunCompleted || state.Status != "completed" || state.PendingConfirmationCount != 0 || state.DiscardedCount != 1 || state.WrittenCount != 0 {
		t.Fatalf("source run memory state = %#v, run status=%s", state, sourceRun.Status)
	}
}
