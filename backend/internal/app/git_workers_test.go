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
	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave42GitReader struct {
	fakeGitEvidence
	before   func(context.Context)
	reads    atomic.Int64
	ancestry atomic.Int64
}

func (reader *wave42GitReader) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	reader.reads.Add(1)
	if reader.before != nil {
		reader.before(ctx)
	}
	if err := ctx.Err(); err != nil {
		return domain.GitCommitDelta{}, err
	}
	return reader.fakeGitEvidence.ReadCommitDelta(ctx, origin, commit, parent)
}

func (reader *wave42GitReader) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	reader.ancestry.Add(1)
	if reader.before != nil {
		reader.before(ctx)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return reader.fakeGitEvidence.IsGitAncestor(ctx, origin, ancestor, descendant)
}

type wave42GitAuthorizer struct {
	authorize func(context.Context, domain.ContentHash) (outbound.GitReadFence, error)
}

func (authorizer *wave42GitAuthorizer) AuthorizeGitRead(ctx context.Context, repo domain.ContentHash) (outbound.GitReadFence, error) {
	return authorizer.authorize(ctx, repo)
}

type wave42GitScope struct{ gitchangeworker.Scope }

type wave42GitStore struct {
	*wave42WorkStore
	claims     atomic.Int64
	claim      func(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.GitChangeJob, error)
	finish     func(context.Context, domain.GitChangeJob) error
	delta      func(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error)
	claimCtx   context.Context
	claimAt    time.Time
	claimLease time.Duration
	finished   []domain.GitChangeJob
}

func (storage *wave42GitStore) ClaimGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
	storage.claims.Add(1)
	storage.claimCtx, storage.claimAt, storage.claimLease = ctx, now, lease
	if storage.claim != nil {
		return storage.claim(ctx, repo, id, now, lease)
	}
	return storage.FSStore.ClaimGitChange(ctx, repo, id, now, lease)
}

func (storage *wave42GitStore) FinishGitChange(ctx context.Context, job domain.GitChangeJob) error {
	storage.finished = append(storage.finished, job)
	if storage.finish != nil {
		return storage.finish(ctx, job)
	}
	return storage.FSStore.FinishGitChange(ctx, job)
}

func (storage *wave42GitStore) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	if storage.delta != nil {
		return storage.delta(ctx, repo, origin, commit, parent)
	}
	return storage.FSStore.GetGitDelta(ctx, repo, origin, commit, parent)
}

type wave42GitDeltaOnly struct {
	outbound.MetadataStore
	outbound.GitChangeStore
	outbound.RepositoryTransactions
	reads int
}

func (storage *wave42GitDeltaOnly) GetGitDelta(context.Context, domain.ContentHash, string, string, string) (domain.GitCommitDelta, error) {
	storage.reads++
	return domain.GitCommitDelta{}, domain.ErrIntegrity
}

func wave42GitFixture(test *testing.T) (GitChangeWorkerConfig, *wave42GitStore, *wave42GitReader, domain.GitChangeJob) {
	test.Helper()
	storage := &wave42GitStore{wave42WorkStore: &wave42WorkStore{FSStore: store.NewFSStore(test.TempDir())}}
	repo := hh(test.Name())
	origin := "https://github.com/example/wave42"
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: repo, GitRemoteURL: origin}); err != nil {
		test.Fatal(err)
	}
	target, candidate, parent := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	bridge := strings.Repeat("3", 40)
	change := domain.GitPathChange{Path: "feature", Before: domain.GitEntry{OID: strings.Repeat("1", 40), Mode: "100644"}, After: domain.GitEntry{OID: strings.Repeat("2", 40), Mode: "100644"}}
	reader := &wave42GitReader{fakeGitEvidence: fakeGitEvidence{t: test, origin: origin, ancestor: true, deltas: map[string]domain.GitCommitDelta{
		target:    {Commit: target, Parents: []string{parent}, Parent: parent, Complete: true, Changes: []domain.GitPathChange{change}},
		candidate: {Commit: candidate, Parents: []string{bridge}, Parent: bridge, Complete: true, Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}},
	}}}
	now := time.Now().UTC()
	job := domain.GitChangeJob{RepoID: repo, GitOrigin: origin, Request: domain.GitChangeRequest{Target: target, Commit: candidate}, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
	job.ID = domain.GitChangeID(repo, origin, job.Request)
	if _, err := storage.FSStore.EnqueueGitChange(systemTestContext(), job); err != nil {
		test.Fatal(err)
	}
	reader.before = func(bound context.Context) {
		if bound.Value(afterCommitKey{}) != nil || bound.Value(wave42TransactionKey{}) != nil || outbound.GitRepository(bound) != repo {
			test.Error("provider work ran inside a transaction or without repository binding")
		}
	}
	return GitChangeWorkerConfig{Profile: gitchangeworker.Complete, Metadata: storage, Blobs: storage.FSStore, Repositories: storage, Reader: reader}, storage, reader, job
}

func wave42ClaimGit(test *testing.T, storage *wave42GitStore, job domain.GitChangeJob) domain.GitChangeJob {
	test.Helper()
	claimed, err := storage.FSStore.ClaimGitChange(systemTestContext(), job.RepoID, job.ID, time.Now().UTC(), 2*time.Minute)
	if err != nil {
		test.Fatal(err)
	}
	return claimed
}

func TestWave42GitWorkerFactoriesValidateActualTypedNilWithoutExecuting(test *testing.T) {
	config, storage, reader, _ := wave42GitFixture(test)
	for _, profile := range []gitchangeworker.Profile{gitchangeworker.Compatibility, gitchangeworker.Complete} {
		config.Profile = profile
		worker, err := NewGitChangeWorker(config)
		if worker == nil || err != nil {
			test.Fatal("complete dependency set rejected", profile, err)
		}
		for _, field := range []string{"Metadata", "Reader"} {
			for _, typed := range []bool{false, true} {
				invalid := config
				value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
				value.Set(reflect.Zero(value.Type()))
				if typed {
					missing := any((*wave42GitStore)(nil))
					if field == "Reader" {
						missing = (*wave42GitReader)(nil)
					}
					value.Set(reflect.ValueOf(missing))
					if value.IsNil() || !value.Elem().IsNil() {
						test.Fatal("fixture did not contain a real nil pointer interface", field)
					}
				}
				if result, err := NewGitChangeWorker(invalid); result != nil || err == nil {
					test.Fatal("missing or typed-nil mandatory dependency accepted", profile, field, typed, err)
				}
			}
		}
	}
	config.Profile = gitchangeworker.Complete
	for _, field := range []string{"Blobs", "Repositories"} {
		for _, typed := range []bool{false, true} {
			invalid := config
			value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
			value.Set(reflect.Zero(value.Type()))
			if typed {
				value.Set(reflect.ValueOf((*store.FSStore)(nil)))
				if value.IsNil() || !value.Elem().IsNil() {
					test.Fatal("fixture is not a typed-nil pointer", field)
				}
			}
			if result, err := NewGitChangeWorker(invalid); result != nil || err == nil {
				test.Fatal("Complete accepted absent mandatory adapter", field, typed, err)
			}
		}
	}
	for _, missing := range []string{"transactions", "jobs"} {
		invalid := config
		if missing == "transactions" {
			invalid.Metadata = storage.FSStore
		} else {
			invalid.Metadata = struct {
				outbound.MetadataStore
				outbound.RepositoryTransactions
			}{MetadataStore: storage.FSStore, RepositoryTransactions: storage}
		}
		if worker, err := NewGitChangeWorker(invalid); worker != nil || err == nil {
			test.Fatal("Complete accepted a missing capability", missing, err)
		}
	}
	for _, profile := range []gitchangeworker.Profile{"", "future-profile"} {
		invalid := config
		invalid.Profile = profile
		if worker, err := NewGitChangeWorker(invalid); worker != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("unknown profile accepted", profile, err)
		}
	}
	if len(storage.calls) != 0 || storage.claims.Load() != 0 || reader.reads.Load() != 0 || reader.ancestry.Load() != 0 || len(storage.finished) != 0 {
		test.Fatal("constructor executed work", storage.calls, storage.claims.Load(), reader.reads.Load())
	}
	native := gitchangeworker.Config{Profile: gitchangeworker.Complete, Scope: &wave42GitScope{}, Jobs: storage, Metadata: storage, Reader: reader}
	if worker, err := gitchangeworker.New(native); worker == nil || err != nil {
		test.Fatal("native constructor rejected complete ports", err)
	}
	for _, field := range []string{"Scope", "Jobs", "Metadata", "Reader"} {
		for _, typed := range []bool{false, true} {
			invalid := native
			value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
			value.Set(reflect.Zero(value.Type()))
			if typed {
				missing := any((*wave42GitStore)(nil))
				if field == "Reader" {
					missing = (*wave42GitReader)(nil)
				} else if field == "Scope" {
					missing = (*wave42GitScope)(nil)
				}
				value.Set(reflect.ValueOf(missing))
				if value.IsNil() || !value.Elem().IsNil() {
					test.Fatal("native fixture is not an actual nil pointer interface", field)
				}
			}
			if worker, err := gitchangeworker.New(invalid); worker != nil || err == nil {
				test.Fatal("native constructor accepted missing port", field, typed, err)
			}
		}
	}
	if len(storage.calls) != 0 || storage.claims.Load() != 0 || reader.reads.Load() != 0 {
		test.Fatal("native validation executed work")
	}
}

func TestWave42GitWorkerCompatibilityFSNilBlobsDeclaresOnlyAvailableIdentity(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	config.Profile, config.Metadata, config.Blobs, config.Repositories = gitchangeworker.Compatibility, storage.FSStore, nil, nil
	providerCalls := 0
	reader.before = func(bound context.Context) {
		providerCalls++
		if _, system := inbound.RepositoryActor(bound); !system {
			test.Fatal("Process did not use its existing trusted system entry")
		}
		if !reflect.DeepEqual(inbound.DocumentIdentities(bound), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) || !errors.Is(outbound.CheckDocumentIdentityCompatibility(bound, domain.DocumentIdentityRootV1), domain.ErrDocumentIdentityUpgradeRequired) {
			test.Fatal("nil Blobs advertised unavailable root readers", inbound.DocumentIdentities(bound))
		}
		if bound.Value(afterCommitKey{}) != nil || outbound.GitRepository(bound) != job.RepoID {
			test.Fatal("FS provider work lost original context boundary")
		}
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil || worker == nil {
		test.Fatal("Compatibility rejected FS metadata-only startup", err)
	}
	ctx := inbound.WithDocumentIdentities(inbound.WithRepositoryActor(context.Background(), "untrusted-caller"), []domain.DocumentIdentity{"future-spoof"})
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{"future-spoof"}, []domain.DocumentIdentity{"future-spoof"})
	if err := worker.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || current.State != "completed" || current.Result == nil || current.Result.Coverage != "full" || providerCalls != 3 || current.ID != job.ID || current.Attempts != 1 {
		test.Fatal("FS worker did not publish original immutable proof", current, providerCalls, err)
	}
}

func TestWave42GitWorkerFactoryProcessUsesSharedAdmissionAndPublicationWork(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	ctx, gate := admissionTestContext(time.Second)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	var claimPermit any
	storage.claim = func(bound context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
		claimPermit = bound.Value(resourcePermitKey{})
		if claimPermit == nil || bound.Value(wave42TransactionKey{}) != nil || repo != "" || id != "" || lease != 2*time.Minute {
			test.Error("claim did not use original admitted selection", repo, id, lease)
			return domain.GitChangeJob{}, domain.ErrIntegrity
		}
		return storage.FSStore.ClaimGitChange(bound, repo, id, now, lease)
	}
	var boundaryError error
	originalBefore := reader.before
	reader.before = func(bound context.Context) {
		originalBefore(bound)
		if bound.Value(resourcePermitKey{}) != claimPermit {
			boundaryError = errors.New("provider replaced claim admission")
		}
		deadline, ok := bound.Deadline()
		if !ok || deadline.Sub(storage.claimAt) < 89*time.Second || deadline.Sub(storage.claimAt) > 91*time.Second {
			boundaryError = errors.New("provider work changed its 90-second deadline")
		}
	}
	config.Authorizer = &wave42GitAuthorizer{authorize: func(bound context.Context, repo domain.ContentHash) (outbound.GitReadFence, error) {
		if bound.Value(wave42TransactionKey{}) != nil || repo != job.RepoID {
			return nil, domain.ErrIntegrity
		}
		return func(write context.Context) error {
			if write.Value(wave42TransactionKey{}) != "write" || write.Value(afterCommitKey{}) == nil || write.Value(resourcePermitKey{}) != claimPermit || write.Value(revisionScopeKey{}) != "evidence" {
				return domain.ErrIntegrity
			}
			return nil
		}, nil
	}}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Process(ctx, 1) }()
	waitResourceQueue(test, gate, ResourceBulk, 1)
	if storage.claims.Load() != 0 || reader.reads.Load() != 0 {
		test.Error("worker claimed or read while waiting on the shared gate")
	}
	release()
	if err := <-done; err != nil {
		test.Fatal(err)
	}
	if boundaryError != nil {
		test.Fatal(boundaryError)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || current.State != "completed" || current.Result == nil || current.Result.Coverage != "full" || current.ID != job.ID || current.Version != 1 || current.Attempts != 1 || !current.LeaseUntil.IsZero() {
		test.Fatal("public factory did not execute real fenced proof publication", current, err)
	}
	if storage.claims.Load() != 1 || reader.reads.Load() != 2 || reader.ancestry.Load() != 1 || len(storage.finished) != 1 || wave42Count(storage.calls, "evidence") != 1 || wave42Count(storage.calls, "graph") != 0 || wave42Count(storage.calls, "pending") != 0 {
		test.Fatal("claim/proof/evidence publication changed", storage.calls, storage.finished)
	}
	history, historyErr := storage.FSStore.ListHistoryEvents(systemTestContext(), job.RepoID)
	refs, refsErr := storage.FSStore.ListRefs(systemTestContext(), job.RepoID)
	if historyErr != nil || refsErr != nil || len(history) != 0 || len(refs) != 0 {
		test.Fatal("Git evidence rewrote raw history or branch refs", history, refs, historyErr, refsErr)
	}
	assertResourcesReleased(test, gate)
}

func TestWave42GitWorkerRunLeaseRetainsCallerActor(test *testing.T) {
	config, storage, _, job := wave42GitFixture(test)
	fixture := makeTeamFixture(test, storage.FSStore)
	if _, err := storage.FSStore.PutRepo(systemTestContext(), domain.Repo{ID: job.RepoID, RepositoryID: fixture.repository.ID}); err != nil {
		test.Fatal(err)
	}
	config.Profile, config.Blobs = gitchangeworker.Compatibility, nil
	checked := false
	config.Authorizer = &wave42GitAuthorizer{authorize: func(bound context.Context, _ domain.ContentHash) (outbound.GitReadFence, error) {
		actor, system := inbound.RepositoryActor(bound)
		if actor != fixture.owner.ID || system {
			test.Fatal("RunLease promoted caller to system", actor, system)
		}
		checked = true
		return func(context.Context) error { return nil }, nil
	}}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithRepositoryActor(context.Background(), fixture.owner.ID)
	if err := worker.RunLease(ctx, wave42ClaimGit(test, storage, job)); err != nil || !checked {
		test.Fatal("direct lease could not use its original authorized actor", checked, err)
	}
}

func TestWave42GitWorkerOriginalFullScanCacheSelectionAndCurrentAuthority(test *testing.T) {
	for _, scenario := range []string{"incidental-delta-reader", "full-cache", "cached-source-revoked"} {
		test.Run(scenario, func(test *testing.T) {
			config, storage, reader, job := wave42GitFixture(test)
			incidental := &wave42GitDeltaOnly{MetadataStore: storage.FSStore, GitChangeStore: storage, RepositoryTransactions: storage}
			cachedReads, authorized, fences := 0, 0, 0
			if scenario == "incidental-delta-reader" {
				config.Metadata = incidental
			} else {
				storage.delta = func(bound context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
					cachedReads++
					if repo != job.RepoID || origin != job.GitOrigin || bound.Value(wave42TransactionKey{}) != nil {
						return domain.GitCommitDelta{}, domain.ErrIntegrity
					}
					if delta, ok := reader.deltas[commit]; ok {
						return delta, nil
					}
					if commit == reader.deltas[job.Request.Commit].Parent {
						return domain.GitCommitDelta{Commit: commit, Parents: []string{job.Request.Target}, Parent: job.Request.Target, Complete: true}, nil
					}
					return domain.GitCommitDelta{}, domain.ErrNotFound
				}
			}
			config.Authorizer = &wave42GitAuthorizer{authorize: func(bound context.Context, repo domain.ContentHash) (outbound.GitReadFence, error) {
				authorized++
				if repo != job.RepoID || bound.Value(afterCommitKey{}) != nil {
					return nil, domain.ErrIntegrity
				}
				return func(bound context.Context) error {
					fences++
					if bound.Value(afterCommitKey{}) == nil {
						return domain.ErrIntegrity
					}
					if scenario == "cached-source-revoked" {
						return domain.ErrForbidden
					}
					return nil
				}, nil
			}}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			if err := worker.Process(context.Background(), 1); err != nil {
				test.Fatal(err)
			}
			current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
			if err != nil || authorized != 1 || fences != 1 || incidental.reads != 0 {
				test.Fatal("cache changed authorization or full-store capability selection", current, authorized, fences, incidental.reads, err)
			}
			if scenario == "incidental-delta-reader" {
				if current.State != "completed" || reader.reads.Load() != 2 || reader.ancestry.Load() != 1 {
					test.Fatal("incidental delta method enabled a new cache path", current, reader.reads.Load())
				}
			} else if reader.reads.Load() != 0 || reader.ancestry.Load() != 0 || cachedReads != 3 {
				test.Fatal("original full cache path was not reused", cachedReads, reader.reads.Load(), reader.ancestry.Load())
			}
			if scenario == "cached-source-revoked" {
				if current.State != "retrying" || current.Result != nil {
					test.Fatal("cache hit bypassed current source fence", current)
				}
			} else if current.State != "completed" || current.Result == nil || current.Result.Coverage != "full" {
				test.Fatal("cache selection changed verified result", current)
			}
		})
	}
}

func TestWave42GitWorkerNullFenceFailsBeforeProvider(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	config.Authorizer = &wave42GitAuthorizer{authorize: func(context.Context, domain.ContentHash) (outbound.GitReadFence, error) {
		return nil, nil
	}}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.RunLease(systemTestContext(), wave42ClaimGit(test, storage, job)); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("null publication fence accepted", err)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || current.State != "attention" || current.Reason != "integrity_check_failed" || current.Result != nil || reader.reads.Load() != 0 || reader.ancestry.Load() != 0 {
		test.Fatal("null fence reached provider or published proof", current, err)
	}
}

func TestWave42GitWorkerTypedNilAuthorizerCannotReadProvider(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	config.Authorizer = (*wave42GitAuthorizer)(nil)
	if config.Authorizer == nil || !reflect.ValueOf(config.Authorizer).IsNil() {
		test.Fatal("authorizer fixture is not an interface containing a nil pointer")
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		if worker != nil || storage.claims.Load() != 0 || len(storage.calls) != 0 || reader.reads.Load() != 0 {
			test.Fatal("rejected authorizer construction executed work", err)
		}
		return
	}
	if worker == nil {
		test.Fatal("successful constructor returned nil worker")
	}
	if err := worker.RunLease(systemTestContext(), wave42ClaimGit(test, storage, job)); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("typed-nil source policy did not fail closed", err)
	}
	if reader.reads.Load() != 0 || reader.ancestry.Load() != 0 {
		test.Fatal("typed-nil source policy reached provider")
	}
}

func TestWave42GitWorkerSourceDenialPrecedesProviderAndIsDeferred(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	config.Authorizer = &wave42GitAuthorizer{authorize: func(context.Context, domain.ContentHash) (outbound.GitReadFence, error) {
		return nil, domain.ErrForbidden
	}}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.RunLease(systemTestContext(), wave42ClaimGit(test, storage, job)); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("source denial lost original error", err)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || current.State != "retrying" || current.Reason != "temporary_provider_or_storage_failure" || current.Result != nil || reader.reads.Load() != 0 || reader.ancestry.Load() != 0 {
		test.Fatal("source denial read provider or changed retry classification", current, err)
	}
}

func TestWave42GitWorkerCompatibilityUnsupportedIdentityDoesNotSpendAttempt(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	config.Profile, config.Blobs = gitchangeworker.Compatibility, nil
	if err := storage.FSStore.RequireDocumentIdentity(systemTestContext(), job.RepoID, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	root := []domain.DocumentIdentity{domain.DocumentIdentityRootV1}
	ctx := outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(systemTestContext(), root), root, root)
	if err := worker.Process(ctx, 1); err != nil {
		test.Fatal(err)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || !reflect.DeepEqual(current, job) || reader.reads.Load() != 0 || reader.ancestry.Load() != 0 || len(storage.finished) != 0 {
		test.Fatal("spoofed support admitted an unsupported claim", current, err)
	}
}

func TestWave42GitWorkerOriginAndCurrentSourceFence(test *testing.T) {
	for _, scenario := range []string{"origin-before-read", "origin-after-provider", "source-revoked"} {
		test.Run(scenario, func(test *testing.T) {
			config, storage, reader, job := wave42GitFixture(test)
			claimed := wave42ClaimGit(test, storage, job)
			changed := scenario == "origin-before-read"
			storage.get = func(bound context.Context, repo domain.ContentHash) (domain.Repo, error) {
				metadata, err := storage.FSStore.GetRepo(bound, repo)
				if changed {
					metadata.GitRemoteURL = "https://github.com/example/rebound"
				}
				return metadata, err
			}
			originalBefore := reader.before
			reader.before = func(bound context.Context) {
				originalBefore(bound)
				if scenario == "origin-after-provider" {
					changed = true
				}
			}
			fences := 0
			config.Authorizer = &wave42GitAuthorizer{authorize: func(context.Context, domain.ContentHash) (outbound.GitReadFence, error) {
				return func(bound context.Context) error {
					fences++
					if bound.Value(wave42TransactionKey{}) != "write" {
						return domain.ErrIntegrity
					}
					if scenario == "source-revoked" {
						return domain.ErrForbidden
					}
					return nil
				}, nil
			}}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			wantError, wantState, wantReason := error(domain.ErrConflict), "attention", "origin_or_lease_changed"
			if scenario == "source-revoked" {
				wantError, wantState, wantReason = domain.ErrForbidden, "retrying", "temporary_provider_or_storage_failure"
			}
			if err := worker.RunLease(systemTestContext(), claimed); !errors.Is(err, wantError) {
				test.Fatal("current source change did not reject proof", err)
			}
			current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
			if err != nil || current.State != wantState || current.Reason != wantReason || current.Result != nil || current.ID != job.ID || current.Version != claimed.Version {
				test.Fatal("revoked source published or relabeled evidence", current, err)
			}
			if scenario == "origin-before-read" && reader.reads.Load() != 0 || scenario != "source-revoked" && fences != 0 || scenario == "source-revoked" && fences != 1 {
				test.Fatal("origin/fence ordering changed", reader.reads.Load(), fences)
			}
		})
	}
}

func TestWave42GitWorkerLateIdentityChangeBlocksSuccessAndFailurePublication(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	config.Profile, config.Blobs = gitchangeworker.Compatibility, nil
	claimed := wave42ClaimGit(test, storage, job)
	upgraded := false
	originalBefore := reader.before
	reader.before = func(bound context.Context) {
		originalBefore(bound)
		if !upgraded {
			upgraded = true
			if err := storage.FSStore.RequireDocumentIdentity(systemTestContext(), job.RepoID, domain.DocumentIdentityRootV1); err != nil {
				test.Fatal(err)
			}
		}
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.RunLease(systemTestContext(), claimed); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatal("late identity change did not reject publication", err)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || !reflect.DeepEqual(current, claimed) || len(storage.finished) != 0 || wave42Count(storage.calls, "evidence") != 0 {
		test.Fatal("current identity was bypassed by success or cleanup", current, storage.finished, err)
	}
}

func TestWave42GitWorkerCancellationUsesUncanceledBoundedCleanup(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	claimed := wave42ClaimGit(test, storage, job)
	ctx, cancel := context.WithCancel(systemTestContext())
	defer cancel()
	reader.before = func(context.Context) { cancel() }
	cleaned := false
	storage.finish = func(bound context.Context, next domain.GitChangeJob) error {
		deadline, ok := bound.Deadline()
		if bound.Err() != nil || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second || bound.Value(afterCommitKey{}) == nil {
			return domain.ErrIntegrity
		}
		cleaned = true
		return storage.FSStore.FinishGitChange(bound, next)
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.RunLease(ctx, claimed); !errors.Is(err, context.Canceled) || !cleaned {
		test.Fatal("cancellation lost cleanup or original error", cleaned, err)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || current.State != "retrying" || current.Result != nil || !current.LeaseUntil.IsZero() {
		test.Fatal("canceled provider published evidence or retained lease", current, err)
	}
}

func TestWave42GitWorkerFailureClassificationBackoffAndRetryLimit(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		failure  error
		attempts int
		state    string
		reason   string
	}{
		{name: "provider", failure: errors.New("offline"), attempts: 1, state: "retrying", reason: "temporary_provider_or_storage_failure"},
		{name: "integrity", failure: domain.ErrIntegrity, attempts: 1, state: "attention", reason: "integrity_check_failed"},
		{name: "validation", failure: domain.ErrValidation, attempts: 1, state: "attention", reason: "invalid_or_ambiguous_git_evidence"},
		{name: "conflict", failure: domain.ErrConflict, attempts: 1, state: "attention", reason: "origin_or_lease_changed"},
		{name: "not found", failure: domain.ErrNotFound, attempts: 1, state: "attention", reason: "repository_missing"},
		{name: "backoff cap", failure: errors.New("offline"), attempts: 9, state: "retrying", reason: "temporary_provider_or_storage_failure"},
		{name: "attempt cap", failure: errors.New("offline"), attempts: 20, state: "attention", reason: "retry_limit_reached"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			config, storage, reader, job := wave42GitFixture(test)
			claimed := wave42ClaimGit(test, storage, job)
			for claimed.Attempts < scenario.attempts {
				var err error
				claimed, err = storage.FSStore.ClaimGitChange(systemTestContext(), job.RepoID, job.ID, claimed.LeaseUntil.Add(time.Second), 2*time.Minute)
				if err != nil {
					test.Fatal(err)
				}
			}
			reader.err = scenario.failure
			storage.finish = func(bound context.Context, next domain.GitChangeJob) error {
				if next.State != scenario.state || next.Reason != scenario.reason || next.Result != nil || !next.LeaseUntil.IsZero() || next.ID != job.ID || next.Version != claimed.Version || next.Attempts != claimed.Attempts || next.NextAttempt.Sub(next.UpdatedAt) != time.Second<<min(claimed.Attempts, 8) {
					return domain.ErrIntegrity
				}
				return storage.FSStore.FinishGitChange(bound, next)
			}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			if err := worker.RunLease(systemTestContext(), claimed); !errors.Is(err, scenario.failure) || errors.Is(err, domain.ErrIntegrity) && !errors.Is(scenario.failure, domain.ErrIntegrity) || len(storage.finished) != 1 {
				test.Fatal("original classification or retry budget changed", storage.finished, err)
			}
			current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
			if err != nil || current.State != scenario.state || current.Reason != scenario.reason || current.Attempts != scenario.attempts || current.Result != nil {
				test.Fatal("failure classification was not durably fenced", current, err)
			}
		})
	}
}

func TestWave42GitWorkerFinishFailureAndUnknownCommitPreserveEvidenceFence(test *testing.T) {
	for _, scenario := range []string{"finish-failed", "cleanup-failed", "commit-ack-lost", "stale-lease"} {
		test.Run(scenario, func(test *testing.T) {
			config, storage, _, job := wave42GitFixture(test)
			claimed := wave42ClaimGit(test, storage, job)
			failure, cleanupFailure := errors.New("finish failure"), errors.New("cleanup failure")
			var replacement domain.GitChangeJob
			if scenario == "stale-lease" {
				var err error
				replacement, err = storage.FSStore.ClaimGitChange(systemTestContext(), job.RepoID, job.ID, claimed.LeaseUntil.Add(time.Second), 2*time.Minute)
				if err != nil {
					test.Fatal(err)
				}
			}
			storage.finish = func(bound context.Context, next domain.GitChangeJob) error {
				if next.State == "completed" {
					if scenario == "stale-lease" {
						return storage.FSStore.FinishGitChange(bound, next)
					}
					if scenario == "commit-ack-lost" {
						if err := storage.FSStore.FinishGitChange(bound, next); err != nil {
							return err
						}
					}
					return failure
				}
				if scenario == "cleanup-failed" {
					return cleanupFailure
				}
				return storage.FSStore.FinishGitChange(bound, next)
			}
			worker, err := NewGitChangeWorker(config)
			if err != nil {
				test.Fatal(err)
			}
			err = worker.RunLease(systemTestContext(), claimed)
			if err == nil || scenario != "stale-lease" && !errors.Is(err, failure) || scenario == "cleanup-failed" && !errors.Is(err, cleanupFailure) || (scenario == "commit-ack-lost" || scenario == "stale-lease") && !errors.Is(err, domain.ErrConflict) {
				test.Fatal("finish or cleanup error lost", scenario, err)
			}
			if len(storage.finished) != 2 || storage.finished[0].State != "completed" || storage.finished[1].Result != nil || wave42Count(storage.calls, "write") != 2 {
				test.Fatal("unknown outcome retried or failure retained success proof", storage.finished, storage.calls)
			}
			current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
			if err != nil {
				test.Fatal(err)
			}
			switch scenario {
			case "commit-ack-lost":
				if current.State != "completed" || current.Result == nil || current.Result.Coverage != "full" {
					test.Fatal("lost acknowledgement overwrote terminal evidence", current)
				}
			case "stale-lease":
				if !reflect.DeepEqual(current, replacement) {
					test.Fatal("stale worker overwrote newer lease", current, replacement)
				}
			case "cleanup-failed":
				if !reflect.DeepEqual(current, claimed) {
					test.Fatal("failed finish fabricated durable state", current)
				}
			case "finish-failed":
				if current.State != "retrying" || current.Result != nil {
					test.Fatal("failed completion was not deferred", current)
				}
			}
		})
	}
}

func TestWave42GitWorkerProcessCancellationClaimErrorsAndLimit(test *testing.T) {
	config, storage, reader, _ := wave42GitFixture(test)
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.Process(context.Background(), 0); err != nil || storage.claims.Load() != 0 {
		test.Fatal("zero limit claimed work", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := worker.Process(ctx, 1); !errors.Is(err, context.Canceled) || storage.claims.Load() != 0 {
		test.Fatal("canceled Process claimed a job", err)
	}
	claimFailure := errors.New("claim failed")
	storage.claim = func(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.GitChangeJob, error) {
		return domain.GitChangeJob{}, claimFailure
	}
	if err := worker.Process(context.Background(), 3); !errors.Is(err, claimFailure) || storage.claims.Load() != 1 || reader.reads.Load() != 0 {
		test.Fatal("claim failure was swallowed or reached provider", err)
	}
	storage.claim = nil
	reader.err = errors.New("provider offline")
	if err := worker.Process(context.Background(), 1); err != nil || storage.claims.Load() != 2 || len(storage.finished) != 1 || storage.finished[0].State != "retrying" {
		test.Fatal("Process changed original deferred-run error behavior", storage.finished, err)
	}
}

func TestWave42GitWorkerProcessLimitFourAndDrainRetainOriginalJobIDs(test *testing.T) {
	config, storage, reader, first := wave42GitFixture(test)
	ids := map[string]bool{first.ID: true}
	for _, digit := range []string{"e", "f", "1", "2"} {
		job := first
		job.Request.Commit = strings.Repeat(digit, 40)
		job.ID = domain.GitChangeID(job.RepoID, job.GitOrigin, job.Request)
		ids[job.ID] = true
		delta := reader.deltas[first.Request.Commit]
		delta.Commit = job.Request.Commit
		reader.deltas[job.Request.Commit] = delta
		if _, err := storage.FSStore.EnqueueGitChange(systemTestContext(), job); err != nil {
			test.Fatal(err)
		}
	}
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	if err := worker.Process(context.Background(), 4); err != nil {
		test.Fatal(err)
	}
	jobs, err := storage.FSStore.ListGitChanges(systemTestContext(), first.RepoID, "", 100)
	if err != nil || len(jobs) != 5 || storage.claims.Load() != 4 || reader.reads.Load() != 8 || reader.ancestry.Load() != 4 {
		test.Fatal("Process did not honor the original four-job batch", jobs, storage.claims.Load(), err)
	}
	completed, waiting := 0, 0
	for _, job := range jobs {
		if !ids[job.ID] {
			test.Fatal("worker replaced the original durable job key", job)
		}
		switch job.State {
		case "completed":
			completed++
		case "waiting":
			waiting++
		default:
			test.Fatal("batch left an unexpected job state", job)
		}
	}
	if completed != 4 || waiting != 1 {
		test.Fatal("Process exceeded or truncated the requested batch", completed, waiting)
	}
	if err := worker.Process(context.Background(), 4); err != nil || storage.claims.Load() != 6 {
		test.Fatal("drain did not stop at the first empty claim", storage.claims.Load(), err)
	}
	for id := range ids {
		job, err := storage.FSStore.GetGitChange(systemTestContext(), first.RepoID, id)
		if err != nil || job.State != "completed" || job.Attempts != 1 || job.Version != 1 || job.Result == nil || job.Result.Coverage != "full" {
			test.Fatal("drain replayed work or lost an immutable proof", job, err)
		}
	}
}

func TestWave42GitWorkerWaitingCancellationDoesNotClaimOrRead(test *testing.T) {
	config, storage, reader, job := wave42GitFixture(test)
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	worker, err := NewGitChangeWorker(config)
	if err != nil {
		test.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- worker.Process(ctx, 1) }()
	waitResourceQueue(test, gate, ResourceBulk, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		test.Fatal("queued cancellation did not reach caller", err)
	}
	release()
	if storage.claims.Load() != 0 || reader.reads.Load() != 0 || len(storage.calls) != 0 || len(storage.finished) != 0 {
		test.Fatal("queued cancellation spent a lease or touched repository work", storage.calls, storage.finished)
	}
	current, err := storage.FSStore.GetGitChange(systemTestContext(), job.RepoID, job.ID)
	if err != nil || !reflect.DeepEqual(current, job) {
		test.Fatal("queued cancellation mutated pending job", current, err)
	}
	assertResourcesReleased(test, gate)
}

func TestWave42GitWorkerRunImmediatePollOneSecondAndJoinedCancellation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		config, storage, _, _ := wave42GitFixture(test)
		storage.claim = func(context.Context, domain.ContentHash, string, time.Time, time.Duration) (domain.GitChangeJob, error) {
			return domain.GitChangeJob{}, domain.ErrNotFound
		}
		worker, err := NewGitChangeWorker(config)
		if err != nil {
			test.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			worker.Run(ctx)
		}()
		synctest.Wait()
		if storage.claims.Load() != 1 {
			test.Error("Run did not poll immediately", storage.claims.Load())
		}
		time.Sleep(999 * time.Millisecond)
		synctest.Wait()
		if storage.claims.Load() != 1 {
			test.Error("Run polled before its original one-second interval", storage.claims.Load())
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if storage.claims.Load() != 2 {
			test.Error("Run lost its one-second poll", storage.claims.Load())
		}
		cancel()
		<-done
		claims := storage.claims.Load()
		time.Sleep(2 * time.Second)
		if storage.claims.Load() != claims || claims != 2 {
			test.Fatal("Run claimed after cancellation or failed to join", storage.claims.Load())
		}
	})
}
