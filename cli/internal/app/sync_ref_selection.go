package app

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

func selectSyncRefs(refs []domain.Ref, branch string) ([]domain.Ref, error) {
	if branch == "" {
		return refs, nil
	}
	if domain.ValidateBranchName(branch) != nil {
		return nil, domain.ErrInvalidRef
	}
	var selected []domain.Ref
	found := false
	for _, ref := range refs {
		if ref.Kind == domain.RefBranch && ref.Name == branch {
			selected = append(selected, ref)
			found = true
			continue
		}
		event, lifecycle, err := domain.ParseBranchLifecycleRef(ref)
		if err != nil {
			return nil, err
		}
		if lifecycle && event.Branch == branch {
			selected = append(selected, ref)
		}
	}
	if !found {
		return nil, domain.ErrNotFound
	}
	return selected, nil
}

// Deferred PR promotions can change another branch. An explicitly scoped push
// leaves those jobs queued for the ordinary repository-wide synchronization.
func (s *SyncRepoService) flushPRDeliveriesForSelection(ctx context.Context, repo, branch string) error {
	if branch != "" {
		return nil
	}
	return s.flushPRDeliveries(ctx, repo)
}
