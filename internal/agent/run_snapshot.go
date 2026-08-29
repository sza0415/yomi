package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// RunSnapshot is the durable, task-level state needed for inspection and
// conservative restart handling. Conversation messages remain in SessionStore.
type RunSnapshot struct {
	ID           string    `json:"run_id"`
	SessionID    string    `json:"session_id"`
	AgentID      string    `json:"agent_id"`
	Status       RunStatus `json:"status"`
	StatusReason string    `json:"status_reason,omitempty"`
	Error        string    `json:"error,omitempty"`
	Budget       RunBudget `json:"budget,omitempty"`
	Usage        RunUsage  `json:"usage"`
	QueuedAt     time.Time `json:"queued_at"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Snapshot returns a consistent copy of the Run's durable fields.
func (r *Run) Snapshot() RunSnapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return RunSnapshot{
		ID: r.ID, SessionID: r.SessionID, AgentID: r.AgentID,
		Status: r.Status, StatusReason: r.StatusReason, Error: r.Error,
		Budget: r.Budget, Usage: r.Usage, QueuedAt: r.QueuedAt,
		StartedAt: r.StartedAt, FinishedAt: r.FinishedAt, UpdatedAt: time.Now(),
	}
}

// RunSnapshotStore persists task-level snapshots and can mark unfinished runs
// after a process restart. It deliberately does not replay tools.
type RunSnapshotStore interface {
	Save(RunSnapshot) error
	Load(runID string) (RunSnapshot, error)
	List(sessionID string) ([]RunSnapshot, error)
	MarkInterrupted() ([]RunSnapshot, error)
}

// JSONRunSnapshotStore stores one atomically replaced JSON file per Run.
type JSONRunSnapshotStore struct {
	dir string
	mu  sync.Mutex
}

func NewJSONRunSnapshotStore(dir string) (*JSONRunSnapshotStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("agent: run snapshot dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: create run snapshot dir %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("agent: secure run snapshot dir %q: %w", dir, err)
	}
	return &JSONRunSnapshotStore{dir: dir}, nil
}

func (s *JSONRunSnapshotStore) Save(snapshot RunSnapshot) error {
	if strings.TrimSpace(snapshot.ID) == "" {
		return fmt.Errorf("agent: run snapshot id is empty")
	}
	if snapshot.UpdatedAt.IsZero() {
		snapshot.UpdatedAt = time.Now()
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("agent: marshal run snapshot: %w", err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	tmp, err := os.CreateTemp(s.dir, ".run-snapshot-*")
	if err != nil {
		return fmt.Errorf("agent: create run snapshot temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("agent: secure run snapshot temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("agent: write run snapshot: %w", err)
	}
	if _, err := tmp.Write([]byte("\n")); err != nil {
		tmp.Close()
		return fmt.Errorf("agent: write run snapshot newline: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("agent: sync run snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agent: close run snapshot: %w", err)
	}
	if err := os.Rename(tmpName, s.path(snapshot.ID)); err != nil {
		return fmt.Errorf("agent: replace run snapshot: %w", err)
	}
	return nil
}

func (s *JSONRunSnapshotStore) Load(runID string) (RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path(runID))
	if os.IsNotExist(err) {
		return RunSnapshot{}, fmt.Errorf("agent: run snapshot %q not found", runID)
	}
	if err != nil {
		return RunSnapshot{}, fmt.Errorf("agent: read run snapshot: %w", err)
	}
	var snapshot RunSnapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return RunSnapshot{}, fmt.Errorf("agent: parse run snapshot: %w", err)
	}
	if snapshot.ID != runID {
		return RunSnapshot{}, fmt.Errorf("agent: run snapshot identity mismatch: %q", runID)
	}
	return snapshot, nil
}

// List returns snapshots newest first. An empty sessionID lists all sessions.
func (s *JSONRunSnapshotStore) List(sessionID string) ([]RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("agent: list run snapshots: %w", err)
	}
	result := make([]RunSnapshot, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("agent: read run snapshot %q: %w", path, err)
		}
		var snapshot RunSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, fmt.Errorf("agent: parse run snapshot %q: %w", path, err)
		}
		if sessionID == "" || snapshot.SessionID == sessionID {
			result = append(result, snapshot)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return result[i].UpdatedAt.After(result[j].UpdatedAt)
	})
	return result, nil
}

func (s *JSONRunSnapshotStore) MarkInterrupted() ([]RunSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("agent: list run snapshots: %w", err)
	}
	var interrupted []RunSnapshot
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("agent: read run snapshot %q: %w", path, err)
		}
		var snapshot RunSnapshot
		if err := json.Unmarshal(data, &snapshot); err != nil {
			return nil, fmt.Errorf("agent: parse run snapshot %q: %w", path, err)
		}
		if isRunTerminal(snapshot.Status) {
			continue
		}
		snapshot.Status = RunFailed
		snapshot.StatusReason = "interrupted by process restart"
		snapshot.Error = "agent: run interrupted by process restart"
		snapshot.FinishedAt = time.Now()
		snapshot.UpdatedAt = snapshot.FinishedAt
		encoded, err := json.Marshal(snapshot)
		if err != nil {
			return nil, fmt.Errorf("agent: marshal interrupted run snapshot: %w", err)
		}
		if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
			return nil, fmt.Errorf("agent: update interrupted run snapshot: %w", err)
		}
		interrupted = append(interrupted, snapshot)
	}
	return interrupted, nil
}

func (s *JSONRunSnapshotStore) path(runID string) string {
	safe := filepath.Base(filepath.Clean("/" + runID))
	if safe == "." || safe == "/" || safe == "" {
		safe = "default"
	}
	return filepath.Join(s.dir, safe+".json")
}
