package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var _ inbound.DocumentFindFragmentQuery = (*Service)(nil)

func (service *Service) ReadDocFindFragment(ctx context.Context, repo, hash domain.ContentHash, request domain.DocFindFragmentRequest) (domain.DocFindFragmentPage, error) {
	queries, err := service.DocumentQueries()
	if err != nil {
		return domain.DocFindFragmentPage{}, err
	}
	return queries.ReadDocFindFragment(ctx, repo, hash, request)
}
