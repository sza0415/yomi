package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/memory"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

func (l *Loop) startMemoryExtraction(runCtx context.Context, run *Run, in bus.InboundMessage, answer string) {
	if l == nil || l.Memory == nil || (l.MemoryExtractor == nil && l.MemoryCurator == nil) || strings.TrimSpace(in.UserID) == "" {
		return
	}
	timeout := l.MemoryTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	run.setMemoryState(MemoryRunState{Status: "pending"})
	l.persistSnapshot(run)
	ctx, cancel := context.WithTimeout(l.lifecycleContext(runCtx), timeout)
	go func() {
		defer cancel()
		startedAt := time.Now().UTC()
		run.setMemoryState(MemoryRunState{Status: "running", StartedAt: startedAt})
		l.persistSnapshot(run)
		outcome, confirmations := l.extractAndStoreMemory(ctx, run, in, answer)
		outcome.StartedAt = startedAt
		if outcome.PendingConfirmationCount == 0 {
			outcome.FinishedAt = time.Now().UTC()
		}
		run.setMemoryState(outcome)
		l.persistSnapshot(run)
		for _, confirmation := range confirmations {
			// 将需要用户确认的记忆替换作为run进入该session的队列
			l.enqueueMemoryConfirmation(ctx, confirmation)
		}
	}()
}

func (l *Loop) extractAndStoreMemory(ctx context.Context, run *Run, in bus.InboundMessage, answer string) (outcome MemoryRunState, confirmations []*memoryConfirmationRequest) {
	outcome.Status = "completed"
	defer func() {
		outcome.ConfirmationCount = len(confirmations)
		outcome.PendingConfirmationCount = len(confirmations)
		if len(confirmations) > 0 {
			outcome.Status = "waiting_user"
		}
	}()
	started := time.Now()
	traceData := map[string]any{"user_id_hash": hashScope(in.UserID), "source_session_id": in.SessionID}
	l.record(ctx, run, tracing.EventMemoryExtractionStarted, "started", traceData)
	proposals, err := l.proposeMemories(ctx, memory.ExtractionInput{
		UserID: in.UserID, SessionID: in.SessionID, RunID: run.ID,
		UserText: in.Text, AssistantText: answer, ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		outcome.Status = "failed"
		outcome.Error = err.Error()
		l.recordDuration(ctx, run, tracing.EventMemoryExtractionFailed, "failed", time.Since(started), map[string]any{"error": err.Error(), "user_id_hash": hashScope(in.UserID)})
		return outcome, confirmations
	}
	outcome.CandidateCount = len(proposals)
	l.recordDuration(ctx, run, tracing.EventMemoryExtractionFinished, "completed", time.Since(started), map[string]any{"candidate_count": len(proposals), "user_id_hash": hashScope(in.UserID)})
	policyResult, acceptedProposals := l.applyMemoryProposalPolicy(proposals)
	outcome.RejectedCount = policyResult.Rejected
	l.record(ctx, run, tracing.EventMemoryPolicyApplied, "completed", map[string]any{
		"candidate_count": len(proposals), "accepted_count": len(acceptedProposals),
		"rejected_count": policyResult.Rejected, "reasons": policyResult.Reasons, "user_id_hash": hashScope(in.UserID),
	})
	if policyResult.Rejected > 0 {
		l.record(ctx, run, tracing.EventMemoryCandidateRejected, "rejected", map[string]any{"count": policyResult.Rejected, "reasons": policyResult.Reasons, "user_id_hash": hashScope(in.UserID)})
	}
	accepted := make([]memory.Memory, 0, len(policyResult.Accepted))
	retiredIndexIDs := make([]string, 0)
	for _, proposal := range acceptedProposals {
		candidate := proposal.Candidate
		requestedSubject, requestedAttribute := candidate.Subject, candidate.Attribute
		resolution, needsConfirmation, resolveErr := l.resolveMemoryProposal(ctx, in.UserID, in.Text, &candidate, proposal)
		if resolveErr != nil {
			outcome.Status = "failed"
			outcome.Error = resolveErr.Error()
			l.record(ctx, run, tracing.EventMemoryCandidateRejected, "lookup_failed", map[string]any{
				"reason": resolveErr.Error(), "kind": candidate.Kind, "operation": proposal.Operation, "user_id_hash": hashScope(in.UserID),
			})
			continue
		}
		l.record(ctx, run, tracing.EventMemoryProposalResolved, "completed", map[string]any{
			"operation": proposal.Operation, "resolution": resolution.Action,
			"requested_subject": requestedSubject, "requested_attribute": requestedAttribute,
			"resolved_subject": candidate.Subject, "resolved_attribute": candidate.Attribute,
			"related_memory_ids": resolution.RelatedIDs, "needs_confirmation": needsConfirmation,
			"user_id_hash": hashScope(in.UserID),
		})
		if resolution.Action == memory.ResolutionDuplicate {
			l.record(ctx, run, tracing.EventMemoryCandidateRejected, "duplicate", map[string]any{"reason": resolution.Reason, "operation": proposal.Operation, "related_memory_ids": resolution.RelatedIDs, "user_id_hash": hashScope(in.UserID)})
			continue
		}
		if needsConfirmation {
			targetIDs := append([]string(nil), resolution.RelatedIDs...)
			confirmation := &memoryConfirmationRequest{
				SourceRun: run, Inbound: in, Candidate: candidate,
				TargetIDs: targetIDs,
				Reason:    firstMemoryReason(proposal.Reason, resolution.Reason),
			}
			if proposalStore, ok := l.Memory.(memory.ProposalStore); ok && len(targetIDs) > 0 {
				record := memory.ProposalRecord{
					UserID: in.UserID, SourceSessionID: in.SessionID, SourceRunID: run.ID,
					ChannelID: in.ChannelID, Operation: memory.ProposalNeedsConfirmation,
					Candidate: candidate, TargetIDs: targetIDs, Reason: confirmation.Reason,
					Status: memory.ProposalStatusPending, CreatedAt: time.Now().UTC(),
					ExpiresAt: time.Now().UTC().Add(l.memoryConfirmationTimeout()),
				}
				created, proposalErr := proposalStore.CreateProposal(ctx, record)
				if proposalErr != nil {
					outcome.Status = "failed"
					outcome.Error = proposalErr.Error()
					continue
				}
				confirmation.ProposalID = created.ID
				confirmation.TargetVersions = created.TargetVersions
				confirmation.ExpiresAt = created.ExpiresAt
				l.record(ctx, run, tracing.EventMemoryProposalPersisted, "pending", map[string]any{
					"proposal_id": created.ID, "operation": created.Operation,
					"target_ids": created.TargetIDs, "expires_at": created.ExpiresAt,
					"user_id_hash": hashScope(in.UserID),
				})
			}
			confirmations = append(confirmations, confirmation)
			continue
		}
		resolution.Reason = firstMemoryReason(proposal.Reason, resolution.Reason)
		item, retiredIDs, writeErr := l.storeMemoryCandidate(ctx, run, in, candidate, resolution, "")
		if writeErr != nil {
			outcome.Status = "failed"
			outcome.Error = writeErr.Error()
			continue
		}
		accepted = append(accepted, item)
		retiredIndexIDs = append(retiredIndexIDs, retiredIDs...)
		outcome.WrittenCount++
	}
	indexed, indexErr := l.indexMemoryItems(ctx, run, in.UserID, accepted, retiredIndexIDs)
	outcome.IndexedCount = indexed
	if indexErr != nil {
		outcome.Status = "failed"
		outcome.Error = indexErr.Error()
	}
	return outcome, confirmations
}

func (l *Loop) memoryConfirmationTimeout() time.Duration {
	if l.MemoryConfirmationTimeout > 0 {
		return l.MemoryConfirmationTimeout
	}
	return defaultMemoryConfirmationTimeout
}

func (l *Loop) proposeMemories(ctx context.Context, input memory.ExtractionInput) ([]memory.Proposal, error) {
	if l.MemoryCurator != nil {
		// Once hierarchy-aware curation is available, fail this maintenance pass
		// closed. Falling back to the legacy extractor here would silently restore
		// exact attribute matching and could recreate the duplicate-slot bug.
		return l.MemoryCurator.Curate(ctx, input)
	}
	candidates, err := l.MemoryExtractor.Extract(ctx, input)
	if err != nil {
		return nil, err
	}
	proposals := make([]memory.Proposal, 0, len(candidates))
	for _, candidate := range candidates {
		proposals = append(proposals, memory.Proposal{Candidate: candidate})
	}
	return proposals, nil
}

func (l *Loop) applyMemoryProposalPolicy(proposals []memory.Proposal) (memory.PolicyResult, []memory.Proposal) {
	result := memory.PolicyResult{Reasons: make(map[string]int)}
	accepted := make([]memory.Proposal, 0, len(proposals))
	for _, proposal := range proposals {
		if reason := invalidMemoryProposalReason(proposal); reason != "" {
			result.Rejected++
			result.Reasons[reason]++
			continue
		}
		checked := l.MemoryPolicy.Apply([]memory.Candidate{proposal.Candidate})
		if checked.Rejected > 0 {
			result.Rejected += checked.Rejected
			for reason, count := range checked.Reasons {
				result.Reasons[reason] += count
			}
			continue
		}
		result.Accepted = append(result.Accepted, proposal.Candidate)
		accepted = append(accepted, proposal)
	}
	return result, accepted
}

func invalidMemoryProposalReason(proposal memory.Proposal) string {
	if proposal.Operation == "" {
		return ""
	}
	switch proposal.Operation {
	case memory.ProposalAdd:
		if len(proposal.TargetIDs) != 0 {
			return "add_has_targets"
		}
	case memory.ProposalReplace, memory.ProposalCoexist, memory.ProposalNoop, memory.ProposalNeedsConfirmation:
		if len(proposal.TargetIDs) == 0 {
			return "missing_targets"
		}
	default:
		return "unsupported_operation"
	}
	return ""
}

func (l *Loop) resolveMemoryProposal(ctx context.Context, userID, sourceText string, candidate *memory.Candidate, proposal memory.Proposal) (memory.Resolution, bool, error) {
	if candidate == nil {
		return memory.Resolution{}, false, errors.New("memory: proposal candidate is nil")
	}
	switch proposal.Operation {
	case "":
		existing, err := l.findExistingMemories(ctx, userID, *candidate)
		if err != nil {
			return memory.Resolution{}, false, err
		}
		resolution := memory.ResolveCandidate(existing, *candidate)
		return resolution, resolution.Action == memory.ResolutionReplace && memory.NeedsReplacementConfirmation(*candidate, sourceText), nil
	case memory.ProposalNoop:
		targets, err := l.loadMemoryTargets(ctx, userID, candidate.Kind, proposal.TargetIDs)
		if err != nil {
			return memory.Resolution{}, false, err
		}
		return memory.Resolution{Action: memory.ResolutionDuplicate, RelatedIDs: memoryIDs(targets), Reason: firstMemoryReason(proposal.Reason, "curator found an existing semantic duplicate")}, false, nil
	case memory.ProposalReplace, memory.ProposalNeedsConfirmation:
		targets, err := l.loadMemoryTargets(ctx, userID, candidate.Kind, proposal.TargetIDs)
		if err != nil {
			return memory.Resolution{}, false, err
		}
		adoptMemoryLocation(candidate, targets)
		resolution := memory.Resolution{Action: memory.ResolutionReplace, RelatedIDs: memoryIDs(targets), Reason: proposal.Reason}
		needsConfirmation := proposal.Operation == memory.ProposalNeedsConfirmation || !memory.HasExplicitReplacementSignal(sourceText)
		return resolution, needsConfirmation, nil
	case memory.ProposalAdd:
		existing, err := l.findExistingMemories(ctx, userID, *candidate)
		if err != nil {
			return memory.Resolution{}, false, err
		}
		resolution := memory.ResolveCandidate(existing, *candidate)
		if resolution.Action == memory.ResolutionDuplicate {
			return resolution, false, nil
		}
		if resolution.Action == memory.ResolutionConflict || resolution.Action == memory.ResolutionReplace {
			resolution.Action = memory.ResolutionReplace
			resolution.Reason = "add proposal overlaps an existing structured slot"
			return resolution, true, nil
		}
		return memory.Resolution{Action: memory.ResolutionCoexist, Reason: firstMemoryReason(proposal.Reason, "curator found no related memory")}, false, nil
	case memory.ProposalCoexist:
		targets, err := l.loadMemoryTargets(ctx, userID, candidate.Kind, proposal.TargetIDs)
		if err != nil {
			return memory.Resolution{}, false, err
		}
		adoptMemoryLocation(candidate, targets)
		return memory.Resolution{Action: memory.ResolutionCoexist, Reason: firstMemoryReason(proposal.Reason, "curator determined both memories remain valid")}, false, nil
	default:
		return memory.Resolution{}, false, fmt.Errorf("memory: unsupported proposal operation %q", proposal.Operation)
	}
}

func (l *Loop) loadMemoryTargets(ctx context.Context, userID, kind string, ids []string) ([]memory.Memory, error) {
	if len(ids) == 0 {
		return nil, errors.New("memory: replacement proposal requires target_ids")
	}
	seen := make(map[string]struct{}, len(ids))
	targets := make([]memory.Memory, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			return nil, errors.New("memory: replacement target id is empty")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		item, err := l.Memory.Get(ctx, userID, id)
		if err != nil {
			return nil, fmt.Errorf("memory: load proposal target %s: %w", id, err)
		}
		if item.Status != memory.StatusActive && item.Status != memory.StatusConflict {
			return nil, fmt.Errorf("memory: proposal target %s is no longer active", id)
		}
		if item.Kind != kind {
			return nil, fmt.Errorf("memory: proposal target %s has kind %q, candidate has kind %q", id, item.Kind, kind)
		}
		if !item.ExpiresAt.IsZero() && !item.ExpiresAt.After(time.Now().UTC()) {
			return nil, fmt.Errorf("memory: proposal target %s has expired", id)
		}
		targets = append(targets, item)
	}
	return targets, nil
}

func adoptMemoryLocation(candidate *memory.Candidate, targets []memory.Memory) {
	if candidate == nil || len(targets) == 0 {
		return
	}
	// Keep a path the curator deliberately reused. Otherwise choose the first
	// verified target path so label drift does not create another sibling slot.
	for _, target := range targets {
		if strings.EqualFold(strings.TrimSpace(target.Subject), strings.TrimSpace(candidate.Subject)) && strings.EqualFold(strings.TrimSpace(target.Attribute), strings.TrimSpace(candidate.Attribute)) {
			candidate.Subject = target.Subject
			candidate.Attribute = target.Attribute
			return
		}
	}
	candidate.Subject = targets[0].Subject
	candidate.Attribute = targets[0].Attribute
}

func memoryIDs(items []memory.Memory) []string {
	ids := make([]string, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.ID)
	}
	return ids
}

func firstMemoryReason(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return "memory proposal"
}

func (l *Loop) findExistingMemories(ctx context.Context, userID string, candidate memory.Candidate) ([]memory.Memory, error) {
	if relatedStore, ok := l.Memory.(memory.RelatedStore); ok && strings.TrimSpace(candidate.Attribute) != "" {
		return relatedStore.FindRelated(ctx, userID, candidate.Kind, candidate.Subject, candidate.Attribute)
	}
	// 旧版非结构化候选继续通过规范化正文查重。
	return l.Memory.Search(ctx, memory.Query{UserID: userID, Text: candidate.Content, Limit: 8})
}

func (l *Loop) storeMemoryCandidate(ctx context.Context, run *Run, in bus.InboundMessage, candidate memory.Candidate, resolution memory.Resolution, proposalID string) (memory.Memory, []string, error) {
	mutationStore, canMutate := l.Memory.(memory.MutationStore)
	if !canMutate && resolution.Action != memory.ResolutionDuplicate && resolution.Action != memory.ResolutionCoexist {
		resolution = memory.Resolution{Action: memory.ResolutionCoexist, Reason: "store does not support atomic memory mutations"}
	}
	item := memory.Memory{
		ID: memory.NewID("mem"), UserID: in.UserID, Kind: candidate.Kind,
		Subject: candidate.Subject, Attribute: candidate.Attribute, Value: candidate.Value,
		Content: candidate.Content, Status: memory.StatusActive,
		SourceRunID: run.ID, SourceSessionID: in.SessionID, Evidence: candidate.Evidence,
		Confidence: candidate.Confidence, Importance: candidate.Importance,
		ValidFrom: candidate.ValidFrom, ExpiresAt: candidate.ExpiresAt,
		IndexStatus: "pending", CreatedAt: time.Now().UTC(),
	}
	if item.ValidFrom.IsZero() {
		item.ValidFrom = time.Now().UTC()
	}
	mutation := memory.Mutation{Memory: item, Reason: resolution.Reason, ProposalID: proposalID}
	retiredIDs := make([]string, 0)
	switch resolution.Action {
	case memory.ResolutionReplace:
		mutation.SupersedeIDs = resolution.RelatedIDs
		retiredIDs = append(retiredIDs, resolution.RelatedIDs...)
		if len(resolution.RelatedIDs) > 0 {
			item.SupersedesID = resolution.RelatedIDs[0]
			mutation.Memory.SupersedesID = item.SupersedesID
		}
	case memory.ResolutionConflict:
		mutation.ConflictIDs = resolution.RelatedIDs
		retiredIDs = append(retiredIDs, resolution.RelatedIDs...)
		item.Status = memory.StatusConflict
		mutation.Memory.Status = item.Status
	}
	var err error
	if canMutate {
		err = mutationStore.ApplyMutation(ctx, mutation)
	} else {
		err = l.Memory.Upsert(ctx, item)
	}
	if err != nil {
		l.record(ctx, run, tracing.EventMemoryWriteFailed, "failed", map[string]any{"error": err.Error(), "kind": item.Kind, "user_id_hash": hashScope(in.UserID)})
		return memory.Memory{}, nil, err
	}
	l.record(ctx, run, tracing.EventMemoryCandidateAccepted, "accepted", map[string]any{
		"memory_id": item.ID, "kind": item.Kind, "action": resolution.Action,
		"related_memory_ids": resolution.RelatedIDs, "reason": resolution.Reason,
		"user_id_hash": hashScope(in.UserID),
	})
	l.record(ctx, run, tracing.EventMemoryWriteCompleted, "completed", map[string]any{"memory_id": item.ID, "kind": item.Kind, "user_id_hash": hashScope(in.UserID)})
	return item, retiredIDs, nil
}

func (l *Loop) indexMemoryItems(ctx context.Context, run *Run, userID string, accepted []memory.Memory, retiredIndexIDs []string) (int, error) {
	if len(accepted) == 0 || l.MemoryEmbedder == nil || l.MemoryIndexer == nil {
		return 0, nil
	}
	texts := make([]string, len(accepted))
	for i := range accepted {
		texts[i] = accepted[i].Content
	}
	vectors, err := l.MemoryEmbedder.Embed(ctx, texts)
	if err != nil {
		l.markMemoryIndexState(ctx, accepted, "failed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), 0)
		l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "count": len(accepted), "user_id_hash": hashScope(userID)})
		return 0, err
	}
	if len(vectors) != len(accepted) {
		err = fmt.Errorf("embedding count %d does not match memory count %d", len(vectors), len(accepted))
		l.markMemoryIndexState(ctx, accepted, "failed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), 0)
		l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "user_id_hash": hashScope(userID)})
		return 0, err
	}
	dimension := 0
	if len(vectors) > 0 {
		dimension = len(vectors[0])
	}
	records := make([]memory.VectorRecord, 0, len(accepted))
	for i, item := range accepted {
		records = append(records, memory.VectorRecord{ID: item.ID, Vector: vectors[i], Payload: map[string]any{"memory_id": item.ID, "user_id": item.UserID, "kind": item.Kind, "status": item.Status, "source_run_id": item.SourceRunID}})
	}
	if err := l.MemoryIndexer.Upsert(ctx, records); err != nil {
		l.markMemoryIndexState(ctx, accepted, "failed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), dimension)
		l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "count": len(records), "user_id_hash": hashScope(userID)})
		return 0, err
	}
	if len(retiredIndexIDs) > 0 {
		if err := l.MemoryIndexer.Delete(ctx, retiredIndexIDs); err != nil {
			l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "count": len(retiredIndexIDs), "user_id_hash": hashScope(userID)})
			return 0, err
		}
	}
	l.markMemoryIndexState(ctx, accepted, "indexed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), dimension)
	l.record(ctx, run, tracing.EventMemoryIndexCompleted, "completed", map[string]any{"count": len(records), "model": l.MemoryEmbedder.Model(), "version": l.MemoryEmbedder.Version(), "user_id_hash": hashScope(userID)})
	return len(records), nil
}

func (l *Loop) markMemoryIndexState(ctx context.Context, items []memory.Memory, status, model, version string, dimension int) {
	store, ok := l.Memory.(memory.IndexStateStore)
	if !ok {
		return
	}
	for _, item := range items {
		if err := store.MarkIndexed(ctx, item.UserID, item.ID, status, model, version, dimension); err != nil {
			// 索引状态只是运行元数据；即使状态更新失败，也不能把一次
			// 已经成功的向量写入判定为用户 Run 失败。
			continue
		}
	}
}

func hashScope(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:8])
}
