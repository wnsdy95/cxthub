package documentworker_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestFinalizationOwnerJoinsRenewalBeforeStageAndCompletion(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		job, _ := finalizationFixture(test, domain.DocumentIdentityRootV1)
		preparing, releasePrepare := make(chan struct{}), make(chan struct{})
		renewing, releaseRenew := make(chan struct{}), make(chan struct{})
		staging, releaseStage := make(chan struct{}), make(chan struct{})
		completing, releaseComplete := make(chan struct{}), make(chan struct{})
		var renewCalls, stageCalls, closeCalls atomic.Int32
		publication := &finalizationPublication{
			stage: func(ctx context.Context, _ domain.DocFinalizationJob, _ time.Duration) error {
				stageCalls.Add(1)
				close(staging)
				<-releaseStage
				return ctx.Err()
			},
			complete: func(ctx context.Context, _ domain.DocFinalizationJob, _ time.Time) error {
				close(completing)
				<-releaseComplete
				return ctx.Err()
			},
			close: func() error { closeCalls.Add(1); return nil },
		}
		storage := finalizationStreamingStore{
			finalizationStore: &finalizationStore{renew: func(ctx context.Context, got domain.DocFinalizationJob, now time.Time, lease time.Duration) error {
				assertFinalizationDeadline(test, ctx, 5*time.Second)
				if got.ID != job.ID || got.Version != job.Version || lease != documentworker.Lease || !now.Equal(time.Now().UTC()) {
					test.Error("renewal changed job fence, lease, or timestamp")
				}
				renewCalls.Add(1)
				close(renewing)
				<-releaseRenew
				return ctx.Err()
			}},
			root: func(context.Context, domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
				close(preparing)
				<-releasePrepare
				return publication, nil
			},
		}
		owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
		done := make(chan error, 1)
		go func() { done <- owner.RunJob(context.Background(), job) }()
		<-preparing
		synctest.Wait()
		time.Sleep(documentworker.Lease / 3)
		<-renewing
		close(releasePrepare)
		synctest.Wait()
		if stageCalls.Load() != 0 {
			test.Fatal("staging overtook in-flight renewal")
		}
		close(releaseRenew)
		<-staging
		synctest.Wait()
		time.Sleep(documentworker.Lease)
		synctest.Wait()
		if renewCalls.Load() != 1 {
			test.Fatal("renewal continued during staging", renewCalls.Load())
		}
		close(releaseStage)
		<-completing
		synctest.Wait()
		time.Sleep(documentworker.Lease)
		synctest.Wait()
		if renewCalls.Load() != 1 {
			test.Fatal("renewal continued during completion", renewCalls.Load())
		}
		close(releaseComplete)
		if err := <-done; err != nil || stageCalls.Load() != 1 || closeCalls.Load() != 1 {
			test.Fatal("successful publication changed lifecycle", err, stageCalls.Load(), closeCalls.Load())
		}
	})
}

func TestFinalizationOwnerCancellationLeavesShutdownLeaseAndFinishesLostLease(test *testing.T) {
	for _, lostLease := range []bool{false, true} {
		synctest.Test(test, func(test *testing.T) {
			job, _ := finalizationFixture(test, domain.DocumentIdentityRootV1)
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), finalizationCallerKey{}, "caller"))
			defer cancel()
			preparing := make(chan struct{})
			var closes, finishes, renewals atomic.Int32
			publication := &finalizationPublication{
				stage: func(context.Context, domain.DocFinalizationJob, time.Duration) error {
					test.Error("canceled preparation staged")
					return nil
				},
				complete: func(context.Context, domain.DocFinalizationJob, time.Time) error {
					test.Error("canceled preparation completed")
					return nil
				},
				close: func() error { closes.Add(1); return nil },
			}
			storage := finalizationStreamingStore{
				finalizationStore: &finalizationStore{
					renew: func(context.Context, domain.DocFinalizationJob, time.Time, time.Duration) error {
						renewals.Add(1)
						return domain.ErrConflict
					},
					finish: func(finish context.Context, got domain.DocFinalizationJob, _ time.Time) error {
						finishes.Add(1)
						assertFinalizationDeadline(test, finish, 5*time.Second)
						if finish.Err() != nil || finish.Value(finalizationCallerKey{}) != "caller" || got.State != "retrying" || got.Reason != "temporary_failure" || got.Version != job.Version || !got.LeaseUntil.IsZero() {
							test.Error("lost-lease recovery inherited cancellation or changed the fence", got)
						}
						return domain.ErrConflict
					},
				},
				root: func(work context.Context, _ domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
					close(preparing)
					<-work.Done()
					return publication, nil
				},
			}
			owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
			done := make(chan error, 1)
			go func() { done <- owner.RunJob(ctx, job) }()
			<-preparing
			synctest.Wait()
			if lostLease {
				time.Sleep(documentworker.Lease / 3)
			} else {
				cancel()
			}
			err := <-done
			if !errors.Is(err, context.Canceled) || closes.Load() != 1 {
				test.Fatal("cancellation skipped cleanup or hid cancellation", err, closes.Load())
			}
			if lostLease && (!errors.Is(err, domain.ErrConflict) || finishes.Load() != 1 || renewals.Load() != 1) || !lostLease && (finishes.Load() != 0 || renewals.Load() != 0) {
				test.Fatal("shutdown consumed its lease or lost-lease recovery changed", lostLease, err, finishes.Load(), renewals.Load())
			}
		})
	}
}

func TestFinalizationOwnerWorkLimitRemainsIndependentOfRenewal(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		job, _ := finalizationFixture(test, domain.DocumentIdentityRootV1)
		finishes := 0
		storage := finalizationStreamingStore{
			finalizationStore: &finalizationStore{
				renew: func(ctx context.Context, _ domain.DocFinalizationJob, _ time.Time, _ time.Duration) error {
					return ctx.Err()
				},
				finish: func(ctx context.Context, got domain.DocFinalizationJob, _ time.Time) error {
					finishes++
					if ctx.Err() != nil || got.State != "retrying" || got.Reason != "temporary_failure" {
						test.Error("work-limit failure lost its independent finish context", got)
					}
					return nil
				},
			},
			root: func(ctx context.Context, _ domain.DocFinalizationJob) (outbound.PreparedRootDocPublication, error) {
				deadline, bounded := ctx.Deadline()
				if !bounded || !deadline.Equal(time.Now().Add(10*time.Minute)) || documentworker.WorkLimit != 10*time.Minute || documentworker.Lease != 2*time.Minute {
					test.Error("finalization work defense or lease changed", deadline)
				}
				<-ctx.Done()
				return nil, ctx.Err()
			},
		}
		owner := newFinalizationOwner(test, storage, nil, &finalizationScope{})
		if err := owner.RunJob(context.Background(), job); !errors.Is(err, context.DeadlineExceeded) || finishes != 1 {
			test.Fatal("renewal extended the ten-minute job defense", err, finishes)
		}
	})
}

func TestFinalizationSharedLeaseReturnsWorkOutcomeAfterJoiningRenewal(test *testing.T) {
	for _, runFailure := range []error{nil, errors.New("work failed")} {
		synctest.Test(test, func(test *testing.T) {
			var workContext, renewalContext context.Context
			ctx := context.WithValue(context.Background(), finalizationCallerKey{}, "caller")
			err := documentworker.WithLease(ctx, documentworker.Lease, func(renew context.Context) error {
				renewalContext = renew
				assertFinalizationDeadline(test, renew, 5*time.Second)
				return domain.ErrConflict
			}, func(work context.Context) error {
				workContext = work
				if deadline, bounded := work.Deadline(); !bounded || !deadline.Equal(time.Now().Add(documentworker.WorkLimit)) || work.Value(finalizationCallerKey{}) != "caller" {
					test.Error("shared lease changed work deadline or context values")
				}
				<-work.Done()
				return runFailure
			})
			if err != runFailure || workContext == nil || !errors.Is(workContext.Err(), context.Canceled) || renewalContext == nil || !errors.Is(renewalContext.Err(), context.Canceled) {
				test.Fatal("shared lease changed the work result or failed to cancel renewal", err, runFailure, workContext, renewalContext)
			}
		})
	}
}
