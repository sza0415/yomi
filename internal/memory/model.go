package memory

import (
	"context"
	"time"
)

const (
	KindFact       = "fact"
	KindPreference = "preference"
	KindEpisode    = "episode"

	StatusActive     = "active"
	StatusSuperseded = "superseded"
	StatusConflict   = "conflict"
	StatusDeleted    = "deleted"

	EventUpsert    = "upsert"
	EventSupersede = "supersede"
	EventConflict  = "conflict"
	EventDelete    = "delete"
)

// Memory is the canonical user-scoped record. Search indexes are derived from
// this record and must not be treated as the source of truth.
type Memory struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	Kind             string    `json:"kind"`
	Subject          string    `json:"subject,omitempty"`
	Content          string    `json:"content"`
	Status           string    `json:"status"`
	SourceRunID      string    `json:"source_run_id,omitempty"`
	SourceSessionID  string    `json:"source_session_id,omitempty"`
	Evidence         string    `json:"evidence,omitempty"`
	Confidence       float64   `json:"confidence"`
	Importance       float64   `json:"importance"`
	ValidFrom        time.Time `json:"valid_from,omitempty"`
	ExpiresAt        time.Time `json:"expires_at,omitempty"`
	SupersedesID     string    `json:"supersedes_id,omitempty"`
	IndexStatus      string    `json:"index_status,omitempty"`
	EmbeddingModel   string    `json:"embedding_model,omitempty"`
	EmbeddingVersion string    `json:"embedding_version,omitempty"`
	EmbeddingDim     int       `json:"embedding_dim,omitempty"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Query struct {
	UserID           string
	Text             string
	Limit            int
	IncludeConflicts bool
}

type Event struct {
	EventID   string    `json:"event_id"`
	MemoryID  string    `json:"memory_id"`
	UserID    string    `json:"user_id"`
	Type      string    `json:"type"`
	Memory    Memory    `json:"memory"`
	Reason    string    `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type Store interface {
	Search(ctx context.Context, query Query) ([]Memory, error)
	Upsert(ctx context.Context, memory Memory) error
	Get(ctx context.Context, userID, id string) (Memory, error)
	List(ctx context.Context, userID string) ([]Memory, error)
	Delete(ctx context.Context, userID, id, reason string) error
	Rebuild(ctx context.Context, userID string) error
}

// IndexStateStore is an optional extension implemented by stores that persist
// the status of a derived embedding index without changing memory content.
type IndexStateStore interface {
	MarkIndexed(ctx context.Context, userID, memoryID, status, model, version string, dimension int) error
}
