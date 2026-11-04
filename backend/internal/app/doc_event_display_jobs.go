package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func (service *Service) buildDocEventDisplay(ctx context.Context, job domain.DocDisplayJob) (bool, error) {
	worker, err := service.documentDisplayWorker()
	if err != nil {
		return false, err
	}
	return worker.BuildEvent(ctx, job)
}
