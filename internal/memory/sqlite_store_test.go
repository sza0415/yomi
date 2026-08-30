package memory

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteStoreScopesSearchAndDelete(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memories", "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Upsert(ctx, Memory{ID: "mem-a", UserID: "alice", Kind: KindPreference, Content: "用户偏好中文回答", Confidence: 0.9}); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(ctx, Memory{ID: "mem-b", UserID: "bob", Kind: KindFact, Content: "用户住在上海", Confidence: 0.8}); err != nil {
		t.Fatal(err)
	}

	got, err := store.Search(ctx, Query{UserID: "alice", Text: "中文", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "mem-a" {
		t.Fatalf("alice search = %#v", got)
	}
	got, err = store.Search(ctx, Query{UserID: "alice", Text: "上海", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("cross-user search = %#v", got)
	}

	if err := store.Delete(ctx, "alice", "mem-a", "user requested forget"); err != nil {
		t.Fatal(err)
	}
	got, err = store.Search(ctx, Query{UserID: "alice", Text: "中文", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("deleted search = %#v", got)
	}
	deleted, err := store.Get(ctx, "alice", "mem-a")
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Status != StatusDeleted {
		t.Fatalf("deleted status = %q", deleted.Status)
	}
}

func TestSQLiteStoreFiltersKinds(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, item := range []Memory{
		{ID: "profile", UserID: "alice", Kind: KindPreference, Content: "用户偏好中文回答", Status: StatusActive, Importance: 0.9},
		{ID: "episode", UserID: "alice", Kind: KindEpisode, Content: "用户昨天确认了东京行程", Status: StatusActive, Importance: 0.9},
	} {
		if err := store.Upsert(ctx, item); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.Search(ctx, Query{UserID: "alice", Text: "用户", Kinds: []string{KindFact, KindPreference}, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "profile" {
		t.Fatalf("profile search = %#v", got)
	}
	got, err = store.Search(ctx, Query{UserID: "alice", Text: "用户", Kinds: []string{KindEpisode}, Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "episode" {
		t.Fatalf("episode search = %#v", got)
	}
}

func TestSQLiteStoreReopensAndRebuildsFTS(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "memory.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-time.Hour)
	if err := store.Upsert(ctx, Memory{ID: "mem-1", UserID: "alice", Kind: KindFact, Content: "用户目前居住在北京", CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Search(ctx, Query{UserID: "alice", Text: "北京", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "mem-1" {
		t.Fatalf("reopened search = %#v", got)
	}
	if _, err := reopened.db.Exec(`DELETE FROM memory_fts`); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Rebuild(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	got, err = reopened.Search(ctx, Query{UserID: "alice", Text: "北京", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "用户目前居住在北京" {
		t.Fatalf("rebuilt search = %#v", got)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteStoreRejectsMissingScope(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Search(context.Background(), Query{Text: "anything"}); err == nil {
		t.Fatal("expected missing user scope error")
	}
	if err := store.Upsert(context.Background(), Memory{Content: "no user"}); err == nil {
		t.Fatal("expected missing user id error")
	}
	if _, err := store.Get(context.Background(), "alice", "missing"); err == nil || err == sql.ErrNoRows {
		t.Fatalf("expected wrapped not-found error, got %v", err)
	}
}

func TestSQLiteStoreAppliesSupersedeAndConflictAtomically(t *testing.T) {
	ctx := context.Background()
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.Upsert(ctx, Memory{ID: "mem-old", UserID: "alice", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "北京", Content: "用户住在北京", Status: StatusActive}); err != nil {
		t.Fatal(err)
	}
	if err := store.ApplyMutation(ctx, Mutation{
		Memory:       Memory{ID: "mem-new", UserID: "alice", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "上海", Content: "用户住在上海", Status: StatusActive},
		SupersedeIDs: []string{"mem-old"}, Reason: "user explicitly moved",
	}); err != nil {
		t.Fatal(err)
	}
	old, err := store.Get(ctx, "alice", "mem-old")
	if err != nil || old.Status != StatusSuperseded {
		t.Fatalf("old memory = %#v, err=%v", old, err)
	}
	got, err := store.Search(ctx, Query{UserID: "alice", Text: "北京", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("superseded memory search = %#v", got)
	}

	if err := store.ApplyMutation(ctx, Mutation{
		Memory:      Memory{ID: "mem-conflict", UserID: "alice", Kind: KindFact, Subject: "self", Attribute: "home_city", Value: "广州", Content: "用户住在广州", Status: StatusConflict},
		ConflictIDs: []string{"mem-new"}, Reason: "conflicting unconfirmed value",
	}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"mem-new", "mem-conflict"} {
		item, getErr := store.Get(ctx, "alice", id)
		if getErr != nil || item.Status != StatusConflict {
			t.Fatalf("conflict memory %s = %#v, err=%v", id, item, getErr)
		}
	}
	got, err = store.Search(ctx, Query{UserID: "alice", Text: "广州", Limit: 8})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("default conflict search = %#v", got)
	}
	got, err = store.Search(ctx, Query{UserID: "alice", Text: "广州", Limit: 8, IncludeConflicts: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "mem-conflict" {
		t.Fatalf("included conflict search = %#v", got)
	}
}
