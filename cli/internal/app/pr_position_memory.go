package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// ResolvePRSourcePosition resolves the recorded memory of a completed PR's
// frozen source. It does not select a worktree or read mutable memory pointers.
// The result is the causal maximum among exact pinned source observations;
// receipts without an observation ID cannot identify a publication-time cutoff.
func (s *ContextHistoryService) ResolvePRSourcePosition(ctx context.Context, receipt domain.HistoryEvent) (domain.WorkingPosition, error) {
	events, err := s.history.ListHistoryEvents(ctx, receipt.RepoID)
	if err != nil {
		return domain.WorkingPosition{}, err
	}
	return s.ResolvePRSourcePositionFromHistory(ctx, receipt, events)
}

// ResolvePRSourcePositionFromHistory uses the caller's verified observation.
// It neither rereads a newer observation nor adopts remote history locally.
func (s *ContextHistoryService) ResolvePRSourcePositionFromHistory(ctx context.Context, receipt domain.HistoryEvent, events []domain.HistoryEvent) (domain.WorkingPosition, error) {
	var zero domain.WorkingPosition
	if err := domain.ValidateHistoryEvent(receipt); err != nil {
		return zero, fmt.Errorf("invalid PR receipt: %w", err)
	}
	if receipt.Kind != "pr-merge" || !receipt.PRCompleted {
		return zero, fmt.Errorf("%w: completed PR receipt required", domain.ErrSyncConflict)
	}
	// Completion.Target may contain later work. Validate only the frozen source,
	// with memory explicitly pinned so validation cannot inherit a mutable pointer.
	proof := domain.HistoryEvent{ID: receipt.ID, RepoID: receipt.RepoID, BranchID: receipt.SourceBranchID,
		Branch: receipt.PR.HeadBranch, Kind: "position", Source: receipt.Source, Target: receipt.Source,
		GitAfter: receipt.PR.HeadSHA, MemoryPinned: true, CreatedAt: receipt.CreatedAt}
	if _, err := s.ValidateHistorySource(ctx, proof); err != nil {
		return zero, err
	}
	var err error
	events, err = domain.OrderHistoryEvents(events)
	if err != nil {
		return zero, err
	}
	var candidates []domain.WorkingPosition
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		if !domain.IsPinnedContextEvent(e) || e.RepoID != receipt.RepoID ||
			e.BranchID != receipt.SourceBranchID || e.GitAfter != receipt.PR.HeadSHA || e.Target != receipt.Source {
			continue
		}
		if _, err := s.ValidateHistorySource(ctx, e); err != nil {
			return zero, fmt.Errorf("PR source observation %s: %w", e.ID, err)
		}
		candidates = append(candidates, domain.WorkingPosition{RepoID: receipt.RepoID, Branch: receipt.Branch, BranchID: receipt.BranchID,
			GitCommit: receipt.PR.MergeSHA, Snapshot: receipt.Source, MemoryHash: e.MemoryHash, MemorySource: e.MemorySource, MemoryPinned: true})
	}
	p, err := s.recordedMemorySelection(ctx, candidates)
	if err != nil {
		return zero, err
	}
	proof.MemoryHash, proof.MemorySource = p.MemoryHash, p.MemorySource
	if _, err := s.ValidateHistorySource(ctx, proof); err != nil {
		return zero, err
	}
	return p, nil
}
