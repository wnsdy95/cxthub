package storage

import (
	"context"
	"errors"
	"reflect"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

var _ outbound.WorkingPositionCASStore = (*FileStore)(nil)
var _ outbound.WorkingCodePositionCASStore = (*FileStore)(nil)

// CompareAndSwapWorkingPosition never moves a shared branch. Retention roots
// and the previous selection are persisted before the atomic position write;
// an interruption can only leave the old position or the complete new one.
func (s *FileStore) CompareAndSwapWorkingPosition(ctx context.Context, expected, next domain.WorkingPosition, expectedRef domain.Ref) error {
	return s.compareAndSwapWorkingPosition(ctx, expected, next, expectedRef, false)
}

func (s *FileStore) CompareAndSwapWorkingCodePosition(ctx context.Context, expected, next domain.WorkingPosition, expectedRef domain.Ref) error {
	return s.compareAndSwapWorkingPosition(ctx, expected, next, expectedRef, true)
}

func (s *FileStore) compareAndSwapWorkingPosition(ctx context.Context, expected, next domain.WorkingPosition, expectedRef domain.Ref, codeMove bool) error {
	if (codeMove && expected.GitCommit == next.GitCommit) || (!codeMove && expected.GitCommit != next.GitCommit) {
		return domain.ErrSyncConflict
	}
	if s.worktreeID == "" || expected.WorktreeID != s.worktreeID || expected.GitBranch() != s.gitBranch || next.GitCommit != s.gitCommit ||
		next.WorktreeID != expected.WorktreeID || next.RepoID != expected.RepoID || next.Branch != expected.Branch || next.BranchID != expected.BranchID ||
		next.GitBranch() != expected.GitBranch() || expected.Branch == "" || expected.GitCommit == "" || expected.Orphan || next.Orphan ||
		expectedRef.Kind != domain.RefBranch || expectedRef.Symbolic != "" || expectedRef.RepoID != expected.RepoID || expectedRef.Name != expected.Branch || next.SharedTarget != expectedRef.Target {
		return domain.ErrSyncConflict
	}
	if err := domain.ValidateRef(expectedRef); err != nil {
		return err
	}
	identity := expectedRef.BranchID
	if identity == "" {
		identity = domain.LegacyContextBranchID(expectedRef.RepoID, expectedRef.Name)
	}
	if identity != expected.BranchID {
		return domain.ErrSyncConflict
	}
	return s.withRefMutationLock(ctx, func() error {
		current, err := s.readPosition()
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrSyncConflict
		}
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(current, expected) {
			return domain.ErrSyncConflict
		}
		// Read the logical projection too: an archived branch can retain its raw
		// file after interruption, but must not authorize a new selection.
		ref, err := s.GetRef(ctx, expectedRef.RepoID, expectedRef.Kind, expectedRef.Name)
		if errors.Is(err, domain.ErrNotFound) {
			return domain.ErrSyncConflict
		}
		if err != nil {
			return err
		}
		if ref != expectedRef {
			return domain.ErrSyncConflict
		}
		if next.Selection == nil || next.Selection.Source != expected.Snapshot || next.Selection.GitBefore != expected.GitCommit {
			return domain.ErrHashMismatch
		}
		if err := s.validateConditionalPosition(ctx, next); err != nil {
			return err
		}
		return s.writePosition(next)
	})
}

func (s *FileStore) validateConditionalPosition(ctx context.Context, p domain.WorkingPosition) error {
	if p.Snapshot == "" || !p.MemoryPinned || p.Selection == nil {
		return domain.ErrHashMismatch
	}
	e := p.Selection
	if err := domain.ValidateHistoryEvent(*e); err != nil {
		return err
	}
	if e.Kind != "position" || e.RepoID != p.RepoID || e.Branch != p.Branch || e.BranchID != p.BranchID || e.LocalBranch != p.LocalBranch || e.GitAfter != p.GitCommit ||
		e.Target != p.Snapshot || e.MemoryHash != p.MemoryHash || e.MemorySource != p.MemorySource || !e.MemoryPinned || (e.WorktreeID != "" && e.WorktreeID != p.WorktreeID) {
		return domain.ErrHashMismatch
	}
	for _, id := range []domain.ContentHash{p.Snapshot, p.SharedTarget, p.MemorySource} {
		if id == "" {
			continue
		}
		snap, err := s.GetSnapshot(ctx, id)
		if err != nil {
			return err
		}
		if snap.RepoID != p.RepoID {
			return domain.ErrHashMismatch
		}
		if _, err := s.GetDoc(ctx, snap.DocHash); err != nil {
			return err
		}
	}
	if p.MemoryHash != "" {
		memory, err := s.GetMemory(ctx, p.MemoryHash)
		if err != nil {
			return err
		}
		if memory.SnapshotID != p.Snapshot && memory.SnapshotID != p.MemorySource {
			return domain.ErrHashMismatch
		}
	}
	return nil
}
