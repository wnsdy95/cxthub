package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// initialMemorySelections omits only an explicitly superseded empty position
// from selection candidates. Both immutable events remain in the proof. All
// other empty/nonempty pairs still reach recordedMemorySelection unchanged.
// Events have already passed history validation and dependency ordering.
func (s *ContextHistoryService) initialMemorySelections(ctx context.Context, events []domain.HistoryEvent) ([]domain.HistoryEvent, error) {
	byID := make(map[string]domain.HistoryEvent, len(events))
	for _, e := range events {
		byID[e.ID] = e
	}
	superseded := map[string]bool{}
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if e.MemorySelectionParent == "" {
			continue
		}
		if !domain.IsInitialMemorySelection(byID[e.MemorySelectionParent], e) {
			return nil, domain.ErrHashMismatch
		}
		digest, err := s.store.GetMemory(ctx, e.MemoryHash)
		if err != nil {
			return nil, err
		}
		hash, err := domain.MemoryDigestHash(digest)
		if err != nil || hash != e.MemoryHash || digest.SnapshotID != e.Target {
			return nil, domain.ErrHashMismatch
		}
		if digest.PreviousMemoryHash != "" {
			return nil, domain.ErrHashMismatch
		}
		superseded[e.MemorySelectionParent] = true
	}
	selected := make([]domain.HistoryEvent, 0, len(events))
	for _, e := range events {
		if !superseded[e.ID] {
			selected = append(selected, e)
		}
	}
	return selected, nil
}

// RewriteHistorySources projects alias candidates without modifying retained
// evidence. Dependency and payload checks precede selection, even if a malformed
// row is outside the requested worktree. Digest reads stay within that scope.
func (s *ContextHistoryService) RewriteHistorySources(ctx context.Context, events []domain.HistoryEvent, branchID, worktreeID string) ([]domain.HistoryEvent, error) {
	for _, e := range events {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := domain.ValidateHistoryEvent(e); err != nil {
			return nil, err
		}
	}
	ordered, err := domain.OrderHistoryEvents(events)
	if err != nil {
		return nil, err
	}
	var sources []domain.HistoryEvent
	for _, e := range ordered {
		if e.BranchID == branchID && e.WorktreeID == worktreeID {
			sources = append(sources, e)
		}
	}
	return s.initialMemorySelections(ctx, sources)
}
