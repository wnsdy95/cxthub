package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) memoryProjectionReader() MemoryProjectionSource {
	if service.projectionMemory != nil {
		return service.projectionMemory
	}
	return serviceProjectionSource{service.meta, service.blobs}
}

func (service *Service) memoryMetadataReader() outbound.MemoryMetadataReader {
	if service.projectionMemory != nil {
		return service.projectionMemory
	}
	return service.meta
}

func (service *Service) memoryPinReader() any {
	if service.projectionMemory != nil {
		return service.projectionMemory
	}
	return service.meta
}

func (service *Service) memoryHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	if service.projectionMemory != nil {
		return service.projectionMemory.ListHistoryEvents(ctx, repo)
	}
	return service.ListHistory(ctx, repo)
}

func (service *Service) memoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if service.projectionMemory != nil {
		return service.projectionMemory.RepositoryRevision(ctx, repo)
	}
	return service.RepositoryRevision(ctx, repo)
}
