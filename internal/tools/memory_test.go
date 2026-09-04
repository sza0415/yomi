package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziangsun/szabot/internal/memory"
)

func TestMemoryToolsBrowseHierarchyAndEnforceUserScope(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, item := range []memory.Memory{
		{ID: "alice-home", UserID: "alice", Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "云南昭通", Content: "用户家在云南昭通", Status: memory.StatusActive},
		{ID: "alice-friend", UserID: "alice", Kind: memory.KindFact, Subject: "friend:小王", Attribute: "home_province", Value: "四川", Content: "小王家在四川", Status: memory.StatusActive},
		{ID: "bob-home", UserID: "bob", Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "北京", Content: "用户家在北京", Status: memory.StatusActive},
	} {
		if err := store.Upsert(context.Background(), item); err != nil {
			t.Fatal(err)
		}
	}
	browse, _ := NewMemoryBrowse(store)
	ctx := WithUser(context.Background(), "alice")

	subjects, err := browse.Execute(ctx, json.RawMessage(`{"level":"subjects","kind":"fact"}`))
	if err != nil || !strings.Contains(subjects, `"self"`) || !strings.Contains(subjects, `friend:小王`) {
		t.Fatalf("subjects=%s err=%v", subjects, err)
	}
	attributes, err := browse.Execute(ctx, json.RawMessage(`{"level":"attributes","kind":"fact","subject":"self"}`))
	if err != nil || !strings.Contains(attributes, `home_city`) || strings.Contains(attributes, `home_province`) {
		t.Fatalf("attributes=%s err=%v", attributes, err)
	}
	items, err := browse.Execute(ctx, json.RawMessage(`{"level":"memories","kind":"fact","subject":"self","attribute":"home_city"}`))
	if err != nil || !strings.Contains(items, `alice-home`) || strings.Contains(items, `bob-home`) {
		t.Fatalf("items=%s err=%v", items, err)
	}
	if _, err := browse.Execute(context.Background(), json.RawMessage(`{"level":"kinds"}`)); err == nil {
		t.Fatal("expected missing user scope error")
	}
}

func TestMemorySearchAndGetToolsUseBoundUser(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Upsert(context.Background(), memory.Memory{ID: "mem-1", UserID: "alice", Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "云南昭通", Content: "用户家在云南昭通", Status: memory.StatusActive}); err != nil {
		t.Fatal(err)
	}
	search, _ := NewMemorySearch(store)
	get, _ := NewMemoryGet(store)
	ctx := WithUser(context.Background(), "alice")
	result, err := search.Execute(ctx, json.RawMessage(`{"query":"云南"}`))
	if err != nil || !strings.Contains(result, `mem-1`) {
		t.Fatalf("search=%s err=%v", result, err)
	}
	result, err = get.Execute(ctx, json.RawMessage(`{"ids":["mem-1"]}`))
	if err != nil || !strings.Contains(result, `云南昭通`) {
		t.Fatalf("get=%s err=%v", result, err)
	}
	if _, err := get.Execute(WithUser(context.Background(), "bob"), json.RawMessage(`{"ids":["mem-1"]}`)); err == nil {
		t.Fatal("expected cross-user get to fail")
	}
	if err := store.Delete(context.Background(), "alice", "mem-1", "test discarded memory filtering"); err != nil {
		t.Fatal(err)
	}
	if _, err := get.Execute(ctx, json.RawMessage(`{"ids":["mem-1"]}`)); err == nil {
		t.Fatal("expected discarded memory get to fail")
	}
}
