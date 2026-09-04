package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const defaultSearchLimit = 8

type SQLiteStore struct {
	db *sql.DB
}

func NewSQLiteStore(path string) (*SQLiteStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("memory: sqlite path is empty")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return nil, fmt.Errorf("memory: create sqlite directory: %w", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		return nil, fmt.Errorf("memory: secure sqlite directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("memory: open sqlite: %w", err)
	}
	store := &SQLiteStore{db: db}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("memory: secure sqlite file: %w", err)
	}
	return store, nil
}

func (s *SQLiteStore) init() error {
	for _, statement := range []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA foreign_keys=ON`,
		`CREATE TABLE IF NOT EXISTS memories (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			subject TEXT NOT NULL DEFAULT '',
			attribute TEXT NOT NULL DEFAULT '',
			value TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			status TEXT NOT NULL,
			source_run_id TEXT NOT NULL DEFAULT '',
			source_session_id TEXT NOT NULL DEFAULT '',
			evidence TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0,
			importance REAL NOT NULL DEFAULT 0,
			valid_from TEXT,
			expires_at TEXT,
			supersedes_id TEXT NOT NULL DEFAULT '',
			index_status TEXT NOT NULL DEFAULT 'pending',
			embedding_model TEXT NOT NULL DEFAULT '',
			embedding_version TEXT NOT NULL DEFAULT '',
			embedding_dim INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS memories_user_status_idx ON memories(user_id, status, updated_at)`,
		`CREATE TABLE IF NOT EXISTS memory_events (
			event_id TEXT PRIMARY KEY,
			memory_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			type TEXT NOT NULL,
			payload TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS memory_events_memory_idx ON memory_events(user_id, memory_id, created_at)`,
		`CREATE TABLE IF NOT EXISTS memory_proposals (
			proposal_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL,
			source_session_id TEXT NOT NULL,
			source_run_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			operation TEXT NOT NULL,
			candidate_json TEXT NOT NULL,
			target_ids_json TEXT NOT NULL,
			target_versions_json TEXT NOT NULL,
			reason TEXT NOT NULL,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			decided_at TEXT
		)`,
		`CREATE INDEX IF NOT EXISTS memory_proposals_status_idx ON memory_proposals(status, expires_at)`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(
			memory_id UNINDEXED,
			user_id UNINDEXED,
			content,
			subject
		)`,
	} {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("memory: initialize sqlite: %w", err)
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "attribute", definition: `TEXT NOT NULL DEFAULT ''`},
		{name: "value", definition: `TEXT NOT NULL DEFAULT ''`},
	} {
		if err := s.ensureMemoryColumn(column.name, column.definition); err != nil {
			return err
		}
	}
	return nil
}

func (s *SQLiteStore) ensureMemoryColumn(name, definition string) error {
	rows, err := s.db.Query(`PRAGMA table_info(memories)`)
	if err != nil {
		return fmt.Errorf("memory: inspect sqlite schema: %w", err)
	}
	found := false
	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return fmt.Errorf("memory: scan sqlite schema: %w", err)
		}
		if columnName == name {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("memory: close sqlite schema rows: %w", err)
	}
	if found {
		return nil
	}
	if _, err := s.db.Exec(`ALTER TABLE memories ADD COLUMN ` + name + ` ` + definition); err != nil {
		return fmt.Errorf("memory: add sqlite column %s: %w", name, err)
	}
	return nil
}

func (s *SQLiteStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// ClearAll 永久删除所有权威记忆、事件以及全文搜索（FTS）记录。
func (s *SQLiteStore) ClearAll(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: begin clear: %w", err)
	}
	defer tx.Rollback()
	for _, statement := range []string{"DELETE FROM memory_events", "DELETE FROM memory_proposals", "DELETE FROM memories", "DELETE FROM memory_fts"} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("memory: clear data: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit clear: %w", err)
	}
	return nil
}

func (s *SQLiteStore) Upsert(ctx context.Context, item Memory) error {
	return s.ApplyMutation(ctx, Mutation{Memory: item})
}

func (s *SQLiteStore) ApplyMutation(ctx context.Context, mutation Mutation) error {
	item := mutation.Memory
	if err := validateMemory(item); err != nil {
		return err
	}
	if item.ID == "" {
		item.ID = NewID("mem")
	}
	if item.Status == "" {
		item.Status = StatusActive
	}
	if item.Kind == "" {
		item.Kind = KindFact
	}
	if item.IndexStatus == "" {
		item.IndexStatus = "pending"
	}
	now := time.Now().UTC()
	if item.CreatedAt.IsZero() {
		item.CreatedAt = now
	}
	item.UpdatedAt = now

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: begin mutation: %w", err)
	}
	defer tx.Rollback()
	if mutation.ProposalID != "" {
		if err := validatePendingProposalTx(ctx, tx, item, mutation.ProposalID, mutation.SupersedeIDs); err != nil {
			return err
		}
	}
	for _, id := range uniqueIDs(mutation.SupersedeIDs, item.ID) {
		if err := transitionMemory(ctx, tx, item.UserID, id, StatusSuperseded, EventSupersede, mutation.Reason, true, now); err != nil {
			return err
		}
	}
	for _, id := range uniqueIDs(mutation.ConflictIDs, item.ID) {
		if err := transitionMemory(ctx, tx, item.UserID, id, StatusConflict, EventConflict, mutation.Reason, false, now); err != nil {
			return err
		}
	}
	if len(mutation.ConflictIDs) > 0 {
		item.Status = StatusConflict
	}
	var existingUser string
	err = tx.QueryRowContext(ctx, `SELECT user_id FROM memories WHERE id = ?`, item.ID).Scan(&existingUser)
	if err == nil && existingUser != item.UserID {
		return fmt.Errorf("memory: id %q belongs to another user", item.ID)
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("memory: check existing memory: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO memories (
			id, user_id, kind, subject, attribute, value, content, status, source_run_id,
			source_session_id, evidence, confidence, importance, valid_from,
			expires_at, supersedes_id, index_status, embedding_model,
			embedding_version, embedding_dim, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			kind=excluded.kind, subject=excluded.subject, attribute=excluded.attribute,
			value=excluded.value, content=excluded.content,
			status=excluded.status, source_run_id=excluded.source_run_id,
			source_session_id=excluded.source_session_id, evidence=excluded.evidence,
			confidence=excluded.confidence, importance=excluded.importance,
			valid_from=excluded.valid_from, expires_at=excluded.expires_at,
			supersedes_id=excluded.supersedes_id, index_status=excluded.index_status,
			embedding_model=excluded.embedding_model, embedding_version=excluded.embedding_version,
			embedding_dim=excluded.embedding_dim, updated_at=excluded.updated_at
	`, memoryArgs(item)...); err != nil {
		return fmt.Errorf("memory: upsert record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, item.ID); err != nil {
		return fmt.Errorf("memory: replace fts record: %w", err)
	}
	if item.Status != StatusDeleted {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_fts(memory_id, user_id, content, subject) VALUES (?, ?, ?, ?)`, item.ID, item.UserID, item.Content, item.Subject); err != nil {
			return fmt.Errorf("memory: index fts record: %w", err)
		}
	}
	eventType := EventUpsert
	if item.Status == StatusConflict {
		eventType = EventConflict
	}
	if err := appendEvent(ctx, tx, Event{EventID: NewID("evt"), MemoryID: item.ID, UserID: item.UserID, Type: eventType, Memory: item, Reason: mutation.Reason, CreatedAt: now}); err != nil {
		return err
	}
	if mutation.ProposalID != "" {
		if err := completeProposalTx(ctx, tx, item.UserID, mutation.ProposalID, ProposalStatusApplied, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit mutation: %w", err)
	}
	return nil
}

func uniqueIDs(ids []string, exclude string) []string {
	seen := make(map[string]struct{}, len(ids))
	result := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || id == exclude {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	return result
}

func transitionMemory(ctx context.Context, tx *sql.Tx, userID, id, status, eventType, reason string, removeFTS bool, now time.Time) error {
	row := tx.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? AND m.id = ?`, memoryColumns("m")), userID, id)
	item, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("memory: related memory not found: %s", id)
	}
	if err != nil {
		return fmt.Errorf("memory: load related memory %s: %w", id, err)
	}
	if item.Status == status {
		return nil
	}
	item.Status = status
	item.IndexStatus = "pending"
	item.UpdatedAt = now
	if _, err := tx.ExecContext(ctx, `UPDATE memories SET status = ?, index_status = ?, updated_at = ? WHERE id = ? AND user_id = ?`, status, item.IndexStatus, now.Format(time.RFC3339Nano), id, userID); err != nil {
		return fmt.Errorf("memory: transition related memory %s: %w", id, err)
	}
	if removeFTS {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, id); err != nil {
			return fmt.Errorf("memory: remove related fts record %s: %w", id, err)
		}
	}
	return appendEvent(ctx, tx, Event{EventID: NewID("evt"), MemoryID: id, UserID: userID, Type: eventType, Memory: item, Reason: reason, CreatedAt: now})
}

func (s *SQLiteStore) Search(ctx context.Context, query Query) ([]Memory, error) {
	if strings.TrimSpace(query.UserID) == "" {
		return nil, errors.New("memory: search user id is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = defaultSearchLimit
	}
	if limit > 100 {
		limit = 100
	}
	statusClause := `m.status = 'active'`
	if query.IncludeConflicts {
		statusClause = `m.status IN ('active', 'conflict')`
	}
	kindClause, kindArgs := kindFilter(query.Kinds)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	text := strings.TrimSpace(query.Text)
	var rows *sql.Rows
	var err error
	if text == "" {
		args := []any{query.UserID}
		args = append(args, kindArgs...)
		args = append(args, now, limit)
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? %s AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) ORDER BY m.importance DESC, m.updated_at DESC LIMIT ?`, memoryColumns("m"), kindClause, statusClause), args...)
	} else if fts := ftsQuery(text); fts != "" {
		args := []any{fts, query.UserID}
		args = append(args, kindArgs...)
		args = append(args, now, limit)
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM memory_fts f JOIN memories m ON m.id = f.memory_id WHERE memory_fts MATCH ? AND m.user_id = ? %s AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) ORDER BY bm25(memory_fts), m.importance DESC, m.updated_at DESC LIMIT ?`, memoryColumns("m"), kindClause, statusClause), args...)
	}
	var items []Memory
	if err == nil && rows != nil {
		items, err = scanMemories(rows)
		if err != nil {
			return nil, err
		}
	}
	if len(items) > 0 || text == "" {
		return items, nil
	}
	// 在 FTS 分词之外有意补充子字符串回退检索，确保短 ID 和不使用空格分词的
	// 语言内容仍然可以被搜索到。
	args := []any{query.UserID}
	args = append(args, kindArgs...)
	args = append(args, now, "%"+text+"%", "%"+text+"%", limit)
	rows, err = s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? %s AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) AND (m.content LIKE ? OR m.subject LIKE ?) ORDER BY m.importance DESC, m.updated_at DESC LIMIT ?`, memoryColumns("m"), kindClause, statusClause), args...)
	if err != nil {
		return nil, fmt.Errorf("memory: fallback search: %w", err)
	}
	items, err = scanMemories(rows)
	if err != nil {
		return nil, err
	}
	return items, nil
}

func kindFilter(kinds []string) (string, []any) {
	seen := make(map[string]struct{}, len(kinds))
	values := make([]any, 0, len(kinds))
	for _, kind := range kinds {
		kind = strings.TrimSpace(kind)
		if kind == "" {
			continue
		}
		if _, ok := seen[kind]; ok {
			continue
		}
		seen[kind] = struct{}{}
		values = append(values, kind)
	}
	if len(values) == 0 {
		return "", nil
	}
	placeholders := make([]string, len(values))
	for i := range placeholders {
		placeholders[i] = "?"
	}
	return "AND m.kind IN (" + strings.Join(placeholders, ",") + ")", values
}

func (s *SQLiteStore) Get(ctx context.Context, userID, id string) (Memory, error) {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(id) == "" {
		return Memory{}, errors.New("memory: user id and memory id are required")
	}
	row := s.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? AND m.id = ?`, memoryColumns("m")), userID, id)
	item, err := scanMemory(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Memory{}, fmt.Errorf("memory: not found: %s", id)
	}
	return item, err
}

func (s *SQLiteStore) FindRelated(ctx context.Context, userID, kind, subject, attribute string) ([]Memory, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("memory: related search user id is required")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? AND m.kind = ? AND lower(trim(m.subject)) = lower(trim(?)) AND lower(trim(m.attribute)) = lower(trim(?)) AND m.status IN ('active', 'conflict') AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) ORDER BY m.updated_at DESC`, memoryColumns("m")), userID, kind, subject, attribute, now)
	if err != nil {
		return nil, fmt.Errorf("memory: search related memories: %w", err)
	}
	return scanMemories(rows)
}

func (s *SQLiteStore) Browse(ctx context.Context, query BrowseQuery) (BrowseResult, error) {
	if strings.TrimSpace(query.UserID) == "" {
		return BrowseResult{}, errors.New("memory: browse user id is required")
	}
	limit := query.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	statusClause := `m.status = 'active'`
	if query.IncludeConflicts {
		statusClause = `m.status IN ('active', 'conflict')`
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result := BrowseResult{Level: query.Level}

	switch query.Level {
	case BrowseKinds:
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT m.kind, COUNT(*) FROM memories m WHERE m.user_id = ? AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) GROUP BY m.kind ORDER BY m.kind LIMIT ?`, statusClause), query.UserID, now, limit)
		if err != nil {
			return BrowseResult{}, fmt.Errorf("memory: browse kinds: %w", err)
		}
		entries, err := scanBrowseEntries(rows)
		result.Entries = entries
		return result, err
	case BrowseSubjects:
		if !validKind(query.Kind) {
			return BrowseResult{}, fmt.Errorf("memory: browse subjects requires kind fact, preference, or episode")
		}
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT m.subject, COUNT(*) FROM memories m WHERE m.user_id = ? AND m.kind = ? AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) GROUP BY lower(trim(m.subject)) ORDER BY m.subject LIMIT ?`, statusClause), query.UserID, query.Kind, now, limit)
		if err != nil {
			return BrowseResult{}, fmt.Errorf("memory: browse subjects: %w", err)
		}
		entries, err := scanBrowseEntries(rows)
		result.Entries = entries
		return result, err
	case BrowseAttributes:
		if !validKind(query.Kind) {
			return BrowseResult{}, fmt.Errorf("memory: browse attributes requires kind fact, preference, or episode")
		}
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT m.attribute, COUNT(*) FROM memories m WHERE m.user_id = ? AND m.kind = ? AND lower(trim(m.subject)) = lower(trim(?)) AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) GROUP BY lower(trim(m.attribute)) ORDER BY m.attribute LIMIT ?`, statusClause), query.UserID, query.Kind, query.Subject, now, limit)
		if err != nil {
			return BrowseResult{}, fmt.Errorf("memory: browse attributes: %w", err)
		}
		entries, err := scanBrowseEntries(rows)
		result.Entries = entries
		return result, err
	case BrowseMemories:
		if !validKind(query.Kind) {
			return BrowseResult{}, fmt.Errorf("memory: browse memories requires kind fact, preference, or episode")
		}
		rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? AND m.kind = ? AND lower(trim(m.subject)) = lower(trim(?)) AND lower(trim(m.attribute)) = lower(trim(?)) AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) ORDER BY m.importance DESC, m.updated_at DESC LIMIT ?`, memoryColumns("m"), statusClause), query.UserID, query.Kind, query.Subject, query.Attribute, now, limit)
		if err != nil {
			return BrowseResult{}, fmt.Errorf("memory: browse records: %w", err)
		}
		items, err := scanMemories(rows)
		result.Memories = items
		return result, err
	default:
		return BrowseResult{}, fmt.Errorf("memory: unsupported browse level %q", query.Level)
	}
}

func (s *SQLiteStore) Catalog(ctx context.Context, userID string, includeConflicts bool) ([]CatalogEntry, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, errors.New("memory: catalog user id is required")
	}
	statusClause := `m.status = 'active'`
	if includeConflicts {
		statusClause = `m.status IN ('active', 'conflict')`
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows, err := s.db.QueryContext(ctx, fmt.Sprintf(`SELECT m.kind, m.subject, m.attribute, COUNT(*) FROM memories m WHERE m.user_id = ? AND %s AND (m.expires_at IS NULL OR m.expires_at = '' OR m.expires_at > ?) GROUP BY m.kind, lower(trim(m.subject)), lower(trim(m.attribute)) ORDER BY m.kind, m.subject, m.attribute LIMIT 200`, statusClause), userID, now)
	if err != nil {
		return nil, fmt.Errorf("memory: build catalog: %w", err)
	}
	defer rows.Close()
	var entries []CatalogEntry
	for rows.Next() {
		var entry CatalogEntry
		if err := rows.Scan(&entry.Kind, &entry.Subject, &entry.Attribute, &entry.Count); err != nil {
			return nil, fmt.Errorf("memory: scan catalog: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate catalog: %w", err)
	}
	return entries, nil
}

func scanBrowseEntries(rows *sql.Rows) ([]BrowseEntry, error) {
	defer rows.Close()
	var entries []BrowseEntry
	for rows.Next() {
		var entry BrowseEntry
		if err := rows.Scan(&entry.Name, &entry.Count); err != nil {
			return nil, fmt.Errorf("memory: scan browse entry: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate browse entries: %w", err)
	}
	return entries, nil
}

func validKind(kind string) bool {
	kind = strings.TrimSpace(kind)
	return kind == KindFact || kind == KindPreference || kind == KindEpisode
}

func (s *SQLiteStore) List(ctx context.Context, userID string) ([]Memory, error) {
	return s.Search(ctx, Query{UserID: userID, Limit: 100})
}

func (s *SQLiteStore) CreateProposal(ctx context.Context, proposal ProposalRecord) (ProposalRecord, error) {
	if strings.TrimSpace(proposal.UserID) == "" || strings.TrimSpace(proposal.SourceSessionID) == "" || strings.TrimSpace(proposal.SourceRunID) == "" {
		return ProposalRecord{}, errors.New("memory: proposal user, session, and run are required")
	}
	if err := validateMemory(Memory{UserID: proposal.UserID, Kind: proposal.Candidate.Kind, Content: proposal.Candidate.Content}); err != nil {
		return ProposalRecord{}, err
	}
	if len(proposal.TargetIDs) == 0 {
		return ProposalRecord{}, errors.New("memory: proposal target ids are required")
	}
	if proposal.ID == "" {
		proposal.ID = NewID("proposal")
	}
	if proposal.Status == "" {
		proposal.Status = ProposalStatusPending
	}
	if proposal.Status != ProposalStatusPending {
		return ProposalRecord{}, fmt.Errorf("memory: new proposal status must be pending, got %q", proposal.Status)
	}
	if proposal.Operation != ProposalNeedsConfirmation {
		return ProposalRecord{}, fmt.Errorf("memory: persisted proposal operation must be %q, got %q", ProposalNeedsConfirmation, proposal.Operation)
	}
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = time.Now().UTC()
	}
	if proposal.ExpiresAt.IsZero() || !proposal.ExpiresAt.After(proposal.CreatedAt) {
		return ProposalRecord{}, errors.New("memory: proposal expiry must be after creation")
	}
	targetIDs := uniqueIDs(proposal.TargetIDs, "")
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: begin proposal creation: %w", err)
	}
	defer tx.Rollback()
	proposal.TargetIDs = targetIDs
	proposal.TargetVersions = make(map[string]time.Time, len(targetIDs))
	now := time.Now().UTC()
	for _, targetID := range targetIDs {
		var status, kind, updatedAt string
		var expiresAt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT status, kind, updated_at, expires_at FROM memories WHERE id = ? AND user_id = ?`, targetID, proposal.UserID).Scan(&status, &kind, &updatedAt, &expiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ProposalRecord{}, fmt.Errorf("memory: proposal target not found: %s", targetID)
		}
		if err != nil {
			return ProposalRecord{}, fmt.Errorf("memory: load proposal target %s: %w", targetID, err)
		}
		if status != StatusActive && status != StatusConflict {
			return ProposalRecord{}, fmt.Errorf("memory: proposal target %s is %s", targetID, status)
		}
		if kind != proposal.Candidate.Kind {
			return ProposalRecord{}, fmt.Errorf("memory: proposal target %s has kind %q, candidate has kind %q", targetID, kind, proposal.Candidate.Kind)
		}
		expiry, err := parseTime(expiresAt)
		if err != nil {
			return ProposalRecord{}, fmt.Errorf("memory: parse proposal target %s expiry: %w", targetID, err)
		}
		if !expiry.IsZero() && !expiry.After(now) {
			return ProposalRecord{}, fmt.Errorf("memory: proposal target %s has expired", targetID)
		}
		parsed, err := parseTime(sql.NullString{String: updatedAt, Valid: updatedAt != ""})
		if err != nil {
			return ProposalRecord{}, fmt.Errorf("memory: parse proposal target %s version: %w", targetID, err)
		}
		proposal.TargetVersions[targetID] = parsed
	}
	candidateJSON, err := json.Marshal(proposal.Candidate)
	if err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: encode proposal candidate: %w", err)
	}
	targetIDsJSON, err := json.Marshal(targetIDs)
	if err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: encode proposal target ids: %w", err)
	}
	targetVersionsJSON, err := json.Marshal(proposal.TargetVersions)
	if err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: encode proposal target versions: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_proposals (
		proposal_id, user_id, source_session_id, source_run_id, channel_id,
		operation, candidate_json, target_ids_json, target_versions_json,
		reason, status, created_at, expires_at, decided_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		proposal.ID, proposal.UserID, proposal.SourceSessionID, proposal.SourceRunID, proposal.ChannelID,
		proposal.Operation, string(candidateJSON), string(targetIDsJSON), string(targetVersionsJSON),
		proposal.Reason, proposal.Status, proposal.CreatedAt.UTC().Format(time.RFC3339Nano), proposal.ExpiresAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: create proposal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: commit proposal creation: %w", err)
	}
	return proposal, nil
}

func (s *SQLiteStore) CompleteProposal(ctx context.Context, userID, proposalID, status string) error {
	if !terminalProposalStatus(status) {
		return fmt.Errorf("memory: invalid terminal proposal status %q", status)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: begin proposal completion: %w", err)
	}
	defer tx.Rollback()
	if err := completeProposalTx(ctx, tx, userID, proposalID, status, time.Now().UTC()); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit proposal completion: %w", err)
	}
	return nil
}

func (s *SQLiteStore) ListPendingProposals(ctx context.Context) ([]ProposalRecord, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT proposal_id, user_id, source_session_id, source_run_id, channel_id, operation, candidate_json, target_ids_json, target_versions_json, reason, status, created_at, expires_at, decided_at FROM memory_proposals WHERE status = ? ORDER BY created_at`, ProposalStatusPending)
	if err != nil {
		return nil, fmt.Errorf("memory: list pending proposals: %w", err)
	}
	defer rows.Close()
	var proposals []ProposalRecord
	for rows.Next() {
		proposal, err := scanProposal(rows)
		if err != nil {
			return nil, err
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: iterate pending proposals: %w", err)
	}
	return proposals, nil
}

func validatePendingProposalTx(ctx context.Context, tx *sql.Tx, item Memory, proposalID string, supersedeIDs []string) error {
	var status, operation, candidateJSON, targetIDsJSON, targetVersionsJSON, sourceRunID, sourceSessionID, expiresAt string
	if err := tx.QueryRowContext(ctx, `SELECT status, operation, candidate_json, target_ids_json, target_versions_json, source_run_id, source_session_id, expires_at FROM memory_proposals WHERE proposal_id = ? AND user_id = ?`, proposalID, item.UserID).Scan(&status, &operation, &candidateJSON, &targetIDsJSON, &targetVersionsJSON, &sourceRunID, &sourceSessionID, &expiresAt); errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("memory: proposal not found: %s", proposalID)
	} else if err != nil {
		return fmt.Errorf("memory: load proposal %s: %w", proposalID, err)
	}
	if status != ProposalStatusPending {
		return fmt.Errorf("memory: proposal %s is %s", proposalID, status)
	}
	if ProposalOperation(operation) != ProposalNeedsConfirmation {
		return fmt.Errorf("memory: proposal %s has unsupported operation %q", proposalID, operation)
	}
	deadline, err := parseTime(sql.NullString{String: expiresAt, Valid: expiresAt != ""})
	if err != nil {
		return fmt.Errorf("memory: parse proposal %s expiry: %w", proposalID, err)
	}
	validationTime := time.Now().UTC()
	if !deadline.After(validationTime) {
		return fmt.Errorf("memory: proposal %s has expired", proposalID)
	}
	if item.SourceRunID != sourceRunID || item.SourceSessionID != sourceSessionID {
		return fmt.Errorf("memory: proposal %s source identity changed", proposalID)
	}
	var candidate Candidate
	if err := json.Unmarshal([]byte(candidateJSON), &candidate); err != nil {
		return fmt.Errorf("memory: decode proposal %s candidate: %w", proposalID, err)
	}
	if item.Kind != candidate.Kind || item.Subject != candidate.Subject || item.Attribute != candidate.Attribute || item.Value != candidate.Value || item.Content != candidate.Content || item.Evidence != candidate.Evidence || item.Confidence != candidate.Confidence || item.Importance != candidate.Importance || !item.ExpiresAt.Equal(candidate.ExpiresAt) || (!candidate.ValidFrom.IsZero() && !item.ValidFrom.Equal(candidate.ValidFrom)) {
		return fmt.Errorf("memory: proposal %s candidate changed", proposalID)
	}
	var targetIDs []string
	if err := json.Unmarshal([]byte(targetIDsJSON), &targetIDs); err != nil {
		return fmt.Errorf("memory: decode proposal %s targets: %w", proposalID, err)
	}
	if !sameIDSet(targetIDs, supersedeIDs) {
		return fmt.Errorf("memory: proposal %s target set changed", proposalID)
	}
	var targetVersions map[string]time.Time
	if err := json.Unmarshal([]byte(targetVersionsJSON), &targetVersions); err != nil {
		return fmt.Errorf("memory: decode proposal %s target versions: %w", proposalID, err)
	}
	if len(targetVersions) != len(targetIDs) {
		return fmt.Errorf("memory: proposal %s target versions are incomplete", proposalID)
	}
	for _, targetID := range targetIDs {
		var targetStatus, kind, subject, attribute, updatedAt string
		var targetExpiresAt sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT status, kind, subject, attribute, updated_at, expires_at FROM memories WHERE id = ? AND user_id = ?`, targetID, item.UserID).Scan(&targetStatus, &kind, &subject, &attribute, &updatedAt, &targetExpiresAt)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("memory: proposal %s target disappeared: %s", proposalID, targetID)
		}
		if err != nil {
			return fmt.Errorf("memory: reload proposal %s target %s: %w", proposalID, targetID, err)
		}
		if targetStatus != StatusActive && targetStatus != StatusConflict {
			return fmt.Errorf("memory: proposal %s target %s is %s", proposalID, targetID, targetStatus)
		}
		targetExpiry, err := parseTime(targetExpiresAt)
		if err != nil {
			return fmt.Errorf("memory: parse proposal %s target %s expiry: %w", proposalID, targetID, err)
		}
		if !targetExpiry.IsZero() && !targetExpiry.After(validationTime) {
			return fmt.Errorf("memory: proposal %s target %s has expired", proposalID, targetID)
		}
		currentVersion, err := parseTime(sql.NullString{String: updatedAt, Valid: updatedAt != ""})
		if err != nil {
			return fmt.Errorf("memory: parse proposal %s target %s version: %w", proposalID, targetID, err)
		}
		if expected, ok := targetVersions[targetID]; !ok || !currentVersion.Equal(expected) {
			return fmt.Errorf("memory: proposal %s target %s changed", proposalID, targetID)
		}
		rows, err := tx.QueryContext(ctx, `SELECT id FROM memories WHERE user_id = ? AND kind = ? AND lower(trim(subject)) = lower(trim(?)) AND lower(trim(attribute)) = lower(trim(?)) AND status IN ('active', 'conflict') AND (expires_at IS NULL OR expires_at = '' OR expires_at > ?)`, item.UserID, kind, subject, attribute, validationTime.Format(time.RFC3339Nano))
		if err != nil {
			return fmt.Errorf("memory: inspect proposal %s target siblings: %w", proposalID, err)
		}
		for rows.Next() {
			var siblingID string
			if err := rows.Scan(&siblingID); err != nil {
				rows.Close()
				return fmt.Errorf("memory: scan proposal %s target sibling: %w", proposalID, err)
			}
			if !containsID(targetIDs, siblingID) {
				rows.Close()
				return fmt.Errorf("memory: proposal %s target hierarchy changed", proposalID)
			}
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("memory: close proposal %s target siblings: %w", proposalID, err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("memory: iterate proposal %s target siblings: %w", proposalID, err)
		}
	}
	return nil
}

func containsID(ids []string, target string) bool {
	for _, id := range ids {
		if id == target {
			return true
		}
	}
	return false
}

func sameIDSet(left, right []string) bool {
	left = uniqueIDs(left, "")
	right = uniqueIDs(right, "")
	if len(left) != len(right) {
		return false
	}
	seen := make(map[string]struct{}, len(left))
	for _, id := range left {
		seen[id] = struct{}{}
	}
	for _, id := range right {
		if _, ok := seen[id]; !ok {
			return false
		}
	}
	return true
}

func completeProposalTx(ctx context.Context, tx *sql.Tx, userID, proposalID, status string, decidedAt time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE memory_proposals SET status = ?, decided_at = ? WHERE proposal_id = ? AND user_id = ? AND status = ?`, status, decidedAt.UTC().Format(time.RFC3339Nano), proposalID, userID, ProposalStatusPending)
	if err != nil {
		return fmt.Errorf("memory: complete proposal %s: %w", proposalID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("memory: inspect proposal completion %s: %w", proposalID, err)
	}
	if affected != 1 {
		return fmt.Errorf("memory: pending proposal not found: %s", proposalID)
	}
	return nil
}

func terminalProposalStatus(status string) bool {
	switch status {
	case ProposalStatusApplied, ProposalStatusRejected, ProposalStatusTimedOut, ProposalStatusCancelled, ProposalStatusStale, ProposalStatusFailed:
		return true
	default:
		return false
	}
}

type proposalScanner interface{ Scan(dest ...any) error }

func scanProposal(scanner proposalScanner) (ProposalRecord, error) {
	var proposal ProposalRecord
	var operation, candidateJSON, targetIDsJSON, targetVersionsJSON string
	var createdAt, expiresAt string
	var decidedAt sql.NullString
	if err := scanner.Scan(&proposal.ID, &proposal.UserID, &proposal.SourceSessionID, &proposal.SourceRunID, &proposal.ChannelID, &operation, &candidateJSON, &targetIDsJSON, &targetVersionsJSON, &proposal.Reason, &proposal.Status, &createdAt, &expiresAt, &decidedAt); err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: scan proposal: %w", err)
	}
	proposal.Operation = ProposalOperation(operation)
	if err := json.Unmarshal([]byte(candidateJSON), &proposal.Candidate); err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: decode proposal candidate: %w", err)
	}
	if err := json.Unmarshal([]byte(targetIDsJSON), &proposal.TargetIDs); err != nil {
		return ProposalRecord{}, fmt.Errorf("memory: decode proposal targets: %w", err)
	}
	if targetVersionsJSON != "" && targetVersionsJSON != "null" {
		if err := json.Unmarshal([]byte(targetVersionsJSON), &proposal.TargetVersions); err != nil {
			return ProposalRecord{}, fmt.Errorf("memory: decode proposal versions: %w", err)
		}
	}
	var err error
	if proposal.CreatedAt, err = parseTime(sql.NullString{String: createdAt, Valid: createdAt != ""}); err != nil {
		return ProposalRecord{}, err
	}
	if proposal.ExpiresAt, err = parseTime(sql.NullString{String: expiresAt, Valid: expiresAt != ""}); err != nil {
		return ProposalRecord{}, err
	}
	if decidedAt.Valid {
		proposal.DecidedAt, err = parseTime(decidedAt)
		if err != nil {
			return ProposalRecord{}, err
		}
	}
	return proposal, nil
}

func (s *SQLiteStore) Delete(ctx context.Context, userID, id, reason string) error {
	item, err := s.Get(ctx, userID, id)
	if err != nil {
		return err
	}
	item.Status = StatusDeleted
	item.IndexStatus = "pending"
	item.UpdatedAt = time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: begin delete: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE memories SET status = ?, index_status = ?, updated_at = ? WHERE id = ? AND user_id = ?`, StatusDeleted, item.IndexStatus, item.UpdatedAt.Format(time.RFC3339Nano), id, userID); err != nil {
		return fmt.Errorf("memory: delete record: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, id); err != nil {
		return fmt.Errorf("memory: delete fts record: %w", err)
	}
	if err := appendEvent(ctx, tx, Event{EventID: NewID("evt"), MemoryID: id, UserID: userID, Type: EventDelete, Memory: item, Reason: reason, CreatedAt: item.UpdatedAt}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit delete: %w", err)
	}
	return nil
}

func (s *SQLiteStore) MarkIndexed(ctx context.Context, userID, id, status, model, version string, dimension int) error {
	if strings.TrimSpace(userID) == "" || strings.TrimSpace(id) == "" {
		return errors.New("memory: user id and memory id are required")
	}
	if status == "" {
		status = "indexed"
	}
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET index_status = ?, embedding_model = ?, embedding_version = ?, embedding_dim = ?, updated_at = ? WHERE user_id = ? AND id = ?`, status, model, version, dimension, time.Now().UTC().Format(time.RFC3339Nano), userID, id)
	if err != nil {
		return fmt.Errorf("memory: mark index state: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("memory: inspect index state update: %w", err)
	} else if affected == 0 {
		return fmt.Errorf("memory: not found: %s", id)
	}
	return nil
}

func (s *SQLiteStore) Rebuild(ctx context.Context, userID string) error {
	if strings.TrimSpace(userID) == "" {
		return errors.New("memory: rebuild user id is required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("memory: begin rebuild: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE user_id = ?`, userID); err != nil {
		return fmt.Errorf("memory: clear fts records: %w", err)
	}
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`SELECT %s FROM memories m WHERE m.user_id = ? AND m.status IN ('active', 'conflict')`, memoryColumns("m")), userID)
	if err != nil {
		return fmt.Errorf("memory: load records for rebuild: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		item, err := scanMemory(rows)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_fts(memory_id, user_id, content, subject) VALUES (?, ?, ?, ?)`, item.ID, item.UserID, item.Content, item.Subject); err != nil {
			return fmt.Errorf("memory: rebuild fts record: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("memory: rebuild rows: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("memory: commit rebuild: %w", err)
	}
	return nil
}

func validateMemory(item Memory) error {
	if strings.TrimSpace(item.UserID) == "" {
		return errors.New("memory: user id is required")
	}
	if strings.TrimSpace(item.Content) == "" {
		return errors.New("memory: content is required")
	}
	if item.Kind != "" && item.Kind != KindFact && item.Kind != KindPreference && item.Kind != KindEpisode {
		return fmt.Errorf("memory: unsupported kind %q", item.Kind)
	}
	return nil
}

func memoryArgs(item Memory) []any {
	return []any{item.ID, item.UserID, item.Kind, item.Subject, item.Attribute, item.Value, item.Content, item.Status, item.SourceRunID, item.SourceSessionID, item.Evidence, item.Confidence, item.Importance, nullableTime(item.ValidFrom), nullableTime(item.ExpiresAt), item.SupersedesID, item.IndexStatus, item.EmbeddingModel, item.EmbeddingVersion, item.EmbeddingDim, item.CreatedAt.UTC().Format(time.RFC3339Nano), item.UpdatedAt.UTC().Format(time.RFC3339Nano)}
}

func nullableTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func appendEvent(ctx context.Context, tx *sql.Tx, event Event) error {
	payload, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("memory: marshal event: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_events(event_id, memory_id, user_id, type, payload, created_at) VALUES (?, ?, ?, ?, ?, ?)`, event.EventID, event.MemoryID, event.UserID, event.Type, payload, event.CreatedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("memory: append event: %w", err)
	}
	return nil
}

func memoryColumns(alias string) string {
	return alias + `.id, ` + alias + `.user_id, ` + alias + `.kind, ` + alias + `.subject, ` + alias + `.attribute, ` + alias + `.value, ` + alias + `.content, ` + alias + `.status, ` + alias + `.source_run_id, ` + alias + `.source_session_id, ` + alias + `.evidence, ` + alias + `.confidence, ` + alias + `.importance, ` + alias + `.valid_from, ` + alias + `.expires_at, ` + alias + `.supersedes_id, ` + alias + `.index_status, ` + alias + `.embedding_model, ` + alias + `.embedding_version, ` + alias + `.embedding_dim, ` + alias + `.created_at, ` + alias + `.updated_at`
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanMemory(row rowScanner) (Memory, error) {
	var item Memory
	var validFrom, expiresAt, createdAt, updatedAt sql.NullString
	if err := row.Scan(&item.ID, &item.UserID, &item.Kind, &item.Subject, &item.Attribute, &item.Value, &item.Content, &item.Status, &item.SourceRunID, &item.SourceSessionID, &item.Evidence, &item.Confidence, &item.Importance, &validFrom, &expiresAt, &item.SupersedesID, &item.IndexStatus, &item.EmbeddingModel, &item.EmbeddingVersion, &item.EmbeddingDim, &createdAt, &updatedAt); err != nil {
		return Memory{}, err
	}
	var err error
	item.ValidFrom, err = parseTime(validFrom)
	if err != nil {
		return Memory{}, err
	}
	item.ExpiresAt, err = parseTime(expiresAt)
	if err != nil {
		return Memory{}, err
	}
	item.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return Memory{}, err
	}
	item.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return Memory{}, err
	}
	return item, nil
}

func parseTime(value sql.NullString) (time.Time, error) {
	if !value.Valid || strings.TrimSpace(value.String) == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value.String)
	if err != nil {
		return time.Time{}, fmt.Errorf("memory: parse timestamp %q: %w", value.String, err)
	}
	return parsed, nil
}

func scanMemories(rows *sql.Rows) ([]Memory, error) {
	defer rows.Close()
	var items []Memory
	for rows.Next() {
		item, err := scanMemory(rows)
		if err != nil {
			return nil, fmt.Errorf("memory: scan result: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("memory: read results: %w", err)
	}
	return items, nil
}

func ftsQuery(text string) string {
	parts := strings.Fields(text)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.ReplaceAll(part, `"`, `""`)
		if part != "" {
			quoted = append(quoted, `"`+part+`"`)
		}
	}
	return strings.Join(quoted, " OR ")
}

func NewID(prefix string) string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return prefix + "_" + hex.EncodeToString(buf)
}
