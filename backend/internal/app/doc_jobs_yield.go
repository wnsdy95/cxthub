package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) yieldRootPreparation(ctx context.Context, publication outbound.PreparedDocPublication) error {
	finalizer, err := service.documentFinalizer(nil)
	if err != nil {
		return err
	}
	return finalizer.YieldRootPreparation(ctx, publication)
}
