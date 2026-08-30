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
	ctx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), timeout)
	go func() {
		defer cancel()
		run.setMemoryState(MemoryRunState{Status: "running", StartedAt: time.Now().UTC()})
		l.persistSnapshot(run)
		outcome := l.extractAndStoreMemory(ctx, run, in, answer)
		outcome.StartedAt = run.Snapshot().Memory.StartedAt
		outcome.FinishedAt = time.Now().UTC()
		run.setMemoryState(outcome)
		l.persistSnapshot(run)
	}()
}

func (l *Loop) extractAndStoreMemory(ctx context.Context, run *Run, in bus.InboundMessage, answer string) (outcome MemoryRunState) {
	outcome.Status = "completed"
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
		return outcome
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
		candidate = memory.SanitizeChangeHint(candidate, in.Text)
		var existing []memory.Memory
		var searchErr error
		if relatedStore, ok := l.Memory.(memory.RelatedStore); ok && strings.TrimSpace(candidate.Attribute) != "" {
			existing, searchErr = relatedStore.FindRelated(ctx, in.UserID, candidate.Kind, candidate.Subject, candidate.Attribute)
		} else {
			// Legacy candidates without an attribute key retain the old text
			// lookup path and are only deduplicated by normalized content.
			existing, searchErr = l.Memory.Search(ctx, memory.Query{UserID: in.UserID, Text: candidate.Content, Limit: 8})
		}
		resolution := memory.ResolveCandidate(existing, candidate)
		mutationStore, canMutate := l.Memory.(memory.MutationStore)
		if !canMutate && resolution.Action != memory.ResolutionDuplicate && resolution.Action != memory.ResolutionCoexist {
			// A custom legacy Store cannot atomically transition related rows;
			// preserve its previous append-only behavior rather than pretending
			// that a conflict was resolved.
			resolution = memory.Resolution{Action: memory.ResolutionCoexist, Reason: "store does not support atomic memory mutations"}
		}
		if searchErr == nil && resolution.Action == memory.ResolutionDuplicate {
			l.record(ctx, run, tracing.EventMemoryCandidateRejected, "duplicate", map[string]any{"reason": "duplicate", "user_id_hash": hashScope(in.UserID)})
			continue
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
		switch resolution.Action {
		case memory.ResolutionSupersede:
			mutation.SupersedeIDs = resolution.RelatedIDs
			if len(resolution.RelatedIDs) > 0 {
				item.SupersedesID = resolution.RelatedIDs[0]
				mutation.Memory.SupersedesID = item.SupersedesID
			}
		case memory.ResolutionConflict:
			mutation.ConflictIDs = resolution.RelatedIDs
			item.Status = memory.StatusConflict
			mutation.Memory.Status = item.Status
		}
		var writeErr error
		if canMutate {
			writeErr = mutationStore.ApplyMutation(ctx, mutation)
		} else {
			writeErr = l.Memory.Upsert(ctx, item)
		}
		if writeErr != nil {
			outcome.Status = "failed"
			outcome.Error = writeErr.Error()
			l.record(ctx, run, tracing.EventMemoryWriteFailed, "failed", map[string]any{"error": writeErr.Error(), "kind": item.Kind, "user_id_hash": hashScope(in.UserID)})
			continue
		}
		accepted = append(accepted, item)
		if resolution.Action == memory.ResolutionSupersede || resolution.Action == memory.ResolutionConflict {
			retiredIndexIDs = append(retiredIndexIDs, resolution.RelatedIDs...)
		}
		outcome.WrittenCount++
		l.record(ctx, run, tracing.EventMemoryCandidateAccepted, "accepted", map[string]any{
			"memory_id": item.ID, "kind": item.Kind, "action": resolution.Action,
			"related_memory_ids": resolution.RelatedIDs, "reason": resolution.Reason,
			"user_id_hash": hashScope(in.UserID),
		})
		l.record(ctx, run, tracing.EventMemoryWriteCompleted, "completed", map[string]any{"memory_id": item.ID, "kind": item.Kind, "user_id_hash": hashScope(in.UserID)})
	}
	if len(accepted) == 0 || l.MemoryEmbedder == nil || l.MemoryIndexer == nil {
		return outcome
	}
	texts := make([]string, len(accepted))
	for i := range accepted {
		texts[i] = accepted[i].Content
	}
	vectors, err := l.MemoryEmbedder.Embed(ctx, texts)
	if err != nil {
		outcome.Status = "failed"
		outcome.Error = err.Error()
		l.markMemoryIndexState(ctx, accepted, "failed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), 0)
		l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "count": len(accepted), "user_id_hash": hashScope(in.UserID)})
		return outcome
	}
	if len(vectors) != len(accepted) {
		outcome.Status = "failed"
		outcome.Error = fmt.Sprintf("embedding count %d does not match memory count %d", len(vectors), len(accepted))
		l.markMemoryIndexState(ctx, accepted, "failed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), 0)
		l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": fmt.Sprintf("embedding count %d does not match memory count %d", len(vectors), len(accepted)), "user_id_hash": hashScope(in.UserID)})
		return outcome
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
		outcome.Status = "failed"
		outcome.Error = err.Error()
		l.markMemoryIndexState(ctx, accepted, "failed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), dimension)
		l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "count": len(records), "user_id_hash": hashScope(in.UserID)})
		return outcome
	}
	if len(retiredIndexIDs) > 0 {
		if err := l.MemoryIndexer.Delete(ctx, retiredIndexIDs); err != nil {
			outcome.Status = "failed"
			outcome.Error = err.Error()
			l.record(ctx, run, tracing.EventMemoryIndexFailed, "failed", map[string]any{"error": err.Error(), "count": len(retiredIndexIDs), "user_id_hash": hashScope(in.UserID)})
			return outcome
		}
	}
	outcome.IndexedCount = len(records)
	l.markMemoryIndexState(ctx, accepted, "indexed", l.MemoryEmbedder.Model(), l.MemoryEmbedder.Version(), dimension)
	l.record(ctx, run, tracing.EventMemoryIndexCompleted, "completed", map[string]any{"count": len(records), "model": l.MemoryEmbedder.Model(), "version": l.MemoryEmbedder.Version(), "user_id_hash": hashScope(in.UserID)})
	return outcome
}

func (l *Loop) markMemoryIndexState(ctx context.Context, items []memory.Memory, status, model, version string, dimension int) {
	store, ok := l.Memory.(memory.IndexStateStore)
	if !ok {
		return
	}
	for _, item := range items {
		if err := store.MarkIndexed(ctx, item.UserID, item.ID, status, model, version, dimension); err != nil {
			// Index status is operational metadata; an update failure must not
			// turn a successful vector write into a failed user Run.
			continue
		}
	}
}

func hashScope(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:8])
}
