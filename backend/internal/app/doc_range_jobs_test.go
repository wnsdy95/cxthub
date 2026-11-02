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
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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

func TestDocViewReadinessQueuesWithoutLoadingHeaderOrBody(t *testing.T) {
	ctx, repo, directory := systemTestContext(), h('1'), t.TempDir()
	storage := store.NewFSStore(directory)
	doc, _ := seedRootReadDoc(t, directory, storage, repo, historyMessage(domain.RoleUser, "queued range"))
	for _, state := range []string{"waiting", "running", "retrying", "rejected"} {
		t.Run(state, func(t *testing.T) {
			queued := 0
			fake := &docRangeJobFake{FSStore: storage, enqueue: func(_ context.Context, gotRepo, hash domain.ContentHash, _ time.Time) (domain.DocRangeIndexJob, error) {
				queued++
				if gotRepo != repo || hash != doc.Hash {
					t.Fatal("wrong queue scope")
				}
				return domain.DocRangeIndexJob{State: state, Reason: "invalid_document"}, nil
			}}
			service := NewService(storage, fake, nil, nil, nil)
			_, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, "chat")
			want := domain.ErrDocRangeIndexPending
			if state == "rejected" {
				want = domain.ErrIntegrity
			}
			if !errors.Is(err, want) || queued != 1 || fake.ensures != 0 || fake.headers != 0 {
				t.Fatal("viewer performed work or hid queue state", err, queued, fake.ensures, fake.headers)
			}
			_, err = service.ReadDocViewFragment(ctx, repo, doc.Hash, 0, 0, 32)
			if !errors.Is(err, want) || queued != 2 {
				t.Fatal("fragment skipped queue contract", err, queued)
			}
			_, err = service.ReadDocViewEvents(outbound.WithDocReadOnly(ctx), repo, doc.Hash, "", 0, 20, "chat")
			if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) || queued != 2 {
				t.Fatal("read-only request queued maintenance", err, queued)
			}
			fake.ready = true
			if _, err := service.ReadDocViewEvents(ctx, repo, doc.Hash, "", 0, 20, "chat"); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
				t.Fatal("readiness was mistaken for a verified index", err)
			}
		})
	}
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
			service := NewService(preparationMetadata{}, fake, nil, nil, nil)
			err := service.runDocRangeJob(ctx, fake, job, docJobLease)
			if !errors.Is(err, scenario.failure) || (finished == 1) != (scenario.state != "") {
				t.Fatal("incorrect completion", err, finished)
			}
		})
	}
}

func TestDocRangeWorkerRenewsAndBoundsWork(t *testing.T) {
	for _, lost := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			renewals, finished := 0, 0
			fake := &docRangeJobFake{
				build: func(ctx context.Context, _ domain.DocRangeIndexJob) error {
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
			service := NewService(preparationMetadata{}, fake, nil, nil, nil)
			err := service.runDocRangeJob(context.Background(), fake, domain.DocRangeIndexJob{RepoID: h('1'), Attempts: 1}, docJobLease)
			if err == nil || finished != 1 || renewals < 1 || (lost && renewals != 1) {
				t.Fatal("unbounded or unrenewed work", err, finished, renewals)
			}
			fake.build = func(context.Context, domain.DocRangeIndexJob) error { return nil }
			if err := service.runDocRangeJob(context.Background(), fake, domain.DocRangeIndexJob{RepoID: h('1'), Attempts: 2, Version: 2}, docJobLease); err != nil || finished != 1 {
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
	service := NewService(preparationMetadata{}, fake, nil, nil, nil)
	if err := service.ProcessDocRangeJobs(ctx, 1); !errors.Is(err, domain.ErrResourceOverloaded) {
		t.Fatal("worker bypassed admission", err)
	}
	release()
	assertResourcesReleased(t, gate)
}
