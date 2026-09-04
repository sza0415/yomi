package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ziangsun/szabot/internal/memory"
	"github.com/ziangsun/szabot/internal/providers"
)

func TestToolMemoryCuratorBrowsesHierarchyBeforeProposal(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Upsert(context.Background(), memory.Memory{ID: "mem-old", UserID: "alice", Kind: memory.KindFact, Subject: "self", Attribute: "home_city", Value: "云南昭通", Content: "用户家在云南昭通", Status: memory.StatusActive}); err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{responses: []providers.ChatResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "memory_browse", Arguments: json.RawMessage(`{"level":"subjects","kind":"fact"}`)}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "memory_browse", Arguments: json.RawMessage(`{"level":"attributes","kind":"fact","subject":"self"}`)}}},
		{ToolCalls: []providers.ToolCall{{ID: "3", Name: "memory_browse", Arguments: json.RawMessage(`{"level":"memories","kind":"fact","subject":"self","attribute":"home_city"}`)}}},
		{Content: `[{"operation":"replace","candidate":{"kind":"fact","subject":"self","attribute":"home_province","value":"四川","content":"用户家在四川","evidence":"用户说其实在四川","confidence":0.95,"importance":0.8},"target_ids":["mem-old"],"reason":"用户纠正家庭所在地"}]`},
	}}
	curator, err := NewToolMemoryCurator(provider, "test", store)
	if err != nil {
		t.Fatal(err)
	}
	proposals, err := curator.Curate(context.Background(), memory.ExtractionInput{UserID: "alice", SessionID: "s", UserText: "我的家其实在四川", AssistantText: "好的"})
	if err != nil {
		t.Fatal(err)
	}
	if len(proposals) != 1 || proposals[0].Operation != memory.ProposalReplace || len(proposals[0].TargetIDs) != 1 {
		t.Fatalf("proposals=%#v", proposals)
	}
	if len(provider.requests) != 4 || !strings.Contains(string(provider.requests[0].Messages[len(provider.requests[0].Messages)-1].Content), "home_city") {
		t.Fatalf("curator requests=%#v", provider.requests)
	}
}

func TestToolMemoryCuratorRejectsUndiscoveredTarget(t *testing.T) {
	store, err := memory.NewSQLiteStore(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	provider := &scriptedProvider{responses: []providers.ChatResponse{{
		Content: `[{"operation":"replace","candidate":{"kind":"fact","subject":"self","attribute":"home_city","value":"四川","content":"用户家在四川","confidence":0.95,"importance":0.8},"target_ids":["invented-id"]}]`,
	}}}
	curator, err := NewToolMemoryCurator(provider, "test", store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := curator.Curate(context.Background(), memory.ExtractionInput{UserID: "alice", UserText: "我的家其实在四川"}); err == nil || !strings.Contains(err.Error(), "was not discovered") {
		t.Fatalf("error=%v, want undiscovered target rejection", err)
	}
}
