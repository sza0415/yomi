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
