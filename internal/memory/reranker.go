package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HTTPReranker speaks the common rerank API shape used by hosted and local
// cross-encoder services. The response order is converted back to Memory
// objects by index; the canonical text never comes from the remote payload.
type HTTPReranker struct {
	BaseURL    string
	APIKey     string
	ModelName  string
	TopN       int
	HTTPClient *http.Client
}

func (r *HTTPReranker) Rerank(ctx context.Context, query string, items []Memory) ([]Memory, error) {
	if r == nil || strings.TrimSpace(r.BaseURL) == "" || strings.TrimSpace(r.APIKey) == "" || strings.TrimSpace(r.ModelName) == "" {
		return nil, errors.New("memory: reranker configuration is incomplete")
	}
	if strings.TrimSpace(query) == "" || len(items) == 0 {
		return items, nil
	}
	documents := make([]string, len(items))
	for i, item := range items {
		documents[i] = item.Content
	}
	topN := r.TopN
	if topN <= 0 || topN > len(documents) {
		topN = len(documents)
	}
	body, err := json.Marshal(map[string]any{
		"model": r.ModelName, "query": query, "documents": documents,
		"top_n": topN, "return_documents": false,
	})
	if err != nil {
		return nil, fmt.Errorf("memory: marshal reranker request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(r.BaseURL, "/")+"/rerank", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("memory: create reranker request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+r.APIKey)
	client := r.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memory: reranker request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var detail any
		_ = json.NewDecoder(resp.Body).Decode(&detail)
		return nil, fmt.Errorf("memory: reranker status %s: %v", resp.Status, detail)
	}
	var wire struct {
		Results []struct {
			Index          int      `json:"index"`
			RelevanceScore *float64 `json:"relevance_score,omitempty"`
			Score          *float64 `json:"score,omitempty"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("memory: decode reranker response: %w", err)
	}
	if len(wire.Results) == 0 {
		return nil, errors.New("memory: reranker returned no results")
	}
	result := make([]Memory, 0, len(wire.Results))
	seen := make(map[int]struct{}, len(wire.Results))
	for _, hit := range wire.Results {
		if hit.Index < 0 || hit.Index >= len(items) {
			continue
		}
		if _, ok := seen[hit.Index]; ok {
			continue
		}
		seen[hit.Index] = struct{}{}
		result = append(result, items[hit.Index])
	}
	if len(result) == 0 {
		return nil, errors.New("memory: reranker returned no valid indexes")
	}
	return result, nil
}
