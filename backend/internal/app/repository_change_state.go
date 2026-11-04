package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (service *Service) RepositoryChangeState(ctx context.Context, repo domain.ContentHash) (domain.RepositoryChangeState, error) {
	queries, err := service.RepositoryStateQueries()
	if err != nil {
		return domain.RepositoryChangeState{}, err
	}
	return queries.RepositoryChangeState(ctx, repo)
}

var _ inbound.RepositoryChangeQuery = (*Service)(nil)
