package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (s *Service) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GraphState{}, err
	}
	ctx, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		return domain.GraphState{}, err
	}
	defer release()
	return repositoryReadForRepo(ctx, s, repo, func(ctx context.Context) (domain.GraphState, error) {
		v, err := s.loadRepositoryView(ctx, repo)
		if err != nil {
			return domain.GraphState{}, err
		}
		return s.cachedGraphState(ctx, repo, v, position)
	})
}

var _ inbound.GraphStateQuery = (*Service)(nil)
