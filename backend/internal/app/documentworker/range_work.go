package documentworker

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func (worker *RangeWorker) Build(ctx context.Context, job domain.DocRangeIndexJob) (published bool, err error) {
	if err = ctx.Err(); err != nil {
		return false, err
	}
	preparation, supported := worker.storage.(outbound.DocRangeWorkStore)
	if !supported {
		err = worker.storage.BuildDocRangeJob(ctx, job)
		return err == nil, err
	}
	prepared, err := preparation.PrepareDocRangeWork(ctx, job)
	if prepared != nil {
		defer func() {
			if prepared != nil {
				err = errors.Join(err, prepared.Close())
			}
		}()
	}
	if err != nil {
		return false, err
	}
	if err = ctx.Err(); err != nil {
		return false, err
	}
	if prepared == nil {
		err = preparation.BuildDocRangeCompatibilityJob(ctx, job)
		return err == nil, err
	}
	for {
		if err = ctx.Err(); err != nil {
			return false, err
		}
		progress, stepErr := prepared.Step(ctx, job)
		if stepErr != nil {
			return false, stepErr
		}
		if err = ctx.Err(); err != nil {
			return false, err
		}
		switch progress {
		case outbound.DocRangeWorkPending, outbound.DocRangeWorkPublishable:
		case outbound.DocRangeWorkCompatibilityRequired:
			private := prepared
			prepared = nil
			if err = private.Close(); err != nil {
				return false, err
			}
			if err = ctx.Err(); err != nil {
				return false, err
			}
			err = preparation.BuildDocRangeCompatibilityJob(ctx, job)
			return err == nil, err
		default:
			return false, domain.ErrIntegrity
		}
		resources, resourcesErr := prepared.RetainedResources()
		if resourcesErr != nil {
			return false, resourcesErr
		}
		if worker.scope.CanYield(ctx) {
			if err = worker.scope.Yield(ctx, resources); err != nil {
				return false, err
			}
		}
		if progress == outbound.DocRangeWorkPublishable {
			if err = ctx.Err(); err != nil {
				return false, err
			}
			err = prepared.Publish(ctx, job)
			return err == nil, err
		}
	}
}
