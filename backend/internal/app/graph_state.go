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
	return repositoryRead(ctx, s, func(ctx context.Context) (domain.GraphState, error) {
		v, err := s.loadRepositoryView(ctx, repo)
		if err != nil {
			return domain.GraphState{}, err
		}
		return domain.ProjectGraphState(v, v.DefaultBranch, position)
	})
}

var _ inbound.GraphStateQuery = (*Service)(nil)
