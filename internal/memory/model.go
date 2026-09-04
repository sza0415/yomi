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

	ChangeHintReplace = "replace"
	ChangeHintCoexist = "coexist"
	ChangeHintUnknown = "unknown"

	ProposalStatusPending   = "pending"
	ProposalStatusApplied   = "applied"
	ProposalStatusRejected  = "rejected"
	ProposalStatusTimedOut  = "timed_out"
	ProposalStatusCancelled = "cancelled"
	ProposalStatusStale     = "stale"
	ProposalStatusFailed    = "failed"
)

// Memory 是按用户隔离的权威记录。搜索索引由该记录派生，不能被当作事实来源。
type Memory struct {
	ID               string    `json:"id"`
	UserID           string    `json:"user_id"`
	Kind             string    `json:"kind"`
	Subject          string    `json:"subject,omitempty"`
	Attribute        string    `json:"attribute,omitempty"`
	Value            string    `json:"value,omitempty"`
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
	// Kinds 可以把检索范围限制在指定的记忆类型集合中。
	// 空列表保持原有行为，即检索所有类型。
	Kinds []string
}

type BrowseLevel string

const (
	BrowseKinds      BrowseLevel = "kinds"
	BrowseSubjects   BrowseLevel = "subjects"
	BrowseAttributes BrowseLevel = "attributes"
	BrowseMemories   BrowseLevel = "memories"
)

// BrowseQuery describes one step through the logical
// kind -> subject -> attribute -> memory hierarchy. UserID is supplied by the
// host, never by model-generated tool arguments.
type BrowseQuery struct {
	UserID           string
	Level            BrowseLevel
	Kind             string
	Subject          string
	Attribute        string
	IncludeConflicts bool
	Limit            int
}

type BrowseEntry struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type BrowseResult struct {
	Level    BrowseLevel   `json:"level"`
	Entries  []BrowseEntry `json:"entries,omitempty"`
	Memories []Memory      `json:"memories,omitempty"`
}

// CatalogEntry is the compact L0 directory injected into model context. It
// deliberately carries no memory values or evidence.
type CatalogEntry struct {
	Kind      string `json:"kind"`
	Subject   string `json:"subject"`
	Attribute string `json:"attribute"`
	Count     int    `json:"count"`
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

// Mutation 把一个候选记忆及其关联状态转换作为一笔权威事务统一应用。
// 只有该事务提交后，才会更新派生的向量索引。
type Mutation struct {
	Memory       Memory
	SupersedeIDs []string
	ConflictIDs  []string
	Reason       string
	ProposalID   string
}

type Store interface {
	Search(ctx context.Context, query Query) ([]Memory, error)
	Upsert(ctx context.Context, memory Memory) error
	Get(ctx context.Context, userID, id string) (Memory, error)
	List(ctx context.Context, userID string) ([]Memory, error)
	Delete(ctx context.Context, userID, id, reason string) error
	Rebuild(ctx context.Context, userID string) error
}

// IndexStateStore 是可选扩展，由需要持久化派生嵌入索引状态的存储实现，
// 更新索引状态时不会修改记忆正文。
type IndexStateStore interface {
	MarkIndexed(ctx context.Context, userID, memoryID, status, model, version string, dimension int) error
}

// RelatedStore 是供冲突解析器使用的可选扩展，用于查找占用同一结构化
// 主体/属性位置的记忆。
type RelatedStore interface {
	FindRelated(ctx context.Context, userID, kind, subject, attribute string) ([]Memory, error)
}

// Browser exposes bounded, user-scoped hierarchy navigation without exposing
// SQL or allowing the caller to choose a different user's scope.
type Browser interface {
	Browse(ctx context.Context, query BrowseQuery) (BrowseResult, error)
	Catalog(ctx context.Context, userID string, includeConflicts bool) ([]CatalogEntry, error)
}

// MutationStore 是可选扩展，可以原子地应用候选记忆及其关联状态转换。
type MutationStore interface {
	ApplyMutation(ctx context.Context, mutation Mutation) error
}

type ProposalStore interface {
	CreateProposal(ctx context.Context, proposal ProposalRecord) (ProposalRecord, error)
	CompleteProposal(ctx context.Context, userID, proposalID, status string) error
	ListPendingProposals(ctx context.Context) ([]ProposalRecord, error)
}

type SearchStats struct {
	LexicalCount      int    `json:"lexical_count"`
	SemanticCount     int    `json:"semantic_count"`
	FusedCount        int    `json:"fused_count"`
	SemanticAttempted bool   `json:"semantic_attempted"`
	SemanticFallback  bool   `json:"semantic_fallback"`
	SemanticError     string `json:"semantic_error,omitempty"`
	RerankAttempted   bool   `json:"rerank_attempted"`
	RerankFallback    bool   `json:"rerank_fallback"`
	RerankError       string `json:"rerank_error,omitempty"`
}

type SearchResult struct {
	Memories []Memory
	Stats    SearchStats
}

// DetailedStore 是用于分层检索和细粒度可观测性的可选只读扩展。
// Store.Search 仍作为兼容接口保留。
type DetailedStore interface {
	SearchDetailed(ctx context.Context, query Query) (SearchResult, error)
}
