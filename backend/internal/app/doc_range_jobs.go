package app

import (
	"context"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docRangePreparationError(job domain.DocRangeIndexJob) error {
	if job.Reason == "byte_backfill_required" {
		return domain.ErrDocByteRangeBackfillRequired
	}
	if job.State == "completed" {
		return nil
	}
	if job.State == "rejected" {
		if job.Reason == "source_unavailable" {
			return domain.ErrNotFound
		}
		return domain.ErrIntegrity
	}
	return domain.ErrDocRangeIndexPending
}

func (service *Service) runDocRangeJob(ctx context.Context, storage outbound.DocRangeJobStore, job domain.DocRangeIndexJob, lease time.Duration) error {
	return service.documentRangeWorker(storage).RunJob(ctx, job, lease)
}

func (service *Service) ProcessDocRangeJobs(ctx context.Context, limit int) error {
	storage, ok := service.blobs.(outbound.DocRangeJobStore)
	if !ok {
		return nil
	}
	return service.documentRangeWorker(storage).Process(ctx, limit)
}

func (service *Service) RunDocRangeWorker(ctx context.Context) {
	storage, ok := service.blobs.(outbound.DocRangeJobStore)
	if !ok {
		return
	}
	service.documentRangeWorker(storage).Run(ctx)
}
