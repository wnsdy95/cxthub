package documentworker

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type RangeWorker struct {
	storage outbound.DocRangeJobStore
	scope   Scope
}

func NewRangeWorker(storage outbound.DocRangeJobStore, scope Scope) *RangeWorker {
	return &RangeWorker{storage: storage, scope: scope}
}

func (worker *RangeWorker) RunJob(ctx context.Context, job domain.DocRangeIndexJob, lease time.Duration) error {
	published := false
	err := WithLease(ctx, lease, func(renew context.Context) error {
		return worker.storage.RenewDocRangeJob(renew, job, time.Now().UTC(), lease)
	}, func(work context.Context) error {
		err := worker.scope.WithinRepositoryRead(work, job.RepoID, func(context.Context) error {
			return nil
		})
		if err != nil {
			return err
		}
		published, err = worker.Build(work, job)
		return err
	})
	if err == nil || ctx.Err() != nil || published {
		return err
	}
	now := time.Now().UTC()
	job.State, job.Reason = "retrying", "temporary_failure"
	job.NextAttempt = now.Add(time.Second << min(job.Attempts, 8))
	job.LeaseUntil = time.Time{}
	switch {
	case errors.Is(err, domain.ErrDocByteRangeBackfillRequired):
		job.Reason = "byte_backfill_required"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		job.Reason = "verification_interrupted"
	case errors.Is(err, domain.ErrPreparedDocRangeObject):
		job.Reason = "prepared_artifact_corrupt"
	case errors.Is(err, domain.ErrNotFound):
		job.State, job.Reason = "rejected", "source_unavailable"
	case errors.Is(err, domain.ErrIntegrity), errors.Is(err, domain.ErrValidation), errors.Is(err, domain.ErrConversationManifest), errors.Is(err, domain.ErrUnsupportedCIRVersion), errors.Is(err, domain.ErrUnsupportedDocumentIdentity):
		job.State, job.Reason = "rejected", "invalid_document"
	case errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired):
		job.Reason = "identity_upgrade_required"
	case errors.Is(err, domain.ErrDocumentResourceLimit):
		job.Reason = "resource_limit"
	}
	finish, release := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer release()
	return errors.Join(err, worker.storage.FinishDocRangeJob(finish, job, now))
}

func (worker *RangeWorker) Process(ctx context.Context, limit int) error {
	if absent(worker.storage) {
		return nil
	}
	ctx = worker.scope.WorkerContext(ctx)
	for processed := 0; processed < limit; processed++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		claimed := false
		err := worker.scope.WithinWork(ctx, func(bound context.Context) error {
			job, err := worker.storage.ClaimDocRangeJob(bound, time.Now().UTC(), Lease)
			if err != nil {
				return err
			}
			claimed = true
			return worker.RunJob(bound, job, Lease)
		})
		if !claimed && errors.Is(err, domain.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (worker *RangeWorker) Run(ctx context.Context) {
	if absent(worker.storage) {
		return
	}
	for {
		if err := worker.Process(ctx, 1); err != nil && ctx.Err() == nil {
			log.Printf("document range index: %v", err)
		}
		timer := time.NewTimer(time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}
