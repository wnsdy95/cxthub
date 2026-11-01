package documentworker

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rangeWave38ContextKey string

type rangeWave38Scope struct {
	worker   func(context.Context) context.Context
	work     func(context.Context, func(context.Context) error) error
	read     func(context.Context, domain.ContentHash, func(context.Context) error) error
	canYield func(context.Context) bool
	yield    func(context.Context, outbound.RootPublicationResources) error
}

func (scope rangeWave38Scope) WorkerContext(ctx context.Context) context.Context {
	if scope.worker != nil {
		return scope.worker(ctx)
	}
	return ctx
}

func (scope rangeWave38Scope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	if scope.work != nil {
		return scope.work(ctx, work)
	}
	return work(ctx)
}

func (scope rangeWave38Scope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if scope.read != nil {
		return scope.read(ctx, repo, read)
	}
	return read(ctx)
}

func (scope rangeWave38Scope) CanYield(ctx context.Context) bool {
	return scope.canYield != nil && scope.canYield(ctx)
}

func (scope rangeWave38Scope) Yield(ctx context.Context, resources outbound.RootPublicationResources) error {
	return scope.yield(ctx, resources)
}

type rangeWave38Store struct {
	outbound.DocRangeJobStore
	claim  func(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error)
	build  func(context.Context, domain.DocRangeIndexJob) error
	renew  func(context.Context, domain.DocRangeIndexJob, time.Time, time.Duration) error
	finish func(context.Context, domain.DocRangeIndexJob, time.Time) error
}

func (storage *rangeWave38Store) ClaimDocRangeJob(ctx context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	return storage.claim(ctx, now, lease)
}

func (storage *rangeWave38Store) BuildDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	return storage.build(ctx, job)
}

func (storage *rangeWave38Store) RenewDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time, lease time.Duration) error {
	return storage.renew(ctx, job, now, lease)
}

func (storage *rangeWave38Store) FinishDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	return storage.finish(ctx, job, now)
}

func TestRangeWave38ProcessKeepsAdmissionIdentityAndReadBoundary(test *testing.T) {
	ctx := context.WithValue(context.Background(), rangeWave38ContextKey("request"), "preserved")
	job := domain.DocRangeIndexJob{RepoID: "repo", DocHash: "document", BaseHash: "base", Version: 17, Attempts: 3, State: "running"}
	var calls []string
	admitted, reading := false, false
	scope := rangeWave38Scope{
		worker: func(selected context.Context) context.Context {
			if selected != ctx {
				test.Fatal("worker context lost its caller")
			}
			calls = append(calls, "worker-context")
			return context.WithValue(selected, rangeWave38ContextKey("worker"), "identity")
		},
		work: func(selected context.Context, work func(context.Context) error) error {
			if admitted || selected.Value(rangeWave38ContextKey("worker")) != "identity" {
				test.Fatal("work skipped worker identity or reused active admission")
			}
			calls = append(calls, "admit")
			admitted = true
			err := work(context.WithValue(selected, rangeWave38ContextKey("admission"), "owned"))
			admitted = false
			calls = append(calls, "release")
			return err
		},
		read: func(selected context.Context, repo domain.ContentHash, read func(context.Context) error) error {
			if !admitted || repo != job.RepoID {
				test.Fatal("repository preflight changed scope or preceded admission")
			}
			calls = append(calls, "read")
			reading = true
			err := read(context.WithValue(selected, rangeWave38ContextKey("read"), "snapshot"))
			reading = false
			calls = append(calls, "read-closed")
			return err
		},
	}
	claims := 0
	storage := &rangeWave38Store{
		claim: func(selected context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
			claims++
			if !admitted || reading || selected.Value(rangeWave38ContextKey("admission")) != "owned" || lease != Lease || now.Location() != time.UTC {
				test.Fatal("claim changed admission, lease, or clock")
			}
			calls = append(calls, "claim")
			return job, nil
		},
		build: func(selected context.Context, got domain.DocRangeIndexJob) error {
			deadline, bounded := selected.Deadline()
			if !admitted || reading || got != job || selected.Value(rangeWave38ContextKey("read")) != nil ||
				selected.Value(rangeWave38ContextKey("request")) != "preserved" || selected.Value(rangeWave38ContextKey("worker")) != "identity" ||
				selected.Value(rangeWave38ContextKey("admission")) != "owned" || !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > WorkLimit {
				test.Fatal("build changed job, lifetime, or context boundary")
			}
			calls = append(calls, "build")
			return nil
		},
		finish: func(context.Context, domain.DocRangeIndexJob, time.Time) error {
			test.Fatal("successful build was finished again")
			return nil
		},
	}
	if err := NewRangeWorker(storage, scope).Process(ctx, 2); err != nil {
		test.Fatal(err)
	}
	want := []string{"worker-context", "admit", "claim", "read", "read-closed", "build", "release", "admit", "claim", "read", "read-closed", "build", "release"}
	if claims != 2 || admitted || reading || !reflect.DeepEqual(calls, want) {
		test.Fatal("process changed its limit or lifecycle", claims, calls)
	}
}

func TestRangeWave38ProcessDistinguishesEmptyFromClaimedFailure(tests *testing.T) {
	claimFailure := errors.New("claim unavailable")
	for _, scenario := range []struct {
		name                        string
		admission, claim, preflight error
		build                       error
		want                        error
		claims, builds, finishes    int
	}{
		{name: "admission", admission: domain.ErrResourceOverloaded, want: domain.ErrResourceOverloaded},
		{name: "empty", claim: errors.Join(domain.ErrNotFound, claimFailure), claims: 1},
		{name: "claim-failure", claim: claimFailure, want: claimFailure, claims: 1},
		{name: "missing-source", build: domain.ErrNotFound, want: domain.ErrNotFound, claims: 1, builds: 1, finishes: 1},
		{name: "preflight", preflight: domain.ErrDocumentIdentityUpgradeRequired, want: domain.ErrDocumentIdentityUpgradeRequired, claims: 1, finishes: 1},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			claims, builds, finishes := 0, 0, 0
			scope := rangeWave38Scope{
				work: func(ctx context.Context, work func(context.Context) error) error {
					if scenario.admission != nil {
						return scenario.admission
					}
					return work(ctx)
				},
				read: func(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
					if scenario.preflight != nil {
						return scenario.preflight
					}
					return read(ctx)
				},
			}
			storage := &rangeWave38Store{
				claim: func(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error) {
					claims++
					return domain.DocRangeIndexJob{RepoID: "repo"}, scenario.claim
				},
				build: func(context.Context, domain.DocRangeIndexJob) error {
					builds++
					return scenario.build
				},
				finish: func(context.Context, domain.DocRangeIndexJob, time.Time) error {
					finishes++
					return nil
				},
			}
			err := NewRangeWorker(storage, scope).Process(context.Background(), 5)
			if !errors.Is(err, scenario.want) || claims != scenario.claims || builds != scenario.builds || finishes != scenario.finishes {
				test.Fatal("empty queue swallowed claimed work or failed admission claimed a job", err, claims, builds, finishes)
			}
		})
	}
}

func TestRangeWave38RunJobPreservesFailureOrderingAndBackoff(tests *testing.T) {
	temporary, finishFailure := errors.New("temporary"), errors.New("finish failed")
	for _, scenario := range []struct {
		name, state, reason string
		failure             error
	}{
		{"temporary", "retrying", "temporary_failure", temporary},
		{"generation", "retrying", "temporary_failure", domain.ErrConflict},
		{"backfill-first", "retrying", "byte_backfill_required", errors.Join(domain.ErrDocByteRangeBackfillRequired, context.Canceled)},
		{"cancel-before-prepared", "retrying", "verification_interrupted", errors.Join(domain.ErrPreparedDocRangeObject, context.Canceled)},
		{"deadline-before-invalid", "retrying", "verification_interrupted", errors.Join(domain.ErrIntegrity, context.DeadlineExceeded)},
		{"prepared-before-missing", "retrying", "prepared_artifact_corrupt", errors.Join(domain.ErrPreparedDocRangeObject, domain.ErrNotFound)},
		{"missing-before-invalid", "rejected", "source_unavailable", errors.Join(domain.ErrNotFound, domain.ErrIntegrity)},
		{"integrity", "rejected", "invalid_document", domain.ErrIntegrity},
		{"validation", "rejected", "invalid_document", domain.ErrValidation},
		{"manifest", "rejected", "invalid_document", domain.ErrConversationManifest},
		{"cir-version", "rejected", "invalid_document", domain.ErrUnsupportedCIRVersion},
		{"identity", "rejected", "invalid_document", domain.ErrUnsupportedDocumentIdentity},
		{"upgrade-before-resources", "retrying", "identity_upgrade_required", errors.Join(domain.ErrDocumentIdentityUpgradeRequired, domain.ErrDocumentResourceLimit)},
		{"resources", "retrying", "resource_limit", domain.ErrDocumentResourceLimit},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			for _, attempts := range []int{0, 2, 8, 12} {
				ctx := context.WithValue(context.Background(), rangeWave38ContextKey("finish"), "caller")
				job := domain.DocRangeIndexJob{RepoID: "repo", DocHash: "document", BaseHash: "base", State: "running", Reason: "old", Version: 23, Attempts: attempts, CreatedAt: time.Unix(123, 0).UTC(), LeaseUntil: time.Now().Add(time.Minute)}
				finished := 0
				storage := &rangeWave38Store{
					build: func(_ context.Context, selected domain.DocRangeIndexJob) error {
						if selected != job {
							test.Fatal("build changed job before failure")
						}
						return scenario.failure
					},
					finish: func(finish context.Context, selected domain.DocRangeIndexJob, now time.Time) error {
						finished++
						want := job
						want.State, want.Reason = scenario.state, scenario.reason
						want.NextAttempt = now.Add(time.Second << min(attempts, 8))
						want.LeaseUntil = time.Time{}
						deadline, bounded := finish.Deadline()
						if selected != want || now.Location() != time.UTC || finish.Err() != nil || finish.Value(rangeWave38ContextKey("finish")) != "caller" ||
							!bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
							test.Fatal("finish changed fence, backoff, error classification, or detached context", selected, want)
						}
						return finishFailure
					},
				}
				err := NewRangeWorker(storage, rangeWave38Scope{}).RunJob(ctx, job, Lease)
				if !errors.Is(err, scenario.failure) || !errors.Is(err, finishFailure) || finished != 1 {
					test.Fatal("failure recording lost errors or repeated", err, finished)
				}
			}
		})
	}
}

func TestRangeWave38UnsupportedAndZeroLimitDoNotClaim(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var typedNil *rangeWave38Store
	for _, storage := range []outbound.DocRangeJobStore{nil, typedNil} {
		worker := NewRangeWorker(storage, nil)
		if err := worker.Process(ctx, 1); err != nil {
			test.Fatal("unsupported worker did work", err)
		}
		worker.Run(ctx)
	}
	workerContexts := 0
	scope := rangeWave38Scope{worker: func(ctx context.Context) context.Context {
		workerContexts++
		return ctx
	}}
	worker := NewRangeWorker(&rangeWave38Store{}, scope)
	for _, limit := range []int{-1, 0} {
		if err := worker.Process(ctx, limit); err != nil {
			test.Fatal("nonpositive limit checked cancellation or claimed", err)
		}
	}
	if err := worker.Process(ctx, 1); !errors.Is(err, context.Canceled) || workerContexts != 3 {
		test.Fatal("positive limit missed cancellation or worker context", err, workerContexts)
	}
}

func TestRangeWave38RunJobShutdownDoesNotFinish(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage := &rangeWave38Store{
		build: func(context.Context, domain.DocRangeIndexJob) error {
			cancel()
			return errors.Join(domain.ErrIntegrity, context.Canceled)
		},
		finish: func(context.Context, domain.DocRangeIndexJob, time.Time) error {
			test.Fatal("shutdown rewrote claimed state")
			return nil
		},
	}
	if err := NewRangeWorker(storage, rangeWave38Scope{}).RunJob(ctx, domain.DocRangeIndexJob{}, Lease); !errors.Is(err, context.Canceled) {
		test.Fatal("shutdown lost cancellation", err)
	}
}

func TestRangeWave38RunPollsUntilCancellation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started, claims := time.Now(), 0
		storage := &rangeWave38Store{claim: func(_ context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
			claims++
			if now.Sub(started) != time.Duration(claims-1)*time.Second || lease != Lease {
				test.Fatal("worker changed poll interval or lease", claims, now.Sub(started), lease)
			}
			if claims == 3 {
				cancel()
			}
			return domain.DocRangeIndexJob{}, domain.ErrNotFound
		}}
		NewRangeWorker(storage, rangeWave38Scope{}).Run(ctx)
		if claims != 3 || time.Since(started) != 2*time.Second {
			test.Fatal("worker stopped early or polled after shutdown", claims, time.Since(started))
		}
	})
}
