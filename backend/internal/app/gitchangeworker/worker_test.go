package gitchangeworker_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type callerKey struct{}
type workerKey struct{}
type admissionKey struct{}
type transactionKey struct{}

type changeScope struct{ fixture *changeFixture }
type changeJobs struct {
	outbound.GitChangeStore
	fixture *changeFixture
}
type changeMetadata struct{ fixture *changeFixture }
type changeProvider struct{ fixture *changeFixture }
type changeCache struct{ fixture *changeFixture }
type changeAuthorizer struct{ fixture *changeFixture }

type changeFixture struct {
	test          *testing.T
	ctx           context.Context
	job           domain.GitChangeJob
	repository    domain.Repo
	deltas        map[string]domain.GitCommitDelta
	scope         *changeScope
	jobs          *changeJobs
	metadata      *changeMetadata
	provider      *changeProvider
	cache         *changeCache
	authorizer    *changeAuthorizer
	events        []string
	reading       bool
	writing       bool
	active        bool
	claims        atomic.Int64
	finishes      atomic.Int64
	workerCalls   atomic.Int64
	claimCtx      context.Context
	providerCtx   context.Context
	authorizeCtx  context.Context
	finishCtxs    []context.Context
	finished      []domain.GitChangeJob
	admitHook     func(context.Context) error
	claimHook     func(context.Context) (domain.GitChangeJob, error)
	readHook      func(context.Context) (domain.Repo, error)
	metadataHook  func(context.Context) (domain.Repo, error)
	providerHook  func(context.Context, string, string) (domain.GitCommitDelta, error)
	ancestorHook  func(context.Context) (bool, error)
	cacheHook     func(context.Context, string, string) (domain.GitCommitDelta, error)
	authorizeHook func(context.Context) (outbound.GitReadFence, error)
	fenceHook     func(context.Context) error
	finishHook    func(context.Context, domain.GitChangeJob) error
	writeHook     func(context.Context, func(context.Context) error) error
}

func newChangeFixture(test *testing.T) *changeFixture {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	origin := "https://github.com/example/repository"
	target, candidate := strings.Repeat("a", 40), strings.Repeat("b", 40)
	targetParent, candidateParent := strings.Repeat("c", 40), strings.Repeat("d", 40)
	change := domain.GitPathChange{Path: "feature.txt",
		Before: domain.GitEntry{OID: strings.Repeat("1", 40), Mode: "100644"},
		After:  domain.GitEntry{OID: strings.Repeat("2", 40), Mode: "100644"}}
	now := time.Now().UTC()
	request := domain.GitChangeRequest{Target: target, TargetParent: targetParent, Commit: candidate, Parent: candidateParent}
	fixture := &changeFixture{
		test: test, ctx: context.WithValue(inbound.WithRepositoryActor(context.Background(), "caller"), callerKey{}, "preserved"),
		repository: domain.Repo{ID: repo, GitRemoteURL: origin},
		job: domain.GitChangeJob{ID: domain.GitChangeID(repo, origin, request), RepoID: repo, GitOrigin: origin,
			Request: request, State: "running", Attempts: 2, Version: 17,
			CreatedAt: now.Add(-time.Hour), UpdatedAt: now, LeaseUntil: now.Add(2 * time.Minute)},
		deltas: map[string]domain.GitCommitDelta{
			target: {Commit: target, Parents: []string{targetParent}, Parent: targetParent, Complete: true,
				Changes: []domain.GitPathChange{change}},
			candidate: {Commit: candidate, Parents: []string{candidateParent}, Parent: candidateParent, Complete: true,
				Changes: []domain.GitPathChange{{Path: change.Path, Before: change.After, After: change.Before}}},
		},
	}
	fixture.scope = &changeScope{fixture: fixture}
	fixture.jobs = &changeJobs{fixture: fixture}
	fixture.metadata = &changeMetadata{fixture: fixture}
	fixture.provider = &changeProvider{fixture: fixture}
	fixture.cache = &changeCache{fixture: fixture}
	fixture.authorizer = &changeAuthorizer{fixture: fixture}
	if err := fixture.job.Validate(); err != nil {
		test.Fatal("invalid original job fixture", err)
	}
	for _, delta := range fixture.deltas {
		if err := delta.Validate(); err != nil {
			test.Fatal("invalid original evidence fixture", err)
		}
	}
	return fixture
}

func (fixture *changeFixture) config(profile gitchangeworker.Profile) gitchangeworker.Config {
	return gitchangeworker.Config{Profile: profile, Scope: fixture.scope, Jobs: fixture.jobs,
		Metadata: fixture.metadata, Reader: fixture.provider, Authorizer: fixture.authorizer}
}

func (fixture *changeFixture) worker() *gitchangeworker.Worker {
	fixture.test.Helper()
	worker, err := gitchangeworker.New(fixture.config(gitchangeworker.Complete))
	if err != nil {
		fixture.test.Fatal(err)
	}
	return worker
}

func (fixture *changeFixture) order(expected ...string) {
	fixture.test.Helper()
	if !reflect.DeepEqual(fixture.events, expected) {
		fixture.test.Fatal("unexpected execution order", fixture.events, expected)
	}
}

func (fixture *changeFixture) publication(state, reason string, delay time.Duration) domain.GitChangeJob {
	fixture.test.Helper()
	if len(fixture.finished) == 0 {
		fixture.test.Fatal("no finish attempted", fixture.events)
	}
	next := fixture.finished[len(fixture.finished)-1]
	if next.State != state || next.Reason != reason || !next.LeaseUntil.IsZero() {
		fixture.test.Fatal("wrong publication state/reason/lease", next)
	}
	if state != "completed" && (next.Result != nil || next.NextAttempt.Sub(next.UpdatedAt) != delay) {
		fixture.test.Fatal("failure retained proof or changed backoff", next)
	}
	if next.ID != fixture.job.ID || next.RepoID != fixture.job.RepoID || next.GitOrigin != fixture.job.GitOrigin ||
		next.Request != fixture.job.Request || next.Version != fixture.job.Version || next.Attempts != fixture.job.Attempts ||
		!next.CreatedAt.Equal(fixture.job.CreatedAt) {
		fixture.test.Fatal("publication changed the durable claim identity", next, fixture.job)
	}
	if err := next.Validate(); err != nil {
		fixture.test.Fatal("invalid publication", err, next)
	}
	return next
}

func (scope *changeScope) WorkerContext(ctx context.Context) context.Context {
	fixture := scope.fixture
	fixture.workerCalls.Add(1)
	fixture.events = append(fixture.events, "worker")
	ctx = inbound.WithDocumentIdentities(ctx, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
	return context.WithValue(ctx, workerKey{}, scope)
}

func (scope *changeScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	fixture := scope.fixture
	fixture.events = append(fixture.events, "admit")
	if fixture.admitHook != nil {
		if err := fixture.admitHook(ctx); err != nil {
			return err
		}
	}
	fixture.active = true
	defer func() {
		fixture.active = false
		fixture.events = append(fixture.events, "release")
	}()
	return work(context.WithValue(ctx, admissionKey{}, scope))
}

func (scope *changeScope) ReadRepository(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	fixture := scope.fixture
	fixture.events = append(fixture.events, "read")
	if fixture.reading || fixture.writing || repo != fixture.job.RepoID {
		fixture.test.Error("recursive, transactional or foreign initial read")
	}
	fixture.reading = true
	defer func() {
		fixture.reading = false
		fixture.events = append(fixture.events, "read-release")
	}()
	if fixture.readHook != nil {
		return fixture.readHook(ctx)
	}
	return fixture.repository, nil
}

func (scope *changeScope) WithinEvidenceWrite(ctx context.Context, repo domain.ContentHash, write func(context.Context) error) error {
	fixture := scope.fixture
	fixture.events = append(fixture.events, "write")
	if fixture.writing || fixture.reading || repo != fixture.job.RepoID {
		fixture.test.Error("recursive write, retained read or foreign repository")
	}
	fixture.writing = true
	defer func() { fixture.writing = false }()
	bound := context.WithValue(ctx, transactionKey{}, scope)
	if fixture.writeHook != nil {
		return fixture.writeHook(bound, write)
	}
	return write(bound)
}

func (jobs *changeJobs) ClaimGitChange(ctx context.Context, repo domain.ContentHash, id string, now time.Time, lease time.Duration) (domain.GitChangeJob, error) {
	fixture := jobs.fixture
	fixture.claims.Add(1)
	fixture.events = append(fixture.events, "claim")
	fixture.claimCtx = ctx
	if repo != "" || id != "" || lease != 2*time.Minute || time.Since(now) < 0 || time.Since(now) > time.Second || now.Location() != time.UTC ||
		!fixture.active || ctx.Value(admissionKey{}) != fixture.scope || ctx.Value(workerKey{}) != fixture.scope {
		fixture.test.Error("claim filters, lease, UTC clock or shared admission changed")
	}
	if fixture.claimHook != nil {
		return fixture.claimHook(ctx)
	}
	return fixture.job, nil
}

func (jobs *changeJobs) FinishGitChange(ctx context.Context, job domain.GitChangeJob) error {
	fixture := jobs.fixture
	fixture.events = append(fixture.events, "finish")
	fixture.finishCtxs = append(fixture.finishCtxs, ctx)
	fixture.finished = append(fixture.finished, job)
	fixture.finishes.Add(1)
	if !fixture.writing || fixture.reading || ctx.Value(transactionKey{}) != fixture.scope {
		fixture.test.Error("finish escaped the evidence write transaction")
	}
	if fixture.finishHook != nil {
		return fixture.finishHook(ctx, job)
	}
	return nil
}

func (metadata *changeMetadata) GetRepo(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	fixture := metadata.fixture
	fixture.events = append(fixture.events, "metadata")
	if !fixture.writing || fixture.reading || ctx.Value(transactionKey{}) != fixture.scope || repo != fixture.job.RepoID {
		fixture.test.Error("raw metadata lookup escaped the publication transaction")
	}
	if fixture.metadataHook != nil {
		return fixture.metadataHook(ctx)
	}
	return fixture.repository, nil
}

func (provider *changeProvider) ReadCommitDelta(ctx context.Context, origin, commit, parent string) (domain.GitCommitDelta, error) {
	fixture := provider.fixture
	fixture.events = append(fixture.events, "delta:"+commit)
	fixture.providerCtx = ctx
	if fixture.reading || fixture.writing || outbound.GitRepository(ctx) != fixture.job.RepoID || origin != fixture.job.GitOrigin {
		fixture.test.Error("provider read retained a transaction or lost repository/origin binding")
	}
	if fixture.providerHook != nil {
		return fixture.providerHook(ctx, commit, parent)
	}
	delta, found := fixture.deltas[commit]
	if !found {
		return domain.GitCommitDelta{}, domain.ErrNotFound
	}
	if parent != delta.Parent {
		fixture.test.Error("explicit comparison parent was changed", parent, delta.Parent)
	}
	return delta, nil
}

func (provider *changeProvider) IsGitAncestor(ctx context.Context, origin, ancestor, descendant string) (bool, error) {
	fixture := provider.fixture
	fixture.events = append(fixture.events, "ancestor")
	if fixture.reading || fixture.writing || outbound.GitRepository(ctx) != fixture.job.RepoID || origin != fixture.job.GitOrigin ||
		ancestor != fixture.job.Request.Target || descendant != fixture.job.Request.Parent {
		fixture.test.Error("ancestry I/O lost repository/parent binding or retained a transaction")
	}
	if fixture.ancestorHook != nil {
		return fixture.ancestorHook(ctx)
	}
	return true, nil
}

func (cache *changeCache) GetGitDelta(ctx context.Context, repo domain.ContentHash, origin, commit, parent string) (domain.GitCommitDelta, error) {
	fixture := cache.fixture
	fixture.events = append(fixture.events, "cache:"+commit)
	if fixture.reading || fixture.writing || repo != fixture.job.RepoID || origin != fixture.job.GitOrigin {
		fixture.test.Error("cache read changed scope or retained a transaction")
	}
	if fixture.cacheHook != nil {
		return fixture.cacheHook(ctx, commit, parent)
	}
	return domain.GitCommitDelta{}, domain.ErrNotFound
}

func (authorizer *changeAuthorizer) AuthorizeGitRead(ctx context.Context, repo domain.ContentHash) (outbound.GitReadFence, error) {
	fixture := authorizer.fixture
	fixture.events = append(fixture.events, "authorize")
	fixture.authorizeCtx = ctx
	if fixture.reading || fixture.writing || repo != fixture.job.RepoID || outbound.GitRepository(ctx) != repo {
		fixture.test.Error("source authorization retained a transaction or lost repository binding")
	}
	if fixture.authorizeHook != nil {
		return fixture.authorizeHook(ctx)
	}
	return func(tx context.Context) error {
		fixture.events = append(fixture.events, "fence")
		if !fixture.writing || tx.Value(transactionKey{}) != fixture.scope {
			fixture.test.Error("source fence escaped the publication transaction")
		}
		if fixture.fenceHook != nil {
			return fixture.fenceHook(tx)
		}
		return nil
	}, nil
}

func TestNewValidatesProfilesDependenciesAndRealTypedNils(test *testing.T) {
	for _, profile := range []gitchangeworker.Profile{"", "other", gitchangeworker.Compatibility, gitchangeworker.Complete} {
		fixture := newChangeFixture(test)
		worker, err := gitchangeworker.New(fixture.config(profile))
		valid := profile == gitchangeworker.Compatibility || profile == gitchangeworker.Complete
		if valid && (worker == nil || err != nil) || !valid && (worker != nil || !errors.Is(err, domain.ErrValidation)) || len(fixture.events) != 0 {
			test.Fatal("profile validation performed work or accepted an invalid profile", profile, worker, err)
		}
	}
	for _, profile := range []gitchangeworker.Profile{gitchangeworker.Compatibility, gitchangeworker.Complete} {
		for _, field := range []string{"scope", "jobs", "metadata", "reader", "cache", "authorizer"} {
			for _, typedNil := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%s/typed-%t", profile, field, typedNil), func(test *testing.T) {
					fixture := newChangeFixture(test)
					config := fixture.config(profile)
					switch field {
					case "scope":
						config.Scope = nil
						if typedNil {
							config.Scope = (*changeScope)(nil)
						}
					case "jobs":
						config.Jobs = nil
						if typedNil {
							config.Jobs = (*changeJobs)(nil)
						}
					case "metadata":
						config.Metadata = nil
						if typedNil {
							config.Metadata = (*changeMetadata)(nil)
						}
					case "reader":
						config.Reader = nil
						if typedNil {
							config.Reader = (*changeProvider)(nil)
						}
					case "cache":
						config.Cache = nil
						if typedNil {
							config.Cache = (*changeCache)(nil)
						}
					case "authorizer":
						config.Authorizer = nil
						if typedNil {
							config.Authorizer = (*changeAuthorizer)(nil)
						}
					}
					worker, err := gitchangeworker.New(config)
					allowed := !typedNil && field != "scope" && (profile == gitchangeworker.Compatibility || field == "cache" || field == "authorizer")
					if allowed && (worker == nil || err != nil) || !allowed && (worker != nil || !errors.Is(err, domain.ErrValidation)) || len(fixture.events) != 0 {
						test.Fatal("missing dependency or typed-nil contract changed", allowed, worker, err, fixture.events)
					}
				})
			}
		}
	}
}

func TestProcessOriginalLimitsClaimAndAdmissionErrors(test *testing.T) {
	for _, limit := range []int{-10, 0, 1, 4, 129} {
		test.Run(fmt.Sprint(limit), func(test *testing.T) {
			fixture := newChangeFixture(test)
			ctx, cancel := context.WithCancel(fixture.ctx)
			if limit <= 0 {
				cancel()
			} else {
				defer cancel()
			}
			if err := fixture.worker().Process(ctx, limit); err != nil || fixture.claims.Load() != int64(max(limit, 0)) || fixture.finishes.Load() != int64(max(limit, 0)) {
				test.Fatal("original unrestricted positive limit or nonpositive no-op changed", err, fixture.claims.Load(), fixture.finishes.Load())
			}
			if fixture.active || fixture.reading || fixture.writing {
				test.Fatal("process retained resources")
			}
		})
	}
	for _, stage := range []string{"claim", "admit"} {
		for _, failure := range []error{domain.ErrNotFound, domain.ErrIntegrity, domain.ErrConflict, domain.ErrValidation,
			context.Canceled, context.DeadlineExceeded, errors.Join(domain.ErrNotFound, domain.ErrIntegrity)} {
			test.Run(stage+"/"+failure.Error(), func(test *testing.T) {
				fixture := newChangeFixture(test)
				if stage == "admit" {
					fixture.admitHook = func(context.Context) error { return failure }
				} else {
					fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) { return domain.GitChangeJob{}, failure }
				}
				err := fixture.worker().Process(fixture.ctx, 4)
				if errors.Is(failure, domain.ErrNotFound) {
					if err != nil {
						test.Fatal("NotFound no longer terminates the batch before other errors", err)
					}
				} else if err != failure {
					test.Fatal("claim/admission error identity changed", err)
				}
				if fixture.finishes.Load() != 0 || fixture.active {
					test.Fatal("unclaimed work executed or leaked admission")
				}
				if stage == "admit" {
					fixture.order("worker", "admit")
				} else {
					fixture.order("worker", "admit", "claim", "release")
				}
			})
		}
	}
	test.Run("canceled-before-claim", func(test *testing.T) {
		fixture := newChangeFixture(test)
		ctx, cancel := context.WithCancel(fixture.ctx)
		cancel()
		if err := fixture.worker().Process(ctx, 4); err != context.Canceled {
			test.Fatal(err)
		}
		fixture.order("worker")
	})
	test.Run("claim-after-idle", func(test *testing.T) {
		fixture := newChangeFixture(test)
		fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) {
			if fixture.claims.Load() == 3 {
				return domain.GitChangeJob{}, domain.ErrNotFound
			}
			return fixture.job, nil
		}
		if err := fixture.worker().Process(fixture.ctx, 20); err != nil || fixture.claims.Load() != 3 || fixture.finishes.Load() != 2 {
			test.Fatal("idle did not terminate the original batch", err)
		}
	})
}

func TestCompatibilityMissingPortsAreLazyAndNonpositiveLimitsRemainNoOp(test *testing.T) {
	for _, field := range []string{"jobs", "metadata", "reader"} {
		test.Run(field, func(test *testing.T) {
			fixture := newChangeFixture(test)
			config := fixture.config(gitchangeworker.Compatibility)
			switch field {
			case "jobs":
				config.Jobs = nil
			case "metadata":
				config.Metadata = nil
			case "reader":
				config.Reader = nil
			}
			worker, err := gitchangeworker.New(config)
			if err != nil {
				test.Fatal(err)
			}
			for _, limit := range []int{-1, 0} {
				if err := worker.Process(fixture.ctx, limit); err != nil {
					test.Fatal("no-op gained validation", err)
				}
			}
			if err := worker.RunLease(fixture.ctx, fixture.job); err == nil || err.Error() != "durable Git change verification unavailable" || errors.Is(err, domain.ErrValidation) {
				test.Fatal("missing compatibility ports changed the original helper error", err)
			}
			fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) { return domain.GitChangeJob{}, domain.ErrNotFound }
			err = worker.Process(fixture.ctx, 1)
			if field == "jobs" {
				if err == nil || err.Error() != "durable Git change verification unavailable" || fixture.claims.Load() != 0 {
					test.Fatal("missing jobs changed the helper error or claimed work", err)
				}
			} else if err != nil || fixture.claims.Load() != 1 {
				test.Fatal("idle compatibility worker required a reader before the original claim", err)
			}
			ctx, cancel := context.WithCancel(fixture.ctx)
			cancel()
			if err := worker.Process(ctx, 1); err != context.Canceled {
				test.Fatal("missing compatibility port overrode caller cancellation", err)
			}
			if fixture.finishes.Load() != 0 {
				test.Fatal("unavailable worker performed work")
			}
		})
	}
}

func TestRunLeaseOwnsVerificationPublicationAndPreservesCaller(test *testing.T) {
	fixture := newChangeFixture(test)
	started := time.Now()
	fixture.readHook = func(ctx context.Context) (domain.Repo, error) {
		deadline, present := ctx.Deadline()
		if !present || deadline.Sub(started) < 89*time.Second || deadline.Sub(started) > 91*time.Second ||
			ctx.Value(callerKey{}) != "preserved" || outbound.GitRepository(ctx) != fixture.job.RepoID {
			test.Error("lease timeout or caller/repository context changed", deadline)
		}
		return fixture.repository, nil
	}
	if err := fixture.worker().RunLease(fixture.ctx, fixture.job); err != nil {
		test.Fatal(err)
	}
	fixture.order("read", "read-release", "authorize", "delta:"+fixture.job.Request.Target,
		"delta:"+fixture.job.Request.Commit, "ancestor", "write", "metadata", "fence", "finish")
	next := fixture.publication("completed", "", 0)
	expected := &domain.GitReversalEvidence{Commit: fixture.job.Request.Commit, Target: fixture.job.Request.Target,
		Parent: fixture.job.Request.Parent, TargetParent: fixture.job.Request.TargetParent, Coverage: "full",
		Paths: []string{"feature.txt"}, UnverifiedPaths: []string{}, OtherPaths: []string{}}
	if !reflect.DeepEqual(next.Result, expected) || fixture.workerCalls.Load() != 0 || fixture.claims.Load() != 0 {
		test.Fatal("direct lease used a worker entry point or changed evidence", next.Result)
	}
	for _, ctx := range []context.Context{fixture.authorizeCtx, fixture.providerCtx, fixture.finishCtxs[0]} {
		user, system := inbound.RepositoryActor(ctx)
		if user != "caller" || system || ctx.Value(callerKey{}) != "preserved" || ctx.Value(workerKey{}) != nil {
			test.Fatal("RunLease escalated its caller or created a worker context", user, system)
		}
	}
	if fixture.providerCtx.Err() != context.Canceled {
		test.Fatal("lease context outlived its work")
	}
}

func TestProcessOnlyEntryInjectsSystemActorAndContinuesAfterRunFailure(test *testing.T) {
	fixture := newChangeFixture(test)
	failure := errors.New("provider unavailable")
	fixture.providerHook = func(ctx context.Context, _, _ string) (domain.GitCommitDelta, error) {
		if !fixture.active || ctx.Value(admissionKey{}) != fixture.scope || ctx.Value(workerKey{}) != fixture.scope {
			test.Error("provider work escaped the original admission")
		}
		return domain.GitCommitDelta{}, failure
	}
	if err := fixture.worker().Process(fixture.ctx, 3); err != nil || fixture.claims.Load() != 3 || fixture.finishes.Load() != 3 || fixture.active {
		test.Fatal("per-job error stopped processing, escaped admission or changed Process result", err)
	}
	for _, ctx := range []context.Context{fixture.claimCtx, fixture.authorizeCtx, fixture.providerCtx, fixture.finishCtxs[0]} {
		user, system := inbound.RepositoryActor(ctx)
		if !system || user != "" || ctx.Value(callerKey{}) != "preserved" ||
			!reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) {
			test.Fatal("trusted Process entry lost its system actor, supported identities or caller values", user, system)
		}
	}
	user, system := inbound.RepositoryActor(fixture.ctx)
	if user != "caller" || system {
		test.Fatal("Process mutated the caller context")
	}
	fixture.publication("retrying", "temporary_provider_or_storage_failure", 4*time.Second)
}

func TestRunLeaseEarlyReadsAndAuthorizationFailBeforeProvider(test *testing.T) {
	for _, candidate := range []struct {
		name   string
		reason string
		fail   error
	}{
		{name: "missing", reason: "repository_missing", fail: domain.ErrNotFound},
		{name: "identity", reason: "temporary_provider_or_storage_failure", fail: domain.ErrDocumentIdentityUpgradeRequired},
		{name: "denied", reason: "temporary_provider_or_storage_failure", fail: domain.ErrForbidden},
		{name: "integrity", reason: "integrity_check_failed", fail: domain.ErrIntegrity},
		{name: "invalid", reason: "invalid_or_ambiguous_git_evidence", fail: domain.ErrValidation},
		{name: "conflict", reason: "origin_or_lease_changed", fail: domain.ErrConflict},
	} {
		for _, stage := range []string{"read", "authorize"} {
			test.Run(stage+"/"+candidate.name, func(test *testing.T) {
				fixture := newChangeFixture(test)
				if stage == "read" {
					fixture.readHook = func(context.Context) (domain.Repo, error) { return fixture.repository, candidate.fail }
				} else {
					fixture.authorizeHook = func(context.Context) (outbound.GitReadFence, error) { return nil, candidate.fail }
				}
				if err := fixture.worker().RunLease(fixture.ctx, fixture.job); !errors.Is(err, candidate.fail) {
					test.Fatal(err)
				}
				state := "attention"
				if candidate.reason == "temporary_provider_or_storage_failure" {
					state = "retrying"
				}
				fixture.publication(state, candidate.reason, 4*time.Second)
				if fixture.providerCtx != nil || fixture.finishes.Load() != 1 {
					test.Fatal("early failure reached provider or repeated finish")
				}
				if stage == "read" {
					fixture.order("read", "read-release", "write", "finish")
				} else {
					fixture.order("read", "read-release", "authorize", "write", "finish")
				}
			})
		}
	}
	test.Run("origin-rebound-before-provider", func(test *testing.T) {
		fixture := newChangeFixture(test)
		fixture.repository.GitRemoteURL = "https://github.com/example/rebound"
		if err := fixture.worker().RunLease(fixture.ctx, fixture.job); !errors.Is(err, domain.ErrConflict) {
			test.Fatal(err)
		}
		fixture.order("read", "read-release", "write", "finish")
		fixture.publication("attention", "origin_or_lease_changed", 4*time.Second)
	})
	test.Run("nil-source-fence-is-integrity", func(test *testing.T) {
		fixture := newChangeFixture(test)
		fixture.authorizeHook = func(context.Context) (outbound.GitReadFence, error) { return nil, nil }
		if err := fixture.worker().RunLease(fixture.ctx, fixture.job); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal(err)
		}
		fixture.order("read", "read-release", "authorize", "write", "finish")
		fixture.publication("attention", "integrity_check_failed", 4*time.Second)
	})
	test.Run("nil-authorizer-keeps-operator-noop", func(test *testing.T) {
		fixture := newChangeFixture(test)
		config := fixture.config(gitchangeworker.Complete)
		config.Authorizer = nil
		worker, err := gitchangeworker.New(config)
		if err != nil {
			test.Fatal(err)
		}
		if err := worker.RunLease(fixture.ctx, fixture.job); err != nil {
			test.Fatal(err)
		}
		fixture.order("read", "read-release", "delta:"+fixture.job.Request.Target,
			"delta:"+fixture.job.Request.Commit, "ancestor", "write", "metadata", "finish")
		fixture.publication("completed", "", 0)
	})
}

func TestRunLeaseRechecksRawMetadataAndLiveFenceBeforeFinish(test *testing.T) {
	for _, stage := range []string{"origin", "metadata", "fence", "transaction"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newChangeFixture(test)
			failure := domain.ErrConflict
			switch stage {
			case "origin":
				fixture.providerHook = func(_ context.Context, commit, _ string) (domain.GitCommitDelta, error) {
					fixture.repository.GitRemoteURL = "https://github.com/example/rebound"
					return fixture.deltas[commit], nil
				}
			case "metadata":
				failure = domain.ErrNotFound
				fixture.metadataHook = func(context.Context) (domain.Repo, error) { return domain.Repo{}, failure }
			case "fence":
				var revoked bool
				fixture.providerHook = func(_ context.Context, commit, _ string) (domain.GitCommitDelta, error) {
					revoked = true
					return fixture.deltas[commit], nil
				}
				fixture.fenceHook = func(context.Context) error {
					if !revoked {
						test.Error("fence did not observe the live source change")
					}
					return failure
				}
			case "transaction":
				writes := 0
				fixture.writeHook = func(ctx context.Context, write func(context.Context) error) error {
					writes++
					if writes == 1 {
						return failure
					}
					return write(ctx)
				}
			}
			if err := fixture.worker().RunLease(fixture.ctx, fixture.job); !errors.Is(err, failure) {
				test.Fatal(err)
			}
			reason := "origin_or_lease_changed"
			if stage == "metadata" {
				reason = "repository_missing"
			}
			fixture.publication("attention", reason, 4*time.Second)
			if len(fixture.finished) != 1 || fixture.finished[0].Result != nil {
				test.Fatal("stale evidence was published")
			}
			expected := []string{"read", "read-release", "authorize", "delta:" + fixture.job.Request.Target,
				"delta:" + fixture.job.Request.Commit, "ancestor", "write"}
			if stage != "transaction" {
				expected = append(expected, "metadata")
			}
			if stage == "fence" {
				expected = append(expected, "fence")
			}
			expected = append(expected, "write", "finish")
			fixture.order(expected...)
		})
	}
}

func TestRunLeaseVerificationErrorOrderAndUnverifiedResults(test *testing.T) {
	for _, stage := range []string{"target-error", "candidate-error", "ancestry-error", "wrong-commit", "wrong-parent", "malformed-delta", "not-ancestor", "incomplete"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newChangeFixture(test)
			providerFailure := errors.New("provider failure")
			var failure error
			switch stage {
			case "target-error", "candidate-error":
				failure = providerFailure
				fixture.providerHook = func(_ context.Context, commit, _ string) (domain.GitCommitDelta, error) {
					if stage == "target-error" || commit == fixture.job.Request.Commit {
						return domain.GitCommitDelta{}, providerFailure
					}
					return fixture.deltas[commit], nil
				}
			case "ancestry-error":
				failure = providerFailure
				fixture.ancestorHook = func(context.Context) (bool, error) { return false, providerFailure }
			case "wrong-commit", "wrong-parent", "malformed-delta", "incomplete":
				delta := fixture.deltas[fixture.job.Request.Commit]
				switch stage {
				case "wrong-commit":
					failure = domain.ErrIntegrity
					delta.Commit = strings.Repeat("e", 40)
				case "wrong-parent":
					failure = domain.ErrIntegrity
					delta.Parent = strings.Repeat("e", 40)
					fixture.providerHook = func(_ context.Context, commit, parent string) (domain.GitCommitDelta, error) {
						if commit == fixture.job.Request.Commit {
							if parent != fixture.job.Request.Parent {
								test.Error("explicit comparison parent was changed", parent, fixture.job.Request.Parent)
							}
							return delta, nil
						}
						return fixture.deltas[commit], nil
					}
				case "malformed-delta":
					failure = domain.ErrValidation
					delta.Parents = []string{delta.Commit}
				case "incomplete":
					delta.Complete = false
				}
				fixture.deltas[fixture.job.Request.Commit] = delta
			case "not-ancestor":
				fixture.ancestorHook = func(context.Context) (bool, error) { return false, nil }
			}
			err := fixture.worker().RunLease(fixture.ctx, fixture.job)
			if failure != nil {
				if !errors.Is(err, failure) {
					test.Fatal("verification error was changed", err, failure)
				}
				state, reason := "retrying", "temporary_provider_or_storage_failure"
				if failure == domain.ErrIntegrity {
					state, reason = "attention", "integrity_check_failed"
				}
				if failure == domain.ErrValidation {
					state, reason = "attention", "invalid_or_ambiguous_git_evidence"
				}
				fixture.publication(state, reason, 4*time.Second)
			} else {
				if err != nil {
					test.Fatal(err)
				}
				reason := "incomplete_git_evidence"
				if stage == "not-ancestor" {
					reason = "target_not_in_candidate_ancestry"
				}
				next := fixture.publication("completed", reason, 0)
				if next.Result == nil || next.Result.Coverage != "unverified" {
					test.Fatal("unverified work was asserted as proof", next)
				}
			}
			expected := []string{"read", "read-release", "authorize", "delta:" + fixture.job.Request.Target}
			if stage != "target-error" {
				expected = append(expected, "delta:"+fixture.job.Request.Commit)
			}
			if stage == "ancestry-error" || stage == "not-ancestor" || stage == "incomplete" {
				expected = append(expected, "ancestor")
			}
			expected = append(expected, "write")
			if failure == nil {
				expected = append(expected, "metadata", "fence")
			}
			expected = append(expected, "finish")
			fixture.order(expected...)
		})
	}
}

func TestRunLeaseFailurePriorityRetryCapBackoffAndJoinedStorageErrors(test *testing.T) {
	for _, candidate := range []struct {
		name     string
		failure  error
		attempts int
		state    string
		reason   string
		delay    time.Duration
	}{
		{name: "first", failure: errors.New("offline"), attempts: 1, state: "retrying", reason: "temporary_provider_or_storage_failure", delay: 2 * time.Second},
		{name: "zero", failure: errors.New("offline"), attempts: 0, state: "retrying", reason: "temporary_provider_or_storage_failure", delay: time.Second},
		{name: "backoff-bound", failure: errors.New("offline"), attempts: 8, state: "retrying", reason: "temporary_provider_or_storage_failure", delay: 256 * time.Second},
		{name: "last-retry", failure: errors.New("offline"), attempts: 19, state: "retrying", reason: "temporary_provider_or_storage_failure", delay: 256 * time.Second},
		{name: "exhausted", failure: errors.New("offline"), attempts: 20, state: "attention", reason: "retry_limit_reached", delay: 256 * time.Second},
		{name: "past-exhausted", failure: errors.New("offline"), attempts: 21, state: "attention", reason: "retry_limit_reached", delay: 256 * time.Second},
		{name: "integrity-first", failure: errors.Join(domain.ErrNotFound, domain.ErrConflict, domain.ErrValidation, domain.ErrIntegrity), attempts: 20, state: "attention", reason: "integrity_check_failed", delay: 256 * time.Second},
		{name: "validation-next", failure: errors.Join(domain.ErrNotFound, domain.ErrConflict, domain.ErrValidation), attempts: 20, state: "attention", reason: "invalid_or_ambiguous_git_evidence", delay: 256 * time.Second},
		{name: "conflict-next", failure: errors.Join(domain.ErrNotFound, domain.ErrConflict), attempts: 20, state: "attention", reason: "origin_or_lease_changed", delay: 256 * time.Second},
		{name: "missing-last", failure: domain.ErrNotFound, attempts: 20, state: "attention", reason: "repository_missing", delay: 256 * time.Second},
		{name: "canceled", failure: context.Canceled, attempts: 2, state: "retrying", reason: "temporary_provider_or_storage_failure", delay: 4 * time.Second},
		{name: "deadline", failure: context.DeadlineExceeded, attempts: 2, state: "retrying", reason: "temporary_provider_or_storage_failure", delay: 4 * time.Second},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			fixture := newChangeFixture(test)
			fixture.job.Attempts = candidate.attempts
			fixture.providerHook = func(context.Context, string, string) (domain.GitCommitDelta, error) {
				return domain.GitCommitDelta{}, candidate.failure
			}
			finishFailure := errors.New("failure finish unavailable")
			fixture.finishHook = func(context.Context, domain.GitChangeJob) error { return finishFailure }
			err := fixture.worker().RunLease(fixture.ctx, fixture.job)
			if !errors.Is(err, candidate.failure) || !errors.Is(err, finishFailure) || len(fixture.finished) != 1 {
				test.Fatal("run error was swallowed, changed or retried", err)
			}
			fixture.publication(candidate.state, candidate.reason, candidate.delay)
		})
	}
	test.Run("exhausted-success-is-still-completed", func(test *testing.T) {
		fixture := newChangeFixture(test)
		fixture.job.Attempts = 21
		if err := fixture.worker().RunLease(fixture.ctx, fixture.job); err != nil {
			test.Fatal(err)
		}
		fixture.publication("completed", "", 0)
	})
}

func TestRunLeaseLostAcknowledgmentCannotReplaceTerminalEvidence(test *testing.T) {
	fixture := newChangeFixture(test)
	current := fixture.job
	unknownACK := errors.New("commit acknowledgment lost")
	fixture.finishHook = func(_ context.Context, next domain.GitChangeJob) error {
		if err := current.AcceptFinish(next); err != nil {
			return err
		}
		current = next
		return unknownACK
	}
	err := fixture.worker().RunLease(fixture.ctx, fixture.job)
	if !errors.Is(err, unknownACK) || !errors.Is(err, domain.ErrConflict) || len(fixture.finished) != 2 ||
		fixture.finished[0].State != "completed" || fixture.finished[0].Result == nil ||
		fixture.finished[1].State != "retrying" || fixture.finished[1].Result != nil ||
		!reflect.DeepEqual(current, fixture.finished[0]) {
		test.Fatal("unknown commit outcome overwrote the terminal row or lost its error", err, current, fixture.finished)
	}
	fixture.order("read", "read-release", "authorize", "delta:"+fixture.job.Request.Target,
		"delta:"+fixture.job.Request.Commit, "ancestor", "write", "metadata", "fence", "finish", "write", "finish")
}

func TestRunLeaseStaleVersionAndFailedWriteCannotPublishEvidence(test *testing.T) {
	for _, stage := range []string{"version", "finish", "failure-write"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newChangeFixture(test)
			current := fixture.job
			current.Version++
			failure := errors.New("storage unavailable")
			switch stage {
			case "version":
				fixture.finishHook = func(_ context.Context, next domain.GitChangeJob) error { return current.AcceptFinish(next) }
			case "finish":
				fixture.finishHook = func(context.Context, domain.GitChangeJob) error { return failure }
			case "failure-write":
				fixture.providerHook = func(context.Context, string, string) (domain.GitCommitDelta, error) {
					return domain.GitCommitDelta{}, domain.ErrIntegrity
				}
				fixture.writeHook = func(context.Context, func(context.Context) error) error { return failure }
			}
			err := fixture.worker().RunLease(fixture.ctx, fixture.job)
			if stage == "version" {
				if !errors.Is(err, domain.ErrConflict) || len(fixture.finished) != 2 {
					test.Fatal("stale claim accepted", err)
				}
				fixture.publication("attention", "origin_or_lease_changed", 4*time.Second)
			} else if !errors.Is(err, failure) {
				test.Fatal("write failure lost", err)
			}
			if stage == "failure-write" && (!errors.Is(err, domain.ErrIntegrity) || len(fixture.finished) != 0) {
				test.Fatal("failed evidence transaction invoked its callback or lost the primary error", err)
			}
			if current.State != "running" || current.Result != nil || current.Version != fixture.job.Version+1 {
				test.Fatal("stale worker changed the live claim", current)
			}
		})
	}
}

func TestRunLeaseUsesSharedReversalCacheAndRepositoryBoundFallback(test *testing.T) {
	for _, mode := range []string{"cached", "missing-deltas", "missing-ancestry", "cache-error", "wrong-cached-commit"} {
		test.Run(mode, func(test *testing.T) {
			fixture := newChangeFixture(test)
			config := fixture.config(gitchangeworker.Complete)
			config.Cache = fixture.cache
			fixture.cacheHook = func(_ context.Context, commit, parent string) (domain.GitCommitDelta, error) {
				if mode == "cache-error" {
					return domain.GitCommitDelta{}, domain.ErrIntegrity
				}
				if mode == "missing-deltas" {
					return domain.GitCommitDelta{}, domain.ErrNotFound
				}
				if commit == fixture.job.Request.Parent {
					if parent != "" {
						test.Error("ancestry cache used a selected comparison parent")
					}
					if mode == "missing-ancestry" {
						return domain.GitCommitDelta{}, domain.ErrNotFound
					}
					return domain.GitCommitDelta{Commit: commit, Parents: []string{fixture.job.Request.Target},
						Parent: fixture.job.Request.Target, Complete: true}, nil
				}
				delta := fixture.deltas[commit]
				if parent != delta.Parent {
					test.Error("delta cache lost an explicit parent")
				}
				if mode == "wrong-cached-commit" {
					delta.Commit = strings.Repeat("e", 40)
				}
				return delta, nil
			}
			worker, err := gitchangeworker.New(config)
			if err != nil {
				test.Fatal(err)
			}
			err = worker.RunLease(fixture.ctx, fixture.job)
			if mode == "cache-error" || mode == "wrong-cached-commit" {
				if !errors.Is(err, domain.ErrIntegrity) || fixture.providerCtx != nil {
					test.Fatal("cache corruption fell back to the provider", err)
				}
				fixture.publication("attention", "integrity_check_failed", 4*time.Second)
			} else {
				if err != nil {
					test.Fatal(err)
				}
				fixture.publication("completed", "", 0)
			}
			expected := []string{"read", "read-release", "authorize", "cache:" + fixture.job.Request.Target}
			if mode == "missing-deltas" {
				expected = append(expected, "delta:"+fixture.job.Request.Target)
			}
			if mode != "cache-error" {
				expected = append(expected, "cache:"+fixture.job.Request.Commit)
				if mode == "missing-deltas" {
					expected = append(expected, "delta:"+fixture.job.Request.Commit)
				}
				if mode != "wrong-cached-commit" {
					expected = append(expected, "cache:"+fixture.job.Request.Parent)
					if mode == "missing-deltas" || mode == "missing-ancestry" {
						expected = append(expected, "ancestor")
					}
				}
			}
			expected = append(expected, "write")
			if mode != "cache-error" && mode != "wrong-cached-commit" {
				expected = append(expected, "metadata", "fence")
			}
			expected = append(expected, "finish")
			fixture.order(expected...)
		})
	}
}

func TestRepositoryEvidenceAdapterBindsBothProviderMethodsWithoutChangingCaller(test *testing.T) {
	fixture := newChangeFixture(test)
	other := domain.HashContent([]byte("different caller repository"))
	ctx := outbound.WithGitRepository(fixture.ctx, other)
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	reader := gitchangeworker.RepositoryEvidence(fixture.job.RepoID, fixture.provider)
	delta, err := reader.ReadCommitDelta(ctx, fixture.job.GitOrigin, fixture.job.Request.Target, fixture.job.Request.TargetParent)
	if err != nil || !reflect.DeepEqual(delta, fixture.deltas[fixture.job.Request.Target]) {
		test.Fatal(delta, err)
	}
	ancestor, err := reader.IsGitAncestor(ctx, fixture.job.GitOrigin, fixture.job.Request.Target, fixture.job.Request.Parent)
	if err != nil || !ancestor {
		test.Fatal(ancestor, err)
	}
	if outbound.GitRepository(ctx) != other || outbound.GitRepository(fixture.providerCtx) != fixture.job.RepoID ||
		fixture.providerCtx.Value(callerKey{}) != "preserved" || fixture.providerCtx.Err() != nil {
		test.Fatal("adapter changed its caller or lost existing values")
	}
	expectedDeadline, _ := ctx.Deadline()
	actualDeadline, _ := fixture.providerCtx.Deadline()
	user, system := inbound.RepositoryActor(fixture.providerCtx)
	if !actualDeadline.Equal(expectedDeadline) || user != "caller" || system {
		test.Fatal("adapter changed deadline or actor")
	}
	fixture.order("delta:"+fixture.job.Request.Target, "ancestor")
}

func TestRunLeaseNinetySecondDeadlineCoversReadAuthorizationProviderAndPublication(test *testing.T) {
	for _, stage := range []string{"read", "authorize", "provider", "metadata", "fence"} {
		test.Run(stage, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newChangeFixture(test)
				started := time.Now()
				wait := func(ctx context.Context) error {
					deadline, present := ctx.Deadline()
					if !present || !deadline.Equal(started.Add(90*time.Second)) {
						test.Error("stage received a fresh or missing work timeout", stage, deadline)
					}
					<-ctx.Done()
					return ctx.Err()
				}
				switch stage {
				case "read":
					fixture.readHook = func(ctx context.Context) (domain.Repo, error) { return domain.Repo{}, wait(ctx) }
				case "authorize":
					fixture.authorizeHook = func(ctx context.Context) (outbound.GitReadFence, error) { return nil, wait(ctx) }
				case "provider":
					fixture.providerHook = func(ctx context.Context, _, _ string) (domain.GitCommitDelta, error) {
						return domain.GitCommitDelta{}, wait(ctx)
					}
				case "metadata":
					fixture.metadataHook = func(ctx context.Context) (domain.Repo, error) { return domain.Repo{}, wait(ctx) }
				case "fence":
					fixture.fenceHook = wait
				}
				fixture.finishHook = func(ctx context.Context, next domain.GitChangeJob) error {
					deadline, present := ctx.Deadline()
					if !present || !deadline.Equal(started.Add(95*time.Second)) || ctx.Err() != nil ||
						ctx.Value(callerKey{}) != "preserved" || next.Result != nil || ctx.Value(workerKey{}) != nil {
						test.Error("failure finish did not detach with its original values and five-second limit", deadline, ctx.Err())
					}
					return nil
				}
				if err := fixture.worker().RunLease(fixture.ctx, fixture.job); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 90*time.Second {
					test.Fatal("work limit did not cover the complete stage", err, time.Since(started))
				}
				fixture.publication("retrying", "temporary_provider_or_storage_failure", 4*time.Second)
				if fixture.ctx.Err() != nil || fixture.finishCtxs[0].Err() != context.Canceled {
					test.Fatal("work canceled its caller or retained failure context")
				}
			})
		})
	}
}

func TestRunLeaseEarlierCallerDeadlineAndDetachedFailureTimeout(test *testing.T) {
	for _, callerLimit := range []time.Duration{time.Second, 3 * time.Second} {
		test.Run(callerLimit.String(), func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newChangeFixture(test)
				ctx, cancel := context.WithTimeout(fixture.ctx, callerLimit)
				defer cancel()
				started := time.Now()
				fixture.providerHook = func(work context.Context, _, _ string) (domain.GitCommitDelta, error) {
					deadline, present := work.Deadline()
					if !present || !deadline.Equal(started.Add(callerLimit)) {
						test.Error("lease extended the earlier caller deadline", deadline)
					}
					<-work.Done()
					return domain.GitCommitDelta{}, work.Err()
				}
				fixture.finishHook = func(finish context.Context, _ domain.GitChangeJob) error {
					deadline, present := finish.Deadline()
					if !present || !deadline.Equal(started.Add(callerLimit+5*time.Second)) || finish.Err() != nil ||
						ctx.Err() != context.DeadlineExceeded || finish.Value(callerKey{}) != "preserved" {
						test.Error("failure write inherited cancellation or extended its detached limit", deadline, finish.Err())
					}
					<-finish.Done()
					return finish.Err()
				}
				if err := fixture.worker().RunLease(ctx, fixture.job); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != callerLimit+5*time.Second || len(fixture.finished) != 1 {
					test.Fatal("detached failure did not terminate at its five-second deadline", err, time.Since(started))
				}
				fixture.publication("retrying", "temporary_provider_or_storage_failure", 4*time.Second)
			})
		})
	}
}

func TestRunLeaseTimeoutIsNotRestartedAfterProviderPreparation(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newChangeFixture(test)
		started := time.Now()
		fixture.authorizeHook = func(context.Context) (outbound.GitReadFence, error) {
			time.Sleep(80 * time.Second)
			return func(context.Context) error { return nil }, nil
		}
		fixture.providerHook = func(ctx context.Context, _, _ string) (domain.GitCommitDelta, error) {
			deadline, present := ctx.Deadline()
			if !present || !deadline.Equal(started.Add(90*time.Second)) {
				test.Error("provider restarted the work deadline", deadline)
			}
			<-ctx.Done()
			return domain.GitCommitDelta{}, ctx.Err()
		}
		if err := fixture.worker().RunLease(fixture.ctx, fixture.job); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 90*time.Second {
			test.Fatal("authorization preparation was excluded from the work limit", err, time.Since(started))
		}
		fixture.publication("retrying", "temporary_provider_or_storage_failure", 4*time.Second)
	})
}

func TestRunImmediateFourJobBatchAndOneSecondPolling(test *testing.T) {
	for _, mode := range []string{"productive", "idle", "claim-error", "partial-idle", "job-error"} {
		test.Run(mode, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newChangeFixture(test)
				var perClaims, perFinishes int64 = 4, 4
				switch mode {
				case "idle":
					perClaims, perFinishes = 1, 0
					fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) { return domain.GitChangeJob{}, domain.ErrNotFound }
				case "claim-error":
					perClaims, perFinishes = 1, 0
					fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) { return domain.GitChangeJob{}, domain.ErrIntegrity }
				case "partial-idle":
					perClaims, perFinishes = 3, 2
					fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) {
						if fixture.claims.Load()%3 == 0 {
							return domain.GitChangeJob{}, domain.ErrNotFound
						}
						return fixture.job, nil
					}
				case "job-error":
					fixture.providerHook = func(context.Context, string, string) (domain.GitCommitDelta, error) {
						return domain.GitCommitDelta{}, domain.ErrValidation
					}
				}
				ctx, cancel := context.WithCancel(fixture.ctx)
				defer cancel()
				worker := fixture.worker()
				done := make(chan struct{})
				go func() { defer close(done); worker.Run(ctx) }()
				synctest.Wait()
				if fixture.claims.Load() != perClaims || fixture.finishes.Load() != perFinishes {
					test.Fatal("initial batch was not immediate or bounded to four")
				}
				time.Sleep(time.Second - time.Nanosecond)
				synctest.Wait()
				if fixture.claims.Load() != perClaims || fixture.finishes.Load() != perFinishes {
					test.Fatal("poll ran before one second")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if fixture.claims.Load() != 2*perClaims || fixture.finishes.Load() != 2*perFinishes {
					test.Fatal("poll did not resume at one second")
				}
				cancel()
				<-done
				if fixture.claims.Load() != 2*perClaims || fixture.finishes.Load() != 2*perFinishes || fixture.active || fixture.reading || fixture.writing {
					test.Fatal("canceled polling continued or retained work")
				}
			})
		})
	}
}

func TestRunKeepsOriginalTickerWhileAJobIsInFlight(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newChangeFixture(test)
		first := true
		fixture.providerHook = func(_ context.Context, commit, _ string) (domain.GitCommitDelta, error) {
			if first {
				first = false
				time.Sleep(2500 * time.Millisecond)
			}
			return fixture.deltas[commit], nil
		}
		ctx, cancel := context.WithCancel(fixture.ctx)
		defer cancel()
		worker := fixture.worker()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		synctest.Wait()
		if fixture.claims.Load() != 1 || fixture.finishes.Load() != 0 {
			test.Fatal("job did not remain in flight")
		}
		time.Sleep(2500 * time.Millisecond)
		synctest.Wait()
		if fixture.claims.Load() != 8 || fixture.finishes.Load() != 8 {
			test.Fatal("ticker was reset after work instead of consuming its pending tick")
		}
		cancel()
		<-done
		if fixture.active {
			test.Fatal("ticker cancellation leaked admission")
		}
	})
}

func TestRunCancellationReachesProviderAndDetachesOnlyFailurePublication(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newChangeFixture(test)
		ctx, cancel := context.WithCancel(fixture.ctx)
		defer cancel()
		started := make(chan struct{})
		fixture.providerHook = func(work context.Context, _, _ string) (domain.GitCommitDelta, error) {
			close(started)
			<-work.Done()
			return domain.GitCommitDelta{}, work.Err()
		}
		fixture.finishHook = func(finish context.Context, _ domain.GitChangeJob) error {
			user, system := inbound.RepositoryActor(finish)
			if ctx.Err() != context.Canceled || finish.Err() != nil || finish.Value(callerKey{}) != "preserved" ||
				finish.Value(admissionKey{}) != fixture.scope || !fixture.active || !system || user != "" {
				test.Error("failure detached from original values/permit/actor or retained cancellation")
			}
			return nil
		}
		worker := fixture.worker()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		<-started
		if fixture.claims.Load() != 1 || fixture.finishes.Load() != 0 {
			test.Fatal("cancellation fixture did not stop at the first provider call")
		}
		cancel()
		<-done
		if fixture.claims.Load() != 1 || fixture.finishes.Load() != 1 || fixture.active || fixture.providerCtx.Err() != context.Canceled {
			test.Fatal("cancellation repeated work, skipped failure publication or retained admission")
		}
		fixture.publication("retrying", "temporary_provider_or_storage_failure", 4*time.Second)
	})
}

func TestWorkerLogsNeverExposeProviderOrClaimErrorContents(test *testing.T) {
	var output bytes.Buffer
	original := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(original)
	secret := "provider-response: token=do-not-log https://private.example/source"
	fixture := newChangeFixture(test)
	fixture.providerHook = func(context.Context, string, string) (domain.GitCommitDelta, error) {
		return domain.GitCommitDelta{}, errors.New(secret)
	}
	if err := fixture.worker().Process(fixture.ctx, 1); err != nil {
		test.Fatal(err)
	}
	synctest.Test(test, func(test *testing.T) {
		fixture := newChangeFixture(test)
		fixture.claimHook = func(context.Context) (domain.GitChangeJob, error) { return domain.GitChangeJob{}, errors.New(secret) }
		ctx, cancel := context.WithCancel(fixture.ctx)
		defer cancel()
		worker := fixture.worker()
		done := make(chan struct{})
		go func() { defer close(done); worker.Run(ctx) }()
		synctest.Wait()
		cancel()
		<-done
	})
	if strings.Contains(output.String(), secret) || strings.Contains(output.String(), "do-not-log") ||
		!strings.Contains(output.String(), "Git change verification ") || !strings.Contains(output.String(), "Git change worker deferred") {
		test.Fatal("worker leaked error contents or omitted its safe diagnostic", output.String())
	}
}
