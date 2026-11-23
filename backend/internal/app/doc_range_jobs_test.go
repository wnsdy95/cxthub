package app

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docRangeJobFake struct {
	*store.FSStore
	ready                    bool
	checks, headers, ensures int
	enqueue                  func(context.Context, domain.ContentHash, domain.ContentHash, time.Time) (domain.DocRangeIndexJob, error)
	claim                    func(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error)
	build                    func(context.Context, domain.DocRangeIndexJob) error
	renew                    func(context.Context, domain.DocRangeIndexJob, time.Time, time.Duration) error
	finish                   func(context.Context, domain.DocRangeIndexJob, time.Time) error
}

func (storage *docRangeJobFake) HasDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) (bool, error) {
	storage.checks++
	return storage.ready, nil
}

func (storage *docRangeJobFake) DocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (domain.DocRangeIndex, error) {
	storage.headers++
	return storage.FSStore.DocRangeIndex(ctx, repo, hash)
}

func (storage *docRangeJobFake) EnsureDocRangeIndex(context.Context, domain.ContentHash, domain.ContentHash) error {
	storage.ensures++
	return errors.New("viewer must not build a queued index")
}

func (storage *docRangeJobFake) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	return storage.enqueue(ctx, repo, hash, now)
}

func (storage *docRangeJobFake) ClaimDocRangeJob(ctx context.Context, now time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	return storage.claim(ctx, now, lease)
}

func (storage *docRangeJobFake) BuildDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob) error {
	return storage.build(ctx, job)
}

func (storage *docRangeJobFake) RenewDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time, lease time.Duration) error {
	return storage.renew(ctx, job, now, lease)
}

func (storage *docRangeJobFake) FinishDocRangeJob(ctx context.Context, job domain.DocRangeIndexJob, now time.Time) error {
	return storage.finish(ctx, job, now)
}

func TestDocRangeWorkerFailureAndShutdown(t *testing.T) {
	for _, scenario := range []struct {
		name, state, reason string
		failure             error
	}{
		{"success", "", "", nil},
		{"temporary", "retrying", "temporary_failure", errors.New("connection unavailable")},
		{"changed source generation", "retrying", "temporary_failure", domain.ErrConflict},
		{"wrapped source generation", "retrying", "temporary_failure", fmt.Errorf("source confirmation: %w", domain.ErrConflict)},
		{"missing", "rejected", "source_unavailable", domain.ErrNotFound},
		{"corrupt", "rejected", "invalid_document", domain.ErrIntegrity},
		{"prepared metadata", "retrying", "prepared_artifact_corrupt", domain.ErrPreparedDocRangeObject},
		{"wrapped prepared metadata", "retrying", "prepared_artifact_corrupt", fmt.Errorf("load private spool: %w", domain.ErrPreparedDocRangeObject)},
		{"canceled prepared metadata", "retrying", "verification_interrupted", errors.Join(domain.ErrPreparedDocRangeObject, context.Canceled)},
		{"wrapped cancellation", "retrying", "verification_interrupted", fmt.Errorf("%w: event validation: %w", domain.ErrConversationManifest, context.Canceled)},
		{"wrapped deadline", "retrying", "verification_interrupted", fmt.Errorf("%w: event validation: %w", domain.ErrConversationManifest, context.DeadlineExceeded)},
		{"quota", "retrying", "resource_limit", domain.ErrDocumentResourceLimit},
		{"policy", "retrying", "identity_upgrade_required", domain.ErrDocumentIdentityUpgradeRequired},
		{"byte preparation", "retrying", "byte_backfill_required", domain.ErrDocByteRangeBackfillRequired},
		{"shutdown", "", "", context.Canceled},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			job := domain.DocRangeIndexJob{RepoID: h('1'), DocHash: h('2'), Version: 7, Attempts: 2, State: "running"}
			finished := 0
			fake := &docRangeJobFake{
				build: func(context.Context, domain.DocRangeIndexJob) error {
					if scenario.name == "shutdown" {
						cancel()
					}
					return scenario.failure
				},
				finish: func(finish context.Context, got domain.DocRangeIndexJob, now time.Time) error {
					finished++
					if finish.Err() != nil || got.State != scenario.state || got.Reason != scenario.reason || got.Version != job.Version || !got.NextAttempt.After(now) || !got.LeaseUntil.IsZero() {
						t.Fatal("lost failure state or fence", got, finish.Err())
					}
					return nil
				},
			}
			service := NewService(preparationMetadata{}, fake, nil, nil)
			err := service.runDocRangeJob(ctx, fake, job, docJobLease)
			if !errors.Is(err, scenario.failure) || (finished == 1) != (scenario.state != "") {
				t.Fatal("incorrect completion", err, finished)
			}
		})
	}
}

func TestDocRangeWorkerRenewsUntilCompletionOrLeaseLoss(t *testing.T) {
	for _, lost := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			renewals, finished := 0, 0
			fake := &docRangeJobFake{
				build: func(ctx context.Context, _ domain.DocRangeIndexJob) error {
					if !lost {
						time.Sleep(docJobWorkLimit + docJobLease)
						return ctx.Err()
					}
					<-ctx.Done()
					return fmt.Errorf("%w: event validation: %w", domain.ErrConversationManifest, ctx.Err())
				},
				renew: func(_ context.Context, _ domain.DocRangeIndexJob, _ time.Time, lease time.Duration) error {
					renewals++
					if lease != docJobLease {
						t.Fatal("lease changed")
					}
					if lost {
						return domain.ErrConflict
					}
					return nil
				},
				finish: func(ctx context.Context, got domain.DocRangeIndexJob, _ time.Time) error {
					finished++
					if ctx.Err() != nil || got.State != "retrying" || got.Reason != "verification_interrupted" {
						t.Fatal("failure was not recorded with fresh deadline")
					}
					return nil
				},
			}
			service := NewService(preparationMetadata{}, fake, nil, nil)
			err := service.runDocRangeJob(context.Background(), fake, domain.DocRangeIndexJob{RepoID: h('1'), Attempts: 1}, docJobLease)
			wantFinished := 0
			if lost {
				wantFinished = 1
				if !errors.Is(err, context.Canceled) || !errors.Is(err, domain.ErrConversationManifest) || renewals != 1 {
					t.Fatal("lost lease did not interrupt work or preserve its errors", err, renewals)
				}
			} else if err != nil || renewals <= int(docJobWorkLimit/(docJobLease/3)) {
				t.Fatal("healthy renewal did not outlive the old work limit", err, renewals)
			}
			if finished != wantFinished {
				t.Fatal("completion or lease loss changed failure recording", finished, wantFinished)
			}
			fake.build = func(context.Context, domain.DocRangeIndexJob) error { return nil }
			if err := service.runDocRangeJob(context.Background(), fake, domain.DocRangeIndexJob{RepoID: h('1'), Attempts: 2, Version: 2}, docJobLease); err != nil || finished != wantFinished {
				t.Fatal("interrupted document could not complete on retry", err, finished)
			}
		})
	}
}

func TestDocRangeWorkerAdmissionPrecedesClaim(t *testing.T) {
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	fake := &docRangeJobFake{claim: func(context.Context, time.Time, time.Duration) (domain.DocRangeIndexJob, error) {
		t.Fatal("claimed while bulk lane occupied")
		return domain.DocRangeIndexJob{}, nil
	}}
	service := NewService(preparationMetadata{}, fake, nil, nil)
	if err := service.ProcessDocRangeJobs(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("worker bypassed admission", err)
	}
	release()
	assertResourcesReleased(t, gate)
}
