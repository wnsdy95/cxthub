package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/displaymaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type displayMaintenanceScope struct{}

func NewDisplayMaintenanceWorker(storage outbound.DocDisplayMaintenanceStore) (*displaymaintenance.Worker, error) {
	return displaymaintenance.New(storage, displayMaintenanceScope{})
}

func (displayMaintenanceScope) SystemContext(ctx context.Context) context.Context {
	return inbound.WithSystemActor(ctx)
}

func (displayMaintenanceScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return AcquireResources(ctx, ResourceBulk)
}
