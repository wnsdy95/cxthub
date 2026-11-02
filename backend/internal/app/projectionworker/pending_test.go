package projectionworker_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pendingWorkerKey struct{}
type pendingAdmissionKey struct{}
type pendingCallerKey struct{}

type pendingScope struct {
	events      []string
	workerCalls int
	workCalls   int
	checkCalls  int
	active      bool
	reading     bool
	admit       func(context.Context) error
	check       func(context.Context, domain.ContentHash) error
}

func (scope *pendingScope) WorkerContext(ctx context.Context) context.Context {
	scope.workerCalls++
	scope.events = append(scope.events, "worker")
	return context.WithValue(ctx, pendingWorkerKey{}, scope)
}

func (scope *pendingScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	scope.workCalls++
	if scope.admit != nil {
		if err := scope.admit(ctx); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	scope.events = append(scope.events, "admit")
	scope.active = true
	defer func() {
		scope.active = false
		scope.events = append(scope.events, "release")
	}()
	return work(context.WithValue(ctx, pendingAdmissionKey{}, scope))
}

func (scope *pendingScope) CheckRepository(ctx context.Context, repo domain.ContentHash) error {
	scope.checkCalls++
	scope.events = append(scope.events, "check")
	scope.reading = true
	defer func() { scope.reading = false }()
	if scope.check != nil {
		return scope.check(ctx, repo)
	}
	return ctx.Err()
}

type pendingStore struct {
	outbound.PendingProjectionStore
	claim   func(context.Context, time.Duration) (domain.PendingProjectionLease, error)
	save    func(context.Context, domain.PendingProjectionLease, domain.RepositoryViewSourcePage) error
	load    func(context.Context, domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error)
	publish func(context.Context, domain.PendingProjectionLease, []domain.PendingProjectionPart) error
	fail    func(context.Context, domain.PendingProjectionLease, string, bool) error
}

func (storage *pendingStore) ClaimPendingProjection(ctx context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
	return storage.claim(ctx, lease)
}

func (storage *pendingStore) SavePendingProjectionPage(ctx context.Context, job domain.PendingProjectionLease, page domain.RepositoryViewSourcePage) error {
	return storage.save(ctx, job, page)
}

func (storage *pendingStore) LoadPendingProjectionInputs(ctx context.Context, job domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
	return storage.load(ctx, job)
}

func (storage *pendingStore) PublishPendingProjection(ctx context.Context, job domain.PendingProjectionLease, parts []domain.PendingProjectionPart) error {
	return storage.publish(ctx, job, parts)
}

func (storage *pendingStore) FailPendingProjection(ctx context.Context, job domain.PendingProjectionLease, reason string, retry bool) error {
	return storage.fail(ctx, job, reason, retry)
}

type pendingSource struct {
	read func(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error)
}

func (source *pendingSource) RepositoryViewSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	return source.read(ctx, repo, request)
}

type pendingFailure struct {
	ctx    context.Context
	job    domain.PendingProjectionLease
	reason string
	retry  bool
}

type pendingFixture struct {
	scope       *pendingScope
	storage     *pendingStore
	source      *pendingSource
	job         domain.PendingProjectionLease
	page        domain.RepositoryViewSourcePage
	entries     []domain.RepositoryViewSourceEntry
	parts       []domain.PendingProjectionPart
	claims      int
	sourceReads int
	saves       int
	loads       int
	publishes   int
	failures    []pendingFailure
}

func pendingJSON(test *testing.T, value any) json.RawMessage {
	test.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	return encoded
}

func newPendingFixture(test *testing.T) *pendingFixture {
	test.Helper()
	repo := domain.HashContent([]byte(test.Name()))
	hash := domain.HashContent([]byte(test.Name() + "/snapshot"))
	snapshot := domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, SessionID: "live", Provider: domain.ProviderCodex,
		Branch: "main", Parents: []domain.ContentHash{}, CreatedAt: time.Unix(123, 0).UTC()}
	pending := domain.Pending{RepoID: repo, SessionID: snapshot.SessionID, Branch: snapshot.Branch,
		Provider: snapshot.Provider, Target: snapshot.ID, UpdatedAt: snapshot.CreatedAt}
	revision := domain.RepositoryRevision{Graph: 2, Pending: 3}
	fixture := &pendingFixture{scope: &pendingScope{}, storage: &pendingStore{}, source: &pendingSource{},
		job: domain.PendingProjectionLease{RepoID: repo, Dataset: "11111111-1111-4111-8111-111111111111",
			Epoch: "22222222-2222-4222-8222-222222222222", Fence: 7, Phase: "capture", AppliedSequence: -1,
			InputSequence: 5, LeaseUntil: time.Now().Add(time.Minute), Attempts: 2}}
	fixture.entries = []domain.RepositoryViewSourceEntry{
		{Sequence: 1, Kind: "repository", Key: string(repo), Value: pendingJSON(test, domain.Repo{ID: repo, DefaultBranch: "main"})},
		{Sequence: 2, Kind: "protocol", Key: string(repo), Value: pendingJSON(test, map[string]int{"context_protocol": 0})},
		{Sequence: 3, Kind: "revision", Key: string(repo), Value: pendingJSON(test, revision)},
		{Sequence: 4, Kind: "snapshot", Key: string(hash), Value: pendingJSON(test, snapshot)},
		{Sequence: 5, Kind: "pending", Key: pending.SessionID, Value: pendingJSON(test, pending)},
	}
	want, err := domain.ProjectPendingState(context.Background(), domain.RepositoryView{DefaultBranch: "main", Revision: revision,
		Snapshots: []domain.Snapshot{snapshot}, Pending: []domain.Pending{pending}})
	if err != nil {
		test.Fatal(err)
	}
	fixture.parts, err = domain.EncodePendingProjection(context.Background(), repo, want)
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := &domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryViewSourceScope, RepoID: repo, Epoch: fixture.job.Epoch, Sequence: 5}
	fixture.page = domain.RepositoryViewSourcePage{Version: 1, Scope: domain.RepositoryViewSourceScope, RepoID: repo,
		Epoch: fixture.job.Epoch, Through: 5, Mode: "baseline", Entries: fixture.entries, Checkpoint: checkpoint}
	fixture.scope.check = func(ctx context.Context, requested domain.ContentHash) error {
		if requested != repo {
			test.Fatalf("identity check crossed repositories: %s", requested)
		}
		return ctx.Err()
	}
	fixture.storage.claim = func(ctx context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
		fixture.claims++
		fixture.scope.events = append(fixture.scope.events, "claim")
		if !fixture.scope.active || ctx.Value(pendingWorkerKey{}) != fixture.scope || ctx.Value(pendingAdmissionKey{}) != fixture.scope {
			test.Fatal("claim escaped the worker admission context")
		}
		deadline, bounded := ctx.Deadline()
		if !bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 30*time.Second || lease != time.Minute {
			test.Fatalf("claim budget or lease changed: bounded=%t deadline=%v lease=%v", bounded, deadline, lease)
		}
		return fixture.job, nil
	}
	fixture.source.read = func(ctx context.Context, requested domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
		fixture.sourceReads++
		fixture.scope.events = append(fixture.scope.events, "source")
		if fixture.scope.reading || requested != repo || !reflect.DeepEqual(request, fixture.job.SourceRequest()) {
			test.Fatalf("source read changed scope or checkpoint: repo=%s request=%+v", requested, request)
		}
		return fixture.page, ctx.Err()
	}
	fixture.storage.save = func(ctx context.Context, job domain.PendingProjectionLease, page domain.RepositoryViewSourcePage) error {
		fixture.saves++
		fixture.scope.events = append(fixture.scope.events, "save")
		if fixture.scope.reading || job != fixture.job || !reflect.DeepEqual(page, fixture.page) {
			test.Fatal("capture changed the lease/page or saved inside an identity read")
		}
		return ctx.Err()
	}
	fixture.storage.load = func(ctx context.Context, job domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
		fixture.loads++
		fixture.scope.events = append(fixture.scope.events, "load")
		if fixture.scope.reading || job != fixture.job {
			test.Fatal("input read changed the lease or inherited an identity read")
		}
		return fixture.entries, ctx.Err()
	}
	fixture.storage.publish = func(ctx context.Context, job domain.PendingProjectionLease, parts []domain.PendingProjectionPart) error {
		fixture.publishes++
		fixture.scope.events = append(fixture.scope.events, "publish")
		if fixture.scope.reading || job != fixture.job || !reflect.DeepEqual(parts, fixture.parts) {
			test.Fatalf("publication changed the lease, scope, or projected facts: %+v", parts)
		}
		return ctx.Err()
	}
	fixture.storage.fail = func(ctx context.Context, job domain.PendingProjectionLease, reason string, retry bool) error {
		fixture.scope.events = append(fixture.scope.events, "fail")
		fixture.failures = append(fixture.failures, pendingFailure{ctx: ctx, job: job, reason: reason, retry: retry})
		if fixture.scope.reading || job != fixture.job {
			test.Fatal("failure changed the lease or ran inside an identity read")
		}
		return ctx.Err()
	}
	return fixture
}

func (fixture *pendingFixture) config(profile projectionworker.Profile) projectionworker.PendingConfig {
	return projectionworker.PendingConfig{Profile: profile, Scope: fixture.scope, Jobs: fixture.storage, Source: fixture.source, Enabled: true}
}

func (fixture *pendingFixture) worker(test *testing.T) *projectionworker.Pending {
	test.Helper()
	worker, err := projectionworker.NewPending(fixture.config(projectionworker.Complete))
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

type pendingPollingState struct {
	claims      int
	saves       int
	workerCalls int
	workCalls   int
	checkCalls  int
	active      bool
	reading     bool
	failures    int
}

type pendingPollingScope struct {
	*pendingScope
	fixture   *pendingFixture
	batchSize int
	state     atomic.Pointer[pendingPollingState]
	completed chan pendingPollingState
}

func (scope *pendingPollingScope) observe() pendingPollingState {
	state := pendingPollingState{claims: scope.fixture.claims, saves: scope.fixture.saves,
		workerCalls: scope.workerCalls, workCalls: scope.workCalls, checkCalls: scope.checkCalls,
		active: scope.active, reading: scope.reading, failures: len(scope.fixture.failures)}
	scope.state.Store(&state)
	return state
}

func (scope *pendingPollingScope) WorkerContext(ctx context.Context) context.Context {
	ctx = scope.pendingScope.WorkerContext(ctx)
	scope.observe()
	return ctx
}

func (scope *pendingPollingScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	err := scope.pendingScope.WithinWork(ctx, func(bound context.Context) error {
		scope.observe()
		return work(bound)
	})
	state := scope.observe()
	if state.workCalls%scope.batchSize == 0 {
		select {
		case scope.completed <- state:
		default:
		}
	}
	return err
}

func newPendingPollingWorker(test *testing.T, fixture *pendingFixture, batchSize int) (*projectionworker.Pending, *pendingPollingScope) {
	test.Helper()
	scope := &pendingPollingScope{pendingScope: fixture.scope, fixture: fixture, batchSize: batchSize, completed: make(chan pendingPollingState, 2)}
	scope.observe()
	config := fixture.config(projectionworker.Complete)
	config.Scope = scope
	worker, err := projectionworker.NewPending(config)
	if err != nil {
		test.Fatal(err)
	}
	return worker, scope
}

func TestPendingConstructorRequiresExplicitProfileAndDependencies(test *testing.T) {
	for _, profile := range []projectionworker.Profile{"", "unknown", projectionworker.Compatibility, projectionworker.Complete} {
		test.Run(string(profile), func(test *testing.T) {
			fixture := newPendingFixture(test)
			worker, err := projectionworker.NewPending(fixture.config(profile))
			valid := profile == projectionworker.Compatibility || profile == projectionworker.Complete
			if valid && (worker == nil || err != nil) || !valid && (worker != nil || !errors.Is(err, domain.ErrValidation)) {
				test.Fatalf("profile selection: worker=%v err=%v", worker, err)
			}
			if len(fixture.scope.events) != 0 {
				test.Fatal("construction performed work")
			}
		})
	}
	for _, profile := range []projectionworker.Profile{projectionworker.Compatibility, projectionworker.Complete} {
		for _, dependency := range []string{"scope", "jobs", "source"} {
			for _, typedNil := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%s/typed-nil=%t", profile, dependency, typedNil), func(test *testing.T) {
					fixture := newPendingFixture(test)
					config := fixture.config(profile)
					switch dependency {
					case "scope":
						config.Scope = nil
						if typedNil {
							config.Scope = (*pendingScope)(nil)
						}
					case "jobs":
						config.Jobs = nil
						if typedNil {
							config.Jobs = (*pendingStore)(nil)
						}
					case "source":
						config.Source = nil
						if typedNil {
							config.Source = (*pendingSource)(nil)
						}
					}
					worker, err := projectionworker.NewPending(config)
					rejected := profile == projectionworker.Complete || dependency == "scope" || typedNil
					if rejected && (worker != nil || !errors.Is(err, domain.ErrValidation)) || !rejected && (worker == nil || err != nil) {
						test.Fatalf("dependency validation: rejected=%t worker=%v err=%v", rejected, worker, err)
					}
				})
			}
		}
	}
}

func TestPendingCompatibilityPreservesValidationAndMissingPorts(test *testing.T) {
	for _, jobsPresent := range []bool{false, true} {
		for _, sourcePresent := range []bool{false, true} {
			test.Run(fmt.Sprintf("jobs=%t/source=%t", jobsPresent, sourcePresent), func(test *testing.T) {
				fixture := newPendingFixture(test)
				config := fixture.config(projectionworker.Compatibility)
				if !jobsPresent {
					config.Jobs = nil
				}
				if !sourcePresent {
					config.Source = nil
				}
				worker, err := projectionworker.NewPending(config)
				if err != nil {
					test.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				for _, limit := range []int{-1, 0, 129} {
					if err := worker.Process(ctx, limit); !errors.Is(err, domain.ErrValidation) {
						test.Fatalf("limit validation did not precede port/cancellation checks: %d %v", limit, err)
					}
				}
				if fixture.scope.workerCalls != 0 || fixture.scope.workCalls != 0 || fixture.claims != 0 {
					test.Fatal("invalid limits reached the scope or store")
				}
				var want error
				if jobsPresent {
					want = domain.ErrCatalogUnsupported
					if sourcePresent {
						want = context.Canceled
					}
				}
				if err := worker.Process(ctx, 1); !errors.Is(err, want) {
					test.Fatalf("compatibility port precedence changed: got=%v want=%v", err, want)
				}
				if (!jobsPresent || !sourcePresent) && len(fixture.scope.events) != 0 {
					test.Fatal("missing optional ports reached worker scope")
				}
			})
		}
	}
}

func TestPendingProcessAdmitsClaimAndRunTogether(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newPendingFixture(test)
		ctx := context.WithValue(context.Background(), pendingCallerKey{}, "caller")
		var claimContext context.Context
		claim := fixture.storage.claim
		fixture.storage.claim = func(ctx context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
			claimContext = ctx
			return claim(ctx, lease)
		}
		check := fixture.scope.check
		fixture.scope.check = func(ctx context.Context, repo domain.ContentHash) error {
			if !fixture.scope.active || ctx.Value(pendingWorkerKey{}) != fixture.scope || ctx.Value(pendingAdmissionKey{}) != fixture.scope || ctx.Value(pendingCallerKey{}) != "caller" {
				test.Fatal("run escaped the claim admission or worker context")
			}
			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) != 30*time.Second || !errors.Is(claimContext.Err(), context.Canceled) {
				test.Fatal("claim was not released before a fresh work deadline")
			}
			return check(ctx, repo)
		}
		if err := fixture.worker(test).Process(ctx, 1); err != nil {
			test.Fatal(err)
		}
		want := []string{"worker", "admit", "claim", "check", "source", "save", "release"}
		if !reflect.DeepEqual(fixture.scope.events, want) || fixture.scope.active || fixture.loads != 0 || fixture.publishes != 0 || len(fixture.failures) != 0 {
			test.Fatalf("claim/capture ordering changed: %v", fixture.scope.events)
		}
	})
}

func TestPendingProcessPreservesLimitAndDisabledManualWork(test *testing.T) {
	for _, limit := range []int{1, 128} {
		test.Run(fmt.Sprint(limit), func(test *testing.T) {
			fixture := newPendingFixture(test)
			config := fixture.config(projectionworker.Complete)
			config.Enabled = false
			worker, err := projectionworker.NewPending(config)
			if err != nil {
				test.Fatal(err)
			}
			if err := worker.Process(context.Background(), limit); err != nil {
				test.Fatal(err)
			}
			if fixture.claims != limit || fixture.saves != limit || fixture.scope.workCalls != limit || fixture.scope.checkCalls != limit || fixture.scope.workerCalls != 1 || fixture.scope.active {
				test.Fatalf("manual work limit or admission changed: claims=%d saves=%d scope=%+v", fixture.claims, fixture.saves, fixture.scope)
			}
		})
	}
}

func TestPendingProcessOnlySwallowsNotFoundBeforeClaim(test *testing.T) {
	for _, stage := range []string{"admission", "claim", "identity", "source", "save"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newPendingFixture(test)
			missing := fmt.Errorf("%s: %w", stage, domain.ErrNotFound)
			switch stage {
			case "admission":
				fixture.scope.admit = func(context.Context) error { return missing }
			case "claim":
				fixture.storage.claim = func(context.Context, time.Duration) (domain.PendingProjectionLease, error) {
					return domain.PendingProjectionLease{}, missing
				}
			case "identity":
				fixture.scope.check = func(context.Context, domain.ContentHash) error { return missing }
			case "source":
				fixture.source.read = func(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
					return domain.RepositoryViewSourcePage{}, missing
				}
			case "save":
				fixture.storage.save = func(context.Context, domain.PendingProjectionLease, domain.RepositoryViewSourcePage) error {
					return missing
				}
			}
			err := fixture.worker(test).Process(context.Background(), 3)
			if stage == "admission" || stage == "claim" {
				if err != nil || fixture.scope.checkCalls != 0 || fixture.sourceReads != 0 || len(fixture.failures) != 0 || fixture.scope.workCalls != 1 {
					test.Fatalf("empty claim path performed work: err=%v events=%v", err, fixture.scope.events)
				}
			} else if !errors.Is(err, missing) || fixture.claims != 1 || len(fixture.failures) != 1 || fixture.failures[0].reason != "invalid_source" || fixture.failures[0].retry {
				test.Fatalf("claimed NotFound was swallowed or retried: err=%v failures=%+v", err, fixture.failures)
			}
			if fixture.scope.active {
				test.Fatal("error retained admission")
			}
		})
	}
}

func TestPendingProcessPropagatesAdmissionAndClaimErrors(test *testing.T) {
	for _, stage := range []string{"admission", "claim"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newPendingFixture(test)
			failure := errors.New("capacity or claim failure")
			if stage == "admission" {
				fixture.scope.admit = func(context.Context) error { return failure }
			} else {
				fixture.storage.claim = func(context.Context, time.Duration) (domain.PendingProjectionLease, error) {
					fixture.claims++
					return domain.PendingProjectionLease{}, failure
				}
			}
			if err := fixture.worker(test).Process(context.Background(), 2); !errors.Is(err, failure) || fixture.scope.checkCalls != 0 || len(fixture.failures) != 0 || fixture.scope.workCalls != 1 || fixture.scope.active {
				test.Fatalf("unclaimed error changed: err=%v events=%v", err, fixture.scope.events)
			}
			if stage == "admission" && fixture.claims != 0 || stage == "claim" && fixture.claims != 1 {
				test.Fatal("claim ran outside successful admission or after an error")
			}
		})
	}
}

func TestPendingRunLeaseProjectsAndPublishesWithoutClaiming(test *testing.T) {
	fixture := newPendingFixture(test)
	fixture.job.Phase = "project"
	before := pendingJSON(test, fixture.entries)
	if err := fixture.worker(test).RunLease(context.Background(), fixture.job); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(fixture.scope.events, []string{"check", "load", "publish"}) || fixture.claims != 0 || fixture.sourceReads != 0 || fixture.saves != 0 || fixture.publishes != 1 || len(fixture.failures) != 0 {
		test.Fatalf("project phase changed its scope: %v", fixture.scope.events)
	}
	if string(before) != string(pendingJSON(test, fixture.entries)) {
		test.Fatal("project phase mutated captured inputs")
	}
}

func TestPendingRunLeasePreservesFailureClassifications(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		cause  error
		reason string
		retry  bool
	}{
		{name: "temporary", cause: errors.New("temporary"), reason: "temporary_failure", retry: true},
		{name: "reset", cause: domain.ErrCatalogResetRequired, reason: "source_reset", retry: true},
		{name: "reference conflict", cause: domain.ErrRefConflict, reason: "lease_lost", retry: true},
		{name: "conflict", cause: domain.ErrConflict, reason: "lease_lost", retry: true},
		{name: "limit", cause: domain.ErrRepositoryViewSourceLimit, reason: "resource_limit"},
		{name: "integrity", cause: domain.ErrIntegrity, reason: "invalid_source"},
		{name: "validation", cause: domain.ErrValidation, reason: "invalid_source"},
		{name: "missing", cause: domain.ErrNotFound, reason: "invalid_source"},
		{name: "forbidden", cause: domain.ErrForbidden, reason: "access_denied"},
		{name: "identity", cause: domain.ErrDocumentIdentityUpgradeRequired, reason: "identity_upgrade_required", retry: true},
		{name: "local cancellation", cause: context.Canceled, reason: "temporary_failure", retry: true},
		{name: "local deadline", cause: context.DeadlineExceeded, reason: "temporary_failure", retry: true},
		{name: "classification precedence", cause: errors.Join(domain.ErrForbidden, domain.ErrCatalogResetRequired), reason: "source_reset", retry: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newPendingFixture(test)
				cause := fmt.Errorf("source: %w", scenario.cause)
				finishError := errors.New("failure persistence failed")
				fixture.source.read = func(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
					return domain.RepositoryViewSourcePage{}, cause
				}
				fail := fixture.storage.fail
				fixture.storage.fail = func(ctx context.Context, job domain.PendingProjectionLease, reason string, retry bool) error {
					deadline, bounded := ctx.Deadline()
					if !bounded || time.Until(deadline) != 5*time.Second || ctx.Value(pendingCallerKey{}) != "caller" || ctx.Err() != nil {
						test.Fatal("failure context lost values, liveness, or its five-second budget")
					}
					if err := fail(ctx, job, reason, retry); err != nil {
						test.Fatal(err)
					}
					return finishError
				}
				ctx := context.WithValue(context.Background(), pendingCallerKey{}, "caller")
				err := fixture.worker(test).RunLease(ctx, fixture.job)
				if !errors.Is(err, cause) || !errors.Is(err, finishError) || len(fixture.failures) != 1 || fixture.saves != 0 || fixture.publishes != 0 {
					test.Fatalf("failure causes were lost or work continued: %v", err)
				}
				failure := fixture.failures[0]
				if failure.reason != scenario.reason || failure.retry != scenario.retry || !errors.Is(failure.ctx.Err(), context.Canceled) {
					test.Fatalf("failure classification or cleanup changed: %+v", failure)
				}
			})
		})
	}
}

func TestPendingRunLeaseStopsAtEachFailedPhase(test *testing.T) {
	for _, stage := range []string{"identity", "save", "load", "project", "publish", "invalid_phase"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newPendingFixture(test)
			cause := domain.ErrIntegrity
			want := []string{"check", "fail"}
			switch stage {
			case "identity":
				cause = domain.ErrDocumentIdentityUpgradeRequired
				fixture.scope.check = func(context.Context, domain.ContentHash) error { return cause }
			case "save":
				save := fixture.storage.save
				fixture.storage.save = func(ctx context.Context, job domain.PendingProjectionLease, page domain.RepositoryViewSourcePage) error {
					if err := save(ctx, job, page); err != nil {
						test.Fatal(err)
					}
					return cause
				}
				want = []string{"check", "source", "save", "fail"}
			case "load":
				fixture.job.Phase = "project"
				fixture.storage.load = func(context.Context, domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
					fixture.scope.events = append(fixture.scope.events, "load")
					return nil, cause
				}
				want = []string{"check", "load", "fail"}
			case "project":
				fixture.job.Phase = "project"
				fixture.entries = nil
				want = []string{"check", "load", "fail"}
			case "publish":
				fixture.job.Phase = "project"
				publish := fixture.storage.publish
				fixture.storage.publish = func(ctx context.Context, job domain.PendingProjectionLease, parts []domain.PendingProjectionPart) error {
					if err := publish(ctx, job, parts); err != nil {
						test.Fatal(err)
					}
					return cause
				}
				want = []string{"check", "load", "publish", "fail"}
			case "invalid_phase":
				fixture.job.Phase = "invalid_phase"
			}
			if err := fixture.worker(test).RunLease(context.Background(), fixture.job); !errors.Is(err, cause) || len(fixture.failures) != 1 || !reflect.DeepEqual(fixture.scope.events, want) {
				test.Fatalf("failed phase continued or bypassed failure: err=%v events=%v", err, fixture.scope.events)
			}
		})
	}
}

func TestPendingProcessPreservesIncrementalCaptureAndProjectPhases(test *testing.T) {
	fixture := newPendingFixture(test)
	initialEntries := append([]domain.RepositoryViewSourceEntry(nil), fixture.entries...)
	initialParts := fixture.parts
	jobs := make([]domain.PendingProjectionLease, 5)
	for index := range jobs {
		jobs[index] = fixture.job
		jobs[index].Fence += int64(index)
	}
	jobs[0].AppliedSequence = -1
	jobs[1].AppliedSequence, jobs[1].Cursor = -1, "bootstrap-continuation"
	jobs[2].Phase = "project"
	jobs[3].AppliedSequence, jobs[3].InputSequence = 5, 6
	jobs[4].Phase, jobs[4].AppliedSequence, jobs[4].InputSequence = "project", 5, 6
	pages := []domain.RepositoryViewSourcePage{fixture.page, fixture.page, fixture.page}
	pages[0].Entries, pages[0].NextCursor, pages[0].Checkpoint = initialEntries[:3], jobs[1].Cursor, nil
	pages[1].Entries = initialEntries[3:]
	pages[2].Mode, pages[2].Through = "delta", 6
	pages[2].Entries = []domain.RepositoryViewSourceEntry{{Sequence: 6, Kind: "pending", Key: "live", Deleted: true}}
	checkpoint := *fixture.page.Checkpoint
	checkpoint.Sequence = 6
	pages[2].Checkpoint = &checkpoint
	requests := []domain.RepositoryViewSourceRequest{
		{Version: 1},
		{Version: 1, Cursor: jobs[1].Cursor},
		{Version: 1, After: fixture.page.Checkpoint},
	}
	before := pendingJSON(test, pages)
	claim := fixture.storage.claim
	fixture.storage.claim = func(ctx context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
		if fixture.claims >= len(jobs) {
			test.Fatal("worker exceeded its phase limit")
		}
		fixture.job = jobs[fixture.claims]
		return claim(ctx, lease)
	}
	fixture.source.read = func(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
		index := fixture.sourceReads
		if index >= len(requests) || !reflect.DeepEqual(request, requests[index]) || repo != fixture.job.RepoID || fixture.scope.reading || !fixture.scope.active {
			test.Fatalf("incremental source boundary changed: request=%+v events=%v", request, fixture.scope.events)
		}
		fixture.page = pages[index]
		fixture.sourceReads++
		fixture.scope.events = append(fixture.scope.events, "source")
		return fixture.page, ctx.Err()
	}
	load := fixture.storage.load
	fixture.storage.load = func(ctx context.Context, job domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
		if fixture.loads == 0 {
			if fixture.saves != 2 || job != jobs[2] {
				test.Fatal("project started before bootstrap pages were captured")
			}
			fixture.entries, fixture.parts = initialEntries, initialParts
		} else {
			if fixture.saves != 3 || job != jobs[4] {
				test.Fatal("incremental project started before its delta was captured")
			}
			fixture.entries = initialEntries[:4]
			view, err := domain.ProjectPendingSource(ctx, job.RepoID, fixture.entries)
			if err != nil {
				test.Fatal(err)
			}
			fixture.parts, err = domain.EncodePendingProjection(ctx, job.RepoID, view)
			if err != nil {
				test.Fatal(err)
			}
		}
		return load(ctx, job)
	}
	if err := fixture.worker(test).Process(context.Background(), len(jobs)); err != nil {
		test.Fatal(err)
	}
	want := []string{"worker",
		"admit", "claim", "check", "source", "save", "release",
		"admit", "claim", "check", "source", "save", "release",
		"admit", "claim", "check", "load", "publish", "release",
		"admit", "claim", "check", "source", "save", "release",
		"admit", "claim", "check", "load", "publish", "release"}
	if !reflect.DeepEqual(fixture.scope.events, want) || fixture.publishes != 2 || fixture.scope.checkCalls != len(jobs) || len(fixture.failures) != 0 || string(before) != string(pendingJSON(test, pages)) {
		test.Fatalf("incremental phases, fencing, or immutable source pages changed: %v", fixture.scope.events)
	}
	if reflect.DeepEqual(initialParts, fixture.parts) {
		test.Fatal("deleted pending source was republished as stale state")
	}
}

func TestPendingProcessClaimDeadlineStartsAfterAdmission(test *testing.T) {
	for _, scenario := range []struct {
		name          string
		parentTimeout time.Duration
		queued        time.Duration
		budget        time.Duration
	}{
		{name: "background parent", budget: 30 * time.Second},
		{name: "earlier parent deadline", parentTimeout: 5 * time.Second, budget: 5 * time.Second},
		{name: "after admission", queued: 20 * time.Second, budget: 30 * time.Second},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				fixture := newPendingFixture(test)
				ctx := context.Background()
				if scenario.parentTimeout != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, scenario.parentTimeout)
					defer cancel()
				}
				fixture.scope.admit = func(context.Context) error {
					time.Sleep(scenario.queued)
					if fixture.claims != 0 {
						test.Fatal("claim ran before admission")
					}
					return nil
				}
				var claimContext context.Context
				fixture.storage.claim = func(claim context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
					claimContext = claim
					fixture.claims++
					deadline, bounded := claim.Deadline()
					if !fixture.scope.active || !bounded || time.Until(deadline) != scenario.budget || lease != time.Minute {
						test.Fatal("claim deadline, lease, or admission changed")
					}
					<-claim.Done()
					return domain.PendingProjectionLease{}, claim.Err()
				}
				started := time.Now()
				err := fixture.worker(test).Process(ctx, 1)
				if !errors.Is(err, context.DeadlineExceeded) || fixture.claims != 1 || fixture.scope.active || fixture.scope.checkCalls != 0 || len(fixture.failures) != 0 || time.Since(started) != scenario.queued+scenario.budget {
					test.Fatalf("claim timeout swallowed or retained admission: err=%v elapsed=%v", err, time.Since(started))
				}
				if !errors.Is(claimContext.Err(), context.DeadlineExceeded) || scenario.parentTimeout == 0 && ctx.Err() != nil {
					test.Fatal("claim cancellation escaped its scope")
				}
			})
		})
	}
}

func TestPendingProcessClaimAndWorkBudgetsAreIndependent(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newPendingFixture(test)
		fixture.scope.admit = func(context.Context) error {
			time.Sleep(7 * time.Second)
			return nil
		}
		claim := fixture.storage.claim
		var claimContext context.Context
		fixture.storage.claim = func(ctx context.Context, lease time.Duration) (domain.PendingProjectionLease, error) {
			claimContext = ctx
			time.Sleep(20 * time.Second)
			return claim(ctx, lease)
		}
		fixture.source.read = func(ctx context.Context, _ domain.ContentHash, _ domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
			deadline, bounded := ctx.Deadline()
			if !bounded || time.Until(deadline) != 30*time.Second || !errors.Is(claimContext.Err(), context.Canceled) || !fixture.scope.active {
				test.Fatal("work reused the claim deadline or escaped admission")
			}
			<-ctx.Done()
			return domain.RepositoryViewSourcePage{}, ctx.Err()
		}
		ctx := context.Background()
		started := time.Now()
		if err := fixture.worker(test).Process(ctx, 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 57*time.Second || ctx.Err() != nil || fixture.scope.active || len(fixture.failures) != 1 || fixture.failures[0].reason != "temporary_failure" || !fixture.failures[0].retry {
			test.Fatalf("claim and work budgets merged or failure lost: err=%v elapsed=%v", err, time.Since(started))
		}
	})
}

func TestPendingRunLeaseWorkTimeoutAndParentDeadline(test *testing.T) {
	for _, stage := range []string{"identity", "capture", "project"} {
		for _, parentTimeout := range []time.Duration{0, 5 * time.Second} {
			test.Run(fmt.Sprintf("%s/parent=%v", stage, parentTimeout), func(test *testing.T) {
				synctest.Test(test, func(test *testing.T) {
					fixture := newPendingFixture(test)
					ctx := context.Background()
					budget := 30 * time.Second
					if parentTimeout != 0 {
						var cancel context.CancelFunc
						ctx, cancel = context.WithTimeout(ctx, parentTimeout)
						defer cancel()
						budget = parentTimeout
					}
					block := func(work context.Context) error {
						deadline, bounded := work.Deadline()
						if !bounded || time.Until(deadline) != budget {
							test.Fatal("work deadline was removed or extended")
						}
						<-work.Done()
						return work.Err()
					}
					switch stage {
					case "identity":
						fixture.scope.check = func(work context.Context, _ domain.ContentHash) error { return block(work) }
					case "capture":
						fixture.source.read = func(work context.Context, _ domain.ContentHash, _ domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
							return domain.RepositoryViewSourcePage{}, block(work)
						}
					case "project":
						fixture.job.Phase = "project"
						fixture.storage.load = func(work context.Context, _ domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
							return nil, block(work)
						}
					}
					started := time.Now()
					err := fixture.worker(test).RunLease(ctx, fixture.job)
					if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != budget || fixture.saves != 0 || fixture.publishes != 0 {
						test.Fatalf("work did not respect its deadline: err=%v elapsed=%v", err, time.Since(started))
					}
					if parentTimeout == 0 {
						if len(fixture.failures) != 1 || fixture.failures[0].reason != "temporary_failure" || !fixture.failures[0].retry || ctx.Err() != nil {
							test.Fatal("local work timeout lost its retry or canceled its parent")
						}
					} else if len(fixture.failures) != 0 {
						test.Fatal("parent timeout rewrote the lease failure state")
					}
				})
			})
		}
	}
}

func TestPendingFailureDetachesCancellationAndKeepsAdmission(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newPendingFixture(test)
		ctx := context.WithValue(context.Background(), pendingCallerKey{}, "caller")
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		fixture.source.read = func(context.Context, domain.ContentHash, domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
			return domain.RepositoryViewSourcePage{}, domain.ErrConflict
		}
		fail := fixture.storage.fail
		fixture.storage.fail = func(finish context.Context, job domain.PendingProjectionLease, reason string, retry bool) error {
			deadline, bounded := finish.Deadline()
			if !bounded || time.Until(deadline) != 5*time.Second || finish.Value(pendingCallerKey{}) != "caller" || !fixture.scope.active {
				test.Fatal("failure inherited parent deadline or lost admission/context values")
			}
			if err := fail(finish, job, reason, retry); err != nil {
				test.Fatal(err)
			}
			cancel()
			if finish.Err() != nil {
				test.Fatal("failure context inherited parent cancellation")
			}
			<-finish.Done()
			if !fixture.scope.active {
				test.Fatal("admission released before failure persistence finished")
			}
			return finish.Err()
		}
		started := time.Now()
		err := fixture.worker(test).Process(ctx, 2)
		if !errors.Is(err, domain.ErrConflict) || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 5*time.Second || fixture.scope.active || fixture.claims != 1 || len(fixture.failures) != 1 || fixture.failures[0].reason != "lease_lost" || !fixture.failures[0].retry {
			test.Fatalf("detached failure budget, classification, or release changed: %v", err)
		}
	})
}

func TestPendingRunLeaseParentCancellationDoesNotRecordFailure(test *testing.T) {
	for _, stage := range []string{"identity", "capture", "project", "domain", "save", "publish", "successful save"} {
		test.Run(stage, func(test *testing.T) {
			fixture := newPendingFixture(test)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch stage {
			case "identity":
				fixture.scope.check = func(work context.Context, _ domain.ContentHash) error {
					cancel()
					return work.Err()
				}
			case "capture":
				fixture.source.read = func(work context.Context, _ domain.ContentHash, _ domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
					cancel()
					return domain.RepositoryViewSourcePage{}, work.Err()
				}
			case "project", "domain":
				fixture.job.Phase = "project"
				fixture.storage.load = func(work context.Context, _ domain.PendingProjectionLease) ([]domain.RepositoryViewSourceEntry, error) {
					cancel()
					if stage == "domain" {
						return fixture.entries, nil
					}
					return nil, work.Err()
				}
			case "save", "successful save":
				fixture.storage.save = func(work context.Context, _ domain.PendingProjectionLease, _ domain.RepositoryViewSourcePage) error {
					cancel()
					if stage == "successful save" {
						return nil
					}
					return work.Err()
				}
			case "publish":
				fixture.job.Phase = "project"
				fixture.storage.publish = func(work context.Context, _ domain.PendingProjectionLease, _ []domain.PendingProjectionPart) error {
					cancel()
					return work.Err()
				}
			}
			err := fixture.worker(test).RunLease(ctx, fixture.job)
			if len(fixture.failures) != 0 || stage == "successful save" && err != nil || stage != "successful save" && !errors.Is(err, context.Canceled) {
				test.Fatalf("parent cancellation changed completion/failure semantics: err=%v failures=%+v", err, fixture.failures)
			}
			if stage == "identity" && (fixture.sourceReads != 0 || fixture.loads != 0) || (stage == "project" || stage == "domain") && fixture.publishes != 0 {
				test.Fatal("canceled work continued into the next phase")
			}
		})
	}
}

func TestPendingRunDisabledIsNoOp(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newPendingFixture(test)
		config := fixture.config(projectionworker.Complete)
		config.Enabled = false
		worker, err := projectionworker.NewPending(config)
		if err != nil {
			test.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { worker.Run(ctx); close(done) }()
		synctest.Wait()
		select {
		case <-done:
		default:
			cancel()
			<-done
			test.Fatal("disabled worker did not return immediately")
		}
		if len(fixture.scope.events) != 0 || fixture.claims != 0 {
			test.Fatal("disabled Run reached worker context or storage")
		}
	})
}

func TestPendingRunUsesBatchesOfSixteenAndOneSecondPolling(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newPendingFixture(test)
		worker, scope := newPendingPollingWorker(test, fixture, 16)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		started := time.Now()
		go func() { worker.Run(ctx); close(done) }()
		for batch := 1; batch <= 2; batch++ {
			completed := <-scope.completed
			synctest.Wait()
			state := *scope.state.Load()
			if completed != state || state.claims != 16*batch || state.saves != 16*batch || state.workCalls != 16*batch || state.checkCalls != 16*batch || state.workerCalls != batch || state.active || state.reading || state.failures != 0 || time.Since(started) != time.Duration(batch-1)*time.Second {
				test.Fatalf("Run batch size, polling time, or admission release changed: batch=%d state=%+v elapsed=%v", batch, state, time.Since(started))
			}
			time.Sleep(999 * time.Millisecond)
			synctest.Wait()
			if beforeTick := *scope.state.Load(); beforeTick != state || beforeTick.active || beforeTick.reading {
				test.Fatalf("Run polled before one second or retained resources: %+v", beforeTick)
			}
			if batch == 1 {
				time.Sleep(time.Millisecond)
			}
		}
		cancel()
		<-done
		if fixture.claims != 32 || fixture.saves != 32 || fixture.scope.workCalls != 32 || fixture.scope.checkCalls != 32 || fixture.scope.workerCalls != 2 || fixture.scope.active || fixture.scope.reading || len(fixture.failures) != 0 {
			test.Fatal("Run continued claiming after cancellation or retained admission")
		}
	})
}

func TestPendingRunContinuesAfterProcessError(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		fixture := newPendingFixture(test)
		fixture.storage.claim = func(context.Context, time.Duration) (domain.PendingProjectionLease, error) {
			fixture.claims++
			return domain.PendingProjectionLease{}, errors.New("temporary claim failure")
		}
		worker, scope := newPendingPollingWorker(test, fixture, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		started := time.Now()
		go func() { worker.Run(ctx); close(done) }()
		completed := <-scope.completed
		synctest.Wait()
		state := *scope.state.Load()
		if completed != state || state.claims != 1 || state.workCalls != 1 || state.workerCalls != 1 || state.active || state.reading || state.failures != 0 || time.Since(started) != 0 {
			test.Fatal("Run did not release failed admission before polling")
		}
		time.Sleep(999 * time.Millisecond)
		synctest.Wait()
		if beforeTick := *scope.state.Load(); beforeTick != state || beforeTick.active || beforeTick.reading {
			test.Fatalf("Run retried before one second or retained resources: %+v", beforeTick)
		}
		time.Sleep(time.Millisecond)
		completed = <-scope.completed
		synctest.Wait()
		state = *scope.state.Load()
		if completed != state || state.claims != 2 || state.workCalls != 2 || state.workerCalls != 2 || state.active || state.reading || state.failures != 0 || time.Since(started) != time.Second {
			test.Fatal("Run stopped on error or changed its retry interval")
		}
		cancel()
		<-done
		if fixture.claims != 2 || fixture.scope.workCalls != 2 || fixture.scope.workerCalls != 2 || fixture.scope.active || fixture.scope.reading || len(fixture.failures) != 0 {
			test.Fatal("unclaimed Run error recorded a lease failure or cancellation retained work")
		}
	})
}
