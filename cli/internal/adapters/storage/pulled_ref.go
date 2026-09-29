package storage

import (
	"context"
	"errors"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *FileStore) CompareAndSwapPulledRef(ctx context.Context, expected *domain.Ref, next domain.Ref) error {
	if err := domain.ValidateRef(next); err != nil {
		return err
	}
	if next.Kind == domain.RefHEAD || next.Symbolic != "" {
		return domain.ErrInvalidRef
	}
	if expected != nil && (expected.RepoID != next.RepoID || expected.Kind != next.Kind || expected.Name != next.Name) {
		return domain.ErrInvalidRef
	}
	return s.withRefMutationLock(ctx, func() error {
		current, err := s.GetRef(ctx, next.RepoID, next.Kind, next.Name)
		if expected == nil {
			if err == nil {
				return domain.ErrSyncConflict
			}
			if !errors.Is(err, domain.ErrNotFound) {
				return err
			}
			if next.Kind == domain.RefBranch {
				refs, err := s.listRefsRaw(ctx, next.RepoID)
				if err != nil {
					return err
				}
				last, found, err := domain.LatestBranchLifecycle(refs, next.Name)
				if err != nil {
					return err
				}
				if found && last.State == domain.BranchArchived {
					return domain.ErrBranchArchived
				}
			}
		} else {
			if errors.Is(err, domain.ErrNotFound) {
				return domain.ErrSyncConflict
			}
			if err != nil {
				return err
			}
			if current != *expected {
				return domain.ErrSyncConflict
			}
			if current.Target != next.Target {
				// Retain the exact superseded observation before changing a shared ref.
				// A failed later CAS/write can leave only an extra retention root.
				key := domain.HashContent([]byte(string(current.Kind) + "\x00" + current.Name + "\x00" + string(current.Target)))
				retained := domain.Ref{Kind: domain.RefTag, Name: "cxt-retained/pull/" + hexOf(key), RepoID: next.RepoID, Target: current.Target}
				if err := s.putRefRaw(retained); err != nil {
					return err
				}
			}
		}
		return s.putRefRaw(next)
	})
}

var _ outbound.PulledRefCASStore = (*FileStore)(nil)
