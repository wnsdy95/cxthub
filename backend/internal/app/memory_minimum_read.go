package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) ObserveMemoryReadRequirement(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadRequirement, error) {
	queries, _, err := service.minimumMemoryQueries()
	if err != nil {
		return domain.MemoryReadRequirement{}, err
	}
	return queries.ObserveMemoryReadRequirement(ctx, repo, request)
}

func (service *Service) QueryMinimumMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, requirement domain.MemoryReadRequirement, generation domain.ContentHash) (domain.MemoryReadPage, error) {
	queries, _, err := service.minimumMemoryQueries()
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	return queries.QueryMinimumMemory(ctx, repo, request, requirement, generation)
}
