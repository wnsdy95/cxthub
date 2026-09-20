package app

import (
	"context"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ inbound.ContextHistoryReconciler = (*ContextHistoryService)(nil)
var _ inbound.ContextCodeMoveReconciler = (*ContextHistoryService)(nil)

// SelectPositionIfCurrent applies a caller-proven reconciliation, not a request
// to follow the latest branch. The caller owns eligibility (including historical
// selections); this service validates immutable content and pins memory, while
// storage compares both observations under the same mutation lock.
func (s *ContextHistoryService) SelectPositionIfCurrent(ctx context.Context, expected, next domain.WorkingPosition, expectedRef domain.Ref) error {
	return s.selectPositionIfCurrent(ctx, expected, next, expectedRef, false)
}

// SelectPositionAfterCodeMove applies a caller-proven forward Git move whose
// context selection was deferred. Same-code manual pins use the stricter port.
func (s *ContextHistoryService) SelectPositionAfterCodeMove(ctx context.Context, expected, next domain.WorkingPosition, expectedRef domain.Ref) error {
	return s.selectPositionIfCurrent(ctx, expected, next, expectedRef, true)
}

func (s *ContextHistoryService) selectPositionIfCurrent(ctx context.Context, expected, next domain.WorkingPosition, expectedRef domain.Ref, codeMove bool) error {
	positions, ok := s.store.(outbound.WorkingPositionCASStore)
	if !ok {
		return fmt.Errorf("conditional working position store unavailable")
	}
	compare := positions.CompareAndSwapWorkingPosition
	if codeMove {
		mover, ok := s.store.(outbound.WorkingCodePositionCASStore)
		if !ok {
			return fmt.Errorf("conditional code position store unavailable")
		}
		compare = mover.CompareAndSwapWorkingCodePosition
	}
	if (codeMove && expected.GitCommit == next.GitCommit) || (!codeMove && expected.GitCommit != next.GitCommit) {
		return domain.ErrSyncConflict
	}
	if err := domain.ValidateRef(expectedRef); err != nil {
		return err
	}
	if expected.WorktreeID == "" || expected.Branch == "" || expected.GitCommit == "" || expected.Orphan || next.Orphan ||
		expected.RepoID != next.RepoID || expected.Branch != next.Branch || expected.GitBranch() != next.GitBranch() ||
		(next.WorktreeID != "" && next.WorktreeID != expected.WorktreeID) || (next.BranchID != "" && next.BranchID != expected.BranchID) ||
		expectedRef.Kind != domain.RefBranch || expectedRef.Symbolic != "" || expectedRef.RepoID != expected.RepoID || expectedRef.Name != expected.Branch {
		return domain.ErrSyncConflict
	}
	refIdentity := expectedRef.BranchID
	if refIdentity == "" {
		refIdentity = domain.LegacyContextBranchID(expectedRef.RepoID, expectedRef.Name)
	}
	if refIdentity != expected.BranchID {
		return domain.ErrSyncConflict
	}
	// An exact reconciliation must carry an explicit memory selection, including
	// known-empty memory; do not infer a newer mutable attachment from its target.
	if !next.MemoryPinned || next.Snapshot == "" {
		return fmt.Errorf("conditional position requires a snapshot and pinned memory selection")
	}
	prepared, err := s.preparePosition(ctx, next, &expectedRef)
	if err != nil {
		return err
	}
	if prepared.BranchID != expected.BranchID {
		return domain.ErrSyncConflict
	}
	prepared.WorktreeID = expected.WorktreeID
	prepared, err = positionSelection(expected, prepared)
	if err != nil {
		return err
	}
	return compare(ctx, expected, prepared, expectedRef)
}
