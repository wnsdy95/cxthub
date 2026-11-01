package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadTarget(repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadTarget, error) {
	return memoryquery.ReadTarget(repo, request)
}

func (service *Service) RequestMemoryReadPreparation(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadWork, error) {
	if _, system := inbound.RepositoryActor(ctx); !system || inbound.DocumentReadAuthorizer(ctx) != nil {
		return domain.MemoryReadWork{}, domain.ErrForbidden
	}
	if request.Cursor != "" {
		return domain.MemoryReadWork{}, domain.ErrValidation
	}
	target, err := memoryReadTarget(repo, request)
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	storage, supported := service.meta.(outbound.MemoryReadWorkStore)
	if !supported {
		return domain.MemoryReadWork{}, domain.ErrCatalogUnsupported
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), service.DocumentIdentitiesSupported())
	return storage.RequestMemoryReadWork(ctx, target)
}

func (service *Service) ReadMemoryPreparationStatus(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadWork, error) {
	queries, err := service.MemoryServingQueries()
	if err != nil {
		return domain.MemoryReadWork{}, err
	}
	return queries.ReadMemoryPreparationStatus(ctx, repo, request)
}

func (service *Service) QueryCurrentMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.EffectiveMemoryPage, error) {
	queries, err := service.MemoryServingQueries()
	if err != nil {
		return domain.EffectiveMemoryPage{}, err
	}
	return queries.QueryCurrentMemory(ctx, repo, request)
}
