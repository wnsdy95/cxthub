package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) QueryContext(ctx context.Context, repo domain.ContentHash, selection domain.ContextSelection) (domain.ContextQueryView, error) {
	queries, err := service.ContextQueries()
	if err != nil {
		return domain.ContextQueryView{}, err
	}
	return queries.QueryContext(ctx, repo, selection)
}
