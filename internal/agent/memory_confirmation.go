package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/memory"
	"github.com/ziangsun/szabot/internal/providers"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

const defaultMemoryConfirmationTimeout = 10 * time.Minute

const (
	memoryDecisionConfirmed = "confirmed"
	memoryDecisionRejected  = "rejected"
	memoryDecisionTimedOut  = "timed_out"
	memoryDecisionCancelled = "cancelled"
	memoryDecisionStale     = "stale"
	memoryDecisionFailed    = "failed"
)

var memoryReplacementOptions = []string{"确认替换", "拒绝替换"}

// memoryConfirmationRequest carries an uncommitted candidate. The candidate
// remains outside the authoritative store until its lightweight Run receives
// explicit approval.
type memoryConfirmationRequest struct {
	SourceRun *Run
	Inbound   bus.InboundMessage
	Candidate memory.Candidate
}

func (l *Loop) enqueueMemoryConfirmation(ctx context.Context, request *memoryConfirmationRequest) {
	if request == nil || request.SourceRun == nil {
		return
	}
	if l.Bus == nil {
		l.completeMemoryConfirmation(ctx, request, memoryDecisionFailed, 0, 0, errors.New("memory confirmation bus is unavailable"))
		return
	}
	run := NewRun(request.Inbound.SessionID, RunBudget{})
	item := queuedRun{in: request.Inbound, run: run, memoryConfirmation: request}
	start := l.appendQueuedRun(request.Inbound.SessionID, item)
	queueCtx := l.lifecycleContext(ctx)
	l.record(queueCtx, run, tracing.EventRunQueued, string(RunQueued), map[string]any{
		"work_type": "memory_confirmation", "source_run_id": request.SourceRun.ID,
	})
	l.persistSnapshot(run)
	if start {
		go l.drainSession(queueCtx, request.Inbound.SessionID)
	}
}

func (l *Loop) handleMemoryConfirmation(parent context.Context, in bus.InboundMessage, run *Run, request *memoryConfirmationRequest) {
	if err := l.transitionRun(parent, run, RunRunning, "memory confirmation started"); err != nil {
		l.completeMemoryConfirmation(parent, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	l.record(parent, run, tracing.EventRunStarted, string(RunRunning), map[string]any{
		"work_type": "memory_confirmation", "source_run_id": request.SourceRun.ID,
	})

	timeout := l.MemoryConfirmationTimeout
	if timeout <= 0 {
		timeout = defaultMemoryConfirmationTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ctx = withRoute(withRun(ctx, run), in.SessionID, in.ChannelID)
	handle := &runHandle{run: run, cancel: cancel}
	l.registerRun(in.SessionID, handle)
	defer l.unregisterRun(in.SessionID, handle)

	existing, err := l.findExistingMemories(ctx, in.UserID, request.Candidate)
	if err != nil {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	resolution := memory.ResolveCandidate(existing, request.Candidate)
	if resolution.Action != memory.ResolutionSupersede {
		// The authoritative state changed before this queued interaction ran.
		// Discarding the stale proposal is safer than applying an outdated choice.
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionStale, 0, 0, nil)
		return
	}

	question := renderMemoryReplacementQuestion(existing, request.Candidate)
	approvedRelatedIDs := append([]string(nil), resolution.RelatedIDs...)
	l.record(ctx, request.SourceRun, tracing.EventMemoryConfirmationAsked, "waiting_user", map[string]any{
		"confirmation_run_id": run.ID, "related_memory_ids": resolution.RelatedIDs,
		"user_id_hash": hashScope(in.UserID),
	})
	answer, err := l.Ask(ctx, question, memoryReplacementOptions)
	if err != nil {
		decision := memoryDecisionFailed
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			decision = memoryDecisionTimedOut
		case errors.Is(err, context.Canceled):
			decision = memoryDecisionCancelled
		}
		l.finishMemoryConfirmationRun(ctx, in, run, request, decision, 0, 0, err)
		return
	}
	if !memoryReplacementAccepted(answer) {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionRejected, 0, 0, nil)
		return
	}

	// Re-read after the human wait so the mutation never relies on stale IDs.
	existing, err = l.findExistingMemories(ctx, in.UserID, request.Candidate)
	if err != nil {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	resolution = memory.ResolveCandidate(existing, request.Candidate)
	if resolution.Action == memory.ResolutionDuplicate {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionConfirmed, 0, 0, nil)
		return
	}
	if resolution.Action != memory.ResolutionSupersede || !sameMemoryIDs(approvedRelatedIDs, resolution.RelatedIDs) {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionStale, 0, 0, nil)
		return
	}
	if _, ok := l.Memory.(memory.MutationStore); !ok {
		err = errors.New("memory store does not support atomic replacement")
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	item, retiredIDs, err := l.storeMemoryCandidate(ctx, request.SourceRun, in, request.Candidate, resolution)
	if err != nil {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	indexed, indexErr := l.indexMemoryItems(ctx, request.SourceRun, in.UserID, []memory.Memory{item}, retiredIDs)
	l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionConfirmed, 1, indexed, indexErr)
}

func (l *Loop) finishMemoryConfirmationRun(ctx context.Context, in bus.InboundMessage, run *Run, request *memoryConfirmationRequest, decision string, written, indexed int, workflowErr error) {
	l.completeMemoryConfirmation(ctx, request, decision, written, indexed, workflowErr)
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	status := RunCompleted
	reason := "memory replacement " + decision
	if workflowErr != nil {
		switch decision {
		case memoryDecisionTimedOut:
			status = RunTimedOut
		case memoryDecisionCancelled:
			status = RunCancelled
		default:
			status = RunFailed
		}
		run.setError(workflowErr)
	}
	if err := l.transitionRun(finishCtx, run, status, reason); err != nil {
		logMemoryConfirmationError(run, "terminal transition", err)
	}
	l.publishMemoryConfirmationOutcome(finishCtx, in, run, decision, written, workflowErr)
	data := map[string]any{"work_type": "memory_confirmation", "source_run_id": request.SourceRun.ID, "decision": decision}
	if workflowErr != nil {
		data["error"] = workflowErr.Error()
	}
	l.record(finishCtx, run, tracing.EventRunFinished, string(status), data)
	l.publishRunDone(finishCtx, in, run)
}

func (l *Loop) publishMemoryConfirmationOutcome(ctx context.Context, in bus.InboundMessage, run *Run, decision string, written int, workflowErr error) {
	text := "已保留旧记忆，并丢弃本次新候选。"
	switch decision {
	case memoryDecisionConfirmed:
		if workflowErr != nil && written > 0 {
			text = "长期记忆已更新，但向量索引更新失败；关键词召回仍可用。"
		} else if workflowErr != nil {
			text = "长期记忆更新失败，已保留旧记忆。"
		} else {
			text = "已更新长期记忆。"
		}
	case memoryDecisionTimedOut:
		text = "记忆替换确认已超时，已保留旧记忆并丢弃新候选。"
	case memoryDecisionCancelled:
		text = "记忆替换确认已取消，已保留旧记忆并丢弃新候选。"
	case memoryDecisionStale:
		text = "相关记忆已发生变化，本次新候选已丢弃。"
	case memoryDecisionFailed:
		text = "记忆替换处理失败，已保留旧记忆并丢弃新候选。"
	}
	out := bus.OutboundMessage{
		SessionID: in.SessionID, ChannelID: in.ChannelID, RunID: run.ID,
		AgentID: run.AgentID, Sequence: run.nextSequence(), Text: text,
		Kind: bus.KindAnswer, Delta: true, Time: time.Now(),
	}
	l.record(ctx, run, tracing.EventAssistantCompleted, "completed", map[string]any{
		"message": providers.Message{Role: providers.RoleAssistant, Content: text},
	})
	if err := l.Bus.PublishOutbound(ctx, out); err != nil {
		logMemoryConfirmationError(run, "publish outcome", err)
	}
}

func (l *Loop) completeMemoryConfirmation(ctx context.Context, request *memoryConfirmationRequest, decision string, written, indexed int, workflowErr error) {
	if request == nil || request.SourceRun == nil {
		return
	}
	state := request.SourceRun.updateMemoryState(func(state *MemoryRunState) {
		if state.PendingConfirmationCount > 0 {
			state.PendingConfirmationCount--
		}
		state.WrittenCount += written
		state.IndexedCount += indexed
		if decision == memoryDecisionConfirmed {
			state.ConfirmedCount++
		} else {
			state.DiscardedCount++
		}
		if workflowErr != nil && decision != memoryDecisionTimedOut && decision != memoryDecisionCancelled {
			state.Error = workflowErr.Error()
		}
		if state.PendingConfirmationCount == 0 {
			if state.Error != "" {
				state.Status = "failed"
			} else {
				state.Status = "completed"
			}
			state.FinishedAt = time.Now().UTC()
		} else {
			state.Status = "waiting_user"
		}
	})
	l.persistSnapshot(request.SourceRun)
	data := map[string]any{
		"decision": decision, "written_count": written, "indexed_count": indexed,
		"pending_confirmation_count": state.PendingConfirmationCount,
		"user_id_hash":               hashScope(request.Inbound.UserID),
	}
	if workflowErr != nil {
		data["error"] = workflowErr.Error()
	}
	l.record(context.WithoutCancel(ctx), request.SourceRun, tracing.EventMemoryConfirmationDone, decision, data)
}

func memoryReplacementAccepted(answer string) bool {
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "1", "确认替换", "确认", "替换", "是", "yes", "confirm", "replace":
		return true
	default:
		return false
	}
}

func renderMemoryReplacementQuestion(existing []memory.Memory, candidate memory.Candidate) string {
	oldContents := make([]string, 0, len(existing))
	for _, item := range existing {
		if item.Status != memory.StatusActive && item.Status != memory.StatusConflict {
			continue
		}
		oldContents = append(oldContents, truncateMemoryText(item.Content, 240))
		if len(oldContents) == 3 {
			break
		}
	}
	return fmt.Sprintf("检测到一条新记忆可能替换已有记忆。\n已有记忆：%s\n新记忆：%s\n是否确认替换？", strings.Join(oldContents, "；"), truncateMemoryText(candidate.Content, 240))
}

func truncateMemoryText(value string, limit int) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "..."
}

func sameMemoryIDs(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	counts := make(map[string]int, len(left))
	for _, id := range left {
		counts[id]++
	}
	for _, id := range right {
		if counts[id] == 0 {
			return false
		}
		counts[id]--
	}
	return true
}

func logMemoryConfirmationError(run *Run, operation string, err error) {
	if err != nil {
		log.Printf("[memory-confirmation] run=%s %s failed: %v", run.ID, operation, err)
	}
}
