package agent

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ziangsun/szabot/internal/providers"
)

const encodedSessionNamePrefix = "yomi-session-b64-"

// SessionStore 按 SessionID 持久化对话历史（M8）。
//
// 存储形态：每个 session 一个 jsonl 文件（<dir>/<sessionID>.jsonl），
// 每行是一条 providers.Message 的 JSON。选择"一 session 一文件 + 追加写"
// 是因为它天然匹配对话"只在末尾增长"的特性：Append 就是往文件尾追加一行，
// 无需读改写整个文件。
//
// 注意：这里只存"对话历史"（user / assistant / tool），不存 system prompt。
// system prompt 在启动时构建、全程不变，由 Loop 在每次请求时恒定拼在最前，
// 既避免把它反复写进磁盘，也保证前缀稳定、对 KV Cache 友好。
type SessionStore struct {
	dir        string
	archiveDir string

	mu    sync.Mutex
	cache map[string][]providers.Message
}

// SessionInfo is the lightweight metadata used by clients to discover
// persisted conversations without downloading their full message history.
type SessionInfo struct {
	ID           string    `json:"id"`
	Title        string    `json:"title"`
	Preview      string    `json:"preview"`
	MessageCount int       `json:"message_count"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type sessionSummary struct {
	Summary       string `json:"summary"`
	CoveredCount  int    `json:"covered_count"`
	UpdatedAtUnix int64  `json:"updated_at_unix"`
}

// ArchiveRecord is one independent compaction checkpoint. Unlike the rolling
// summary, archive records are append-only and preserve task evolution.
type ArchiveRecord struct {
	SchemaVersion int               `json:"schema_version"`
	ID            string            `json:"id"`
	SessionID     string            `json:"session_id"`
	RunID         string            `json:"run_id,omitempty"`
	CoveredFrom   int               `json:"covered_from"`
	CoveredTo     int               `json:"covered_to"`
	MessageCount  int               `json:"message_count"`
	Summary       string            `json:"summary"`
	Sections      map[string]string `json:"sections,omitempty"`
	CreatedAt     time.Time         `json:"created_at"`
}

// NewSessionStore 在 dir 下创建/使用会话目录。dir 不存在会被自动创建。
func NewSessionStore(dir string) (*SessionStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("agent: session store dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create session dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: secure session dir %q: %w", dir, err)
	}
	archiveDir := filepath.Join(filepath.Dir(dir), "archives")
	// Tests and embedders often pass a temporary directory directly instead of
	// the conventional conversations/ directory used by cmd/szabot.
	if filepath.Base(filepath.Clean(dir)) != "conversations" {
		archiveDir = filepath.Join(dir, "archives")
	}
	if err := os.MkdirAll(archiveDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create archive dir %q: %w", archiveDir, err)
	}
	if err := os.Chmod(archiveDir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: secure archive dir %q: %w", archiveDir, err)
	}
	return &SessionStore{
		dir:        dir,
		archiveDir: archiveDir,
		cache:      make(map[string][]providers.Message),
	}, nil
}

// Load 返回某个 session 的完整历史（不含 system prompt）。
// 首次访问时从磁盘读入并缓存；之后走内存缓存，避免每轮都读盘。
// session 不存在时返回空切片（而非错误）——新会话就是一段空历史。
func (s *SessionStore) Load(sessionID string) ([]providers.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if history, ok := s.cache[sessionID]; ok {
		return cloneMessages(history), nil
	}

	history, err := s.readFile(sessionID)
	if err != nil {
		return nil, err
	}
	s.cache[sessionID] = history
	return cloneMessages(history), nil
}

// ListSessions returns persisted conversations, newest first. The method only
// exposes metadata; callers must use Load to fetch the actual messages.
func (s *SessionStore) ListSessions() ([]SessionInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("agent: list sessions: %w", err)
	}
	result := make([]SessionInfo, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		sessionID := decodeSessionName(strings.TrimSuffix(entry.Name(), ".jsonl"))
		if sessionID == "" {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("agent: stat session %q: %w", sessionID, err)
		}
		history, err := s.readFile(sessionID)
		if err != nil {
			return nil, err
		}
		// The JSONL format has no per-message timestamp. File times provide
		// stable enough ordering for the local session picker.
		createdAt := info.ModTime()
		updatedAt := info.ModTime()
		result = append(result, SessionInfo{
			ID:           sessionID,
			Title:        sessionTitle(history, sessionID),
			Preview:      sessionPreview(history),
			MessageCount: len(history),
			CreatedAt:    createdAt,
			UpdatedAt:    updatedAt,
		})
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func sessionTitle(history []providers.Message, fallback string) string {
	for _, msg := range history {
		if msg.Role == providers.RoleUser && strings.TrimSpace(msg.Content) != "" {
			return truncateSessionText(strings.TrimSpace(msg.Content), 42)
		}
	}
	return fallback
}

func sessionPreview(history []providers.Message) string {
	for i := len(history) - 1; i >= 0; i-- {
		msg := history[i]
		if (msg.Role == providers.RoleAssistant || msg.Role == providers.RoleUser) && strings.TrimSpace(msg.Content) != "" {
			return truncateSessionText(strings.TrimSpace(msg.Content), 80)
		}
	}
	return "暂无消息"
}

func truncateSessionText(text string, max int) string {
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max]) + "…"
}

// Append 把若干条消息追加到某个 session（内存缓存 + 磁盘各追加一份）。
// 典型调用：一轮结束后追加 [本轮 user, 本轮 assistant 回复]。
func (s *SessionStore) Append(sessionID string, messages ...providers.Message) error {
	if len(messages) == 0 {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// 确保缓存已从磁盘热身，避免缓存与文件不一致。
	if _, ok := s.cache[sessionID]; !ok {
		history, err := s.readFile(sessionID)
		if err != nil {
			return err
		}
		s.cache[sessionID] = history
	}

	if err := s.appendFile(sessionID, messages); err != nil {
		return err
	}
	s.cache[sessionID] = append(s.cache[sessionID], messages...)
	return nil
}

// LoadSummary returns the last compacted summary for a session.
func (s *SessionStore) LoadSummary(sessionID string) (string, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.summaryPath(sessionID))
	if os.IsNotExist(err) {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, fmt.Errorf("agent: read session summary %q: %w", sessionID, err)
	}
	var summary sessionSummary
	if err := json.Unmarshal(data, &summary); err != nil {
		return "", 0, fmt.Errorf("agent: parse session summary %q: %w", sessionID, err)
	}
	return summary.Summary, summary.CoveredCount, nil
}

// SaveSummary atomically persists compacted summary metadata.
func (s *SessionStore) SaveSummary(sessionID, summary string, coveredCount int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(sessionSummary{Summary: summary, CoveredCount: coveredCount, UpdatedAtUnix: time.Now().Unix()})
	if err != nil {
		return fmt.Errorf("agent: marshal session summary: %w", err)
	}
	tmp := s.summaryPath(sessionID) + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("agent: write session summary: %w", err)
	}
	if err := os.Rename(tmp, s.summaryPath(sessionID)); err != nil {
		return fmt.Errorf("agent: replace session summary: %w", err)
	}
	return nil
}

// AppendArchive appends one immutable compaction checkpoint for a session.
// The original Conversation remains untouched and can still be replayed.
func (s *SessionStore) AppendArchive(sessionID string, archive ArchiveRecord) (string, error) {
	if s == nil || strings.TrimSpace(sessionID) == "" {
		return "", fmt.Errorf("agent: archive session is required")
	}
	if archive.CoveredFrom < 0 || archive.CoveredTo <= archive.CoveredFrom {
		return "", fmt.Errorf("agent: invalid archive coverage %d:%d", archive.CoveredFrom, archive.CoveredTo)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if archive.ID == "" {
		archive.ID = fmt.Sprintf("arc_%d", time.Now().UnixNano())
	}
	archive.SchemaVersion = 1
	archive.SessionID = sessionID
	archive.MessageCount = archive.CoveredTo - archive.CoveredFrom
	if archive.CreatedAt.IsZero() {
		archive.CreatedAt = time.Now().UTC()
	}
	data, err := json.Marshal(archive)
	if err != nil {
		return "", fmt.Errorf("agent: marshal archive: %w", err)
	}
	path := s.archivePath(sessionID)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("agent: open archive %q: %w", sessionID, err)
	}
	defer f.Close()
	if _, err := f.Write(append(data, '\n')); err != nil {
		return "", fmt.Errorf("agent: write archive %q: %w", sessionID, err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("agent: sync archive %q: %w", sessionID, err)
	}
	return archive.ID, nil
}

// LoadArchives returns append-only compaction checkpoints in creation order.
func (s *SessionStore) LoadArchives(sessionID string) ([]ArchiveRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.archivePath(sessionID))
	if os.IsNotExist(err) {
		return []ArchiveRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("agent: open archives %q: %w", sessionID, err)
	}
	defer f.Close()
	var archives []ArchiveRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var archive ArchiveRecord
		if err := json.Unmarshal([]byte(line), &archive); err != nil {
			return nil, fmt.Errorf("agent: parse archive %q: %w", sessionID, err)
		}
		archives = append(archives, archive)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("agent: read archives %q: %w", sessionID, err)
	}
	return archives, nil
}

func (s *SessionStore) path(sessionID string) string {
	return filepath.Join(s.dir, s.safeSessionName(sessionID)+".jsonl")
}

func (s *SessionStore) summaryPath(sessionID string) string {
	return strings.TrimSuffix(s.path(sessionID), ".jsonl") + ".summary.json"
}

func (s *SessionStore) archivePath(sessionID string) string {
	return filepath.Join(s.archiveDir, s.safeSessionName(sessionID)+".jsonl")
}

func (s *SessionStore) safeSessionName(sessionID string) string {
	safe := filepath.Base(filepath.Clean("/" + sessionID))
	if safe == "." || safe == "/" || safe == "" {
		return "default"
	}
	if isPortableFilePart(safe) && !strings.HasPrefix(safe, encodedSessionNamePrefix) {
		return safe
	}
	return encodedSessionNamePrefix + base64.RawURLEncoding.EncodeToString([]byte(safe))
}

func decodeSessionName(stored string) string {
	if !strings.HasPrefix(stored, encodedSessionNamePrefix) {
		return stored
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(stored, encodedSessionNamePrefix))
	if err != nil || len(decoded) == 0 {
		return stored
	}
	return string(decoded)
}

// isPortableFilePart applies Windows filename restrictions on every OS so a
// workspace remains portable between operating systems.
func isPortableFilePart(value string) bool {
	if value == "" || value == "." || value == ".." || strings.HasSuffix(value, ".") || strings.HasSuffix(value, " ") {
		return false
	}
	if strings.ContainsAny(value, `<>:"/\|?*`) {
		return false
	}
	for _, r := range value {
		if r < 32 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(value, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" {
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '1' && base[3] <= '9' {
		return false
	}
	return true
}

func (s *SessionStore) readFile(sessionID string) ([]providers.Message, error) {
	f, err := os.Open(s.path(sessionID))
	if err != nil {
		if os.IsNotExist(err) {
			return []providers.Message{}, nil
		}
		return nil, fmt.Errorf("agent: open session %q: %w", sessionID, err)
	}
	defer f.Close()

	var history []providers.Message
	scanner := bufio.NewScanner(f)
	// 单条消息可能较长（工具结果/长回复），放宽单行上限到 1MB。
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var msg providers.Message
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return nil, fmt.Errorf("agent: parse session %q: %w", sessionID, err)
		}
		history = append(history, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("agent: read session %q: %w", sessionID, err)
	}
	return history, nil
}

func (s *SessionStore) appendFile(sessionID string, messages []providers.Message) error {
	f, err := os.OpenFile(s.path(sessionID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("agent: open session %q for append: %w", sessionID, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	for _, msg := range messages {
		data, err := json.Marshal(msg)
		if err != nil {
			return fmt.Errorf("agent: marshal session message: %w", err)
		}
		if _, err := w.Write(data); err != nil {
			return fmt.Errorf("agent: write session %q: %w", sessionID, err)
		}
		if err := w.WriteByte('\n'); err != nil {
			return fmt.Errorf("agent: write session %q: %w", sessionID, err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("agent: flush session %q: %w", sessionID, err)
	}
	// fsync 保证进程/机器异常时历史不丢。
	if err := f.Sync(); err != nil {
		return fmt.Errorf("agent: sync session %q: %w", sessionID, err)
	}
	return nil
}

func cloneMessages(src []providers.Message) []providers.Message {
	if len(src) == 0 {
		return []providers.Message{}
	}
	return append([]providers.Message(nil), src...)
}
