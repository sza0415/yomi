package memory

import (
	"context"
	"sort"
	"strings"
	"time"
)

const defaultRRFK = 60

// HybridStore 以 SQLite 作为权威存储，同时可以选择加入来自 Qdrant 的语义候选列表。
// 向量命中项必须先通过权威存储补全并校验，之后才能返回给调用方。
type HybridStore struct {
	Canonical Store
	Embedder  EmbeddingProvider
	Semantic  SemanticSearcher
	Reranker  Reranker
	RRFK      int
}

type rankedMemory struct {
	item  Memory
	score float64
}

func NewHybridStore(canonical Store, embedder EmbeddingProvider, semantic SemanticSearcher) *HybridStore {
	return &HybridStore{Canonical: canonical, Embedder: embedder, Semantic: semantic}
}

func (h *HybridStore) Search(ctx context.Context, query Query) ([]Memory, error) {
	result, err := h.SearchDetailed(ctx, query)
	return result.Memories, err
}

func (h *HybridStore) SearchDetailed(ctx context.Context, query Query) (SearchResult, error) {
	if h == nil || h.Canonical == nil {
		return SearchResult{}, nil
	}
	lexicalLimit := query.Limit
	if lexicalLimit <= 0 {
		lexicalLimit = defaultSearchLimit
	}
	lexicalLimit *= 4
	if lexicalLimit > 100 {
		lexicalLimit = 100
	}
	lexicalQuery := query
	lexicalQuery.Limit = lexicalLimit
	lexical, err := h.Canonical.Search(ctx, lexicalQuery)
	if err != nil {
		return SearchResult{}, err
	}
	stats := SearchStats{LexicalCount: len(lexical)}
	if strings.TrimSpace(query.Text) == "" || h.Embedder == nil || h.Semantic == nil {
		items := trimMemories(lexical, query.Limit)
		stats.FusedCount = len(items)
		return SearchResult{Memories: items, Stats: stats}, nil
	}

	stats.SemanticAttempted = true
	vectors, err := h.Embedder.Embed(ctx, []string{query.Text})
	if err != nil || len(vectors) != 1 {
		stats.SemanticFallback = true
		if err != nil {
			stats.SemanticError = err.Error()
		} else {
			stats.SemanticError = "embedding result count mismatch"
		}
		items := trimMemories(lexical, query.Limit)
		stats.FusedCount = len(items)
		return SearchResult{Memories: items, Stats: stats}, nil
	}
	hits, err := h.Semantic.Search(ctx, vectors[0], query)
	if err != nil {
		stats.SemanticFallback = true
		stats.SemanticError = err.Error()
		items := trimMemories(lexical, query.Limit)
		stats.FusedCount = len(items)
		return SearchResult{Memories: items, Stats: stats}, nil
	}
	stats.SemanticCount = len(hits)

	k := h.RRFK
	if k <= 0 {
		k = defaultRRFK
	}
	merged := make(map[string]rankedMemory, len(lexical)+len(hits))
	for rank, item := range lexical {
		merged[item.ID] = rankedMemory{item: item, score: 1 / float64(k+rank+1)}
	}
	for rank, hit := range hits {
		item, getErr := h.Canonical.Get(ctx, query.UserID, hit.ID)
		if getErr != nil || !servable(item, query) {
			continue
		}
		entry, exists := merged[item.ID]
		if exists {
			entry.score += 1 / float64(k+rank+1)
			entry.item = item
			merged[item.ID] = entry
			continue
		}
		merged[item.ID] = rankedMemory{item: item, score: 1 / float64(k+rank+1)}
	}
	items := make([]rankedMemory, 0, len(merged))
	for _, entry := range merged {
		if servable(entry.item, query) {
			items = append(items, entry)
		}
	}
	stats.FusedCount = len(items)
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		if items[i].item.Importance != items[j].item.Importance {
			return items[i].item.Importance > items[j].item.Importance
		}
		return items[i].item.UpdatedAt.After(items[j].item.UpdatedAt)
	})
	if h.Reranker != nil && len(items) > 1 {
		stats.RerankAttempted = true
		candidateItems := make([]Memory, 0, len(items))
		for _, entry := range items {
			candidateItems = append(candidateItems, entry.item)
		}
		if reranked, rerankErr := h.Reranker.Rerank(ctx, query.Text, candidateItems); rerankErr == nil && len(reranked) > 0 {
			items = reorderRanked(items, reranked)
		} else {
			stats.RerankFallback = true
			if rerankErr != nil {
				stats.RerankError = rerankErr.Error()
			} else {
				stats.RerankError = "reranker returned no results"
			}
		}
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if len(items) > limit {
		items = items[:limit]
	}
	result := make([]Memory, 0, len(items))
	for _, entry := range items {
		result = append(result, entry.item)
	}
	return SearchResult{Memories: result, Stats: stats}, nil
}

func reorderRanked(current []rankedMemory, reranked []Memory) []rankedMemory {
	byID := make(map[string]rankedMemory, len(current))
	for _, entry := range current {
		byID[entry.item.ID] = entry
	}
	result := make([]rankedMemory, 0, len(current))
	seen := make(map[string]struct{}, len(reranked))
	for _, item := range reranked {
		entry, ok := byID[item.ID]
		if !ok {
			continue
		}
		result = append(result, entry)
		seen[item.ID] = struct{}{}
	}
	for _, entry := range current {
		if _, ok := seen[entry.item.ID]; !ok {
			result = append(result, entry)
		}
	}
	return result
}

func (h *HybridStore) Upsert(ctx context.Context, item Memory) error {
	return h.Canonical.Upsert(ctx, item)
}

func (h *HybridStore) Get(ctx context.Context, userID, id string) (Memory, error) {
	return h.Canonical.Get(ctx, userID, id)
}

func (h *HybridStore) List(ctx context.Context, userID string) ([]Memory, error) {
	return h.Canonical.List(ctx, userID)
}

func (h *HybridStore) Delete(ctx context.Context, userID, id, reason string) error {
	return h.Canonical.Delete(ctx, userID, id, reason)
}

func (h *HybridStore) Rebuild(ctx context.Context, userID string) error {
	return h.Canonical.Rebuild(ctx, userID)
}

func servable(item Memory, query Query) bool {
	if item.UserID != query.UserID || item.Status == StatusDeleted || item.Status == StatusSuperseded {
		return false
	}
	if len(query.Kinds) > 0 && !kindAllowed(item.Kind, query.Kinds) {
		return false
	}
	if item.Status == StatusConflict && !query.IncludeConflicts {
		return false
	}
	return item.ExpiresAt.IsZero() || item.ExpiresAt.After(time.Now().UTC())
}

func kindAllowed(kind string, kinds []string) bool {
	for _, allowed := range kinds {
		if strings.TrimSpace(allowed) == kind {
			return true
		}
	}
	return false
}

func trimMemories(items []Memory, limit int) []Memory {
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if len(items) > limit {
		return items[:limit]
	}
	return items
}
