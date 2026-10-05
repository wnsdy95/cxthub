package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.BranchPullPlanner = (*Service)(nil)

// Only a store-owned repository read snapshot can provide one coherent plan.
func (s *Service) BranchPullVersion() int {
	if _, ok := s.meta.(outbound.RepositoryTransactions); ok {
		return domain.BranchPullVersion
	}
	return 0
}

func (s *Service) PullBranchPlan(ctx context.Context, repoID domain.ContentHash, request domain.BranchPullRequest) (domain.BranchPullPlan, error) {
	if domain.ValidateContentHash(repoID) != nil {
		return domain.BranchPullPlan{}, domain.ErrValidation
	}
	if err := request.Validate(); err != nil {
		return domain.BranchPullPlan{}, err
	}
	if s.BranchPullVersion() == 0 {
		return domain.BranchPullPlan{}, domain.ErrBranchPullUnsupported
	}
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.BranchPullPlan, error) {
		var zero domain.BranchPullPlan
		repo, err := s.meta.GetRepo(ctx, repoID)
		if err != nil {
			return zero, err
		}
		if repo.ID != repoID {
			return zero, domain.ErrIntegrity
		}
		refs, err := s.meta.ListRefs(ctx, repoID)
		if err != nil {
			return zero, err
		}
		history, err := s.ListHistory(ctx, repoID)
		if err != nil {
			return zero, err
		}
		snaps, err := s.meta.ListSnapshots(ctx, repoID, "")
		if err != nil {
			return zero, err
		}
		// The selected metadata tokens freeze attachment hashes; history pins
		// are carried unchanged. Memory bodies are verified during client fetch.
		return domain.SelectBranchPullDependencies(ctx, repo, request, refs, history, snaps)
	})
}
