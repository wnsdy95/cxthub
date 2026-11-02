package outbound

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type DocDisplayMaintenanceStore interface {
	StepDocDisplayMaintenance(context.Context) (domain.DocDisplayMaintenanceStep, error)
}
