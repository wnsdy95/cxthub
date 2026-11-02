package documentworker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rangeWave38PreparingStore struct {
	*rangeWave38Store
	prepare       func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error)
	compatibility func(context.Context, domain.DocRangeIndexJob) error
}

func (storage rangeWave38PreparingStore) PrepareDocRangeWork(ctx context.Context, job domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
	return storage.prepare(ctx, job)
}

func (storage rangeWave38PreparingStore) BuildDocRangeCompatibilityJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	return storage.compatibility(ctx, job)
}

type rangeWave38Prepared struct {
	step                                        func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error)
	resources                                   func() (outbound.RootPublicationResources, error)
	publish                                     func(context.Context, domain.DocRangeIndexJob) error
	close                                       func() error
	steps, resourceChecks, publications, closes int
}

func (prepared *rangeWave38Prepared) Step(ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	prepared.steps++
	if prepared.step != nil {
		return prepared.step(ctx, job)
	}
	return outbound.DocRangeWorkPublishable, nil
}

func (prepared *rangeWave38Prepared) RetainedResources() (outbound.RootPublicationResources, error) {
	prepared.resourceChecks++
	if prepared.resources != nil {
		return prepared.resources()
	}
	return outbound.RootPublicationResources{HeapBytes: 4096, SpoolBytes: 128}, nil
}

func (prepared *rangeWave38Prepared) Publish(ctx context.Context, job domain.DocRangeIndexJob) error {
	prepared.publications++
	if prepared.publish != nil {
		return prepared.publish(ctx, job)
	}
	return nil
}

func (prepared *rangeWave38Prepared) Close() error {
	prepared.closes++
	if prepared.close != nil {
		return prepared.close()
	}
	return nil
}

func TestRangeWave38BuildOrdersBoundedStepsAndYield(tests *testing.T) {
	for _, allowYield := range []bool{false, true} {
		tests.Run(map[bool]string{false: "no-yield", true: "owned-yield"}[allowYield], func(test *testing.T) {
			ctx := context.WithValue(context.Background(), rangeWave38ContextKey("build"), "original")
			job := domain.DocRangeIndexJob{RepoID: "repo", DocHash: "document", BaseHash: "base", State: "running", Version: 31, Attempts: 4}
			resources := outbound.RootPublicationResources{HeapBytes: 8192, SpoolBytes: 256}
			var calls []string
			prepared := &rangeWave38Prepared{}
			check := func(selected context.Context, got domain.DocRangeIndexJob) {
				if selected != ctx || got != job || prepared.closes != 0 {
					test.Fatal("build changed caller, job, or private lifetime")
				}
			}
			prepared.step = func(selected context.Context, got domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
				check(selected, got)
				calls = append(calls, "step")
				if prepared.steps == 1 {
					return outbound.DocRangeWorkPending, nil
				}
				return outbound.DocRangeWorkPublishable, nil
			}
			prepared.resources = func() (outbound.RootPublicationResources, error) {
				calls = append(calls, "resources")
				return resources, nil
			}
			prepared.publish = func(selected context.Context, got domain.DocRangeIndexJob) error {
				check(selected, got)
				calls = append(calls, "publish")
				return nil
			}
			prepared.close = func() error {
				calls = append(calls, "close")
				return nil
			}
			scope := rangeWave38Scope{
				worker: func(context.Context) context.Context {
					test.Fatal("direct build replaced caller identity")
					return nil
				},
				work: func(context.Context, func(context.Context) error) error {
					test.Fatal("direct build opened another admission")
					return nil
				},
				read: func(context.Context, domain.ContentHash, func(context.Context) error) error {
					test.Fatal("direct build changed the repository read boundary")
					return nil
				},
				canYield: func(selected context.Context) bool {
					if selected != ctx || prepared.resourceChecks != prepared.steps {
						test.Fatal("yield eligibility preceded resource accounting")
					}
					calls = append(calls, "can-yield")
					return allowYield
				},
				yield: func(selected context.Context, retained outbound.RootPublicationResources) error {
					if !allowYield || selected != ctx || retained != resources {
						test.Fatal("yield changed resources or bypassed ownership")
					}
					calls = append(calls, "yield")
					return nil
				},
			}
			storage := rangeWave38PreparingStore{prepare: func(selected context.Context, got domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
				check(selected, got)
				calls = append(calls, "prepare")
				return prepared, nil
			}}
			published, err := NewRangeWorker(storage, scope).Build(ctx, job)
			want := []string{"prepare"}
			for step := 0; step < 2; step++ {
				want = append(want, "step", "resources", "can-yield")
				if allowYield {
					want = append(want, "yield")
				}
			}
			want = append(want, "publish", "close")
			if !published || err != nil || prepared.steps != 2 || prepared.resourceChecks != 2 || prepared.publications != 1 || prepared.closes != 1 || !reflect.DeepEqual(calls, want) {
				test.Fatal("build changed step, accounting, yield, or cleanup order", published, err, calls)
			}
		})
	}
}

func TestRangeWave38BuildFallbackRequiresMissingCapability(tests *testing.T) {
	preparationFailure, buildFailure := errors.New("prepare failed"), errors.New("build failed")
	for _, scenario := range []struct {
		name                 string
		preparedPort         bool
		prepareErr, buildErr error
		wantBuild            bool
	}{
		{name: "direct", wantBuild: true},
		{name: "direct-error", buildErr: buildFailure, wantBuild: true},
		{name: "compatibility", preparedPort: true, wantBuild: true},
		{name: "compatibility-error", preparedPort: true, buildErr: buildFailure, wantBuild: true},
		{name: "prepare-error", preparedPort: true, prepareErr: preparationFailure},
		{name: "unsupported-error", preparedPort: true, prepareErr: domain.ErrCatalogUnsupported},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			ctx, job := context.Background(), domain.DocRangeIndexJob{RepoID: "repo", Version: 11}
			builds, preparations := 0, 0
			build := func(selected context.Context, got domain.DocRangeIndexJob) error {
				if selected != ctx || got != job {
					test.Fatal("fallback changed context or job")
				}
				builds++
				return scenario.buildErr
			}
			var storage outbound.DocRangeJobStore = &rangeWave38Store{build: build}
			if scenario.preparedPort {
				storage = rangeWave38PreparingStore{
					rangeWave38Store: &rangeWave38Store{build: func(context.Context, domain.DocRangeIndexJob) error {
						test.Fatal("compatibility reentered generic build")
						return nil
					}},
					prepare: func(selected context.Context, got domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
						if selected != ctx || got != job {
							test.Fatal("preparation changed context or job")
						}
						preparations++
						return nil, scenario.prepareErr
					},
					compatibility: build,
				}
			}
			published, err := NewRangeWorker(storage, nil).Build(ctx, job)
			wantErr := errors.Join(scenario.prepareErr, scenario.buildErr)
			if published != (wantErr == nil) || (wantErr == nil && err != nil) ||
				(scenario.prepareErr != nil && !errors.Is(err, scenario.prepareErr)) || (scenario.buildErr != nil && !errors.Is(err, scenario.buildErr)) {
				test.Fatal("fallback lost its error or publication state", published, err)
			}
			if (builds == 1) != scenario.wantBuild || builds > 1 || (preparations == 1) != scenario.preparedPort {
				test.Fatal("fallback repeated preparation or ignored its failure", builds, preparations)
			}
		})
	}
}

func TestRangeWave38LateFallbackClosesBeforeCompatibility(tests *testing.T) {
	cleanupFailure, fallbackFailure := errors.New("close failed"), errors.New("compatibility failed")
	for _, scenario := range []struct {
		name          string
		closeErr      error
		fallbackErr   error
		cancelOnClose bool
		wantErr       error
		wantFallbacks int
	}{
		{name: "success", wantFallbacks: 1},
		{name: "fallback-error", fallbackErr: fallbackFailure, wantErr: fallbackFailure, wantFallbacks: 1},
		{name: "close-error", closeErr: cleanupFailure, wantErr: cleanupFailure},
		{name: "close-canceled", cancelOnClose: true, wantErr: context.Canceled},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := domain.DocRangeIndexJob{RepoID: "repo", DocHash: "document", Version: 19}
			fallbacks := 0
			var calls []string
			prepared := &rangeWave38Prepared{}
			prepared.step = func(selected context.Context, got domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
				if selected != ctx || got != job || prepared.closes != 0 {
					test.Fatal("step changed context, job, or private lifetime")
				}
				calls = append(calls, "step")
				if prepared.steps == 1 {
					return outbound.DocRangeWorkPending, nil
				}
				return outbound.DocRangeWorkCompatibilityRequired, nil
			}
			prepared.resources = func() (outbound.RootPublicationResources, error) {
				calls = append(calls, "resources")
				return outbound.RootPublicationResources{}, nil
			}
			prepared.close = func() error {
				calls = append(calls, "close")
				if scenario.cancelOnClose {
					cancel()
				}
				return scenario.closeErr
			}
			storage := rangeWave38PreparingStore{
				prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					calls = append(calls, "prepare")
					return prepared, nil
				},
				compatibility: func(selected context.Context, got domain.DocRangeIndexJob) error {
					if selected != ctx || got != job || prepared.closes != 1 || prepared.publications != 0 {
						test.Fatal("late fallback published private work or preceded close")
					}
					fallbacks++
					calls = append(calls, "compatibility")
					return scenario.fallbackErr
				},
			}
			published, err := NewRangeWorker(storage, rangeWave38Scope{}).Build(ctx, job)
			want := []string{"prepare", "step", "resources", "step", "close"}
			if scenario.wantFallbacks != 0 {
				want = append(want, "compatibility")
			}
			if published != (scenario.wantErr == nil) || !errors.Is(err, scenario.wantErr) || fallbacks != scenario.wantFallbacks ||
				prepared.steps != 2 || prepared.resourceChecks != 1 || prepared.publications != 0 || prepared.closes != 1 || !reflect.DeepEqual(calls, want) {
				test.Fatal("late fallback changed cleanup, accounting, or cancellation order", published, err, fallbacks, calls)
			}
		})
	}
}

func TestRangeWave38BuildFailuresCloseOnceBeforeReturning(tests *testing.T) {
	failure, cleanupFailure := errors.New("work failed"), errors.New("cleanup failed")
	for _, stage := range []string{"prepare", "pending", "publishable", "compatibility", "zero-progress", "unknown-progress", "resources", "yield", "publish", "committed"} {
		tests.Run(stage, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			prepared := &rangeWave38Prepared{close: func() error { return cleanupFailure }}
			prepared.step = func(context.Context, domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
				switch stage {
				case "pending":
					return outbound.DocRangeWorkPending, failure
				case "publishable":
					return outbound.DocRangeWorkPublishable, failure
				case "compatibility":
					return outbound.DocRangeWorkCompatibilityRequired, failure
				case "zero-progress":
					return 0, nil
				case "unknown-progress":
					return 255, nil
				}
				return outbound.DocRangeWorkPublishable, nil
			}
			prepared.resources = func() (outbound.RootPublicationResources, error) {
				if stage == "resources" {
					cancel()
					return outbound.RootPublicationResources{}, failure
				}
				return outbound.RootPublicationResources{HeapBytes: 4096}, nil
			}
			prepared.publish = func(context.Context, domain.DocRangeIndexJob) error {
				if stage == "publish" {
					return failure
				}
				return nil
			}
			eligibilityChecks, yields := 0, 0
			scope := rangeWave38Scope{
				canYield: func(context.Context) bool {
					eligibilityChecks++
					return true
				},
				yield: func(context.Context, outbound.RootPublicationResources) error {
					yields++
					if stage == "yield" {
						return failure
					}
					return nil
				},
			}
			storage := rangeWave38PreparingStore{prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
				if stage == "prepare" {
					return prepared, failure
				}
				return prepared, nil
			}}
			published, err := NewRangeWorker(storage, scope).Build(ctx, domain.DocRangeIndexJob{})
			wantErr := failure
			wantSteps, wantResources, wantYields, wantPublications := 1, 0, 0, 0
			switch stage {
			case "prepare":
				wantSteps = 0
			case "zero-progress", "unknown-progress":
				wantErr = domain.ErrIntegrity
			case "resources":
				wantResources = 1
			case "yield":
				wantResources, wantYields = 1, 1
			case "publish":
				wantResources, wantYields, wantPublications = 1, 1, 1
			case "committed":
				wantErr = cleanupFailure
				wantResources, wantYields, wantPublications = 1, 1, 1
			}
			if published != (stage == "committed") || !errors.Is(err, wantErr) || !errors.Is(err, cleanupFailure) || errors.Is(err, context.Canceled) ||
				prepared.steps != wantSteps || prepared.resourceChecks != wantResources || eligibilityChecks != wantYields || yields != wantYields ||
				prepared.publications != wantPublications || prepared.closes != 1 {
				test.Fatal("failure advanced work, lost precedence, or repeated cleanup", published, err, prepared, eligibilityChecks, yields)
			}
		})
	}
}

func TestRangeWave38RunJobDoesNotRewriteCommittedCleanupFailure(tests *testing.T) {
	cleanupFailure, publicationFailure := errors.New("cleanup failed"), errors.New("publication failed")
	for _, committed := range []bool{false, true} {
		tests.Run(map[bool]string{false: "uncommitted", true: "committed"}[committed], func(test *testing.T) {
			finished := 0
			prepared := &rangeWave38Prepared{
				publish: func(context.Context, domain.DocRangeIndexJob) error {
					if !committed {
						return publicationFailure
					}
					return nil
				},
				close: func() error { return cleanupFailure },
			}
			storage := rangeWave38PreparingStore{
				rangeWave38Store: &rangeWave38Store{finish: func(_ context.Context, job domain.DocRangeIndexJob, _ time.Time) error {
					finished++
					if job.State != "retrying" || job.Reason != "temporary_failure" {
						test.Fatal("uncommitted work changed failure state", job)
					}
					return nil
				}},
				prepare: func(context.Context, domain.DocRangeIndexJob) (outbound.PreparedDocRangeWork, error) {
					return prepared, nil
				},
			}
			err := NewRangeWorker(storage, rangeWave38Scope{}).RunJob(context.Background(), domain.DocRangeIndexJob{}, Lease)
			if !errors.Is(err, cleanupFailure) || errors.Is(err, publicationFailure) == committed || (finished == 0) != committed || finished > 1 || prepared.publications != 1 || prepared.closes != 1 {
				test.Fatal("cleanup failure rewrote a committed job or lost an uncommitted failure", err, finished, prepared)
			}
		})
	}
}
