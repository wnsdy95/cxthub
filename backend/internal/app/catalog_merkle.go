package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.CatalogMerkleQuery = (*Service)(nil)
var _ inbound.CatalogMerkleCapabilities = (*Service)(nil)

func (s *Service) CatalogMerkleVersion() int {
	if _, ok := s.meta.(outbound.CatalogMerkleStore); ok && s.CatalogVersion() == domain.CatalogVersion {
		return 1
	}
	return 0
}

// The adapter owns fixed committed source reads and separate immutable cache
// publication. Wrapping this in a source repository transaction would couple
// potentially large hash construction to business mutations.
func (s *Service) CatalogMerkle(ctx context.Context, repo domain.ContentHash, request domain.CatalogMerkleRequest) (domain.CatalogMerklePage, error) {
	var zero domain.CatalogMerklePage
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if domain.ValidateContentHash(repo) != nil {
		return zero, domain.ErrValidation
	}
	if err := request.Validate(); err != nil {
		return zero, err
	}
	store, ok := s.meta.(outbound.CatalogMerkleStore)
	if !ok {
		return zero, domain.ErrCatalogUnsupported
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
	return store.CatalogMerkle(ctx, repo, request)
}
