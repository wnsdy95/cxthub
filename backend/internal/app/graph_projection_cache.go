package app

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) cachedGraphState(ctx context.Context, repo domain.ContentHash, view domain.RepositoryView, position string) (domain.GraphState, error) {
	if err := ctx.Err(); err != nil {
		return domain.GraphState{}, err
	}
	projection, err := service.legacyGraphProjection()
	if err != nil {
		return domain.GraphState{}, err
	}
	return projection.Cached(ctx, repo, view, position)
}

func (service *Service) admittedGraphState(ctx context.Context, view domain.RepositoryView, position string) (domain.GraphState, error) {
	projection, err := service.legacyGraphProjection()
	if err != nil {
		return domain.GraphState{}, err
	}
	return projection.Admitted(ctx, view, position)
}
