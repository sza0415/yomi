package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ziangsun/szabot/internal/providers"
)

const DefaultAgentID = "default"

type RunStatus string

const (
	RunQueued         RunStatus = "queued"
	RunRunning        RunStatus = "running"
	RunWaitingUser    RunStatus = "waiting_user"
	RunCompleted      RunStatus = "completed"
	RunFailed         RunStatus = "failed"
	RunCancelled      RunStatus = "cancelled"
	RunTimedOut       RunStatus = "timed_out"
	RunBudgetExceeded RunStatus = "budget_exceeded"
)

var ErrInvalidRunTransition = errors.New("agent: invalid run status transition")

type RunBudget struct {
	MaxInputTokens  int `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int `json:"max_total_tokens,omitempty"`
	MaxModelCalls   int `json:"max_model_calls,omitempty"`
	MaxToolCalls    int `json:"max_tool_calls,omitempty"`
}

type RunUsage struct {
	providers.Usage
	ModelCalls int `json:"model_calls"`
	ToolCalls  int `json:"tool_calls"`
}

type Run struct {
	ID           string    `json:"run_id"`
	SessionID    string    `json:"session_id"`
	AgentID      string    `json:"agent_id"`
	Status       RunStatus `json:"status"`
	Budget       RunBudget `json:"budget,omitempty"`
	Usage        RunUsage  `json:"usage"`
	QueuedAt     time.Time `json:"queued_at"`
	StartedAt    time.Time `json:"started_at,omitempty"`
	FinishedAt   time.Time `json:"finished_at,omitempty"`
	Error        string    `json:"error,omitempty"`
	StatusReason string    `json:"status_reason,omitempty"`

	mu       sync.Mutex
	sequence uint64
}

func NewRun(sessionID string, budget RunBudget) *Run {
	return &Run{
		ID:        newRunID(),
		SessionID: sessionID,
		AgentID:   DefaultAgentID,
		Status:    RunQueued,
		Budget:    budget,
		QueuedAt:  time.Now(),
	}
}

func (r *Run) nextSequence() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sequence++
	return r.sequence
}

func (r *Run) start() {
	_ = r.Transition(RunRunning, "run started")
}

func (r *Run) finish(status RunStatus, err error) {
	_ = r.Transition(status, string(status))
	r.setError(err)
}

func (r *Run) setError(err error) {
	if err == nil {
		return
	}
	r.mu.Lock()
	r.Error = err.Error()
	r.mu.Unlock()
}

// Transition changes the lifecycle state of a Run after validating the
// allowed transition. Run is the task-level state machine; model and tool
// execution states are reported separately by Runner events.
func (r *Run) Transition(to RunStatus, reason string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.Status == to {
		return nil
	}
	if !validRunTransition(r.Status, to) {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidRunTransition, r.Status, to)
	}

	now := time.Now()
	if to == RunRunning && r.StartedAt.IsZero() {
		r.StartedAt = now
	}
	if isRunTerminal(to) {
		r.FinishedAt = now
	}
	r.Status = to
	if reason != "" {
		r.StatusReason = reason
	}
	return nil
}

func validRunTransition(from, to RunStatus) bool {
	if isRunTerminal(from) {
		return false
	}
	switch from {
	case RunQueued:
		return to == RunRunning || to == RunCancelled
	case RunRunning:
		return to == RunWaitingUser || isRunTerminal(to)
	case RunWaitingUser:
		return to == RunRunning || to == RunCancelled || to == RunTimedOut
	default:
		return false
	}
}

func isRunTerminal(status RunStatus) bool {
	switch status {
	case RunCompleted, RunFailed, RunCancelled, RunTimedOut, RunBudgetExceeded:
		return true
	default:
		return false
	}
}

func (r *Run) setUsage(usage RunUsage) {
	r.mu.Lock()
	r.Usage = usage
	r.mu.Unlock()
}

func newRunID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

type runContextKey struct{}

func withRun(ctx context.Context, run *Run) context.Context {
	return context.WithValue(ctx, runContextKey{}, run)
}

func runFrom(ctx context.Context) (*Run, bool) {
	run, ok := ctx.Value(runContextKey{}).(*Run)
	return run, ok && run != nil
}

var ErrBudgetExceeded = errors.New("agent: run budget exceeded")

type BudgetError struct {
	Resource string
	Limit    int
	Used     int
}

func (e *BudgetError) Error() string {
	return fmt.Sprintf("%s: %s limit=%d used=%d", ErrBudgetExceeded, e.Resource, e.Limit, e.Used)
}

func (e *BudgetError) Unwrap() error { return ErrBudgetExceeded }

func checkBudget(b RunBudget, u RunUsage) error {
	checks := []struct {
		name  string
		limit int
		used  int
	}{
		{"input_tokens", b.MaxInputTokens, u.InputTokens},
		{"output_tokens", b.MaxOutputTokens, u.OutputTokens},
		{"total_tokens", b.MaxTotalTokens, u.TotalTokens},
		{"model_calls", b.MaxModelCalls, u.ModelCalls},
		{"tool_calls", b.MaxToolCalls, u.ToolCalls},
	}
	for _, c := range checks {
		if c.limit > 0 && c.used > c.limit {
			return &BudgetError{Resource: c.name, Limit: c.limit, Used: c.used}
		}
	}
	return nil
}
