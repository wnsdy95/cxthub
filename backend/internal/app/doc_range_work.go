package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (service *Service) buildDocRangeWork(ctx context.Context, storage outbound.DocRangeJobStore, job domain.DocRangeIndexJob) (published bool, err error) {
	return service.documentRangeWorker(storage).Build(ctx, job)
}
