package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) PrepareIncrementalMemoryRead(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, expiresAt time.Time, previous *PreparedMemoryRead) (*PreparedMemoryRead, error) {
	return service.memoryPreparation().PrepareIncrementalMemoryRead(ctx, repo, request, expiresAt, previous)
}

func (service *Service) prepareIncrementalMemoryRead(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, expiresAt time.Time, previous *PreparedMemoryRead, compare bool) (*PreparedMemoryRead, error) {
	return service.memoryPreparation().prepareIncrementalMemoryRead(ctx, repo, request, expiresAt, previous, compare)
}

func (service *Service) findMemoryGraphAnchor(ctx context.Context, repo domain.ContentHash, expiresAt time.Time, previous *PreparedMemoryRead) (*memoryGraphAnchor, error) {
	return service.memoryPreparation().findMemoryGraphAnchor(ctx, repo, expiresAt, previous)
}

func (service *Service) PrepareMemoryRead(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest, previous *CapturedGraphSource) (*PreparedMemoryRead, error) {
	return service.memoryPreparation().PrepareMemoryRead(ctx, repo, request, previous)
}

func (service *Service) PublishPreparedMemoryRead(ctx context.Context, prepared *PreparedMemoryRead, expiresAt time.Time) (domain.ContentHash, error) {
	return service.memoryPreparation().PublishPreparedMemoryRead(ctx, prepared, expiresAt)
}

func (service *Service) persistPreparedMemoryRead(ctx context.Context, prepared *PreparedMemoryRead, expiresAt time.Time) error {
	return service.memoryPreparation().persistPreparedMemoryRead(ctx, prepared, expiresAt)
}

func (service *Service) memoryReadReference(ctx context.Context, repo domain.ContentHash, request domain.EffectiveMemoryRequest) (domain.ContentHash, error) {
	return service.memoryPreparation().memoryReadReference(ctx, repo, request)
}
