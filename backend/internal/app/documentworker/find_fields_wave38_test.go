package documentworker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave38FindFieldsStore struct {
	outbound.DocFindFieldsStore
	scope        *wave38DisplayScope
	job          domain.DocRangeIndexJob
	existing     map[domain.ContentHash]bool
	read         []domain.ContentHash
	put          []domain.DocFindFields
	putReads     []int
	finished     []domain.DocRangeIndexJob
	finishErrors []error
	enqueued     int
	claims       int
	quota        int
}

func (storage *wave38FindFieldsStore) ClaimDocFindFields(ctx context.Context, _ time.Time, lease time.Duration) (domain.DocRangeIndexJob, error) {
	if storage.scope.activeWork != 1 || ctx.Value(wave38DisplayWorkKey{}) != true || lease != Lease {
		storage.scope.test.Fatal("find-fields claim escaped admission or changed lease")
	}
	storage.claims++
	if storage.claims > storage.quota {
		return domain.DocRangeIndexJob{}, domain.ErrNotFound
	}
	return storage.job, nil
}

func (storage *wave38FindFieldsStore) ReadDocFindFields(ctx context.Context, _, _, hash domain.ContentHash, _ int) (domain.DocFindFields, error) {
	storage.scope.requireRead(ctx)
	storage.read = append(storage.read, hash)
	if storage.existing[hash] {
		return domain.DocFindFields{}, nil
	}
	return domain.DocFindFields{}, domain.ErrNotFound
}

func (storage *wave38FindFieldsStore) PutDocFindFields(ctx context.Context, _ domain.DocRangeIndexJob, fields domain.DocFindFields) error {
	storage.scope.requireWrite(ctx)
	storage.put = append(storage.put, fields)
	storage.putReads = append(storage.putReads, storage.scope.reads)
	return nil
}

func (storage *wave38FindFieldsStore) FinishDocFindFields(ctx context.Context, job domain.DocRangeIndexJob, _ time.Time) error {
	storage.scope.requireWrite(ctx)
	deadline, bounded := ctx.Deadline()
	if ctx.Err() != nil || !bounded || time.Until(deadline) > 5*time.Second {
		storage.scope.test.Fatal("find-fields finish lost independent bounded context")
	}
	storage.finished = append(storage.finished, job)
	if len(storage.finished) <= len(storage.finishErrors) {
		return storage.finishErrors[len(storage.finished)-1]
	}
	return nil
}

func (storage *wave38FindFieldsStore) EnqueueDocFindFields(ctx context.Context, repo, hash domain.ContentHash, _ time.Time) (domain.DocRangeIndexJob, error) {
	storage.scope.requireWrite(ctx)
	if repo != storage.job.RepoID || hash != storage.job.DocHash {
		storage.scope.test.Fatal("find-fields refresh lost source identity")
	}
	storage.enqueued++
	return storage.job, nil
}

func TestWave38FindFieldsOwnerBatchesAndPublishesOutsideSourceLifetime(test *testing.T) {
	events := make([]domain.CIREvent, domain.DocRangePageEvents+1)
	for index := range events {
		events[index] = domain.CIREvent{Kind: domain.EventToolResult, Output: "small"}
	}
	events[0] = domain.CIREvent{Kind: domain.EventReasoning, RedactedSummary: "public needle", Locked: &domain.LockedBlob{
		Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("private", 25_000)}}
	events[1] = domain.CIREvent{Kind: domain.EventReasoning, Locked: &domain.LockedBlob{
		Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: strings.Repeat("private", 25_000)}}
	events[2].Output = strings.Repeat("not a find-fields kind", 10_000)
	events[4] = domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("existing", 25_000)}}}
	events[domain.DocRangePageEvents] = domain.CIREvent{Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("public", 25_000)}}}
	fixture, queries := newWave38DisplayFixture(test, events)
	sources := &wave38DisplaySources{queries: queries, scope: fixture.scope}
	storage := &wave38FindFieldsStore{scope: fixture.scope, existing: map[domain.ContentHash]bool{fixture.pages[0][4].Hash: true}}
	job := domain.DocRangeIndexJob{RepoID: fixture.repo, DocHash: fixture.hash}
	if err := NewFindFieldsWorker(storage, sources, fixture.scope).Build(context.Background(), job); err != nil {
		test.Fatal(err)
	}
	if sources.view != 2 || sources.display != 0 || fixture.metadata != 2 || fixture.scope.reads != 2 ||
		len(storage.read) != 3 || len(storage.put) != 2 || !reflect.DeepEqual(storage.putReads, []int{1, 2}) ||
		storage.put[0].EventHash != fixture.pages[0][0].Hash || storage.put[1].EventHash != fixture.pages[1][0].Hash || storage.put[0].Summary == nil || storage.put[1].Blocks == nil {
		test.Fatal("find-fields changed batch, filter, source mode, or write ordering", storage, sources, fixture.metadata)
	}
	if _, err := sources.last.Read(0, 1); !errors.Is(err, context.Canceled) {
		test.Fatal("source remained live beyond the read callback", err)
	}
	fixture.scope.afterRead = errors.New("snapshot finish failed")
	storage.put = nil
	if err := NewFindFieldsWorker(storage, sources, fixture.scope).Build(context.Background(), job); !errors.Is(err, fixture.scope.afterRead) || len(storage.put) != 0 {
		test.Fatal("failed read scope published partial fields", err, storage.put)
	}
}

func TestWave38FindFieldsOwnerFinishRecoveryAndCancellation(test *testing.T) {
	for _, scenario := range []struct {
		name, state, reason string
		failure             error
		finishErrors        []error
		finishes, enqueued  int
	}{
		{name: "success", state: "completed", finishes: 1},
		{name: "completion-source-changed", state: "retrying", reason: "source_changed", finishErrors: []error{domain.ErrConflict}, finishes: 2, enqueued: 1},
		{name: "release-conflict", state: "retrying", reason: "source_changed", finishErrors: []error{domain.ErrConflict, domain.ErrConflict}, finishes: 2},
		{name: "build-source-changed", state: "retrying", reason: "temporary_failure", failure: domain.ErrConflict, finishes: 1, enqueued: 1},
		{name: "missing", state: "rejected", reason: "source_unavailable", failure: domain.ErrNotFound, finishes: 1},
		{name: "integrity", state: "rejected", reason: "invalid_document", failure: domain.ErrIntegrity, finishes: 1},
		{name: "validation", state: "rejected", reason: "invalid_document", failure: domain.ErrValidation, finishes: 1},
		{name: "interrupted", state: "retrying", reason: "verification_interrupted", failure: context.Canceled, finishes: 1},
		{name: "deadline", state: "retrying", reason: "verification_interrupted", failure: context.DeadlineExceeded, finishes: 1},
		{name: "temporary", state: "retrying", reason: "temporary_failure", failure: errors.New("read failed"), finishes: 1},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture, queries := newWave38DisplayFixture(test, nil)
			fixture.scope.readError = scenario.failure
			storage := &wave38FindFieldsStore{scope: fixture.scope, job: domain.DocRangeIndexJob{RepoID: fixture.repo, DocHash: fixture.hash, Attempts: 10}, finishErrors: scenario.finishErrors}
			err := NewFindFieldsWorker(storage, queries, fixture.scope).RunJob(context.Background(), storage.job)
			want := scenario.failure
			if want == nil && len(scenario.finishErrors) > 0 {
				want = scenario.finishErrors[0]
			}
			if !errors.Is(err, want) || len(storage.finished) != scenario.finishes || storage.enqueued != scenario.enqueued {
				test.Fatal("find-fields lost finish or refresh recovery", err, storage)
			}
			finished := storage.finished[len(storage.finished)-1]
			if finished.State != scenario.state || finished.Reason != scenario.reason || !finished.LeaseUntil.IsZero() {
				test.Fatal("find-fields changed job classification", finished)
			}
			if scenario.failure != nil && finished.NextAttempt.Before(time.Now().Add(250*time.Second)) {
				test.Fatal("find-fields retry backoff was not capped at eight shifts", finished.NextAttempt)
			}
		})
	}
	scope := &wave38DisplayScope{test: test}
	storage := &wave38FindFieldsStore{scope: scope}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewFindFieldsWorker(storage, nil, scope).RunJob(ctx, domain.DocRangeIndexJob{}); !errors.Is(err, context.Canceled) || len(storage.finished) != 0 {
		test.Fatal("canceled find-fields job was durably finished", err, storage.finished)
	}
}

func TestWave38FindFieldsOwnerAdmissionAndOldEmptyLimitSemantics(test *testing.T) {
	scope := &wave38DisplayScope{test: test, workError: domain.ErrResourceOverloaded}
	storage := &wave38FindFieldsStore{scope: scope}
	worker := NewFindFieldsWorker(storage, nil, scope)
	if err := worker.Process(context.Background(), 1); !errors.Is(err, domain.ErrResourceOverloaded) || storage.claims != 0 {
		test.Fatal("find-fields claimed before work admission", err, storage.claims)
	}
	scope.workError = nil
	if err := worker.Process(context.Background(), 2); err != nil || storage.claims != 1 || scope.works != 1 {
		test.Fatal("empty find-fields queue lost its normal stop", err, storage.claims)
	}
	if err := worker.Process(context.Background(), -1); err != nil || storage.claims != 1 {
		test.Fatal("nonpositive find-fields limit claimed work", err)
	}
	if err := NewFindFieldsWorker(nil, nil, nil).Process(context.Background(), 1); err != nil {
		test.Fatal("nil find-fields storage stopped being a no-op", err)
	}
}
