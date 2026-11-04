package projectionworker_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type qualificationWorkerKey struct{}
type qualificationWorkKey struct{}
type qualificationReadKey struct{}
type qualificationCallerKey struct{}

type qualificationScope struct {
	workerCalls   int
	workCalls     int
	readCalls     int
	startCalls    int
	identityCalls int
	active        bool
	startErr      error
	workErr       error
	readExitErr   error
	identityErr   error
	events        []string
	activeWork    atomic.Int64
	workComplete  func()
}

func (scope *qualificationScope) WorkerContext(ctx context.Context) context.Context {
	scope.workerCalls++
	return context.WithValue(ctx, qualificationWorkerKey{}, scope)
}

func (scope *qualificationScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	scope.workCalls++
	if scope.workErr != nil {
		return scope.workErr
	}
	scope.activeWork.Add(1)
	defer func() {
		scope.activeWork.Add(-1)
		if scope.workComplete != nil {
			scope.workComplete()
		}
	}()
	return work(context.WithValue(ctx, qualificationWorkKey{}, scope))
}

func (scope *qualificationScope) CheckRepository(context.Context, domain.ContentHash) error {
	return domain.ErrForbidden
}

func (scope *qualificationScope) WithinRead(ctx context.Context, read func(context.Context) error) error {
	scope.readCalls++
	scope.events = append(scope.events, "read-open")
	scope.active = true
	defer func() {
		scope.active = false
		scope.events = append(scope.events, "read-close")
	}()
	if err := read(context.WithValue(ctx, qualificationReadKey{}, scope)); err != nil {
		return err
	}
	return scope.readExitErr
}

func (scope *qualificationScope) ValidateQualificationStart(context.Context) error {
	scope.startCalls++
	return scope.startErr
}

func (scope *qualificationScope) InReadOnlyTransaction(ctx context.Context) bool {
	return ctx.Value(qualificationReadKey{}) == scope
}

func (scope *qualificationScope) CheckIdentity(ctx context.Context, repo domain.ContentHash) error {
	scope.identityCalls++
	if !scope.InReadOnlyTransaction(ctx) || domain.ValidateContentHash(repo) != nil {
		return domain.ErrConflict
	}
	return scope.identityErr
}

type qualificationStore struct {
	outbound.GraphQualificationStore
	outbound.GraphProjectionStore
	test             *testing.T
	scope            *qualificationScope
	lease            domain.GraphQualificationLease
	source           domain.RepositoryViewSourceCheckpoint
	projection       domain.GraphProjectionRead
	expected         domain.ContentHash
	claimed          []domain.ContentHash
	finished         []domain.GraphQualificationLease
	failed           []domain.GraphQualificationLease
	results          []domain.GraphQualificationResult
	reasons          []string
	repaired         []domain.GraphProjectionCorruption
	sourceReads      int
	projectionReads  int
	referenceReads   int
	budgetChecks     int
	claimErr         error
	sourceErr        error
	projectionErr    error
	referenceErr     error
	budgetErr        error
	finishErr        error
	failErr          error
	repairErr        error
	claim            func(context.Context, domain.ContentHash) (domain.GraphQualificationLease, error)
	duringProjection func(context.Context)
	reference        func(context.Context) (domain.ContentHash, error)
	finish           func(context.Context) error
	failureContext   context.Context
}

func (storage *qualificationStore) checkRead(ctx context.Context) {
	storage.test.Helper()
	if !storage.scope.active || !storage.scope.InReadOnlyTransaction(ctx) {
		storage.test.Fatal("comparison escaped its pinned read")
	}
	assertQualificationDeadline(storage.test, ctx, 30*time.Second)
}

func (storage *qualificationStore) checkWrite(ctx context.Context) {
	storage.test.Helper()
	if storage.scope.active || storage.scope.InReadOnlyTransaction(ctx) {
		storage.test.Fatal("qualification persistence retained its read snapshot")
	}
	if ctx.Value(qualificationWorkKey{}) != storage.scope || ctx.Value(qualificationWorkerKey{}) != storage.scope {
		storage.test.Fatal("qualification persistence escaped worker admission")
	}
	assertQualificationDeadline(storage.test, ctx, 5*time.Second)
}

func (storage *qualificationStore) ClaimGraphQualification(ctx context.Context, repo domain.ContentHash, duration time.Duration) (domain.GraphQualificationLease, error) {
	storage.checkWrite(ctx)
	if duration != time.Minute {
		storage.test.Fatal("qualification lease duration changed", duration)
	}
	storage.claimed = append(storage.claimed, repo)
	storage.scope.events = append(storage.scope.events, "claim")
	if storage.claim != nil {
		return storage.claim(ctx, repo)
	}
	return storage.lease, storage.claimErr
}

func (storage *qualificationStore) FinishGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, result domain.GraphQualificationResult) error {
	storage.checkWrite(ctx)
	storage.finished = append(storage.finished, lease)
	storage.results = append(storage.results, result)
	storage.scope.events = append(storage.scope.events, "finish")
	if storage.finish != nil {
		return storage.finish(ctx)
	}
	return storage.finishErr
}

func (storage *qualificationStore) FailGraphQualification(ctx context.Context, lease domain.GraphQualificationLease, reason string) error {
	storage.checkWrite(ctx)
	storage.failed = append(storage.failed, lease)
	storage.reasons = append(storage.reasons, reason)
	storage.failureContext = ctx
	storage.scope.events = append(storage.scope.events, "fail")
	return storage.failErr
}

func (storage *qualificationStore) CheckGraphQualificationInputs(ctx context.Context, repo domain.ContentHash) error {
	storage.checkRead(ctx)
	storage.budgetChecks++
	if repo != storage.lease.RepoID {
		storage.test.Fatal("reference budget checked another repository", repo)
	}
	return storage.budgetErr
}

func (storage *qualificationStore) ReadRepositoryGraphSourceGeneration(ctx context.Context, repo domain.ContentHash) (domain.RepositoryViewSourceCheckpoint, error) {
	storage.checkRead(ctx)
	storage.sourceReads++
	if repo != storage.lease.RepoID {
		storage.test.Fatal("generation read lost repository identity", repo)
	}
	return storage.source, storage.sourceErr
}

func (storage *qualificationStore) ReadGraphProjection(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	storage.checkRead(ctx)
	storage.projectionReads++
	if repo != storage.lease.RepoID {
		storage.test.Fatal("candidate read lost repository identity", repo)
	}
	if storage.duringProjection != nil {
		storage.duringProjection(ctx)
	}
	return storage.projection, storage.projectionErr
}

func (storage *qualificationStore) Reference(ctx context.Context, repo domain.ContentHash) (domain.ContentHash, error) {
	storage.checkRead(ctx)
	storage.referenceReads++
	if err := storage.CheckGraphQualificationInputs(ctx, repo); err != nil {
		return "", err
	}
	if err := storage.scope.CheckIdentity(ctx, repo); err != nil {
		return "", err
	}
	if storage.reference != nil {
		return storage.reference(ctx)
	}
	return storage.expected, storage.referenceErr
}

func (storage *qualificationStore) RequestGraphProjectionRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	storage.checkWrite(ctx)
	if ctx != storage.failureContext {
		storage.test.Fatal("repair did not share the bounded failure context")
	}
	storage.repaired = append(storage.repaired, failure)
	storage.scope.events = append(storage.scope.events, "repair")
	return storage.repairErr
}

func qualificationData(test *testing.T, repo domain.ContentHash) (domain.GraphQualificationLease, domain.GraphProjectionRead, domain.ContentHash) {
	test.Helper()
	view := domain.RepositoryView{Graph: &domain.GraphState{Version: 1}, DefaultBranch: "main"}
	parts, err := domain.EncodeGraphViewProjection(context.Background(), repo, view)
	if err != nil {
		test.Fatal(err)
	}
	expected, err := domain.GraphViewProjectionManifestDigest(context.Background(), repo, parts)
	if err != nil {
		test.Fatal(err)
	}
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
		Scope: domain.RepositoryGraphSourceScope, RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 4}
	projection := domain.GraphProjectionRead{Enabled: true, Ready: true, Complete: true,
		Rules: domain.GraphViewProjectionRules, Dataset: "22222222-2222-4222-8222-222222222222",
		Source: checkpoint, Applied: checkpoint, Parts: parts}
	lease := domain.GraphQualificationLease{GraphQualificationKey: projection.QualificationKey(), Fence: 7,
		LeaseUntil: time.Now().Add(time.Minute)}
	if err := lease.Validate(); err != nil {
		test.Fatal("invalid fixture lease", err)
	}
	return lease, projection, expected
}

func qualificationFixture(test *testing.T) (projectionworker.QualificationConfig, *qualificationScope, *qualificationStore) {
	test.Helper()
	scope := &qualificationScope{}
	storage := &qualificationStore{test: test, scope: scope}
	repo := domain.HashContent([]byte(test.Name()))
	storage.lease, storage.projection, storage.expected = qualificationData(test, repo)
	storage.source = storage.projection.Source
	return projectionworker.QualificationConfig{Profile: projectionworker.Compatibility, Scope: scope,
		Jobs: storage, Projections: storage, Repairs: storage, Generations: storage, Reference: storage,
		Repositories: []domain.ContentHash{repo}}, scope, storage
}

func newQualificationOwner(test *testing.T, config projectionworker.QualificationConfig) *projectionworker.Qualification {
	test.Helper()
	owner, err := projectionworker.NewQualification(config)
	if err != nil {
		test.Fatal(err)
	}
	return owner
}

func assertQualificationDeadline(test *testing.T, ctx context.Context, maximum time.Duration) {
	test.Helper()
	deadline, bounded := ctx.Deadline()
	if remaining := time.Until(deadline); !bounded || remaining <= 0 || remaining > maximum {
		test.Fatal("qualification operation lost its deadline", deadline, maximum)
	}
}

func qualificationCandidate(lease domain.GraphQualificationLease, cause error) *domain.GraphProjectionCorruption {
	return &domain.GraphProjectionCorruption{RepoID: lease.RepoID, Dataset: lease.Dataset,
		Applied: lease.Applied, Rules: lease.Rules, Cause: cause}
}

func TestQualificationOwnerConstructorProfilesAndDependencies(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*projectionworker.QualificationConfig)
	}{
		{name: "jobs", change: func(config *projectionworker.QualificationConfig) { config.Jobs = nil }},
		{name: "projections", change: func(config *projectionworker.QualificationConfig) { config.Projections = nil }},
		{name: "repairs", change: func(config *projectionworker.QualificationConfig) { config.Repairs = nil }},
		{name: "generations", change: func(config *projectionworker.QualificationConfig) { config.Generations = nil }},
		{name: "reference", change: func(config *projectionworker.QualificationConfig) { config.Reference = nil }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			config, scope, _ := qualificationFixture(test)
			fixture.change(&config)
			newQualificationOwner(test, config)
			config.Profile = projectionworker.Complete
			if owner, err := projectionworker.NewQualification(config); owner != nil || !errors.Is(err, domain.ErrCatalogUnsupported) {
				test.Fatal("complete active worker accepted a missing dependency", owner, err)
			}
			config.Repositories = nil
			newQualificationOwner(test, config)
			if scope.startCalls != 0 || scope.workerCalls != 0 || scope.workCalls != 0 || scope.readCalls != 0 {
				test.Fatal("construction performed worker activity", scope)
			}
		})
	}
	var missingStore *qualificationStore
	for _, fixture := range []struct {
		name   string
		change func(*projectionworker.QualificationConfig)
	}{
		{name: "jobs", change: func(config *projectionworker.QualificationConfig) { config.Jobs = missingStore }},
		{name: "projections", change: func(config *projectionworker.QualificationConfig) { config.Projections = missingStore }},
		{name: "repairs", change: func(config *projectionworker.QualificationConfig) { config.Repairs = missingStore }},
		{name: "generations", change: func(config *projectionworker.QualificationConfig) { config.Generations = missingStore }},
		{name: "reference", change: func(config *projectionworker.QualificationConfig) { config.Reference = missingStore }},
	} {
		test.Run("typed-nil/"+fixture.name, func(test *testing.T) {
			config, _, _ := qualificationFixture(test)
			config.Profile = projectionworker.Complete
			fixture.change(&config)
			for _, active := range []bool{true, false} {
				if !active {
					config.Repositories = nil
				}
				if owner, err := projectionworker.NewQualification(config); owner != nil || !errors.Is(err, domain.ErrValidation) {
					test.Fatal("complete assembly accepted typed-nil dependency", active, owner, err)
				}
			}
		})
	}
	config, _, _ := qualificationFixture(test)
	config.Profile = projectionworker.Complete
	newQualificationOwner(test, config)
	for _, profile := range []projectionworker.Profile{"", "unknown"} {
		config.Profile = profile
		if owner, err := projectionworker.NewQualification(config); owner != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("unknown profile accepted", owner, err)
		}
	}
	config.Profile = projectionworker.Compatibility
	config.Repositories = []domain.ContentHash{"invalid"}
	if owner, err := projectionworker.NewQualification(config); owner != nil || !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("invalid repository accepted", owner, err)
	}
	var missingScope *qualificationScope
	for _, scope := range []projectionworker.QualificationScope{nil, missingScope} {
		config.Scope = scope
		if owner, err := projectionworker.NewQualification(config); owner != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("missing scope accepted", owner, err)
		}
	}
}

func TestQualificationOwnerProcessPreservesLimitIdleAndGuardOrder(test *testing.T) {
	scope := &qualificationScope{startErr: domain.ErrConflict}
	config := projectionworker.QualificationConfig{Profile: projectionworker.Compatibility, Scope: scope}
	owner := newQualificationOwner(test, config)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, limit := range []int{-1, 0, 129} {
		if err := owner.Process(ctx, limit); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("limit validation lost precedence", limit, err)
		}
	}
	if err := owner.Process(ctx, 128); err != nil || scope.startCalls != 0 || scope.workerCalls != 0 || scope.workCalls != 0 {
		test.Fatal("idle worker probed dependencies or cancellation", err, scope)
	}
	owner.Run(ctx)
	config.Repositories = []domain.ContentHash{domain.HashContent([]byte(test.Name()))}
	owner = newQualificationOwner(test, config)
	if err := owner.Process(ctx, 1); !errors.Is(err, domain.ErrCatalogUnsupported) || scope.startCalls != 0 {
		test.Fatal("missing jobs lost precedence over the start guard", err, scope)
	}
	config, scope, storage := qualificationFixture(test)
	scope.startErr = domain.ErrConflict
	owner = newQualificationOwner(test, config)
	if err := owner.Process(context.Background(), 1); !errors.Is(err, domain.ErrConflict) || scope.startCalls != 1 || scope.workerCalls != 0 || scope.workCalls != 0 || len(storage.claimed) != 0 {
		test.Fatal("rejected start entered worker admission", err, scope)
	}
	scope.startErr = nil
	scope.workErr = domain.ErrResourceOverloaded
	if err := owner.Process(context.Background(), 1); !errors.Is(err, domain.ErrResourceOverloaded) || scope.workerCalls != 1 || scope.workCalls != 1 || len(storage.claimed) != 0 {
		test.Fatal("admission failure reached the claim", err, scope)
	}
}

func TestQualificationOwnerSharesCursorAndCopiesRepositoryOrder(test *testing.T) {
	config, _, storage := qualificationFixture(test)
	repositories := []domain.ContentHash{domain.HashContent([]byte("third")), domain.HashContent([]byte("first")), domain.HashContent([]byte("second"))}
	config.Repositories = slices.Clone(repositories)
	var next atomic.Uint64
	next.Store(1)
	config.Next = &next
	first := newQualificationOwner(test, config)
	second := newQualificationOwner(test, config)
	config.Repositories[1] = domain.HashContent([]byte("caller mutation"))
	storage.claimErr = domain.ErrIntegrity
	for _, owner := range []*projectionworker.Qualification{first, second, first} {
		if err := owner.Process(context.Background(), 1); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal(err)
		}
	}
	if !slices.Equal(storage.claimed, []domain.ContentHash{repositories[1], repositories[2], repositories[0]}) || next.Load() != 4 {
		test.Fatal("cursor was cloned/reset or repository order was mutated", storage.claimed, next.Load())
	}
	next.Store(2)
	if err := second.Process(context.Background(), 1); !errors.Is(err, domain.ErrIntegrity) || storage.claimed[3] != repositories[2] {
		test.Fatal("owner stopped sharing the external cursor", err, storage.claimed)
	}
	config.Next = nil
	config.Repositories = repositories
	private := newQualificationOwner(test, config)
	storage.claimed = nil
	for range 4 {
		if err := private.Process(context.Background(), 1); !errors.Is(err, domain.ErrIntegrity) {
			test.Fatal(err)
		}
	}
	if !slices.Equal(storage.claimed, []domain.ContentHash{repositories[0], repositories[1], repositories[2], repositories[0]}) {
		test.Fatal("owner-local cursor did not survive Process calls", storage.claimed)
	}
}

func TestQualificationOwnerClaimsEachRepositoryAtMostOnceAndBoundsBatch(test *testing.T) {
	for _, fixture := range []struct {
		limit int
		count int
		idle  int
		want  int
	}{
		{limit: 1, count: 4, idle: 2, want: 1},
		{limit: 2, count: 4, idle: 1, want: 2},
		{limit: 128, count: 130, want: 128},
		{limit: 128, count: 2, want: 2},
		{limit: 1, count: 4, idle: 4, want: 0},
	} {
		test.Run(fmt.Sprintf("limit-%d/repos-%d/idle-%d", fixture.limit, fixture.count, fixture.idle), func(test *testing.T) {
			config, scope, storage := qualificationFixture(test)
			config.Repositories = nil
			for index := 0; index < fixture.count; index++ {
				config.Repositories = append(config.Repositories, domain.HashContent([]byte(fmt.Sprintf("repo-%d", index))))
			}
			storage.claim = func(_ context.Context, repo domain.ContentHash) (domain.GraphQualificationLease, error) {
				if len(storage.claimed) <= fixture.idle {
					return domain.GraphQualificationLease{}, domain.ErrNotFound
				}
				storage.lease, storage.projection, storage.expected = qualificationData(test, repo)
				storage.source = storage.projection.Source
				return storage.lease, nil
			}
			owner := newQualificationOwner(test, config)
			if err := owner.Process(context.Background(), fixture.limit); err != nil || len(storage.finished) != fixture.want || len(storage.failed) != 0 || scope.readCalls != fixture.want {
				test.Fatal("batch did not bound successful leases", err, len(storage.finished), len(storage.claimed))
			}
			claims := min(fixture.count, fixture.idle+fixture.want)
			if len(storage.claimed) != claims || !slices.Equal(storage.claimed, config.Repositories[:claims]) || scope.workCalls != claims {
				test.Fatal("batch revisited a repository or counted idle claims", storage.claimed, claims)
			}
		})
	}
}

func TestQualificationOwnerDistinguishesIdleForeignInvalidAndClaimedFailures(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*qualificationStore)
		want   error
		reads  int
		failed int
	}{
		{name: "idle", change: func(storage *qualificationStore) { storage.claimErr = domain.ErrNotFound }},
		{name: "foreign", change: func(storage *qualificationStore) { storage.lease.RepoID = domain.HashContent([]byte("foreign")) }, want: domain.ErrIntegrity},
		{name: "invalid-fence", change: func(storage *qualificationStore) { storage.lease.Fence = 0 }, want: domain.ErrValidation},
		{name: "invalid-reference", change: func(storage *qualificationStore) { storage.lease.ReferenceExpected = "invalid" }, want: domain.ErrValidation},
		{name: "claimed-not-found", change: func(storage *qualificationStore) { storage.sourceErr = domain.ErrNotFound }, want: domain.ErrNotFound, reads: 1, failed: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			config, scope, storage := qualificationFixture(test)
			fixture.change(storage)
			owner := newQualificationOwner(test, config)
			if err := owner.Process(context.Background(), 1); !errors.Is(err, fixture.want) || scope.readCalls != fixture.reads || len(storage.failed) != fixture.failed || len(storage.finished) != 0 || len(storage.repaired) != 0 {
				test.Fatal("claim outcome lost its error or ownership boundary", err, scope.readCalls, storage.failed)
			}
		})
	}
}

func TestQualificationOwnerComparesPinnedPayloadAndPersistsExactLease(test *testing.T) {
	for _, captured := range []bool{false, true} {
		for _, mismatch := range []bool{false, true} {
			test.Run(fmt.Sprintf("captured-%t/mismatch-%t", captured, mismatch), func(test *testing.T) {
				config, scope, storage := qualificationFixture(test)
				if mismatch {
					storage.expected = domain.HashContent([]byte("different valid reference"))
				}
				if captured {
					storage.lease.ReferenceExpected = storage.expected
					storage.source.Sequence++
					storage.projection.Source = storage.source
					storage.projection.Ready = false
					config.Reference = nil
				}
				owner := newQualificationOwner(test, config)
				if err := owner.Process(context.Background(), 1); err != nil || len(storage.finished) != 1 || len(storage.failed) != 0 || len(storage.repaired) != 0 {
					test.Fatal("shadow comparison did not finish", err, storage.finished, storage.failed)
				}
				result := storage.results[0]
				if result.Validate() != nil || result.Matched() == mismatch || result.GraphQualificationKey != storage.lease.GraphQualificationKey || result.Expected != storage.expected || storage.finished[0] != storage.lease {
					test.Fatal("comparison changed receipt identity or digest", result, storage.finished)
				}
				wantReferences := 1
				if captured {
					wantReferences = 0
				}
				if storage.referenceReads != wantReferences || storage.budgetChecks != wantReferences || scope.identityCalls != 1 || !slices.Equal(scope.events, []string{"claim", "read-open", "read-close", "finish"}) {
					test.Fatal("comparison used the wrong reference path or persistence order", scope.events, storage.referenceReads, scope.identityCalls)
				}
			})
		}
	}
}

func TestQualificationOwnerRejectsSourceAndReceiptDriftBeforeReference(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*qualificationStore)
		want   error
		reads  int
	}{
		{name: "source-error", change: func(storage *qualificationStore) { storage.sourceErr = domain.ErrConflict }, want: domain.ErrConflict},
		{name: "invalid-source", change: func(storage *qualificationStore) { storage.source.Epoch = "invalid" }, want: domain.ErrIntegrity},
		{name: "source-ahead-live", change: func(storage *qualificationStore) { storage.source.Sequence++ }, want: domain.ErrGraphProjectionNotReady},
		{name: "source-behind", change: func(storage *qualificationStore) { storage.source.Sequence-- }, want: domain.ErrGraphProjectionNotReady},
		{name: "source-foreign", change: func(storage *qualificationStore) { storage.source.RepoID = domain.HashContent([]byte("foreign")) }, want: domain.ErrGraphProjectionNotReady},
		{name: "source-epoch", change: func(storage *qualificationStore) { storage.source.Epoch = "33333333-3333-4333-8333-333333333333" }, want: domain.ErrGraphProjectionNotReady},
		{name: "projection-source", change: func(storage *qualificationStore) {
			storage.projection.Source.Sequence++
			storage.projection.Ready = false
		}, want: domain.ErrIntegrity, reads: 1},
		{name: "status-error", change: func(storage *qualificationStore) { storage.projection.ServingFence = -1 }, want: domain.ErrIntegrity, reads: 1},
		{name: "disabled", change: func(storage *qualificationStore) {
			storage.projection.Enabled = false
			storage.projection.Ready = false
		}, want: domain.ErrGraphProjectionNotReady, reads: 1},
		{name: "attention", change: func(storage *qualificationStore) {
			storage.projection.NeedsAttention = true
			storage.projection.Ready = false
		}, want: domain.ErrGraphProjectionNotReady, reads: 1},
		{name: "incomplete", change: func(storage *qualificationStore) {
			storage.projection.Complete = false
			storage.projection.Ready = false
		}, want: domain.ErrGraphProjectionNotReady, reads: 1},
		{name: "not-ready-live", change: func(storage *qualificationStore) { storage.projection.Ready = false }, want: domain.ErrGraphProjectionNotReady, reads: 1},
		{name: "dataset", change: func(storage *qualificationStore) { storage.lease.Dataset = "33333333-3333-4333-8333-333333333333" }, want: domain.ErrGraphProjectionNotReady, reads: 1},
		{name: "applied", change: func(storage *qualificationStore) {
			storage.lease.ReferenceExpected = storage.expected
			storage.projection.Applied.Sequence--
			storage.projection.Ready = false
		}, want: domain.ErrGraphProjectionNotReady, reads: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			config, _, storage := qualificationFixture(test)
			fixture.change(storage)
			owner := newQualificationOwner(test, config)
			if err := owner.Process(context.Background(), 1); !errors.Is(err, fixture.want) || storage.projectionReads != fixture.reads || storage.referenceReads != 0 || len(storage.finished) != 0 || len(storage.failed) != 1 || len(storage.repaired) != 0 {
				test.Fatal("stale or incoherent input reached reference/durable success", err, storage.projectionReads, storage.referenceReads)
			}
		})
	}
}

func TestQualificationOwnerValidatesPayloadBeforeTrustingManifestDigest(test *testing.T) {
	config, _, storage := qualificationFixture(test)
	changed := false
	for index := range storage.projection.Parts {
		if storage.projection.Parts[index].Key == "default_branch" {
			storage.projection.Parts[index].Value = []byte(`{"value":"changed after capture"}`)
			changed = true
			break
		}
	}
	if !changed {
		test.Fatal("fixture did not contain the repository view field")
	}
	digest, err := domain.GraphViewProjectionManifestDigest(context.Background(), storage.lease.RepoID, storage.projection.Parts)
	if err != nil || digest != storage.expected {
		test.Fatal("fixture changed the manifest instead of its payload", digest, err)
	}
	owner := newQualificationOwner(test, config)
	if err := owner.Process(context.Background(), 1); !errors.Is(err, domain.ErrIntegrity) || len(storage.finished) != 0 || len(storage.failed) != 1 || len(storage.repaired) != 1 {
		test.Fatal("matching manifest hid unvalidated candidate payload", err, storage.finished, storage.repaired)
	}
}

func TestQualificationOwnerComparisonKeepsCapabilitiesLateAndReadOnly(test *testing.T) {
	config, scope, storage := qualificationFixture(test)
	config.Generations, config.Projections, config.Reference = nil, nil, nil
	owner := newQualificationOwner(test, config)
	if _, err := owner.Compare(context.Background(), storage.lease); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("read-only guard lost precedence over missing capabilities", err)
	}
	compare := func(owner *projectionworker.Qualification) error {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return scope.WithinRead(ctx, func(bound context.Context) error {
			_, err := owner.Compare(bound, storage.lease)
			return err
		})
	}
	if err := compare(owner); !errors.Is(err, domain.ErrCatalogUnsupported) || storage.sourceReads != 0 {
		test.Fatal("missing generation capability reached storage", err)
	}
	config.Generations = storage
	owner = newQualificationOwner(test, config)
	if err := compare(owner); !errors.Is(err, domain.ErrCatalogUnsupported) || storage.sourceReads != 0 {
		test.Fatal("generation was read before the projection capability check", err)
	}
	config.Projections = storage
	owner = newQualificationOwner(test, config)
	if err := compare(owner); !errors.Is(err, domain.ErrCatalogUnsupported) || storage.sourceReads != 1 || storage.projectionReads != 1 {
		test.Fatal("live reference capability was not branch-local", err)
	}
	storage.lease.ReferenceExpected = storage.expected
	if err := compare(owner); err != nil || scope.identityCalls != 1 || storage.referenceReads != 0 {
		test.Fatal("captured reference required a live reader", err, scope.identityCalls)
	}
}

func TestQualificationOwnerReferenceBudgetIdentityAndReadExitFailures(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*qualificationScope, *qualificationStore)
		want   error
		reason string
	}{
		{name: "source-budget", change: func(_ *qualificationScope, storage *qualificationStore) {
			storage.budgetErr = domain.ErrRepositoryViewSourceLimit
		}, want: domain.ErrRepositoryViewSourceLimit, reason: "resource_limit"},
		{name: "reference-error", change: func(_ *qualificationScope, storage *qualificationStore) { storage.referenceErr = domain.ErrConflict }, want: domain.ErrConflict, reason: "read_error"},
		{name: "live-identity", change: func(scope *qualificationScope, _ *qualificationStore) {
			scope.identityErr = domain.ErrUnsupportedDocumentIdentity
		}, want: domain.ErrUnsupportedDocumentIdentity, reason: "read_error"},
		{name: "captured-identity", change: func(scope *qualificationScope, storage *qualificationStore) {
			scope.identityErr = domain.ErrUnsupportedDocumentIdentity
			storage.lease.ReferenceExpected = storage.expected
		}, want: domain.ErrUnsupportedDocumentIdentity, reason: "read_error"},
		{name: "snapshot-exit", change: func(scope *qualificationScope, _ *qualificationStore) { scope.readExitErr = domain.ErrConflict }, want: domain.ErrConflict, reason: "read_error"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			config, scope, storage := qualificationFixture(test)
			fixture.change(scope, storage)
			storage.projection.Parts = storage.projection.Parts[:1]
			if fixture.name == "snapshot-exit" {
				_, storage.projection, _ = qualificationData(test, storage.lease.RepoID)
			}
			owner := newQualificationOwner(test, config)
			if err := owner.Process(context.Background(), 1); !errors.Is(err, fixture.want) || len(storage.finished) != 0 || len(storage.failed) != 1 || len(storage.repaired) != 0 || !slices.Equal(storage.reasons, []string{fixture.reason}) {
				test.Fatal("reference failure was hidden by payload validation or snapshot exit", err, storage.reasons)
			}
			if fixture.name == "source-budget" && scope.identityCalls != 0 {
				test.Fatal("source budget was checked after materialization/identity")
			}
		})
	}
}

func TestQualificationOwnerFailureClassificationAndCandidateRepair(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		cause  error
		reason string
		repair bool
		codec  bool
	}{
		{name: "ordinary", cause: domain.ErrIntegrity, reason: "read_error"},
		{name: "not-current", cause: domain.ErrGraphProjectionNotReady, reason: "not_current"},
		{name: "reset", cause: domain.ErrCatalogResetRequired, reason: "not_current"},
		{name: "input-limit", cause: domain.ErrRepositoryViewSourceLimit, reason: "resource_limit"},
		{name: "overloaded", cause: domain.ErrResourceOverloaded, reason: "resource_limit"},
		{name: "candidate-integrity", cause: domain.ErrIntegrity, reason: "read_error", repair: true},
		{name: "candidate-size", cause: domain.ErrRepositoryViewSourceLimit, reason: "read_error", repair: true},
		{name: "candidate-codec", cause: domain.ErrIntegrity, reason: "read_error", repair: true, codec: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			config, scope, storage := qualificationFixture(test)
			storage.projectionErr = fixture.cause
			if fixture.repair {
				storage.projectionErr = fmt.Errorf("candidate read: %w", qualificationCandidate(storage.lease, fixture.cause))
			}
			if fixture.codec {
				storage.projectionErr = nil
				storage.projection.Parts = storage.projection.Parts[:1]
			}
			storage.failErr = errors.New("failure persistence failed")
			storage.repairErr = errors.New("repair request failed")
			owner := newQualificationOwner(test, config)
			err := owner.Process(context.Background(), 1)
			if !errors.Is(err, fixture.cause) || !errors.Is(err, storage.failErr) || len(storage.finished) != 0 || len(storage.failed) != 1 || storage.failed[0] != storage.lease || !slices.Equal(storage.reasons, []string{fixture.reason}) {
				test.Fatal("failure classification, fence, or durable failure error changed", err, storage.reasons, storage.failed)
			}
			wantOrder := []string{"claim", "read-open", "read-close", "fail"}
			if fixture.repair {
				wantOrder = append(wantOrder, "repair")
				if len(storage.repaired) != 1 || storage.repaired[0].Serving || storage.repaired[0].Dataset != storage.lease.Dataset || !errors.Is(err, storage.repairErr) {
					test.Fatal("candidate repair lost identity or repair errors", err, storage.repaired)
				}
			} else if len(storage.repaired) != 0 || errors.Is(err, storage.repairErr) {
				test.Fatal("unidentified failure requested a repair", err, storage.repaired)
			}
			if !slices.Equal(scope.events, wantOrder) {
				test.Fatal("failure/repair retained read scope or ran out of order", scope.events)
			}
		})
	}
}

func TestQualificationOwnerCorruptionRejectsForeignServingUnknownAndCanceled(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		change func(*domain.GraphProjectionCorruption)
	}{
		{name: "foreign", change: func(failure *domain.GraphProjectionCorruption) {
			failure.RepoID = domain.HashContent([]byte("foreign"))
			failure.Applied.RepoID = failure.RepoID
		}},
		{name: "dataset", change: func(failure *domain.GraphProjectionCorruption) {
			failure.Dataset = "33333333-3333-4333-8333-333333333333"
		}},
		{name: "newer", change: func(failure *domain.GraphProjectionCorruption) { failure.Applied.Sequence++ }},
		{name: "rules", change: func(failure *domain.GraphProjectionCorruption) { failure.Rules++ }},
		{name: "serving", change: func(failure *domain.GraphProjectionCorruption) { failure.Serving, failure.ServingFence = true, 2 }},
		{name: "unknown", change: func(failure *domain.GraphProjectionCorruption) { failure.Dataset = "" }},
		{name: "canceled", change: func(failure *domain.GraphProjectionCorruption) { failure.Cause = context.Canceled }},
		{name: "deadline", change: func(failure *domain.GraphProjectionCorruption) { failure.Cause = context.DeadlineExceeded }},
		{name: "unclassified", change: func(failure *domain.GraphProjectionCorruption) { failure.Cause = domain.ErrConflict }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			config, _, storage := qualificationFixture(test)
			failure := qualificationCandidate(storage.lease, domain.ErrIntegrity)
			fixture.change(failure)
			storage.projectionErr = fmt.Errorf("candidate: %w", failure)
			owner := newQualificationOwner(test, config)
			if projectionworker.QualificationCorruption(storage.lease, storage.projectionErr) != nil {
				test.Fatal("unrelated corruption matched the claimed candidate")
			}
			if err := owner.Process(context.Background(), 1); err == nil || len(storage.repaired) != 0 || len(storage.failed) != 1 {
				test.Fatal("unrelated output scheduled candidate repair", err, storage.repaired)
			}
		})
	}
	config, _, storage := qualificationFixture(test)
	config.Repairs = nil
	owner := newQualificationOwner(test, config)
	if err := owner.Repair(context.Background(), storage.lease, domain.ErrIntegrity); err != nil {
		test.Fatal("non-candidate failure required repair capability", err)
	}
	failure := qualificationCandidate(storage.lease, domain.ErrIntegrity)
	wrapped := fmt.Errorf("candidate: %w", failure)
	if projectionworker.QualificationCorruption(storage.lease, wrapped) != failure {
		test.Fatal("valid candidate corruption was not preserved")
	}
	if err := owner.Repair(context.Background(), storage.lease, wrapped); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("candidate repair ignored missing capability", err)
	}
	var absent *domain.GraphProjectionCorruption
	if projectionworker.QualificationCorruption(storage.lease, absent) != nil || projectionworker.QualificationCorruption(storage.lease, nil) != nil {
		test.Fatal("nil corruption became repairable")
	}
}

func TestQualificationOwnerFinishFailureAndCallerCancellationAreNotReclassified(test *testing.T) {
	config, _, storage := qualificationFixture(test)
	storage.finishErr = domain.ErrConflict
	owner := newQualificationOwner(test, config)
	if err := owner.Process(context.Background(), 1); !errors.Is(err, domain.ErrConflict) || len(storage.finished) != 1 || len(storage.failed) != 0 || len(storage.repaired) != 0 {
		test.Fatal("stale finish was hidden or reclassified", err)
	}
	config, _, storage = qualificationFixture(test)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	storage.duringProjection = func(context.Context) { cancel() }
	storage.projectionErr = qualificationCandidate(storage.lease, domain.ErrIntegrity)
	owner = newQualificationOwner(test, config)
	if err := owner.Process(ctx, 1); !errors.Is(err, domain.ErrIntegrity) || len(storage.finished) != 0 || len(storage.failed) != 0 || len(storage.repaired) != 0 {
		test.Fatal("canceled caller published failure/repair", err)
	}
}

func TestQualificationOwnerLeaseValidationAndBoundedClaimCompareFinish(test *testing.T) {
	config, scope, storage := qualificationFixture(test)
	owner := newQualificationOwner(test, config)
	invalid := storage.lease
	invalid.Fence = 0
	if err := owner.RunLease(context.Background(), invalid); !errors.Is(err, domain.ErrValidation) || scope.readCalls != 0 {
		test.Fatal("invalid lease reached comparison", err)
	}
	synctest.Test(test, func(test *testing.T) {
		config, _, storage := qualificationFixture(test)
		storage.claim = func(ctx context.Context, _ domain.ContentHash) (domain.GraphQualificationLease, error) {
			<-ctx.Done()
			return domain.GraphQualificationLease{}, ctx.Err()
		}
		started := time.Now()
		owner := newQualificationOwner(test, config)
		if err := owner.Process(context.Background(), 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 5*time.Second || len(storage.failed) != 0 {
			test.Fatal("claim timeout changed or persisted an unclaimed failure", err, time.Since(started))
		}
	})
	synctest.Test(test, func(test *testing.T) {
		config, _, storage := qualificationFixture(test)
		storage.reference = func(ctx context.Context) (domain.ContentHash, error) {
			<-ctx.Done()
			return "", ctx.Err()
		}
		started := time.Now()
		owner := newQualificationOwner(test, config)
		if err := owner.Process(context.Background(), 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 30*time.Second || len(storage.failed) != 1 || len(storage.repaired) != 0 {
			test.Fatal("comparison timeout lost fresh bounded failure context", err, time.Since(started), storage.failed)
		}
	})
	synctest.Test(test, func(test *testing.T) {
		config, _, storage := qualificationFixture(test)
		storage.finish = func(ctx context.Context) error {
			if ctx.Value(qualificationCallerKey{}) != "caller" {
				test.Error("finish lost caller context")
			}
			<-ctx.Done()
			return ctx.Err()
		}
		started := time.Now()
		owner := newQualificationOwner(test, config)
		ctx := context.WithValue(context.Background(), qualificationCallerKey{}, "caller")
		if err := owner.Process(ctx, 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 5*time.Second || len(storage.failed) != 0 {
			test.Fatal("finish timeout was hidden or converted to comparison failure", err, time.Since(started))
		}
	})
	synctest.Test(test, func(test *testing.T) {
		config, _, storage := qualificationFixture(test)
		storage.claim = func(ctx context.Context, _ domain.ContentHash) (domain.GraphQualificationLease, error) {
			<-ctx.Done()
			return domain.GraphQualificationLease{}, ctx.Err()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		started := time.Now()
		owner := newQualificationOwner(test, config)
		if err := owner.Process(ctx, 1); !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != 20*time.Millisecond {
			test.Fatal("claim outlived the caller's shorter deadline", err, time.Since(started))
		}
	})
}

func TestQualificationOwnerRunProcessesOneLeasePerMinute(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		config, scope, storage := qualificationFixture(test)
		config.Repositories = append(config.Repositories, domain.HashContent([]byte("peer")))
		type pollingSnapshot struct {
			claimed    []domain.ContentHash
			finished   int
			activeWork int64
			reading    bool
			at         time.Time
		}
		completed := make(chan pollingSnapshot, 2)
		var claims atomic.Int64
		storage.claim = func(_ context.Context, repo domain.ContentHash) (domain.GraphQualificationLease, error) {
			claims.Add(1)
			storage.lease, storage.projection, storage.expected = qualificationData(test, repo)
			storage.source = storage.projection.Source
			return storage.lease, nil
		}
		owner := newQualificationOwner(test, config)
		ctx, cancel := context.WithCancel(context.Background())
		scope.workComplete = func() {
			snapshot := pollingSnapshot{claimed: slices.Clone(storage.claimed), finished: len(storage.finished),
				activeWork: scope.activeWork.Load(), reading: scope.active, at: time.Now()}
			select {
			case completed <- snapshot:
			case <-ctx.Done():
			}
		}
		done := make(chan struct{})
		defer func() {
			cancel()
			<-done
		}()
		started := time.Now()
		go func() { defer close(done); owner.Run(ctx) }()
		observe := func(want int, elapsed time.Duration) {
			test.Helper()
			select {
			case snapshot := <-completed:
				synctest.Wait()
				if count := claims.Load(); count != int64(want) || snapshot.finished != want || !slices.Equal(snapshot.claimed, config.Repositories[:want]) {
					test.Fatal("worker changed batch size or repository rotation", count, snapshot.finished, snapshot.claimed)
				}
				if snapshot.at.Sub(started) != elapsed || time.Since(started) != elapsed {
					test.Fatal("worker changed its immediate/one-minute polling cadence", snapshot.at.Sub(started), time.Since(started))
				}
				if snapshot.activeWork != 0 || snapshot.reading || scope.activeWork.Load() != 0 {
					test.Fatal("idle worker retained work resources or its read snapshot", snapshot.activeWork, snapshot.reading, scope.activeWork.Load())
				}
			case <-done:
				test.Fatal("worker stopped before publishing its completed polling batch")
			}
		}
		observe(1, 0)
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if count := claims.Load(); count != 1 {
			test.Fatal("worker polled before its one-minute delay", count)
		}
		if scope.activeWork.Load() != 0 {
			test.Fatal("worker retained resources between polling batches", scope.activeWork.Load())
		}
		time.Sleep(time.Nanosecond)
		observe(2, time.Minute)
		cancel()
		<-done
		if len(storage.finished) != 2 || !slices.Equal(storage.claimed, config.Repositories) || scope.active || scope.activeWork.Load() != 0 {
			test.Fatal("worker shutdown changed completed leases, rotation, or resource release", storage.finished, storage.claimed, scope.active, scope.activeWork.Load())
		}
	})
}
