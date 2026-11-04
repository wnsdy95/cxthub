package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/evidencequery"
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave42LegacyGitReader struct {
	read     func(context.Context, string, string, string) (domain.GitCommitDelta, error)
	ancestor func(context.Context, string, string, string) (bool, error)
}

func (reader *wave42LegacyGitReader) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	return reader.read(ctx, origin, commit, parent)
}

func (reader *wave42LegacyGitReader) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	return reader.ancestor(ctx, origin, ancestor, descendant)
}

type wave42LegacyGitAuthorizer struct {
	authorize func(context.Context, domain.ContentHash) (outbound.GitReadFence, error)
}

func (authorizer *wave42LegacyGitAuthorizer) AuthorizeGitRead(ctx context.Context, repo domain.ContentHash) (outbound.GitReadFence, error) {
	return authorizer.authorize(ctx, repo)
}

var (
	_ outbound.GitSourceAuthorizer = (GitSourceAuthorizer)(nil)
	_ GitSourceAuthorizer          = (outbound.GitSourceAuthorizer)(nil)
	_ outbound.GitReadFence        = (GitReadFence)(nil)
	_ GitReadFence                 = (outbound.GitReadFence)(nil)
)

type wave42LegacyGitTransactionKey struct{}

type wave42LegacyGitStore struct {
	*store.FSStore
	claims      int
	claimCtx    context.Context
	claimLease  time.Duration
	claimError  error
	finishes    []domain.GitChangeJob
	finishCtx   []context.Context
	finish      func(context.Context, domain.GitChangeJob) error
	provider    func(context.Context)
	reads       int
	cacheReads  int
	writeScopes []string
	repository  func(context.Context, domain.ContentHash) (domain.Repo, error)
}

func (storage *wave42LegacyGitStore) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	if storage.repository != nil {
		return storage.repository(ctx, repo)
	}
	return storage.FSStore.GetRepo(ctx, repo)
}

func (storage *wave42LegacyGitStore) ClaimGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
	storage.claims++
	storage.claimCtx, storage.claimLease = ctx, lease
	if storage.claimError != nil {
		return domain.GitChangeJob{}, storage.claimError
	}
	return storage.FSStore.ClaimGitChange(ctx, repo, id, now, lease)
}

func (storage *wave42LegacyGitStore) FinishGitChange(ctx context.Context, job domain.GitChangeJob) error {
	storage.finishes = append(storage.finishes, job)
	storage.finishCtx = append(storage.finishCtx, ctx)
	if storage.finish != nil {
		return storage.finish(ctx, job)
	}
	return storage.FSStore.FinishGitChange(ctx, job)
}

func (storage *wave42LegacyGitStore) AdvanceEvidenceRevision(ctx context.Context, repo domain.ContentHash) error {
	scope, _ := ctx.Value(revisionScopeKey{}).(string)
	storage.writeScopes = append(storage.writeScopes, scope)
	return storage.FSStore.AdvanceEvidenceRevision(ctx, repo)
}

func (storage *wave42LegacyGitStore) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	storage.cacheReads++
	return storage.FSStore.GetGitDelta(ctx, repo, origin, commit, parent)
}

type wave42LegacyGitTransactions struct{ *wave42LegacyGitStore }

func (storage *wave42LegacyGitTransactions) WithinRepository(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	return write(context.WithValue(ctx, wave42LegacyGitTransactionKey{}, "write"))
}

func (storage *wave42LegacyGitTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	return read(context.WithValue(ctx, wave42LegacyGitTransactionKey{}, "read"))
}

func wave42LegacyGitFixture(test *testing.T) (GitChangeWorkerConfig, *wave42LegacyGitStore, domain.Repo, *wave42LegacyGitReader) {
	test.Helper()
	storage := &wave42LegacyGitStore{FSStore: store.NewFSStore(test.TempDir())}
	repo := domain.Repo{ID: hh(test.Name()), GitRemoteURL: "https://github.com/example/wave42"}
	if _, err := storage.PutRepo(context.Background(), repo); err != nil {
		test.Fatal(err)
	}
	target, candidate, parent := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	change := domain.GitPathChange{Path: "feature", Before: domain.GitEntry{OID: strings.Repeat("1", 40), Mode: "100644"}, After: domain.GitEntry{OID: strings.Repeat("2", 40), Mode: "100644"}}
	deltas := map[string]domain.GitCommitDelta{
		target:    {Commit: target, Parents: []string{parent}, Parent: parent, Complete: true, Changes: []domain.GitPathChange{change}},
		candidate: {Commit: candidate, Parents: []string{strings.Repeat("d", 40)}, Parent: strings.Repeat("d", 40), Complete: true, Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}},
	}
	observe := func(ctx context.Context) {
		test.Helper()
		storage.reads++
		if outbound.GitRepository(ctx) != repo.ID || ctx.Value(afterCommitKey{}) != nil || ctx.Value(wave42LegacyGitTransactionKey{}) != nil {
			test.Fatal("provider lost repository binding or ran inside a transaction")
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 90*time.Second {
			test.Fatal("provider timeout changed", deadline, bounded)
		}
		if storage.provider != nil {
			storage.provider(ctx)
		}
	}
	reader := &wave42LegacyGitReader{
		read: func(ctx context.Context, origin, commit, _ string) (domain.GitCommitDelta, error) {
			observe(ctx)
			if origin != repo.GitRemoteURL {
				test.Fatal("untrusted provider origin", origin)
			}
			return deltas[commit], nil
		},
		ancestor: func(ctx context.Context, origin, _, _ string) (bool, error) {
			observe(ctx)
			if origin != repo.GitRemoteURL {
				test.Fatal("untrusted ancestry origin", origin)
			}
			return true, nil
		},
	}
	return GitChangeWorkerConfig{Profile: gitchangeworker.Compatibility, Metadata: storage, Blobs: storage, Repositories: storage, Reader: reader}, storage, repo, reader
}

func wave42LegacyEnqueueGitChange(test *testing.T, storage *wave42LegacyGitStore, repo domain.Repo, attempts int) domain.GitChangeJob {
	test.Helper()
	now := time.Now().UTC()
	request := domain.GitChangeRequest{Target: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40)}
	job := domain.GitChangeJob{ID: domain.GitChangeID(repo.ID, repo.GitRemoteURL, request), RepoID: repo.ID, GitOrigin: repo.GitRemoteURL, Request: request, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now, Attempts: attempts}
	queued, err := storage.EnqueueGitChange(context.Background(), job)
	if err != nil {
		test.Fatal(err)
	}
	return queued
}

func wave42LegacyClaimGitChange(test *testing.T, storage *wave42LegacyGitStore, job domain.GitChangeJob) domain.GitChangeJob {
	test.Helper()
	claimed, err := storage.FSStore.ClaimGitChange(systemTestContext(), job.RepoID, job.ID, time.Now().UTC(), 2*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	return claimed
}

func TestWave42LegacyGitWorkerFactoryProfilesAndNilPointers(test *testing.T) {
	config, storage, _, _ := wave42LegacyGitFixture(test)
	for _, profile := range []gitchangeworker.Profile{"", "future-profile"} {
		invalid := config
		invalid.Profile = profile
		if worker, err := NewGitChangeWorker(invalid); worker != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("unknown worker profile accepted", profile, err)
		}
	}
	config.Blobs, config.Repositories = nil, nil
	if worker, err := NewGitChangeWorker(config); worker == nil || err != nil {
		test.Fatal("compatibility rejected FS nil blobs/repositories declaration", err)
	}
	config.Profile, config.Blobs, config.Repositories, config.Metadata = gitchangeworker.Complete, storage, storage, &wave42LegacyGitTransactions{storage}
	if worker, err := NewGitChangeWorker(config); worker == nil || err != nil {
		test.Fatal("complete adapters rejected", err)
	}
	for _, field := range []string{"Metadata", "Blobs", "Repositories", "Reader"} {
		for _, typed := range []bool{false, true} {
			invalid := config
			switch field {
			case "Metadata":
				invalid.Metadata = nil
				if typed {
					invalid.Metadata = (*wave42LegacyGitTransactions)(nil)
				}
			case "Blobs":
				invalid.Blobs = nil
				if typed {
					invalid.Blobs = (*store.FSStore)(nil)
				}
			case "Repositories":
				invalid.Repositories = nil
				if typed {
					invalid.Repositories = (*store.FSStore)(nil)
				}
			case "Reader":
				invalid.Reader = nil
				if typed {
					invalid.Reader = (*wave42LegacyGitReader)(nil)
				}
			}
			if typed {
				value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
				if value.IsNil() || value.Elem().Kind() != reflect.Pointer || !value.Elem().IsNil() {
					test.Fatal("fixture is not a nil pointer inside an interface", field)
				}
			}
			if worker, err := NewGitChangeWorker(invalid); worker != nil || err == nil {
				test.Fatal("complete worker accepted absent dependency", field, typed, err)
			}
		}
	}
	for _, missing := range []string{"transactions", "jobs"} {
		invalid := config
		if missing == "transactions" {
			invalid.Metadata = storage
		} else {
			invalid.Metadata = struct {
				outbound.MetadataStore
				outbound.RepositoryTransactions
			}{storage, &wave42LegacyGitTransactions{storage}}
		}
		if worker, err := NewGitChangeWorker(invalid); worker != nil || err == nil {
			test.Fatal("complete worker fabricated missing capability", missing, err)
		}
	}
	if storage.claims != 0 || storage.reads != 0 || len(storage.finishes) != 0 {
		test.Fatal("construction performed worker I/O")
	}
}

func TestWave42LegacyGitWorkerDirectProcessUsesOriginalAdmissionAndDeclarations(test *testing.T) {
	config, storage, repo, _ := wave42LegacyGitFixture(test)
	config.Metadata, config.Profile = &wave42LegacyGitTransactions{storage}, gitchangeworker.Complete
	queued := wave42LegacyEnqueueGitChange(test, storage, repo, 0)
	var observed *resourcePermit
	storage.provider = func(ctx context.Context) {
		permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
		if permit == nil || permit != observed || permit.owner != processResources || permit.class != ResourceBulk || !permit.live.Load() {
			test.Fatal("provider duplicated or escaped original process admission")
		}
	}
	config.Authorizer = &wave42LegacyGitAuthorizer{authorize: func(ctx context.Context, id domain.ContentHash) (outbound.GitReadFence, error) {
		observed, _ = ctx.Value(resourcePermitKey{}).(*resourcePermit)
		if id != repo.ID || observed == nil {
			test.Fatal("source authorization outside admitted work")
		}
		return func(tx context.Context) error {
			if tx.Value(wave42LegacyGitTransactionKey{}) != "write" || tx.Value(afterCommitKey{}) == nil || tx.Value(resourcePermitKey{}) != observed {
				test.Fatal("source fence outside original publication transaction")
			}
			return nil
		}, nil
	}}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	spoof := []domain.DocumentIdentity{"future-unsupported"}
	ctx := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(context.Background(), "caller"), spoof)
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, spoof, spoof)
	if err := worker.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	if storage.claims != 1 || storage.claimLease != 2*time.Minute || storage.reads != 3 || observed == nil || observed.live.Load() {
		test.Fatal("claim, lease, provider count or permit release changed", storage.claims, storage.claimLease, storage.reads)
	}
	if _, system := inbound.RepositoryActor(storage.claimCtx); !system {
		test.Fatal("trusted queue entry lost system actor")
	}
	if !reflect.DeepEqual(inbound.DocumentIdentities(storage.claimCtx), ReadDocumentIdentities(config.Metadata, config.Blobs)) || outbound.CheckDocumentIdentityCompatibility(storage.claimCtx, domain.DocumentIdentityRootV1) != nil {
		test.Fatal("caller declaration survived worker entry")
	}
	completed, err := storage.GetGitChange(context.Background(), repo.ID, queued.ID)
	if err != nil || completed.State != "completed" || completed.Result == nil || completed.Result.Coverage != "full" || completed.Version != 1 || completed.Attempts != 1 || !completed.LeaseUntil.IsZero() {
		test.Fatal("direct worker did not publish fenced evidence", completed, err)
	}
	if !reflect.DeepEqual(storage.writeScopes, []string{"evidence"}) {
		test.Fatal("worker changed repository revision scope", storage.writeScopes)
	}
	refs, refsErr := storage.ListRefs(context.Background(), repo.ID)
	history, historyErr := storage.ListHistoryEvents(context.Background(), repo.ID)
	if refsErr != nil || historyErr != nil || len(refs) != 0 || len(history) != 0 {
		test.Fatal("evidence worker changed history or refs", refs, history, refsErr, historyErr)
	}
}

func TestWave42LegacyGitWorkerProcessErrorOrderAndLimit(test *testing.T) {
	config, storage, _, _ := wave42LegacyGitFixture(test)
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, limit := range []int{-1, 0} {
		if err := worker.Process(ctx, limit); err != nil || storage.claims != 0 {
			test.Fatal("empty limit changed original cancellation order", limit, err)
		}
	}
	if err := worker.Process(ctx, 1); !errors.Is(err, context.Canceled) || storage.claims != 0 {
		test.Fatal("canceled process claimed work", err)
	}
	if err := worker.Process(context.Background(), 4); err != nil || storage.claims != 1 {
		test.Fatal("empty queue failed to stop at first miss", err, storage.claims)
	}
	storage.claimError = domain.ErrIntegrity
	if err := worker.Process(context.Background(), 4); !errors.Is(err, domain.ErrIntegrity) || storage.claims != 2 {
		test.Fatal("claim error swallowed", err, storage.claims)
	}
}

func TestWave42LegacyGitWorkerMetadataOnlyCompatibilityAndCurrentIdentityFence(test *testing.T) {
	for _, scenario := range []string{"legacy", "before-claim", "before-publish"} {
		test.Run(scenario, func(test *testing.T) {
			config, storage, repo, _ := wave42LegacyGitFixture(test)
			config.Blobs, config.Repositories = nil, nil
			queued := wave42LegacyEnqueueGitChange(test, storage, repo, 0)
			upgrade := func() {
				if err := storage.RequireDocumentIdentity(context.Background(), repo.ID, domain.DocumentIdentityRootV1); err != nil {
					test.Fatal(err)
				}
			}
			if scenario == "before-claim" {
				upgrade()
			}
			if scenario == "before-publish" {
				storage.provider = func(context.Context) {
					if storage.reads == 1 {
						upgrade()
					}
				}
			}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			ctx := inbound.WithDocumentIdentities(context.Background(), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
			if err := worker.Process(ctx, 1); err != nil {
				test.Fatal("per-job identity failure escaped Process", err)
			}
			current, err := storage.GetGitChange(context.Background(), repo.ID, queued.ID)
			if err != nil {
				test.Fatal(err)
			}
			switch scenario {
			case "legacy":
				if current.State != "completed" || current.Result == nil {
					test.Fatal("metadata-only FS recovery failed", current)
				}
			case "before-claim":
				if current.State != "waiting" || current.Attempts != 0 || storage.reads != 0 {
					test.Fatal("caller root declaration spent unsupported claim", current, storage.reads)
				}
			case "before-publish":
				if current.State != "running" || current.Attempts != 1 || current.Result != nil || len(storage.finishes) != 0 {
					test.Fatal("publication crossed current document identity requirement", current)
				}
			}
		})
	}
}

func TestWave42LegacyGitWorkerLeaseFailureClassificationAndBackoff(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		failure  error
		attempts int
		state    string
		reason   string
	}{
		{"temporary", errors.New("offline"), 0, "retrying", "temporary_provider_or_storage_failure"},
		{"integrity", domain.ErrIntegrity, 0, "attention", "integrity_check_failed"},
		{"validation", domain.ErrValidation, 0, "attention", "invalid_or_ambiguous_git_evidence"},
		{"conflict", domain.ErrConflict, 0, "attention", "origin_or_lease_changed"},
		{"missing", domain.ErrNotFound, 0, "attention", "repository_missing"},
		{"forbidden", domain.ErrForbidden, 0, "retrying", "temporary_provider_or_storage_failure"},
		{"limit", errors.New("offline"), 19, "attention", "retry_limit_reached"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			config, storage, repo, reader := wave42LegacyGitFixture(test)
			job := wave42LegacyClaimGitChange(test, storage, wave42LegacyEnqueueGitChange(test, storage, repo, scenario.attempts))
			reader.read = func(context.Context, string, string, string) (domain.GitCommitDelta, error) {
				return domain.GitCommitDelta{}, scenario.failure
			}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			if err := worker.RunLease(systemTestContext(), job); !errors.Is(err, scenario.failure) {
				test.Fatal("lease failure replaced", err)
			}
			next, err := storage.GetGitChange(context.Background(), repo.ID, job.ID)
			if err != nil || next.State != scenario.state || next.Reason != scenario.reason || next.Result != nil || !next.LeaseUntil.IsZero() || next.ID != job.ID || next.Request != job.Request || next.Version != job.Version || next.Attempts != job.Attempts || !next.CreatedAt.Equal(job.CreatedAt) {
				test.Fatal("lease protocol changed", next, err)
			}
			if next.NextAttempt.Sub(next.UpdatedAt) != time.Second<<min(next.Attempts, 8) {
				test.Fatal("original exponential retry cap changed", next)
			}
		})
	}
}

func TestWave42LegacyGitWorkerStaleSourceAndOriginFences(test *testing.T) {
	for _, scenario := range []string{"origin-before-read", "origin-before-publish", "source-before-read", "source-before-publish", "lease-recovered", "unknown-ack"} {
		test.Run(scenario, func(test *testing.T) {
			config, storage, repo, _ := wave42LegacyGitFixture(test)
			config.Metadata = &wave42LegacyGitTransactions{storage}
			job := wave42LegacyClaimGitChange(test, storage, wave42LegacyEnqueueGitChange(test, storage, repo, 0))
			rebind := func() {
				changed := repo
				changed.GitRemoteURL += "-rebound"
				storage.repository = func(context.Context, domain.ContentHash) (domain.Repo, error) { return changed, nil }
				current, err := storage.GetRepo(context.Background(), repo.ID)
				if err != nil || current.GitRemoteURL != changed.GitRemoteURL {
					test.Fatal("origin fixture did not change current metadata", current, err)
				}
			}
			if scenario == "origin-before-read" {
				rebind()
			}
			var recovered domain.GitChangeJob
			storage.provider = func(context.Context) {
				if storage.reads != 1 {
					return
				}
				if scenario == "origin-before-publish" {
					rebind()
				}
				if scenario == "lease-recovered" {
					var err error
					recovered, err = storage.FSStore.ClaimGitChange(systemTestContext(), repo.ID, job.ID, job.LeaseUntil.Add(time.Second), 2*time.Minute)
					if err != nil {
						test.Fatal(err)
					}
				}
			}
			fences := 0
			config.Authorizer = &wave42LegacyGitAuthorizer{authorize: func(context.Context, domain.ContentHash) (outbound.GitReadFence, error) {
				if scenario == "source-before-read" {
					return nil, domain.ErrForbidden
				}
				return func(ctx context.Context) error {
					fences++
					if ctx.Value(wave42LegacyGitTransactionKey{}) != "write" {
						test.Fatal("source fence not transaction-bound")
					}
					if scenario == "source-before-publish" {
						return domain.ErrConflict
					}
					return nil
				}, nil
			}}
			unknown := errors.New("commit acknowledgement lost")
			if scenario == "unknown-ack" {
				storage.finish = func(ctx context.Context, next domain.GitChangeJob) error {
					if err := storage.FSStore.FinishGitChange(ctx, next); err != nil {
						return err
					}
					return unknown
				}
			}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			err = worker.RunLease(systemTestContext(), job)
			current, readErr := storage.GetGitChange(context.Background(), repo.ID, job.ID)
			if readErr != nil {
				test.Fatal(readErr)
			}
			switch scenario {
			case "unknown-ack":
				if !errors.Is(err, unknown) || !errors.Is(err, domain.ErrConflict) || current.State != "completed" || current.Result == nil || len(storage.finishes) != 2 || fences != 1 {
					test.Fatal("lost ACK overwrote completed evidence or blindly replayed", current, err, fences)
				}
			case "lease-recovered":
				if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(current, recovered) {
					test.Fatal("stale lease overwrote recovered worker", current, recovered, err)
				}
			case "source-before-read":
				if !errors.Is(err, domain.ErrForbidden) || storage.reads != 0 || current.State != "retrying" || current.Result != nil || fences != 0 {
					test.Fatal("source denial performed egress or published proof", current, err)
				}
			default:
				if !errors.Is(err, domain.ErrConflict) || current.State != "attention" || current.Result != nil || current.Reason != "origin_or_lease_changed" {
					test.Fatal("stale source/origin published proof", current, err)
				}
				if scenario == "origin-before-read" && storage.reads != 0 || scenario == "source-before-publish" && fences != 1 {
					test.Fatal("source fence ordering changed", storage.reads, fences)
				}
			}
		})
	}
}

func TestWave42LegacyGitWorkerCancellationFinishesWithOriginalCleanupBound(test *testing.T) {
	config, storage, repo, reader := wave42LegacyGitFixture(test)
	job := wave42LegacyClaimGitChange(test, storage, wave42LegacyEnqueueGitChange(test, storage, repo, 0))
	ctx, cancel := context.WithCancel(systemTestContext())
	defer cancel()
	reader.read = func(context.Context, string, string, string) (domain.GitCommitDelta, error) {
		cancel()
		return domain.GitCommitDelta{}, context.Canceled
	}
	storage.finish = func(bound context.Context, next domain.GitChangeJob) error {
		deadline, bounded := bound.Deadline()
		if bound.Err() != nil || !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second {
			test.Fatal("cleanup inherited cancellation or lost five-second bound", bound.Err(), deadline)
		}
		return storage.FSStore.FinishGitChange(bound, next)
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.RunLease(ctx, job); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if len(storage.finishCtx) != 1 {
		test.Fatal("cleanup finish count", len(storage.finishCtx))
	}
	bound := storage.finishCtx[0]
	deadline, bounded := bound.Deadline()
	if !bounded || deadline.Sub(storage.finishes[0].UpdatedAt) < 4*time.Second || deadline.Sub(storage.finishes[0].UpdatedAt) > 6*time.Second || !errors.Is(bound.Err(), context.Canceled) {
		test.Fatal("cleanup lifetime changed", deadline, bound.Err())
	}
	if storage.finishes[0].State != "retrying" {
		test.Fatal("cancellation lost retry recovery")
	}
}

func TestWave42LegacyGitChangesFacadeKeepsMutableLegacySeams(test *testing.T) {
	config, storage, repo, reader := wave42LegacyGitFixture(test)
	core := &Service{meta: storage, blobs: storage, repositories: storage}
	facade, err := NewGitChanges(core, reader)
	if err != nil {
		test.Fatal(err)
	}
	queued := wave42LegacyEnqueueGitChange(test, storage, repo, 0)
	changed := repo
	changed.GitRemoteURL += "-changed"
	if _, err := storage.PutRepo(context.Background(), changed); err != nil {
		test.Fatal(err)
	}
	core.meta = reboundGitRepo{MetadataStore: storage}
	if err := facade.Process(context.Background(), 1); err != nil {
		test.Fatal("per-job failure escaped original Process semantics", err)
	}
	current, err := facade.Get(systemTestContext(), repo.ID, queued.ID)
	if err != nil || current.State != "attention" || storage.reads != 0 {
		test.Fatal("facade froze core.meta or lost captured queue store", current, err)
	}
	if err := facade.Retry(systemTestContext(), repo.ID, queued.ID); err != nil {
		test.Fatal(err)
	}
	core.meta = storage
	if _, err := storage.PutRepo(context.Background(), repo); err != nil {
		test.Fatal(err)
	}
	facade.reader = &wave42LegacyGitReader{read: func(context.Context, string, string, string) (domain.GitCommitDelta, error) {
		return domain.GitCommitDelta{}, domain.ErrValidation
	}}
	if err := facade.Process(context.Background(), 1); err != nil {
		test.Fatal(err)
	}
	current, err = facade.Get(systemTestContext(), repo.ID, queued.ID)
	if err != nil || current.Reason != "invalid_or_ambiguous_git_evidence" {
		test.Fatal("facade froze mutable reader", current, err)
	}
	facade.reader = config.Reader
	facade.SetSourceAuthorizer(&wave42LegacyGitAuthorizer{authorize: func(context.Context, domain.ContentHash) (outbound.GitReadFence, error) {
		return nil, domain.ErrForbidden
	}})
	if err := facade.Retry(systemTestContext(), repo.ID, queued.ID); err != nil {
		test.Fatal(err)
	}
	if err := facade.Process(context.Background(), 1); err != nil {
		test.Fatal(err)
	}
	current, err = facade.Get(systemTestContext(), repo.ID, queued.ID)
	if err != nil || current.State != "retrying" || storage.reads != 0 {
		test.Fatal("facade froze source authorizer", current, err)
	}
	if err := facade.Retry(systemTestContext(), repo.ID, queued.ID); err != nil {
		test.Fatal(err)
	}
	core.meta = struct{ outbound.MetadataStore }{storage}
	facade.SetSourceAuthorizer(nil)
	priorCacheReads, priorProviderReads := storage.cacheReads, storage.reads
	if err := facade.Process(context.Background(), 1); err != nil {
		test.Fatal("current metadata replaced the captured original job store", err)
	}
	current, err = storage.GetGitChange(context.Background(), repo.ID, queued.ID)
	if err != nil || current.State != "completed" || current.Result == nil || storage.cacheReads != priorCacheReads || storage.reads-priorProviderReads != 3 {
		test.Fatal("legacy worker froze original metadata/cache capabilities", current, err, storage.cacheReads, storage.reads)
	}
	config.Metadata = core.meta
	if worker, err := NewGitChangeWorker(config); worker != nil || err == nil {
		test.Fatal("public worker assembly fabricated jobs from a metadata-only declaration", err)
	}
	empty := &GitChanges{core: &Service{meta: storage}, gitChangeQuery: &gitChangeQuery{store: storage}}
	if err := empty.Process(context.Background(), 1); err != nil {
		test.Fatal("compatibility required reader before empty queue claim", err)
	}
}

type wave42LegacyDeltaCache struct {
	outbound.GitScanStore
	read func(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error)
}

func (cache *wave42LegacyDeltaCache) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	return cache.read(ctx, repo, origin, commit, parent)
}

func TestWave42LegacyRepositoryEvidenceAdapterBindsOnlyFallbackProvider(test *testing.T) {
	repo, callerRepo := hh(test.Name()), hh("caller-repository")
	ctx := outbound.WithGitRepository(context.Background(), callerRepo)
	providerCalls, cacheCalls := 0, 0
	provider := &wave42LegacyGitReader{
		read: func(bound context.Context, _, commit, _ string) (domain.GitCommitDelta, error) {
			providerCalls++
			if outbound.GitRepository(bound) != repo {
				test.Fatal("fallback delta used caller repository")
			}
			return domain.GitCommitDelta{Commit: commit}, nil
		},
		ancestor: func(bound context.Context, _, _, _ string) (bool, error) {
			providerCalls++
			if outbound.GitRepository(bound) != repo {
				test.Fatal("fallback ancestry used caller repository")
			}
			return true, nil
		},
	}
	cache := &wave42LegacyDeltaCache{read: func(bound context.Context, id domain.ContentHash, _, _, _ string) (domain.GitCommitDelta, error) {
		cacheCalls++
		if bound != ctx || id != repo || outbound.GitRepository(bound) != callerRepo {
			test.Fatal("pure cache policy introduced context repository binding")
		}
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}}
	reader, err := evidencequery.NewCachedReversalReader(evidencequery.CachedReversalConfig{RepoID: repo, Cache: cache, Provider: gitchangeworker.RepositoryEvidence(repo, provider)})
	if err != nil {
		test.Fatal(err)
	}
	for _, reversal := range []evidencequery.ReversalReader{reader, cachedGitEvidence{cache, repo, provider}} {
		if _, err := reversal.ReadCommitDelta(ctx, "origin", strings.Repeat("a", 40), ""); err != nil {
			test.Fatal(err)
		}
		if found, err := reversal.IsGitAncestor(ctx, "origin", strings.Repeat("b", 40), strings.Repeat("c", 40)); err != nil || !found {
			test.Fatal(found, err)
		}
	}
	if providerCalls != 4 || cacheCalls != 4 || outbound.GitRepository(ctx) != callerRepo {
		test.Fatal("cache fallback bypassed context adapter", providerCalls, cacheCalls)
	}
}

type wave42LegacyPollingScope struct{}

func (wave42LegacyPollingScope) WorkerContext(ctx context.Context) context.Context { return ctx }
func (wave42LegacyPollingScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	return work(ctx)
}
func (wave42LegacyPollingScope) ReadRepository(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, domain.ErrNotFound
}
func (wave42LegacyPollingScope) WithinEvidenceWrite(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	return write(ctx)
}

type wave42LegacyPollingJobs struct {
	outbound.GitChangeStore
	claims atomic.Int64
}

type wave42LegacyPollingMetadata struct{}

func (wave42LegacyPollingMetadata) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return domain.Repo{}, domain.ErrNotFound
}

func (jobs *wave42LegacyPollingJobs) ClaimGitChange(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.GitChangeJob, error) {
	jobs.claims.Add(1)
	return domain.GitChangeJob{}, domain.ErrNotFound
}

func TestWave42LegacyGitWorkerRunPollsImmediatelyAndJoinsCancellation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		jobs := &wave42LegacyPollingJobs{}
		worker, err := gitchangeworker.New(gitchangeworker.Config{Profile: gitchangeworker.Compatibility, Scope: wave42LegacyPollingScope{}, Jobs: jobs, Metadata: wave42LegacyPollingMetadata{}, Reader: &wave42LegacyGitReader{}})
		if err != nil {
			test.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			worker.Run(ctx)
		}()
		synctest.Wait()
		if jobs.claims.Load() != 1 {
			test.Fatal("Run did not poll immediately", jobs.claims.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if jobs.claims.Load() != 2 {
			test.Fatal("original one-second polling changed", jobs.claims.Load())
		}
		cancel()
		<-joined
		time.Sleep(2 * time.Second)
		if jobs.claims.Load() != 2 {
			test.Fatal("worker claimed after cancellation/join", jobs.claims.Load())
		}
	})
}

type wave42LegacyWorkMetadata struct {
	outbound.MetadataStore
	repo        domain.Repo
	getError    error
	revision    error
	evidence    int
	pending     []bool
	readCtx     []context.Context
	revisionCtx context.Context
}

func (metadata *wave42LegacyWorkMetadata) GetRepo(ctx context.Context, _ domain.ContentHash) (domain.Repo, error) {
	metadata.readCtx = append(metadata.readCtx, ctx)
	return metadata.repo, metadata.getError
}

func (metadata *wave42LegacyWorkMetadata) AdvanceEvidenceRevision(ctx context.Context, _ domain.ContentHash) error {
	metadata.evidence++
	metadata.revisionCtx = ctx
	return metadata.revision
}

func (metadata *wave42LegacyWorkMetadata) RepositoryRevision(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
	return domain.RepositoryRevision{}, nil
}

func (metadata *wave42LegacyWorkMetadata) AdvanceRepositoryRevision(ctx context.Context, _ domain.ContentHash, pending bool) error {
	metadata.pending = append(metadata.pending, pending)
	metadata.revisionCtx = ctx
	return metadata.revision
}

type wave42LegacyWorkTransactions struct {
	*wave42LegacyWorkMetadata
	writes   int
	reads    int
	outcomes []error
}

func (metadata *wave42LegacyWorkTransactions) WithinRepository(ctx context.Context, _ domain.ContentHash, write func(context.Context) error) error {
	metadata.writes++
	if err := write(context.WithValue(ctx, wave42LegacyGitTransactionKey{}, "write")); err != nil {
		return err
	}
	if metadata.writes <= len(metadata.outcomes) {
		return metadata.outcomes[metadata.writes-1]
	}
	return nil
}

func (metadata *wave42LegacyWorkTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	metadata.reads++
	return read(context.WithValue(ctx, wave42LegacyGitTransactionKey{}, "read"))
}

type wave42LegacyAbort string

func (failure wave42LegacyAbort) Error() string    { return string(failure) }
func (failure wave42LegacyAbort) SQLState() string { return string(failure) }

func TestWave42LegacyRepositoryWorkSharesOriginalKeysAndNestedServiceTransaction(test *testing.T) {
	repo := hh(test.Name())
	metadata := &wave42LegacyWorkTransactions{wave42LegacyWorkMetadata: &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo}}}
	work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	service := &Service{meta: metadata}
	committed := 0
	ctx := evidenceWriteContext(systemTestContext())
	value, err := repositoryWrite(ctx, service, repo, func(bound context.Context) (string, error) {
		if _, nested := bound.Value(afterCommitKey{}).(*afterCommitActions); !nested {
			test.Fatal("original after-commit key was replaced")
		}
		return repositoryWriteWithWork(bound, work, repo, func(nested context.Context) (string, error) {
			afterRepositoryCommit(nested, func() { committed++ })
			if nested.Value(revisionScopeKey{}) != "evidence" || nested.Value(wave42LegacyGitTransactionKey{}) != "write" || committed != 0 {
				test.Fatal("nested work escaped original transaction or fired before commit")
			}
			return "result", nil
		})
	})
	if err != nil || value != "result" || metadata.writes != 2 || metadata.evidence != 1 || committed != 1 || len(metadata.pending) != 0 {
		test.Fatal("shared work duplicated revision or after-commit actions", value, err, metadata.writes, metadata.evidence, committed)
	}
	for _, scope := range []string{"none", "pending", ""} {
		if err := work.revisionWrite(context.WithValue(ctx, revisionScopeKey{}, scope), repo); err != nil {
			test.Fatal(err)
		}
	}
	if metadata.evidence != 1 || !reflect.DeepEqual(metadata.pending, []bool{true, false}) {
		test.Fatal("original revision keys changed", metadata.evidence, metadata.pending)
	}
	value, err = repositoryReadWithWork(ctx, work, func(bound context.Context) (string, error) {
		if bound.Value(wave42LegacyGitTransactionKey{}) != "read" {
			test.Fatal("read callback not snapshot-bound")
		}
		return "read-result", domain.ErrIntegrity
	})
	if value != "" || !errors.Is(err, domain.ErrIntegrity) || metadata.reads != 1 {
		test.Fatal("failed transactional read leaked result", value, err, metadata.reads)
	}
}

func TestWave42LegacyRepositoryWorkAbortRetriesAndUnknownAcknowledgement(test *testing.T) {
	unknown := errors.New("commit outcome unknown")
	for _, scenario := range []struct {
		name     string
		outcomes []error
		writes   int
		failure  error
	}{
		{"serialization", []error{wave42LegacyAbort("40001"), wave42LegacyAbort("40001")}, 3, nil},
		{"deadlock", []error{wave42LegacyAbort("40P01")}, 2, nil},
		{"retry-limit", []error{wave42LegacyAbort("40001"), wave42LegacyAbort("40001"), wave42LegacyAbort("40001")}, 3, wave42LegacyAbort("40001")},
		{"unknown-ack", []error{unknown}, 1, unknown},
		{"other-state", []error{wave42LegacyAbort("23505")}, 1, wave42LegacyAbort("23505")},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			repo := hh(test.Name())
			metadata := &wave42LegacyWorkTransactions{wave42LegacyWorkMetadata: &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo}}, outcomes: scenario.outcomes}
			work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
			committed, callbacks := 0, 0
			value, err := repositoryWriteWithWork(evidenceWriteContext(systemTestContext()), work, repo, func(bound context.Context) (string, error) {
				callbacks++
				afterRepositoryCommit(bound, func() { committed++ })
				return "value", nil
			})
			if !errors.Is(err, scenario.failure) || metadata.writes != scenario.writes || callbacks != scenario.writes {
				test.Fatal("transaction replay classification changed", err, metadata.writes, callbacks)
			}
			if err == nil && (value != "value" || committed != 1) || err != nil && (value != "" || committed != 0) {
				test.Fatal("failed attempt leaked result or after-commit actions", value, committed, err)
			}
		})
	}
}

func TestWave42LegacyRepositoryWorkPreservesFSFailureAndIdentityOrdering(test *testing.T) {
	repo := hh(test.Name())
	metadata := &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo}, revision: domain.ErrIntegrity}
	work := newRepositoryWork(metadata, nil, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	called := 0
	value, err := repositoryWriteWithWork(evidenceWriteContext(systemTestContext()), work, repo, func(context.Context) (string, error) {
		called++
		return "fs-result", nil
	})
	if value != "fs-result" || !errors.Is(err, domain.ErrIntegrity) || called != 1 {
		test.Fatal("FS callback/revision error result order changed", value, err, called)
	}
	metadata.repo.RequiredDocIdentity = domain.DocumentIdentityRootV1
	metadata.revision = nil
	if err := repositoryWriteErrorWithWork(systemTestContext(), work, repo, func(context.Context) error {
		called++
		return nil
	}); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || called != 1 {
		test.Fatal("unsupported document identity reached write", err, called)
	}
	metadata.getError = domain.ErrNotFound
	if err := work.checkDocumentIdentity(systemTestContext(), repo, true); err != nil {
		test.Fatal("legacy missing repository no longer allowed", err)
	}
	if err := work.checkDocumentIdentity(systemTestContext(), repo, false); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("required repository lookup failure hidden", err)
	}
	if err := work.authorizeRepositoryWrite(context.Background(), repo); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal("anonymous write error ordering changed", err)
	}
	if err := work.authorizeRepositoryWrite(writeAction(inbound.WithRepositoryActor(context.Background(), "actor"), "register"), repo); err != nil {
		test.Fatal("registration incorrectly required an existing binding", err)
	}
}

type wave42LegacyWorkRepositories struct {
	outbound.RepositoryStore
	outbound.OrganizationStore
	record domain.Repository
	role   domain.MemberRole
	locks  int
	audit  []domain.OrganizationAuditEvent
	bound  context.Context
}

func (repositories *wave42LegacyWorkRepositories) GetRepository(context.Context, string) (domain.Repository, error) {
	return repositories.record, nil
}

func (repositories *wave42LegacyWorkRepositories) ListMembers(context.Context, string) ([]domain.Membership, error) {
	return []domain.Membership{{RepositoryID: repositories.record.ID, UserID: "actor", Role: repositories.role}}, nil
}

func (repositories *wave42LegacyWorkRepositories) LockRepositoryAccess(ctx context.Context, _, _ string) error {
	repositories.locks++
	repositories.bound = ctx
	return nil
}

func (repositories *wave42LegacyWorkRepositories) GetNamespace(context.Context, string) (domain.Namespace, error) {
	return domain.Namespace{Kind: domain.NamespaceOrganization, OrganizationID: "organization"}, nil
}

func (repositories *wave42LegacyWorkRepositories) AppendOrganizationAudit(ctx context.Context, event domain.OrganizationAuditEvent) error {
	if repositories.bound == nil || ctx.Value(wave42LegacyGitTransactionKey{}) != "write" || ctx.Value(wave42LegacyGitTransactionKey{}) != repositories.bound.Value(wave42LegacyGitTransactionKey{}) {
		return domain.ErrIntegrity
	}
	repositories.audit = append(repositories.audit, event)
	return nil
}

func TestWave42LegacyRepositoryWorkRechecksAuthorityAndUsesOriginalAuditAction(test *testing.T) {
	repo := hh(test.Name())
	metadata := &wave42LegacyWorkTransactions{wave42LegacyWorkMetadata: &wave42LegacyWorkMetadata{repo: domain.Repo{ID: repo, RepositoryID: "repository"}}}
	repositories := &wave42LegacyWorkRepositories{record: domain.Repository{ID: "repository", OwnerID: "owner", OwnerNamespaceID: "namespace"}, role: domain.RoleMember}
	work := newRepositoryWork(metadata, repositories, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	ctx := auditOperation(inbound.WithRepositoryActor(context.Background(), "actor"), "evidence.updated")
	called := 0
	write := func(context.Context) error { called++; return nil }
	if err := repositoryWriteErrorWithWork(ctx, work, repo, write); err != nil || called != 1 || repositories.locks != 1 || len(repositories.audit) != 1 {
		test.Fatal("shared work lost current authority lock or audit", err, called, repositories.locks, repositories.audit)
	}
	if event := repositories.audit[0]; event.ActorID != "actor" || event.Action != "evidence.updated" || event.TargetID != "repository" || event.TargetType != "repository" {
		test.Fatal("original audit context key changed", event)
	}
	repositories.role = domain.RolePuller
	if err := repositoryWriteErrorWithWork(ctx, work, repo, write); !errors.Is(err, domain.ErrForbidden) || called != 1 || len(repositories.audit) != 1 || repositories.locks != 2 {
		test.Fatal("revoked authority reused cached authorization", err, called, repositories.locks)
	}
	repositories.role = domain.RoleMember
	if err := repositoryWriteErrorWithWork(writeAction(ctx, "manage"), work, repo, write); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("management action ignored original action key", err)
	}
	repositories.role = domain.RoleMaintainer
	repositories.record.SettingsPolicy = "owner"
	if err := repositoryWriteErrorWithWork(writeAction(ctx, "settings"), work, repo, write); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("settings owner policy bypassed", err)
	}
	repositories.record.Archived = true
	if err := repositoryWriteErrorWithWork(ctx, work, repo, write); !errors.Is(err, domain.ErrForbidden) || called != 1 {
		test.Fatal("archived repository reached write", err, called)
	}
}
