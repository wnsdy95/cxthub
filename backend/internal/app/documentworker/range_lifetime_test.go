package documentworker

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestRangeLifetimeHealthyRenewalOutlivesWorkLimit(tests *testing.T) {
	for _, mode := range []string{"prepared", "direct", "compatibility", "late-compatibility"} {
		tests.Run(mode, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				started := time.Now()
				job := domain.DocRangeIndexJob{RepositoryEpoch: "epoch", RepoID: "repo", DocHash: "document", Version: 17, Attempts: 3, State: "running", LeaseUntil: started.Add(Lease)}
				ctx := context.WithValue(context.Background(), rangeWave38ContextKey("caller"), "preserved")
				leaseUntil, renewals, builds, yields := job.LeaseUntil, 0, 0, 0
				var leaseMu sync.Mutex
				var workContext, renewalContext context.Context
				checkWork := func(work context.Context, got domain.DocRangeIndexJob) {
					workContext = work
					if _, bounded := work.Deadline(); bounded || work.Err() != nil || got != job || work.Value(rangeWave38ContextKey("caller")) != "preserved" {
						test.Error("range work gained a total deadline or lost its caller or fence", work.Err(), got)
					}
				}
				build := func(work context.Context, got domain.DocRangeIndexJob) error {
					checkWork(work, got)
					builds++
					timer := time.NewTimer(WorkLimit + Lease)
					defer timer.Stop()
					select {
					case <-work.Done():
						return work.Err()
					case <-timer.C:
						return work.Err()
					}
				}
				storage := &rangeWave38Store{
					build: build,
					renew: func(renew context.Context, got domain.DocRangeIndexJob, now time.Time, lease time.Duration) error {
						leaseMu.Lock()
						defer leaseMu.Unlock()
						renewalContext = renew
						deadline, bounded := renew.Deadline()
						if got != job || lease != Lease || !now.Equal(time.Now().UTC()) || now.Location() != time.UTC || !bounded || !deadline.Equal(time.Now().Add(5*time.Second)) || renew.Err() != nil {
							test.Error("renewal changed its fence, lease, clock, or operation bound")
						}
						if !leaseUntil.After(now) {
							return domain.ErrConflict
						}
						leaseUntil = now.Add(lease)
						renewals++
						return nil
					},
					finish: func(context.Context, domain.DocRangeIndexJob, time.Time) error {
						test.Error("healthy range work was interrupted or finished twice")
						return nil
					},
				}
				prepared := &rangeWave38Prepared{}
				prepared.step = func(work context.Context, got domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
					checkWork(work, got)
					if mode == "late-compatibility" {
						return outbound.DocRangeWorkCompatibilityRequired, nil
					}
					time.Sleep(Lease)
					if prepared.steps < 7 {
						return outbound.DocRangeWorkPending, work.Err()
					}
					return outbound.DocRangeWorkPublishable, work.Err()
				}
				prepared.publish = func(work context.Context, got domain.DocRangeIndexJob) error {
					checkWork(work, got)
					leaseMu.Lock()
					defer leaseMu.Unlock()
					if !leaseUntil.After(time.Now()) || prepared.resourceChecks != 7 || yields != 7 {
						test.Error("publication lost its live lease or bypassed resource accounting")
					}
					return work.Err()
				}
				var selected outbound.DocRangeJobStore = storage
				if mode != "direct" {
					selected = rangeWave38PreparingStore{
						rangeWave38Store: storage,
						prepare: func(work context.Context, got domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
							checkWork(work, got)
							if mode == "compatibility" {
								return nil, nil
							}
							return prepared, nil
						},
						compatibility: func(work context.Context, got domain.DocRangeIndexJob) error {
							if mode == "late-compatibility" && prepared.closes != 1 {
								test.Error("compatibility work preceded private cleanup")
							}
							return build(work, got)
						},
					}
				}
				scope := rangeWave38Scope{
					canYield: func(context.Context) bool { return true },
					yield: func(work context.Context, resources outbound.RootPublicationResources) error {
						yields++
						if work != workContext || resources != (outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}) || prepared.resourceChecks != prepared.steps {
							test.Error("range work changed retained resources or yielded before accounting")
						}
						return work.Err()
					},
				}
				if err := NewRangeWorker(selected, scope).RunJob(ctx, job, Lease); err != nil {
					test.Fatal("healthy renewal did not keep range work alive", err)
				}
				if time.Since(started) <= WorkLimit || renewals <= int(WorkLimit/(Lease/3)) || !leaseUntil.After(time.Now()) {
					test.Fatal("test did not cross the old work limit with a live lease", time.Since(started), renewals)
				}
				if mode == "prepared" {
					if builds != 0 || prepared.steps != 7 || prepared.resourceChecks != 7 || yields != 7 || prepared.publications != 1 || prepared.closes != 1 {
						test.Fatal("long-running prepared work changed its bounded-step lifecycle", prepared, builds, yields)
					}
				} else if builds != 1 || prepared.publications != 0 || prepared.resourceChecks != 0 || yields != 0 || (mode == "late-compatibility" && (prepared.steps != 1 || prepared.closes != 1)) {
					test.Fatal("long-running fallback changed its lifecycle", prepared, builds, yields)
				}
				if workContext == nil || !errors.Is(workContext.Err(), context.Canceled) || renewalContext == nil || !errors.Is(renewalContext.Err(), context.Canceled) {
					test.Fatal("completed range work retained live contexts")
				}
				stoppedRenewals := renewals
				time.Sleep(2 * Lease)
				if renewals != stoppedRenewals {
					test.Fatal("renewal goroutine survived range completion")
				}
			})
		})
	}
}

func TestRangeLifetimeCancellationAfterHealthyRenewal(tests *testing.T) {
	for _, cause := range []string{"revoked", "expired", "renewal-timeout", "parent-canceled"} {
		tests.Run(cause, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				job := domain.DocRangeIndexJob{RepositoryEpoch: "epoch", RepoID: "repo", DocHash: "document", Version: 19, Attempts: 2, State: "running", LeaseUntil: time.Now().Add(Lease)}
				ctx, cancel := context.WithCancel(context.WithValue(context.Background(), rangeWave38ContextKey("caller"), "preserved"))
				defer cancel()
				version, leaseUntil := job.Version, job.LeaseUntil
				var leaseMu sync.Mutex
				renewals, finishes, timeoutRenewal := 0, 0, false
				started := make(chan struct{})
				renewalBlocked := make(chan struct{})
				prepared := &rangeWave38Prepared{step: func(work context.Context, got domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
					if got != job {
						test.Error("step lost the claimed fence")
					}
					close(started)
					<-work.Done()
					return outbound.DocRangeWorkPublishable, nil
				}}
				storage := rangeWave38PreparingStore{
					rangeWave38Store: &rangeWave38Store{
						renew: func(renew context.Context, got domain.DocRangeIndexJob, now time.Time, lease time.Duration) error {
							leaseMu.Lock()
							defer leaseMu.Unlock()
							renewals++
							deadline, bounded := renew.Deadline()
							if got != job || lease != Lease || !bounded || !deadline.Equal(time.Now().Add(5*time.Second)) {
								test.Error("renewal changed the claimed fence or operation bound")
							}
							if timeoutRenewal {
								close(renewalBlocked)
								<-renew.Done()
								return renew.Err()
							}
							if got.Version != version || !leaseUntil.After(now) {
								return domain.ErrConflict
							}
							leaseUntil = now.Add(lease)
							return renew.Err()
						},
						finish: func(finish context.Context, got domain.DocRangeIndexJob, now time.Time) error {
							leaseMu.Lock()
							defer leaseMu.Unlock()
							finishes++
							want := job
							want.State, want.Reason = "retrying", "verification_interrupted"
							want.NextAttempt, want.LeaseUntil = now.Add(time.Second<<job.Attempts), time.Time{}
							deadline, bounded := finish.Deadline()
							if cause == "parent-canceled" || got != want || finish.Err() != nil || finish.Value(rangeWave38ContextKey("caller")) != "preserved" || !bounded || !deadline.Equal(time.Now().Add(5*time.Second)) || prepared.closes != 1 {
								test.Error("cancellation changed cleanup, recovery, or the original fence", got)
							}
							if got.Version != version || !leaseUntil.After(now) {
								return domain.ErrConflict
							}
							return nil
						},
					},
					prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
						return prepared, nil
					},
				}
				done := make(chan error, 1)
				go func() { done <- NewRangeWorker(storage, rangeWave38Scope{}).RunJob(ctx, job, Lease) }()
				<-started
				synctest.Wait()
				time.Sleep(WorkLimit + Lease + time.Second)
				synctest.Wait()
				select {
				case err := <-done:
					test.Fatal("range work stopped before cancellation", err)
				default:
				}
				leaseMu.Lock()
				switch cause {
				case "revoked":
					version++
				case "expired":
					leaseUntil = time.Now()
				case "renewal-timeout", "parent-canceled":
					timeoutRenewal = true
				}
				leaseMu.Unlock()
				if cause == "parent-canceled" {
					<-renewalBlocked
					cancel()
				}
				err := <-done
				lostFence := cause == "revoked" || cause == "expired"
				if !errors.Is(err, context.Canceled) || errors.Is(err, domain.ErrConflict) != lostFence || prepared.steps != 1 || prepared.publications != 0 || prepared.resourceChecks != 0 || prepared.closes != 1 {
					test.Fatal("cancellation published, lost its error, or skipped cleanup", err, prepared)
				}
				if (cause == "parent-canceled" && finishes != 0) || (cause != "parent-canceled" && finishes != 1) {
					test.Fatal("shutdown consumed its lease or renewal failure skipped recovery", finishes)
				}
				stoppedRenewals := renewals
				time.Sleep(2 * Lease)
				if renewals != stoppedRenewals {
					test.Fatal("renewal goroutine survived cancellation")
				}
			})
		})
	}
}

func TestRangeLifetimeJoinsInflightRenewal(tests *testing.T) {
	for _, outcome := range []error{nil, errors.New("build failed")} {
		synctest.Test(tests, func(test *testing.T) {
			renewing, renewed, releaseWork := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var workContext, renewalContext context.Context
			storage := &rangeWave38Store{
				build: func(work context.Context, _ domain.DocRangeIndexJob) error {
					workContext = work
					<-releaseWork
					return outcome
				},
				renew: func(renew context.Context, _ domain.DocRangeIndexJob, _ time.Time, _ time.Duration) error {
					renewalContext = renew
					close(renewing)
					<-renew.Done()
					close(renewed)
					return renew.Err()
				},
				finish: func(context.Context, domain.DocRangeIndexJob, time.Time) error {
					select {
					case <-renewed:
					default:
						test.Error("failure recovery overtook renewal")
					}
					return nil
				},
			}
			done := make(chan error, 1)
			go func() {
				done <- NewRangeWorker(storage, rangeWave38Scope{}).RunJob(context.Background(), domain.DocRangeIndexJob{}, Lease)
			}()
			<-renewing
			close(releaseWork)
			synctest.Wait()
			select {
			case err := <-done:
				test.Fatal("range work returned with a live renewal", err)
			default:
			}
			time.Sleep(5 * time.Second)
			if err := <-done; !errors.Is(err, outcome) || !errors.Is(workContext.Err(), context.Canceled) || !errors.Is(renewalContext.Err(), context.DeadlineExceeded) {
				test.Fatal("joining renewal changed the work result or retained live contexts", err)
			}
			select {
			case <-renewed:
			default:
				test.Fatal("range completion did not join renewal")
			}
		})
	}
}

func TestSharedLeaseRetainsWorkLimitWithHealthyRenewal(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		started, renewals := time.Now(), 0
		err := WithLease(context.Background(), Lease, func(renew context.Context) error {
			renewals++
			return renew.Err()
		}, func(work context.Context) error {
			<-work.Done()
			return work.Err()
		})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != WorkLimit || WorkLimit != 10*time.Minute || renewals == 0 {
			test.Fatal("unrelated workers lost their existing work limit", err, time.Since(started), renewals)
		}
	})
}
