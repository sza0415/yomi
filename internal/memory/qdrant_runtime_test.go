package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEnsureLocalQdrantRejectsRemoteEndpoint(t *testing.T) {
	err := EnsureLocalQdrant(context.Background(), "https://qdrant.example.com:6333")
	if err == nil || !strings.Contains(err.Error(), "non-local") {
		t.Fatalf("error = %v, want non-local endpoint error", err)
	}
}

func TestQdrantPointIDIsStableUUID(t *testing.T) {
	first := qdrantPointID("mem_123")
	second := qdrantPointID("mem_123")
	if first != second {
		t.Fatalf("point ID changed: %q != %q", first, second)
	}
	if len(first) != 36 || first[8] != '-' || first[13] != '-' || first[18] != '-' || first[23] != '-' {
		t.Fatalf("point ID = %q, want UUID shape", first)
	}
}

func TestQdrantSearchParsesPayloadAndSendsScopeFilter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/collections/memories/points/search" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		filter, ok := body["filter"].(map[string]any)
		if !ok {
			t.Fatalf("filter = %#v", body["filter"])
		}
		must, ok := filter["must"].([]any)
		if !ok || len(must) != 2 {
			t.Fatalf("must = %#v", filter["must"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":[{"id":"point-id","score":0.87,"payload":{"memory_id":"mem-1","user_id":"alice","status":"active"}}]}`))
	}))
	defer server.Close()

	indexer, err := NewQdrantIndexer(QdrantConfig{BaseURL: server.URL, Collection: "memories"})
	if err != nil {
		t.Fatal(err)
	}
	hits, err := indexer.Search(context.Background(), []float32{0.1, 0.2}, Query{UserID: "alice", Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "mem-1" || hits[0].Score != 0.87 {
		t.Fatalf("hits = %#v", hits)
	}
}
