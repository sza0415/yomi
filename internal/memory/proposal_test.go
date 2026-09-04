package memory

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSQLiteProposalLifecycleAndAtomicApply(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Upsert(ctx, Memory{ID: "mem-old", UserID: "alice", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "云南", Content: "用户家在云南", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	proposal, err := store.CreateProposal(ctx, ProposalRecord{
		UserID: "alice", SourceSessionID: "session-1", SourceRunID: "run-1", ChannelID: "test",
		Operation: ProposalNeedsConfirmation, Candidate: Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "四川", Content: "用户家在四川", Confidence: 0.9, Importance: 0.8},
		TargetIDs: []string{"mem-old"}, Status: ProposalStatusPending,
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if version, ok := proposal.TargetVersions["mem-old"]; !ok || version.IsZero() {
		t.Fatalf("target versions = %#v", proposal.TargetVersions)
	}
	pending, err := store.ListPendingProposals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != proposal.ID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	if err := store.ApplyMutation(ctx, Mutation{
		ProposalID: proposal.ID, SupersedeIDs: []string{"mem-old"}, Reason: "user approved",
		Memory: Memory{ID: "mem-new", UserID: "alice", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "四川", Content: "用户家在四川", Status: StatusActive, SourceRunID: "run-1", SourceSessionID: "session-1", Confidence: 0.9, Importance: 0.8},
	}); err != nil {
		t.Fatal(err)
	}
	if pending, err := store.ListPendingProposals(ctx); err != nil || len(pending) != 0 {
		t.Fatalf("pending after apply=%#v err=%v", pending, err)
	}
	newItem, err := store.Get(ctx, "alice", "mem-new")
	if err != nil || newItem.Status != StatusActive {
		t.Fatalf("new item=%#v err=%v", newItem, err)
	}
	oldItem, err := store.Get(ctx, "alice", "mem-old")
	if err != nil || oldItem.Status != StatusSuperseded {
		t.Fatalf("old item=%#v err=%v", oldItem, err)
	}
}

func TestSQLiteProposalRejectsChangedTargetVersion(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	old := Memory{ID: "mem-old", UserID: "alice", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "云南", Content: "用户家在云南", Status: StatusActive}
	if err := store.Upsert(ctx, old); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "四川", Content: "用户家在四川", Confidence: 0.9, Importance: 0.8}
	proposal, err := store.CreateProposal(ctx, ProposalRecord{
		UserID: "alice", SourceSessionID: "session-1", SourceRunID: "run-1", ChannelID: "test",
		Operation: ProposalNeedsConfirmation, Candidate: candidate, TargetIDs: []string{"mem-old"},
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	old.Content = "用户家在云南昭通"
	old.Value = "云南昭通"
	if err := store.Upsert(ctx, old); err != nil {
		t.Fatal(err)
	}
	err = store.ApplyMutation(ctx, Mutation{
		ProposalID: proposal.ID, SupersedeIDs: []string{"mem-old"},
		Memory: Memory{ID: "mem-new", UserID: "alice", Kind: candidate.Kind, Subject: candidate.Subject,
			Attribute: candidate.Attribute, Value: candidate.Value, Content: candidate.Content,
			Confidence: candidate.Confidence, Importance: candidate.Importance,
			SourceRunID: "run-1", SourceSessionID: "session-1", Status: StatusActive},
	})
	if err == nil {
		t.Fatal("expected stale target version to reject the mutation")
	}
	if _, err := store.Get(ctx, "alice", "mem-new"); err == nil {
		t.Fatal("stale proposal wrote a new memory")
	}
	pending, err := store.ListPendingProposals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != proposal.ID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}

func TestSQLiteProposalRejectsTargetThatExpiredWhilePending(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.Upsert(ctx, Memory{
		ID: "mem-old", UserID: "alice", Kind: KindFact, Subject: "self",
		Attribute: "home_city", Value: "云南", Content: "用户家在云南",
		Status: StatusActive, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	candidate := Candidate{Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "四川", Content: "用户家在四川", Confidence: 0.9, Importance: 0.8}
	proposal, err := store.CreateProposal(ctx, ProposalRecord{
		UserID: "alice", SourceSessionID: "session-1", SourceRunID: "run-1", ChannelID: "test",
		Operation: ProposalNeedsConfirmation, Candidate: candidate, TargetIDs: []string{"mem-old"},
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE memories SET expires_at = ? WHERE id = ?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), "mem-old"); err != nil {
		t.Fatal(err)
	}
	err = store.ApplyMutation(ctx, Mutation{
		ProposalID: proposal.ID, SupersedeIDs: []string{"mem-old"},
		Memory: Memory{ID: "mem-new", UserID: "alice", Kind: candidate.Kind, Subject: candidate.Subject,
			Attribute: candidate.Attribute, Value: candidate.Value, Content: candidate.Content,
			Confidence: candidate.Confidence, Importance: candidate.Importance,
			SourceRunID: "run-1", SourceSessionID: "session-1", Status: StatusActive},
	})
	if err == nil || !strings.Contains(err.Error(), "has expired") {
		t.Fatalf("error=%v, want expired target rejection", err)
	}
	if _, err := store.Get(ctx, "alice", "mem-new"); err == nil {
		t.Fatal("expired target proposal wrote a new memory")
	}
	pending, err := store.ListPendingProposals(ctx)
	if err != nil || len(pending) != 1 || pending[0].ID != proposal.ID {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
}
