package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/ziangsun/szabot/internal/bus"
	"github.com/ziangsun/szabot/internal/memory"
	tracing "github.com/ziangsun/szabot/internal/trace"
)

func (l *Loop) startMemoryExtraction(runCtx context.Context, run *Run, in bus.InboundMessage, answer string) {
	if l == nil || l.Memory == nil || l.MemoryExtractor == nil || strings.TrimSpace(in.UserID) == "" {
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
	candidates, err := l.MemoryExtractor.Extract(ctx, memory.ExtractionInput{
		UserID: in.UserID, SessionID: in.SessionID, RunID: run.ID,
		UserText: in.Text, AssistantText: answer, ObservedAt: time.Now().UTC(),
	})
	if err != nil {
		outcome.Status = "failed"
		outcome.Error = err.Error()
		l.recordDuration(ctx, run, tracing.EventMemoryExtractionFailed, "failed", time.Since(started), map[string]any{"error": err.Error(), "user_id_hash": hashScope(in.UserID)})
		return outcome, confirmations
	}
	outcome.CandidateCount = len(candidates)
	l.recordDuration(ctx, run, tracing.EventMemoryExtractionFinished, "completed", time.Since(started), map[string]any{"candidate_count": len(candidates), "user_id_hash": hashScope(in.UserID)})
	policyResult := l.MemoryPolicy.Apply(candidates)
	outcome.RejectedCount = policyResult.Rejected
	l.record(ctx, run, tracing.EventMemoryPolicyApplied, "completed", map[string]any{
		"candidate_count": len(candidates), "accepted_count": len(policyResult.Accepted),
		"rejected_count": policyResult.Rejected, "reasons": policyResult.Reasons, "user_id_hash": hashScope(in.UserID),
	})
	if policyResult.Rejected > 0 {
		l.record(ctx, run, tracing.EventMemoryCandidateRejected, "rejected", map[string]any{"count": policyResult.Rejected, "reasons": policyResult.Reasons, "user_id_hash": hashScope(in.UserID)})
	}
	accepted := make([]memory.Memory, 0, len(policyResult.Accepted))
	retiredIndexIDs := make([]string, 0)
	for _, candidate := range policyResult.Accepted {
		existing, searchErr := l.findExistingMemories(ctx, in.UserID, candidate)
		if searchErr != nil {
			outcome.Status = "failed"
			outcome.Error = searchErr.Error()
			l.record(ctx, run, tracing.EventMemoryCandidateRejected, "lookup_failed", map[string]any{
				"reason": searchErr.Error(), "kind": candidate.Kind, "user_id_hash": hashScope(in.UserID),
			})
			continue
		}
		resolution := memory.ResolveCandidate(existing, candidate)
		if resolution.Action == memory.ResolutionDuplicate {
			l.record(ctx, run, tracing.EventMemoryCandidateRejected, "duplicate", map[string]any{"reason": "duplicate", "user_id_hash": hashScope(in.UserID)})
			continue
		}
		if resolution.Action == memory.ResolutionSupersede && memory.NeedsReplacementConfirmation(candidate, in.Text) {
			confirmations = append(confirmations, &memoryConfirmationRequest{
				SourceRun: run, Inbound: in, Candidate: candidate,
			})
			continue
		}
		item, retiredIDs, writeErr := l.storeMemoryCandidate(ctx, run, in, candidate, resolution)
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

func (l *Loop) findExistingMemories(ctx context.Context, userID string, candidate memory.Candidate) ([]memory.Memory, error) {
	if relatedStore, ok := l.Memory.(memory.RelatedStore); ok && strings.TrimSpace(candidate.Attribute) != "" {
		return relatedStore.FindRelated(ctx, userID, candidate.Kind, candidate.Subject, candidate.Attribute)
	}
	// 旧版非结构化候选继续通过规范化正文查重。
	return l.Memory.Search(ctx, memory.Query{UserID: userID, Text: candidate.Content, Limit: 8})
}

func (l *Loop) storeMemoryCandidate(ctx context.Context, run *Run, in bus.InboundMessage, candidate memory.Candidate, resolution memory.Resolution) (memory.Memory, []string, error) {
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
	mutation := memory.Mutation{Memory: item, Reason: resolution.Reason}
	retiredIDs := make([]string, 0)
	switch resolution.Action {
	case memory.ResolutionSupersede:
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
