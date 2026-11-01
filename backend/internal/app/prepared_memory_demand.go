package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.PreparedMemoryDemand = (*Service)(nil)

func (service *Service) RequestPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) error {
	queries, err := service.PreparedMemoryQueries()
	if err != nil {
		return err
	}
	return queries.RequestPreparedMemory(ctx, repo, request)
}

func (service *Service) ConfigurePreparedMemoryDemands(repos []domain.ContentHash) error {
	if len(repos) > domain.MaxMemoryReadWorkerRepos {
		return domain.ErrValidation
	}
	configured := make(map[domain.ContentHash]bool, len(repos))
	for _, repo := range repos {
		if err := domain.ValidateContentHash(repo); err != nil {
			return err
		}
		configured[repo] = true
	}
	if len(configured) != 0 {
		if _, supported := service.meta.(outbound.MemoryReadDemandStore); !supported {
			return domain.ErrCatalogUnsupported
		}
	}
	service.preparedMemoryDemands = configured
	return nil
}
