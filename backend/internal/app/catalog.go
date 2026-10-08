package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.CatalogQuery = (*Service)(nil)
var _ inbound.CatalogCapabilities = (*Service)(nil)

// CatalogVersion advertises only store-owned catalogs with a coherent read snapshot.
func (s *Service) CatalogVersion() int {
	_, catalog := s.meta.(outbound.CatalogStore)
	_, transactional := s.meta.(outbound.RepositoryTransactions)
	if catalog && transactional {
		return domain.CatalogVersion
	}
	return 0
}

// CatalogChanges preserves store-owned pagination and original metadata images.
// In particular, it does not substitute graph projections or object reads.
func (s *Service) CatalogChanges(ctx context.Context, repo domain.ContentHash, request domain.CatalogRequest) (domain.CatalogPage, error) {
	var zero domain.CatalogPage
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if domain.ValidateContentHash(repo) != nil {
		return zero, domain.ErrValidation
	}
	if err := request.Validate(); err != nil {
		return zero, err
	}
	store, ok := s.meta.(outbound.CatalogStore)
	if !ok {
		return zero, domain.ErrCatalogUnsupported
	}
	if request.Limit == 0 {
		request.Limit = domain.DefaultCatalogLimit
	}
	return repositoryReadForRepo(ctx, s, repo, func(bound context.Context) (domain.CatalogPage, error) {
		return store.CatalogChanges(bound, repo, request)
	})
}
