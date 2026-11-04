package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ProcessMemoryReadPreparations(ctx context.Context, limit int) error {
	return service.memoryPreparation().ProcessMemoryReadPreparations(ctx, limit)
}

func (service *Service) processMemoryReadPreparations(ctx context.Context, limit int, anchor **PreparedMemoryRead) error {
	return service.memoryPreparation().processMemoryReadPreparations(ctx, limit, anchor)
}

func (service *Service) runMemoryReadPreparation(ctx context.Context, storage outbound.MemoryReadWorkStore, job domain.MemoryReadWork, anchor **PreparedMemoryRead) error {
	return service.memoryPreparation().runMemoryReadPreparation(ctx, storage, job, anchor)
}

func (service *Service) prepareMemoryReadWork(ctx context.Context, job domain.MemoryReadWork, anchor **PreparedMemoryRead) error {
	return service.memoryPreparation().prepareMemoryReadWork(ctx, job, anchor)
}

func (service *Service) RunMemoryReadPreparationWorker(ctx context.Context) {
	service.memoryPreparation().RunMemoryReadPreparationWorker(ctx)
}
