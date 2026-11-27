package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func reauthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	return ctx, ctx.Err()
}

func (service *Service) ReadDocViewEvents(ctx context.Context, repo, hash domain.ContentHash, offset, limit int, view string) (domain.DocViewPage, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocViewPage{}, err
	}
	return queries.ReadDocViewEvents(ctx, repo, hash, offset, limit, view)
}

func (service *Service) ReadDocViewFragment(ctx context.Context, repo, hash domain.ContentHash, index, offset, budget int) (domain.DocViewFragment, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocViewFragment{}, err
	}
	return queries.ReadDocViewFragment(ctx, repo, hash, index, offset, budget)
}
