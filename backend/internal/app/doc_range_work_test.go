package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docRangeWorkJobStore struct {
	outbound.DocRangeJobStore
	build func(context.Context, domain.DocRangeIndexJob) error
}

func (storage docRangeWorkJobStore) BuildDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	return storage.build(ctx, job)
}

type preparingDocRangeWorkStore struct {
	docRangeWorkJobStore
	prepare       func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error)
	compatibility func(context.Context, domain.DocRangeIndexJob) error
}

func (storage preparingDocRangeWorkStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	return storage.prepare(ctx, job)
}

func (storage preparingDocRangeWorkStore) BuildDocRangeCompatibilityJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	if storage.compatibility != nil {
		return storage.compatibility(ctx, job)
	}
	return storage.BuildDocRangeJob(ctx, job)
}

func TestBuildDocRangeWorkUnsupportedUsesCompatibilityWithoutRepreparation(test *testing.T) {
	preparations, compatibility := 0, 0
	job := domain.DocRangeIndexJob{RepoID: h('1'), DocHash: h('2'), Version: 7}
	ctx := context.Background()
	storage := preparingDocRangeWorkStore{
		docRangeWorkJobStore: docRangeWorkJobStore{build: func(context.Context, domain.DocRangeIndexJob) error {
			test.Fatal("unsupported work reentered generic preparation")
			return domain.ErrIntegrity
		}},
		prepare: func(bound context.Context, selected domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
			preparations++
			if selected != job || bound != ctx {
				test.Fatal("preparation changed selection")
			}
			return nil, nil
		},
		compatibility: func(bound context.Context, selected domain.DocRangeIndexJob) error {
			compatibility++
			if selected != job || bound != ctx {
				test.Fatal("compatibility changed selection")
			}
			return nil
		},
	}
	service := NewService(preparationMetadata{}, nil, nil, nil, nil)
	published, err := service.buildDocRangeWork(ctx, storage, job)
	if err != nil || !published || preparations != 1 || compatibility != 1 {
		test.Fatal("unsupported work did not use one preparation and one compatibility build", published, err, preparations, compatibility)
	}
}

type preparedDocRangeWorkStub struct {
	step      func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error)
	publish   func(context.Context, domain.DocRangeIndexJob) error
	resources func() (outbound.RootPublicationResources, error)
	close     func() error

	steps, publications, resourceChecks, closes int
}

func (prepared *preparedDocRangeWorkStub) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	prepared.steps++
	if prepared.step != nil {
		return prepared.step(ctx, job)
	}
	return outbound.DocRangeWorkPublishable, nil
}

func (prepared *preparedDocRangeWorkStub) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	prepared.publications++
	if prepared.publish != nil {
		return prepared.publish(ctx, job)
	}
	return nil
}

func (prepared *preparedDocRangeWorkStub) RetainedResources() (outbound.RootPublicationResources, error) {
	prepared.resourceChecks++
	if prepared.resources != nil {
		return prepared.resources()
	}
	return outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}, nil
}

func (prepared *preparedDocRangeWorkStub) Close() error {
	prepared.closes++
	if prepared.close != nil {
		return prepared.close()
	}
	return nil
}

func TestDocRangeWorkerDoesNotRetryCommittedPublicationAfterCleanupFailure(tests *testing.T) {
	cleanupFailure, publicationFailure := errors.New("cleanup failed"), errors.New("publication failed")
	for _, committed := range []bool{false, true} {
		tests.Run(map[bool]string{false: "not-committed", true: "committed"}[committed], func(test *testing.T) {
			finished := 0
			legacy := &docRangeJobFake{
				renew: func(context.Context, domain.DocRangeIndexJob, time.Time, time.Duration) error { return nil },
				finish: func(_ context.Context, job domain.DocRangeIndexJob, _ time.Time) error {
					finished++
					if job.State != "retrying" || job.Reason != "temporary_failure" {
						test.Fatal("failed publication did not retain retry state", job)
					}
					return nil
				},
			}
			prepared := &preparedDocRangeWorkStub{
				publish: func(context.Context, domain.DocRangeIndexJob) error {
					if !committed {
						return publicationFailure
					}
					return nil
				},
				close: func() error { return cleanupFailure },
			}
			storage := preparingDocRangeWorkStore{
				docRangeWorkJobStore: docRangeWorkJobStore{DocRangeJobStore: legacy},
				prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					return prepared, nil
				},
			}
			service := NewService(preparationMetadata{}, nil, nil, nil, nil)
			err := service.runDocRangeJob(context.Background(), storage, domain.DocRangeIndexJob{RepoID: h('1')}, docJobLease)
			if !errors.Is(err, cleanupFailure) || errors.Is(err, publicationFailure) == committed ||
				(finished == 0) != committed || finished > 1 || prepared.closes != 1 || prepared.publications != 1 {
				test.Fatal("cleanup error rewrote committed state or lost an uncommitted retry", err, finished, prepared)
			}
		})
	}
}

func TestBuildDocRangeWorkFallsBackOnlyWithoutCapability(tests *testing.T) {
	failure := errors.New("prepare failed")
	for _, scenario := range []struct {
		name         string
		port         bool
		prepareError error
		buildError   error
		wantBuild    bool
	}{
		{name: "no-port", wantBuild: true},
		{name: "explicit-unsupported", port: true, wantBuild: true},
		{name: "legacy-failure", buildError: failure, wantBuild: true},
		{name: "unsupported-build-failure", port: true, buildError: failure, wantBuild: true},
		{name: "unsupported-error-is-failure", port: true, prepareError: domain.ErrCatalogUnsupported},
		{name: "prepare-error-is-failure", port: true, prepareError: failure},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			ctx := context.Background()
			job := domain.DocRangeIndexJob{RepoID: hh(test.Name()), Version: 17, Attempts: 2}
			builds, preparations := 0, 0
			legacy := docRangeWorkJobStore{build: func(bound context.Context, got domain.DocRangeIndexJob) error {
				builds++
				if bound != ctx || got != job {
					test.Fatal("fallback lost job or context")
				}
				return scenario.buildError
			}}
			var storage outbound.DocRangeJobStore = legacy
			if scenario.port {
				storage = preparingDocRangeWorkStore{docRangeWorkJobStore: legacy, prepare: func(bound context.Context, got domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					preparations++
					if bound != ctx || got != job {
						test.Fatal("preparation lost job or context")
					}
					return nil, scenario.prepareError
				}}
			}
			service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
			published, err := service.buildDocRangeWork(ctx, storage, job)
			wantError := errors.Join(scenario.prepareError, scenario.buildError)
			if published != (wantError == nil) || (wantError == nil && err != nil) || (scenario.prepareError != nil && !errors.Is(err, scenario.prepareError)) || (scenario.buildError != nil && !errors.Is(err, scenario.buildError)) {
				test.Fatal("incorrect fallback result", published, err)
			}
			if (builds == 1) != scenario.wantBuild || builds > 1 || (preparations == 1) != scenario.port {
				test.Fatal("incorrect fallback calls", builds, preparations)
			}
		})
	}
}

func TestBuildDocRangeWorkPreservesJobAcrossSteps(tests *testing.T) {
	ctx := context.Background()
	job := domain.DocRangeIndexJob{RepoID: hh(tests.Name()), DocHash: hh("range-document"), BaseHash: hh("range-base"), Version: 23, Attempts: 3, State: "running", LeaseUntil: time.Now().Add(time.Minute)}
	prepared := &preparedDocRangeWorkStub{}
	prepared.step = func(bound context.Context, got domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
		if bound != ctx || got != job || prepared.resourceChecks != prepared.steps-1 {
			tests.Fatal("step lost capability binding or skipped resource checks")
		}
		if prepared.steps == 3 {
			return outbound.DocRangeWorkPublishable, nil
		}
		return outbound.DocRangeWorkPending, nil
	}
	prepared.publish = func(bound context.Context, got domain.DocRangeIndexJob) error {
		if bound != ctx || got != job || prepared.steps != 3 || prepared.resourceChecks != 3 || prepared.closes != 0 {
			tests.Fatal("publication preceded completed steps or resource checks")
		}
		return nil
	}
	storage := preparingDocRangeWorkStore{prepare: func(bound context.Context, got domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
		if bound != ctx || got != job {
			tests.Fatal("preparation lost capability binding")
		}
		return prepared, nil
	}}
	service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
	published, err := service.buildDocRangeWork(ctx, storage, job)
	if err != nil || !published || prepared.publications != 1 || prepared.closes != 1 {
		tests.Fatal("incorrect prepared lifecycle", published, err, prepared)
	}
}

func TestBuildDocRangeWorkClosesOnceAndJoinsFailures(tests *testing.T) {
	failure, cleanupFailure := errors.New("callback failed"), errors.New("cleanup failed")
	for _, stage := range []string{"success", "prepare", "step", "resources", "publish"} {
		for _, cleanupFails := range []bool{false, true} {
			name := stage
			if cleanupFails {
				name += "-cleanup-failure"
			}
			tests.Run(name, func(test *testing.T) {
				prepared := &preparedDocRangeWorkStub{}
				if stage == "step" {
					prepared.step = func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
						return outbound.DocRangeWorkPublishable, failure
					}
				}
				if stage == "resources" {
					prepared.resources = func() (outbound.RootPublicationResources, error) { return outbound.RootPublicationResources{}, failure }
				}
				if stage == "publish" {
					prepared.publish = func(context.Context, domain.DocRangeIndexJob) error { return failure }
				}
				if cleanupFails {
					prepared.close = func() error { return cleanupFailure }
				}
				storage := preparingDocRangeWorkStore{prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					if stage == "prepare" {
						return prepared, failure
					}
					return prepared, nil
				}}
				service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
				published, err := service.buildDocRangeWork(context.Background(), storage, domain.DocRangeIndexJob{})
				if published != (stage == "success") || (stage != "success" && !errors.Is(err, failure)) || (cleanupFails && !errors.Is(err, cleanupFailure)) || (stage == "success" && !cleanupFails && err != nil) {
					test.Fatal("publication status or joined error lost", published, err)
				}
				wantSteps, wantResources, wantPublications := 1, 1, 0
				if stage == "prepare" {
					wantSteps, wantResources = 0, 0
				} else if stage == "step" {
					wantResources = 0
				} else if stage != "resources" {
					wantPublications = 1
				}
				if prepared.steps != wantSteps || prepared.resourceChecks != wantResources || prepared.publications != wantPublications || prepared.closes != 1 {
					test.Fatal("failure advanced work or cleanup repeated", prepared)
				}
			})
		}
	}
}

func TestBuildDocRangeWorkCancellationStopsCallbacks(tests *testing.T) {
	for _, stage := range []string{"entry", "prepare", "unsupported", "incomplete-step", "complete-step", "resources"} {
		tests.Run(stage, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			preparations := 0
			prepared := &preparedDocRangeWorkStub{
				step: func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
					if stage == "incomplete-step" || stage == "complete-step" {
						cancel()
					}
					if stage == "incomplete-step" {
						return outbound.DocRangeWorkPending, nil
					}
					return outbound.DocRangeWorkPublishable, nil
				},
				resources: func() (outbound.RootPublicationResources, error) {
					if stage == "resources" {
						cancel()
					}
					return outbound.RootPublicationResources{HeapBytes: 4096}, nil
				},
			}
			storage := preparingDocRangeWorkStore{prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
				preparations++
				if stage == "prepare" || stage == "unsupported" {
					cancel()
				}
				if stage == "unsupported" {
					return nil, nil
				}
				return prepared, nil
			}}
			if stage == "entry" {
				cancel()
			}
			service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
			published, err := service.buildDocRangeWork(ctx, storage, domain.DocRangeIndexJob{})
			wantCloses := 1
			if stage == "entry" || stage == "unsupported" {
				wantCloses = 0
			}
			if published || !errors.Is(err, context.Canceled) || prepared.publications != 0 || prepared.closes != wantCloses || (preparations == 0) != (stage == "entry") {
				test.Fatal("cancellation advanced work or leaked cleanup", published, err, prepared, preparations)
			}
		})
	}
}

func TestBuildDocRangeWorkYieldsOnlyOwnedBoundedWork(tests *testing.T) {
	for _, reason := range []string{"owned", "borrowed", "unknown-owner", "unknown-transaction", "transaction", "heap-limit", "spool-limit", "keep-permit"} {
		tests.Run(reason, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				ctx, gate := admissionTestContext(time.Minute)
				bound, permit, release := ownedYieldPermit(test, ctx)
				defer release()
				if reason == "borrowed" {
					bound = context.WithValue(bound, resourceYieldOwnerKey{}, (*resourceYieldOwner)(nil))
				} else if reason == "unknown-owner" {
					bound = context.WithValue(bound, resourceYieldOwnerKey{}, "unknown")
				}
				service := NewService(yieldingPreparationMetadata{nested: reason == "transaction"}, nil, nil, nil, nil)
				if reason == "unknown-transaction" {
					service.meta = streamingPreparationMetadata{}
				}
				resources := outbound.RootPublicationResources{HeapBytes: maxParkedRootHeapBytes, SpoolBytes: maxParkedRootSpoolBytes}
				if reason == "heap-limit" {
					resources.HeapBytes++
				} else if reason == "spool-limit" {
					resources.SpoolBytes++
				} else if reason == "keep-permit" {
					resources = outbound.RootPublicationResources{HeapBytes: 1, SpoolBytes: 1, KeepPermit: true}
				}
				var admitted atomic.Int32
				var stepOpen atomic.Bool
				queued := make(chan error, 2)
				prepared := &preparedDocRangeWorkStub{}
				prepared.step = func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
					stepOpen.Store(true)
					defer stepOpen.Store(false)
					if reason == "owned" && admitted.Load() != int32(prepared.steps-1) {
						test.Error("next step preceded queued work")
					}
					go func() {
						_, finish, err := AcquireResources(ctx, ResourceBulk)
						if err == nil {
							gate.mu.Lock()
							retained := gate.active == 2 && gate.bulk == 1 && gate.retained == permit && permit.parked
							gate.mu.Unlock()
							if stepOpen.Load() || (reason == "owned" && !retained) {
								err = errors.New("work yielded before step closed or lost its retained reservation")
							}
							admitted.Add(1)
							finish()
						}
						queued <- err
					}()
					synctest.Wait()
					if prepared.steps == 2 {
						return outbound.DocRangeWorkPublishable, nil
					}
					return outbound.DocRangeWorkPending, nil
				}
				prepared.resources = func() (outbound.RootPublicationResources, error) {
					if stepOpen.Load() {
						test.Error("resource accounting preceded step closure")
					}
					return resources, nil
				}
				prepared.publish = func(context.Context, domain.DocRangeIndexJob) error {
					wantAdmitted := int32(0)
					if reason == "owned" {
						wantAdmitted = 2
					}
					if admitted.Load() != wantAdmitted {
						test.Error("publication did not follow the final eligible yield", admitted.Load())
					}
					return nil
				}
				prepared.close = func() error {
					gate.mu.Lock()
					reserved := gate.active == 1 && gate.bulk == 1 && gate.retained == nil && !permit.parked
					gate.mu.Unlock()
					if !reserved {
						test.Error("cleanup did not retain resumed bulk admission")
					}
					return nil
				}
				storage := preparingDocRangeWorkStore{prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					return prepared, nil
				}}
				published, err := service.buildDocRangeWork(bound, storage, domain.DocRangeIndexJob{})
				if err != nil || !published || prepared.steps != 2 || prepared.resourceChecks != 2 || prepared.publications != 1 || prepared.closes != 1 {
					test.Fatal("incorrect yielded lifecycle", published, err, prepared)
				}
				release()
				for range 2 {
					if err := <-queued; err != nil {
						test.Fatal(err)
					}
				}
				assertResourcesReleased(test, gate)
			})
		})
	}
}

func TestBuildDocRangeWorkCancellationWhileYieldedRetainsCleanup(tests *testing.T) {
	synctest.Test(tests, func(test *testing.T) {
		ctx, gate := admissionTestContext(time.Minute)
		bound, permit, release := ownedYieldPermit(test, ctx)
		defer release()
		bound, cancel := context.WithCancel(bound)
		defer cancel()
		finish, queued := make(chan struct{}), make(chan error, 1)
		cleanupFailure := errors.New("cleanup failed")
		prepared := &preparedDocRangeWorkStub{
			step: func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
				go func() {
					_, done, err := AcquireResources(ctx, ResourceBulk)
					if err == nil {
						cancel()
						<-finish
						done()
					}
					queued <- err
				}()
				synctest.Wait()
				return outbound.DocRangeWorkPublishable, nil
			},
			close: func() error {
				gate.mu.Lock()
				retained := gate.active == 2 && gate.retained == permit && permit.parked
				gate.mu.Unlock()
				if !retained {
					test.Error("canceled yield lost cleanup reservation")
				}
				return cleanupFailure
			},
		}
		storage := preparingDocRangeWorkStore{prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
			return prepared, nil
		}}
		service := NewService(yieldingPreparationMetadata{}, nil, nil, nil, nil)
		published, err := service.buildDocRangeWork(bound, storage, domain.DocRangeIndexJob{})
		close(finish)
		if queuedError := <-queued; queuedError != nil {
			test.Fatal(queuedError)
		}
		if published || !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupFailure) || prepared.publications != 0 || prepared.closes != 1 {
			test.Fatal("canceled yield published or hid cleanup failure", published, err, prepared)
		}
		release()
		assertResourcesReleased(test, gate)
	})
}
