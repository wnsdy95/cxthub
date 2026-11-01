package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

var _ inbound.PreparedMemoryQuery = (*Service)(nil)

func (service *Service) ConfigurePreparedMemoryReads(repos []domain.ContentHash) error {
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
		if _, supported := service.meta.(outbound.MinimumMemoryReadStore); !supported {
			return domain.ErrCatalogUnsupported
		}
	}
	service.preparedMemoryRepos = configured
	return nil
}

type preparedMemoryCursor = memoryquery.PreparedCursor

func encodePreparedMemoryCursor(cursor preparedMemoryCursor) (string, error) {
	return memoryquery.EncodePreparedCursor(cursor)
}

func decodePreparedMemoryCursor(encoded string, target domain.MemoryReadTarget) (preparedMemoryCursor, error) {
	return memoryquery.DecodePreparedCursor(encoded, target)
}

func (service *Service) QueryPreparedMemory(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.MemoryReadPage, error) {
	queries, err := service.PreparedMemoryQueries()
	if err != nil {
		return domain.MemoryReadPage{}, err
	}
	return queries.QueryPreparedMemory(ctx, repo, request)
}
