package app

import (
	"context"
	"log"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) ConfigureDocumentDisplayMaintenance(storage outbound.DocDisplayMaintenanceStore) {
	service.docDisplayMaintenance = storage
}

func (service *Service) ProcessDocDisplayMaintenance(ctx context.Context, limit int) error {
	_, err := service.processDocDisplayMaintenance(ctx, limit)
	return err
}

func (service *Service) processDocDisplayMaintenance(ctx context.Context, limit int) (int, error) {
	if outbound.DocReadOnly(ctx) {
		return 0, domain.ErrForbidden
	}
	if limit <= 0 || service.docDisplayMaintenance == nil {
		return 0, nil
	}
	worker, err := NewDisplayMaintenanceWorker(service.docDisplayMaintenance)
	if err != nil {
		return 0, err
	}
	return worker.Process(ctx, limit)
}

func (service *Service) RunDocDisplayMaintenanceWorker(ctx context.Context) {
	if service.docDisplayMaintenance == nil {
		return
	}
	worker, err := NewDisplayMaintenanceWorker(service.docDisplayMaintenance)
	if err != nil {
		if ctx.Err() == nil {
			log.Printf("document display maintenance: %v", err)
		}
		return
	}
	worker.Run(ctx)
}
