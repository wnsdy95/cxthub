package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func (service *Service) GetPendingState(ctx context.Context, repo domain.ContentHash) (domain.PendingStateView, error) {
	queries, err := service.RepositoryStateQueries()
	if err != nil {
		return domain.PendingStateView{}, err
	}
	return queries.GetPendingState(ctx, repo)
}

var _ inbound.PendingStateQuery = (*Service)(nil)
