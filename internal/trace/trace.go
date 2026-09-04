package trace

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const SchemaVersion = 1

const (
	EventRunQueued                = "run.queued"
	EventRunStatusChanged         = "run.status.changed"
	EventRunStarted               = "run.started"
	EventRunFinished              = "run.finished"
	EventInputReceived            = "input.received"
	EventSystemMessage            = "system.message"
	EventContextInjected          = "context.injected"
	EventContextCompacted         = "context.compacted"
	EventContextArchived          = "context.archived"
	EventContextStrategyApplied   = "context.strategy.applied"
	EventArtifactCreated          = "artifact.created"
	EventModelRequestStarted      = "model.request.started"
	EventModelResponseFinished    = "model.response.finished"
	EventModelRequestFailed       = "model.request.failed"
	EventModelStatusChanged       = "model.status.changed"
	EventAssistantCompleted       = "assistant.message.completed"
	EventToolExecutionStarted     = "tool.execution.started"
	EventToolExecutionFinished    = "tool.execution.finished"
	EventToolExecutionFailed      = "tool.execution.failed"
	EventToolStatusChanged        = "tool.status.changed"
	EventUserQuestionAsked        = "user.question.asked"
	EventUserQuestionAnswered     = "user.question.answered"
	EventMemoryRetrievalStarted   = "memory.retrieval.started"
	EventMemoryRetrievalFinished  = "memory.retrieval.finished"
	EventMemoryRetrievalFailed    = "memory.retrieval.failed"
	EventMemoryContextInjected    = "memory.context.injected"
	EventMemoryExtractionStarted  = "memory.extraction.started"
	EventMemoryExtractionFinished = "memory.extraction.finished"
	EventMemoryExtractionFailed   = "memory.extraction.failed"
	EventMemoryProposalResolved   = "memory.proposal.resolved"
	EventMemoryProposalPersisted  = "memory.proposal.persisted"
	EventMemoryCandidateAccepted  = "memory.candidate.accepted"
	EventMemoryCandidateRejected  = "memory.candidate.rejected"
	EventMemoryConfirmationAsked  = "memory.confirmation.asked"
	EventMemoryConfirmationDone   = "memory.confirmation.done"
	EventMemoryPolicyApplied      = "memory.policy.applied"
	EventMemoryWriteCompleted     = "memory.write.completed"
	EventMemoryWriteFailed        = "memory.write.failed"
	EventMemoryIndexCompleted     = "memory.index.completed"
	EventMemoryIndexFailed        = "memory.index.failed"
	EventMemoryDeleted            = "memory.deleted"
	EventMemoryRebuildCompleted   = "memory.rebuild.completed"
)

type Event struct {
	SchemaVersion int            `json:"schema_version"`
	Sequence      uint64         `json:"sequence"`
	Timestamp     time.Time      `json:"timestamp"`
	SessionID     string         `json:"session_id"`
	RunID         string         `json:"run_id"`
	AgentID       string         `json:"agent_id"`
	Type          string         `json:"type"`
	Status        string         `json:"status,omitempty"`
	DurationMS    int64          `json:"duration_ms,omitempty"`
	Data          map[string]any `json:"data,omitempty"`
}

type Sink interface {
	Record(context.Context, Event) error
}

type Reader interface {
	ReadRun(runID string) ([]Event, error)
	ReadSession(sessionID string) ([]Event, error)
}

type NoopSink struct{}

func (NoopSink) Record(context.Context, Event) error { return nil }

type JSONLSink struct {
	dir string
	mu  sync.Mutex
}

func NewJSONLSink(dir string) (*JSONLSink, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, fmt.Errorf("trace: directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trace: create directory %q: %w", dir, err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trace: secure directory %q: %w", dir, err)
	}
	return &JSONLSink{dir: dir}, nil
}

func (s *JSONLSink) Record(_ context.Context, event Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if event.SchemaVersion == 0 {
		event.SchemaVersion = SchemaVersion
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("trace: marshal event: %w", err)
	}
	f, err := os.OpenFile(s.path(event.RunID), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("trace: open run %q: %w", event.RunID, err)
	}
	defer f.Close()

	w := bufio.NewWriter(f)
	if _, err := w.Write(data); err != nil {
		return fmt.Errorf("trace: write run %q: %w", event.RunID, err)
	}
	if err := w.WriteByte('\n'); err != nil {
		return fmt.Errorf("trace: write run %q: %w", event.RunID, err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("trace: flush run %q: %w", event.RunID, err)
	}
	return nil
}

func (s *JSONLSink) ReadRun(runID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	events, err := readEvents(s.path(runID))
	if err != nil {
		return nil, err
	}
	for _, event := range events {
		if event.RunID != runID {
			return nil, fmt.Errorf("trace: run identity mismatch: requested %q, found %q", runID, event.RunID)
		}
	}
	return events, nil
}

func (s *JSONLSink) ReadSession(sessionID string) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	paths, err := filepath.Glob(filepath.Join(s.dir, "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("trace: list runs: %w", err)
	}
	var events []Event
	for _, path := range paths {
		runEvents, err := readEvents(path)
		if err != nil {
			continue // 一个损坏或正在写入的 Run 不应阻断其他 Run 的查询。
		}
		for _, event := range runEvents {
			if event.SessionID == sessionID {
				events = append(events, event)
			}
		}
	}
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Timestamp.Equal(events[j].Timestamp) {
			if events[i].RunID == events[j].RunID {
				return events[i].Sequence < events[j].Sequence
			}
			return events[i].RunID < events[j].RunID
		}
		return events[i].Timestamp.Before(events[j].Timestamp)
	})
	return events, nil
}

func readEvents(path string) ([]Event, error) {
	f, err := os.Open(path)
	if errorsIsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trace: open %q: %w", path, err)
	}
	defer f.Close()

	reader := bufio.NewReader(f)
	var events []Event
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = []byte(strings.TrimSpace(string(line)))
			if len(line) > 0 {
				var event Event
				if err := json.Unmarshal(line, &event); err != nil {
					return nil, fmt.Errorf("trace: parse %q: %w", path, err)
				}
				if err := validateEvent(event); err != nil {
					return nil, fmt.Errorf("trace: validate %q: %w", path, err)
				}
				events = append(events, event)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("trace: read %q: %w", path, readErr)
		}
	}
	return events, nil
}

func validateEvent(event Event) error {
	if event.SchemaVersion != SchemaVersion {
		return fmt.Errorf("unsupported schema version %d", event.SchemaVersion)
	}
	if event.RunID == "" || event.SessionID == "" || event.Type == "" || event.Sequence == 0 || event.Timestamp.IsZero() {
		return fmt.Errorf("missing required event fields")
	}
	return nil
}

func errorsIsNotExist(err error) bool { return err != nil && os.IsNotExist(err) }

func (s *JSONLSink) path(runID string) string {
	hash := sha256.Sum256([]byte(runID))
	return filepath.Join(s.dir, hex.EncodeToString(hash[:])+".jsonl")
}
