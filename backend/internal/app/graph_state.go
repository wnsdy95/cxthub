package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (service *Service) QueryGraphState(ctx context.Context, repo domain.ContentHash, position string) (domain.GraphState, error) {
	if err := domain.ValidateContentHash(repo); err != nil {
		return domain.GraphState{}, err
	}
	queries, err := service.GraphQueries()
	if err != nil {
		return domain.GraphState{}, err
	}
	return queries.QueryGraphState(ctx, repo, position)
}

var _ inbound.RepositoryGraphQueries = (*Service)(nil)
