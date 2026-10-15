package app

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyStreamingPreparationStore struct {
	*preparationJobStore
	prepareLegacy func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error)
}

func (storage legacyStreamingPreparationStore) PrepareLegacyDocJob(ctx context.Context, job domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
	return storage.prepareLegacy(ctx, job)
}

type legacyStagedPublication struct {
	streamingPreparationPublication
	stage func(context.Context, domain.DocFinalizationJob, time.Duration) error
}

func (publication *legacyStagedPublication) Stage(ctx context.Context, job domain.DocFinalizationJob, lease time.Duration) error {
	return publication.stage(ctx, job, lease)
}

func TestDocJobLegacyStreamingLifecycle(t *testing.T) {
	for _, outcome := range []string{"success", "stage-failure", "resource-limit", "cancellation"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				service, job := preparationFixture(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				failure := errors.New("stage failed")
				if outcome == "resource-limit" {
					failure = domain.ErrDocumentResourceLimit
				}
				if outcome == "cancellation" {
					failure = context.Canceled
				}
				staged, finished := 0, 0
				publication := &legacyStagedPublication{}
				publication.stage = func(work context.Context, got domain.DocFinalizationJob, lease time.Duration) error {
					staged++
					if got.ID != job.ID || got.Version != job.Version || lease != docJobLease {
						t.Fatal("stage lost claim or lease policy")
					}
					time.Sleep(2 * docJobLease)
					if outcome == "cancellation" {
						cancel()
					}
					if outcome != "success" {
						return failure
					}
					return work.Err()
				}
				storage := legacyStreamingPreparationStore{
					preparationJobStore: &preparationJobStore{
						prepare: func(context.Context, domain.VerifiedSessionDoc) (outbound.PreparedDocPublication, error) {
							t.Fatal("v2 streaming worker materialized full document")
							return nil, nil
						},
						renew: func(context.Context, domain.DocFinalizationJob, time.Time, time.Duration) error {
							t.Error("background renewal overlaps staged transactions")
							return nil
						},
						finish: func(_ context.Context, got domain.DocFinalizationJob, now time.Time) error {
							finished++
							reason := "temporary_failure"
							if outcome == "resource-limit" {
								reason = "resource_limit"
							}
							assertPreparationFailure(t, job, got, now, "retrying", reason)
							return nil
						},
					},
					prepareLegacy: func(_ context.Context, got domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
						if got.ID != job.ID {
							t.Fatal("wrong legacy job")
						}
						return publication, nil
					},
				}
				err := service.runDocJob(ctx, storage, job)
				if outcome == "success" && err != nil || outcome != "success" && !errors.Is(err, failure) {
					t.Fatal("unexpected job outcome", err)
				}
				wantComplete, wantFinished := 0, 1
				if outcome == "success" {
					wantComplete, wantFinished = 1, 0
				} else if outcome == "cancellation" {
					wantFinished = 0
				}
				if staged != 1 || publication.closeCalls != 1 || publication.completeCalls != wantComplete || finished != wantFinished {
					t.Fatalf("stage/close/complete/finish=%d/%d/%d/%d", staged, publication.closeCalls, publication.completeCalls, finished)
				}
			})
		})
	}
}
