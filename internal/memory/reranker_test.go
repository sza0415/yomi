package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHTTPRerankerReordersByReturnedIndexes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/rerank" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("authorization = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Model != "rerank-test" || body.Query != "中文" || len(body.Documents) != 3 || body.TopN != 2 {
			t.Fatalf("request body = %#v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[{"index":2,"relevance_score":0.99},{"index":0,"relevance_score":0.80}]}`))
	}))
	defer server.Close()

	reranker := &HTTPReranker{BaseURL: server.URL, APIKey: "test-key", ModelName: "rerank-test", TopN: 2}
	got, err := reranker.Rerank(context.Background(), "中文", []Memory{{ID: "mem-0", Content: "零"}, {ID: "mem-1", Content: "一"}, {ID: "mem-2", Content: "二"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "mem-2" || got[1].ID != "mem-0" {
		t.Fatalf("reranked = %#v", got)
	}
}
