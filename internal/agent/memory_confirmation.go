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
	SourceRun      *Run
	Inbound        bus.InboundMessage
	Candidate      memory.Candidate
	TargetIDs      []string
	TargetVersions map[string]time.Time
	Reason         string
	ProposalID     string
	ExpiresAt      time.Time
}

func (l *Loop) restorePendingMemoryConfirmations(ctx context.Context) {
	proposalStore, ok := l.Memory.(memory.ProposalStore)
	if !ok {
		return
	}
	proposals, err := proposalStore.ListPendingProposals(ctx)
	if err != nil {
		log.Printf("[loop] restore memory proposals error: %v", err)
		return
	}
	if len(proposals) == 0 {
		return
	}
	groupCounts := make(map[string]int)
	for _, proposal := range proposals {
		groupCounts[proposal.SourceRunID]++
	}
	sourceRuns := make(map[string]*Run, len(groupCounts))
	for _, proposal := range proposals {
		sourceRun := sourceRuns[proposal.SourceRunID]
		if sourceRun == nil {
			sourceRun = l.restoreMemorySourceRun(proposal)
			pendingCount := groupCounts[proposal.SourceRunID]
			sourceRun.updateMemoryState(func(state *MemoryRunState) {
				state.Status = "waiting_user"
				state.PendingConfirmationCount = pendingCount
				minimumTotal := state.ConfirmedCount + state.DiscardedCount + pendingCount
				if state.ConfirmationCount < minimumTotal {
					state.ConfirmationCount = minimumTotal
				}
				if state.StartedAt.IsZero() {
					state.StartedAt = proposal.CreatedAt
				}
				state.FinishedAt = time.Time{}
			})
			l.persistSnapshot(sourceRun)
			sourceRuns[proposal.SourceRunID] = sourceRun
		}
		l.enqueueMemoryConfirmation(ctx, &memoryConfirmationRequest{
			SourceRun: sourceRun,
			Inbound: bus.InboundMessage{
				UserID: proposal.UserID, SessionID: proposal.SourceSessionID,
				ChannelID: proposal.ChannelID, Time: proposal.CreatedAt,
			},
			Candidate: proposal.Candidate, TargetIDs: proposal.TargetIDs,
			TargetVersions: proposal.TargetVersions, Reason: proposal.Reason,
			ProposalID: proposal.ID, ExpiresAt: proposal.ExpiresAt,
		})
	}
	log.Printf("[loop] restored %d pending memory confirmation(s)", len(proposals))
}

func (l *Loop) restoreMemorySourceRun(proposal memory.ProposalRecord) *Run {
	if l.Snapshots != nil {
		if snapshot, err := l.Snapshots.Load(proposal.SourceRunID); err == nil {
			return &Run{
				ID: snapshot.ID, SessionID: snapshot.SessionID, AgentID: snapshot.AgentID,
				Status: snapshot.Status, Budget: snapshot.Budget, Usage: snapshot.Usage,
				QueuedAt: snapshot.QueuedAt, StartedAt: snapshot.StartedAt,
				FinishedAt: snapshot.FinishedAt, Error: snapshot.Error,
				StatusReason: snapshot.StatusReason, Memory: snapshot.Memory,
			}
		}
	}
	return &Run{
		ID: proposal.SourceRunID, SessionID: proposal.SourceSessionID,
		AgentID: DefaultAgentID, Status: RunCompleted, QueuedAt: proposal.CreatedAt,
		StartedAt: proposal.CreatedAt, FinishedAt: proposal.CreatedAt,
	}
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

	timeout := l.memoryConfirmationTimeout()
	if !request.ExpiresAt.IsZero() {
		timeout = time.Until(request.ExpiresAt)
		if timeout <= 0 {
			l.finishMemoryConfirmationRun(parent, in, run, request, memoryDecisionTimedOut, 0, 0, context.DeadlineExceeded)
			return
		}
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	ctx = withRoute(withRun(ctx, run), in.SessionID, in.ChannelID)
	handle := &runHandle{run: run, cancel: cancel}
	l.registerRun(in.SessionID, handle)
	defer l.unregisterRun(in.SessionID, handle)

	existing, resolution, err := l.resolveMemoryConfirmation(ctx, request)
	if err != nil {
		if l.preserveMemoryConfirmationOnShutdown(parent, in, run, request, err) {
			return
		}
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	if resolution.Action != memory.ResolutionReplace {
		// The authoritative state changed before this queued interaction ran.
		// Discarding the stale proposal is safer than applying an outdated choice.
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionStale, 0, 0, nil)
		return
	}

	question := renderMemoryReplacementQuestion(existing, request.Candidate)
	approvedRelatedIDs := append([]string(nil), resolution.RelatedIDs...)
	approvedTargets := append([]memory.Memory(nil), existing...)
	l.record(ctx, request.SourceRun, tracing.EventMemoryConfirmationAsked, "waiting_user", map[string]any{
		"confirmation_run_id": run.ID, "related_memory_ids": resolution.RelatedIDs,
		"user_id_hash": hashScope(in.UserID),
	})
	answer, err := l.Ask(ctx, question, memoryReplacementOptions)
	if err != nil {
		if l.preserveMemoryConfirmationOnShutdown(parent, in, run, request, err) {
			return
		}
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
	existing, resolution, err = l.resolveMemoryConfirmation(ctx, request)
	if err != nil {
		if l.preserveMemoryConfirmationOnShutdown(parent, in, run, request, err) {
			return
		}
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	if resolution.Action == memory.ResolutionDuplicate {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionConfirmed, 0, 0, nil)
		return
	}
	if resolution.Action != memory.ResolutionReplace || !sameMemoryIDs(approvedRelatedIDs, resolution.RelatedIDs) || !sameMemoryVersions(approvedTargets, existing) {
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionStale, 0, 0, nil)
		return
	}
	if _, ok := l.Memory.(memory.MutationStore); !ok {
		err = errors.New("memory store does not support atomic replacement")
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	item, retiredIDs, err := l.storeMemoryCandidate(ctx, request.SourceRun, in, request.Candidate, resolution, request.ProposalID)
	if err != nil {
		if l.preserveMemoryConfirmationOnShutdown(parent, in, run, request, err) {
			return
		}
		l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionFailed, 0, 0, err)
		return
	}
	indexed, indexErr := l.indexMemoryItems(ctx, request.SourceRun, in.UserID, []memory.Memory{item}, retiredIDs)
	l.finishMemoryConfirmationRun(ctx, in, run, request, memoryDecisionConfirmed, 1, indexed, indexErr)
}

func (l *Loop) preserveMemoryConfirmationOnShutdown(parent context.Context, in bus.InboundMessage, run *Run, request *memoryConfirmationRequest, workflowErr error) bool {
	if parent.Err() == nil || !errors.Is(workflowErr, context.Canceled) {
		return false
	}
	finishCtx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Second)
	defer cancel()
	run.setError(workflowErr)
	if err := l.transitionRun(finishCtx, run, RunCancelled, "memory confirmation interrupted by shutdown"); err != nil {
		logMemoryConfirmationError(run, "shutdown transition", err)
	}
	l.record(finishCtx, run, tracing.EventRunFinished, string(RunCancelled), map[string]any{
		"work_type": "memory_confirmation", "source_run_id": request.SourceRun.ID,
		"decision": "pending_restart", "error": workflowErr.Error(),
	})
	l.publishRunDone(finishCtx, in, run)
	return true
}

func sameMemoryVersions(before, after []memory.Memory) bool {
	if len(before) != len(after) {
		return false
	}
	byID := make(map[string]memory.Memory, len(after))
	for _, item := range after {
		byID[item.ID] = item
	}
	for _, item := range before {
		current, ok := byID[item.ID]
		if !ok || !current.UpdatedAt.Equal(item.UpdatedAt) || current.Status != item.Status || current.Content != item.Content || current.Value != item.Value {
			return false
		}
	}
	return true
}

func (l *Loop) resolveMemoryConfirmation(ctx context.Context, request *memoryConfirmationRequest) ([]memory.Memory, memory.Resolution, error) {
	if len(request.TargetIDs) > 0 {
		targets, err := l.loadMemoryTargets(ctx, request.Inbound.UserID, request.Candidate.Kind, request.TargetIDs)
		if err != nil {
			return nil, memory.Resolution{}, err
		}
		siblingIDs, err := l.confirmationSiblingIDs(ctx, request.Inbound.UserID, targets)
		if err != nil {
			return nil, memory.Resolution{}, err
		}
		if !memoryIDsContained(siblingIDs, request.TargetIDs) {
			return targets, memory.Resolution{Action: memory.ResolutionConflict, RelatedIDs: memoryIDs(targets), Reason: "related memories changed while confirmation was pending"}, nil
		}
		if !samePersistedMemoryVersions(request.TargetVersions, targets) {
			return targets, memory.Resolution{Action: memory.ResolutionConflict, RelatedIDs: memoryIDs(targets), Reason: "related memory versions changed while confirmation was pending"}, nil
		}
		return targets, memory.Resolution{
			Action: memory.ResolutionReplace, RelatedIDs: memoryIDs(targets),
			Reason: firstMemoryReason(request.Reason, "user-confirmed semantic replacement"),
		}, nil
	}
	existing, err := l.findExistingMemories(ctx, request.Inbound.UserID, request.Candidate)
	if err != nil {
		return nil, memory.Resolution{}, err
	}
	return existing, memory.ResolveCandidate(existing, request.Candidate), nil
}

func (l *Loop) confirmationSiblingIDs(ctx context.Context, userID string, targets []memory.Memory) ([]string, error) {
	seenPaths := make(map[string]struct{}, len(targets))
	seenIDs := make(map[string]struct{})
	var ids []string
	for _, target := range targets {
		path := target.Kind + "\x00" + strings.ToLower(strings.TrimSpace(target.Subject)) + "\x00" + strings.ToLower(strings.TrimSpace(target.Attribute))
		if _, ok := seenPaths[path]; ok {
			continue
		}
		seenPaths[path] = struct{}{}
		siblings, err := l.findExistingMemories(ctx, userID, memory.Candidate{
			Kind: target.Kind, Subject: target.Subject, Attribute: target.Attribute, Content: target.Content,
		})
		if err != nil {
			return nil, err
		}
		for _, sibling := range siblings {
			if _, ok := seenIDs[sibling.ID]; ok {
				continue
			}
			seenIDs[sibling.ID] = struct{}{}
			ids = append(ids, sibling.ID)
		}
	}
	return ids, nil
}

func memoryIDsContained(ids, allowed []string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, id := range allowed {
		allowedSet[id] = struct{}{}
	}
	for _, id := range ids {
		if _, ok := allowedSet[id]; !ok {
			return false
		}
	}
	return true
}

func samePersistedMemoryVersions(expected map[string]time.Time, current []memory.Memory) bool {
	if len(expected) == 0 {
		return true
	}
	if len(expected) != len(current) {
		return false
	}
	for _, item := range current {
		version, ok := expected[item.ID]
		if !ok || !version.Equal(item.UpdatedAt) {
			return false
		}
	}
	return true
}

func (l *Loop) finishMemoryConfirmationRun(ctx context.Context, in bus.InboundMessage, run *Run, request *memoryConfirmationRequest, decision string, written, indexed int, workflowErr error) {
	decision, workflowErr = l.completeMemoryConfirmation(ctx, request, decision, written, indexed, workflowErr)
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

func (l *Loop) completeMemoryConfirmation(ctx context.Context, request *memoryConfirmationRequest, decision string, written, indexed int, workflowErr error) (string, error) {
	if request == nil || request.SourceRun == nil {
		return decision, workflowErr
	}
	// A successful proposal-backed mutation marks the proposal applied inside
	// the same SQLite transaction. Other outcomes still need explicit closure.
	if request.ProposalID != "" && !(decision == memoryDecisionConfirmed && written > 0) {
		if proposalStore, ok := l.Memory.(memory.ProposalStore); ok {
			status := memory.ProposalStatusFailed
			switch decision {
			case memoryDecisionConfirmed:
				if workflowErr == nil || written > 0 {
					status = memory.ProposalStatusApplied
				}
			case memoryDecisionRejected:
				status = memory.ProposalStatusRejected
			case memoryDecisionTimedOut:
				status = memory.ProposalStatusTimedOut
			case memoryDecisionCancelled:
				status = memory.ProposalStatusCancelled
			case memoryDecisionStale:
				status = memory.ProposalStatusStale
			}
			if err := proposalStore.CompleteProposal(context.WithoutCancel(ctx), request.Inbound.UserID, request.ProposalID, status); err != nil {
				// The proposal is still pending (or its terminal state is
				// uncertain), so leave the source count untouched. A later
				// restart can safely reload and resolve it.
				l.record(context.WithoutCancel(ctx), request.SourceRun, tracing.EventMemoryConfirmationDone, memoryDecisionFailed, map[string]any{
					"decision": memoryDecisionFailed, "error": err.Error(),
					"proposal_id": request.ProposalID, "pending_confirmation_count": request.SourceRun.Snapshot().Memory.PendingConfirmationCount,
					"user_id_hash": hashScope(request.Inbound.UserID),
				})
				return memoryDecisionFailed, err
			}
		}
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
	return decision, workflowErr
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
