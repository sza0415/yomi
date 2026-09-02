package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

type EmbeddingProvider interface {
	Model() string
	Version() string
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

type VectorRecord struct {
	ID      string
	Vector  []float32
	Payload map[string]any
}

type Indexer interface {
	Upsert(ctx context.Context, records []VectorRecord) error
	Delete(ctx context.Context, ids []string) error
}

type Resetter interface {
	Reset(ctx context.Context) error
}

type SemanticHit struct {
	ID    string
	Score float64
}

// SemanticSearcher is an optional read extension for vector indexes. The
// canonical MemoryStore is still responsible for hydrating and validating IDs.
type SemanticSearcher interface {
	Search(ctx context.Context, vector []float32, query Query) ([]SemanticHit, error)
}

// Reranker is an optional cross-encoder/neural reranking hook. It receives the
// fused candidate list and may return a reordered list. Implementations are
// intentionally external so deployments can choose a local or hosted model.
type Reranker interface {
	Rerank(ctx context.Context, query string, items []Memory) ([]Memory, error)
}

type OpenAIEmbeddingProvider struct {
	BaseURL    string
	APIKey     string
	ModelName  string
	VersionTag string
	HTTPClient *http.Client
}

func (p *OpenAIEmbeddingProvider) Model() string { return p.ModelName }
func (p *OpenAIEmbeddingProvider) Version() string {
	if p.VersionTag != "" {
		return p.VersionTag
	}
	return p.ModelName
}

func (p *OpenAIEmbeddingProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if p == nil || strings.TrimSpace(p.BaseURL) == "" || strings.TrimSpace(p.APIKey) == "" || strings.TrimSpace(p.ModelName) == "" {
		return nil, errors.New("memory: embedding provider configuration is incomplete")
	}
	body, err := json.Marshal(map[string]any{"model": p.ModelName, "input": texts})
	if err != nil {
		return nil, fmt.Errorf("memory: marshal embeddings: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(p.BaseURL, "/")+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("memory: create embedding request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)
	client := p.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("memory: embedding request: %w", err)
	}
	defer resp.Body.Close()
	var wire struct {
		Data []struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
		Error any `json:"error,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("memory: decode embeddings: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("memory: embedding status %s: %v", resp.Status, wire.Error)
	}
	result := make([][]float32, len(texts))
	for _, item := range wire.Data {
		if item.Index >= 0 && item.Index < len(result) {
			result[item.Index] = item.Embedding
		}
	}
	for i, vector := range result {
		if len(vector) == 0 {
			return nil, fmt.Errorf("memory: embedding result missing index %d", i)
		}
	}
	return result, nil
}

type QdrantConfig struct {
	BaseURL    string
	Collection string
	Dimension  int
	APIKey     string
	HTTPClient *http.Client
}

type QdrantIndexer struct {
	config QdrantConfig
}

func NewQdrantIndexer(config QdrantConfig) (*QdrantIndexer, error) {
	if strings.TrimSpace(config.BaseURL) == "" || strings.TrimSpace(config.Collection) == "" {
		return nil, errors.New("memory: qdrant base URL and collection are required")
	}
	return &QdrantIndexer{config: config}, nil
}

func (q *QdrantIndexer) Upsert(ctx context.Context, records []VectorRecord) error {
	if len(records) == 0 {
		return nil
	}
	if q.config.Dimension <= 0 {
		q.config.Dimension = len(records[0].Vector)
	}
	if err := q.ensureCollection(ctx); err != nil {
		return err
	}
	points := make([]map[string]any, 0, len(records))
	for _, record := range records {
		points = append(points, map[string]any{"id": qdrantPointID(record.ID), "vector": record.Vector, "payload": record.Payload})
	}
	return q.doJSON(ctx, http.MethodPut, "/collections/"+q.config.Collection+"/points?wait=true", map[string]any{"points": points}, nil)
}

func (q *QdrantIndexer) Delete(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	points := make([]string, 0, len(ids))
	for _, id := range ids {
		points = append(points, qdrantPointID(id))
	}
	return q.doJSON(ctx, http.MethodPost, "/collections/"+q.config.Collection+"/points/delete?wait=true", map[string]any{"points": points}, nil)
}

func (q *QdrantIndexer) Reset(ctx context.Context) error {
	err := q.doJSON(ctx, http.MethodDelete, "/collections/"+q.config.Collection, nil, nil)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "404") {
		return err
	}
	return nil
}

func (q *QdrantIndexer) Search(ctx context.Context, vector []float32, query Query) ([]SemanticHit, error) {
	if len(vector) == 0 {
		return nil, errors.New("memory: semantic search vector is empty")
	}
	if strings.TrimSpace(query.UserID) == "" {
		return nil, errors.New("memory: semantic search user id is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 100 {
		limit = 100
	}
	statusValues := []any{"active"}
	if query.IncludeConflicts {
		statusValues = append(statusValues, "conflict")
	}
	must := []any{
		map[string]any{"key": "user_id", "match": map[string]any{"value": query.UserID}},
		map[string]any{"key": "status", "match": map[string]any{"any": statusValues}},
	}
	if len(query.Kinds) > 0 {
		kinds := make([]any, 0, len(query.Kinds))
		for _, kind := range query.Kinds {
			if kind = strings.TrimSpace(kind); kind != "" {
				kinds = append(kinds, kind)
			}
		}
		if len(kinds) > 0 {
			must = append(must, map[string]any{"key": "kind", "match": map[string]any{"any": kinds}})
		}
	}
	body := map[string]any{
		"vector":       vector,
		"limit":        limit,
		"with_payload": true,
		"with_vector":  false,
		"filter":       map[string]any{"must": must},
	}
	var response struct {
		Result []struct {
			ID      any            `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := q.doJSON(ctx, http.MethodPost, "/collections/"+q.config.Collection+"/points/search", body, &response); err != nil {
		return nil, err
	}
	hits := make([]SemanticHit, 0, len(response.Result))
	for _, result := range response.Result {
		id := ""
		if value, ok := result.Payload["memory_id"].(string); ok {
			id = strings.TrimSpace(value)
		}
		if id == "" {
			if value, ok := result.ID.(string); ok {
				id = strings.TrimSpace(value)
			}
		}
		if id == "" {
			continue
		}
		hits = append(hits, SemanticHit{ID: id, Score: result.Score})
	}
	return hits, nil
}

func (q *QdrantIndexer) ensureCollection(ctx context.Context) error {
	body := map[string]any{"vectors": map[string]any{"size": q.config.Dimension, "distance": "Cosine"}}
	err := q.doJSON(ctx, http.MethodPut, "/collections/"+q.config.Collection, body, nil)
	if err != nil && !strings.Contains(strings.ToLower(err.Error()), "already exists") {
		return err
	}
	return nil
}

func (q *QdrantIndexer) doJSON(ctx context.Context, method, path string, body any, response any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("memory: marshal qdrant request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(q.config.BaseURL, "/")+path, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("memory: create qdrant request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if q.config.APIKey != "" {
		req.Header.Set("api-key", q.config.APIKey)
	}
	client := q.config.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("memory: qdrant request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var detail map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&detail)
		return fmt.Errorf("memory: qdrant status %s: %v", resp.Status, detail)
	}
	if response != nil {
		if err := json.NewDecoder(resp.Body).Decode(response); err != nil {
			return fmt.Errorf("memory: decode qdrant response: %w", err)
		}
	}
	return nil
}

func qdrantPointID(id string) string {
	hash := sha256.Sum256([]byte(id))
	encoded := hex.EncodeToString(hash[:16])
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
