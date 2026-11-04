package serverruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const wave48ApplicationWorkerProfile = "bounded-workers-v1"

func wave48WorkerEnvironment(test *testing.T) string {
	test.Helper()
	infrastructureRuntimeEnvironment(test)
	for _, variable := range []string{
		"CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS", "CXT_GRAPH_SHADOW_REPOS",
		"CXT_MEMORY_PREPARATION_REPOS", "CXT_GRAPH_READ_REPOS", "CXT_GITHUB_TOKEN",
		"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET",
		"CXT_GITHUB_APP_PRIVATE_KEY", "CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET",
		"RESEND_API_KEY", "RESEND_FROM", "CXT_WEB_URL",
	} {
		test.Setenv(variable, "")
	}
	data := filepath.Join(test.TempDir(), "untouched")
	test.Setenv("CXT_DATA", data)
	return data
}

func wave48WorkerBudget(test *testing.T, profile string) []byte {
	test.Helper()
	var budget executionprofile.Budget
	if err := json.Unmarshal(runtimeExecutionBudget(test), &budget); err != nil {
		test.Fatal(err)
	}
	budget.Profile = profile
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func wave48WorkerStartupError(test *testing.T, worker *WorkerRuntime, err error, data, want string, category error) {
	test.Helper()
	if worker != nil {
		test.Cleanup(func() { _ = worker.Close() })
		test.Fatal("invalid worker configuration opened infrastructure")
	}
	if err == nil || category != nil && !errors.Is(err, category) || want != "" && !strings.Contains(err.Error(), want) {
		test.Fatalf("worker startup did not reject configuration at %q", want)
	}
	if strings.Contains(err.Error(), "wave48-secret") {
		test.Fatal("worker startup exposed a configuration secret")
	}
	if _, statErr := os.Stat(data); !errors.Is(statErr, os.ErrNotExist) {
		test.Fatal("rejected worker startup touched local data storage")
	}
}

func TestWave48WorkerRootsRequireExplicitProfileAndManifest(test *testing.T) {
	for name, open := range map[string]func(context.Context) (*WorkerRuntime, error){
		"worker": func(ctx context.Context) (*WorkerRuntime, error) { return OpenWorker(ctx) }, "documents": OpenDocumentWorker,
	} {
		test.Run(name, func(test *testing.T) {
			for _, profile := range []string{"", "embedded", "future-profile", outbound.BoundedDocumentsProfile, wave48ApplicationWorkerProfile} {
				if name == "documents" && profile == wave48ApplicationWorkerProfile {
					continue
				}
				test.Run("profile="+profile, func(test *testing.T) {
					data := wave48WorkerEnvironment(test)
					test.Setenv("CXT_EXECUTION_PROFILE", profile)
					test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
					test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
					want, category := "CXT_EXECUTION_PROFILE", domain.ErrValidation
					if profile == "future-profile" {
						category = errors.ErrUnsupported
					}
					if profile == outbound.BoundedDocumentsProfile || profile == wave48ApplicationWorkerProfile {
						want = "CXT_EXECUTION_BUDGET_FILE"
					}
					worker, err := open(context.Background())
					wave48WorkerStartupError(test, worker, err, data, want, category)
				})
			}
		})
	}
}

func TestWave48WorkerRejectsProfileManifestMismatchBeforeStorage(test *testing.T) {
	for _, selection := range []struct {
		environment string
		manifest    string
	}{
		{environment: wave48ApplicationWorkerProfile, manifest: outbound.BoundedDocumentsProfile},
		{environment: outbound.BoundedDocumentsProfile, manifest: wave48ApplicationWorkerProfile},
	} {
		test.Run(selection.environment, func(test *testing.T) {
			data := wave48WorkerEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", selection.environment)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, wave48WorkerBudget(test, selection.manifest)))
			test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
			worker, err := OpenWorker(context.Background())
			wave48WorkerStartupError(test, worker, err, data, "profile", domain.ErrValidation)
		})
	}
}

func TestWave48DocumentWorkerRejectsApplicationProfileBeforeStorage(test *testing.T) {
	data := wave48WorkerEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", wave48ApplicationWorkerProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, wave48WorkerBudget(test, wave48ApplicationWorkerProfile)))
	test.Setenv("CXT_POSTGRES_DSN", "postgres://user:wave48-secret@localhost/db?pool_max_conns=invalid")
	worker, err := OpenDocumentWorker(context.Background())
	wave48WorkerStartupError(test, worker, err, data, "", domain.ErrValidation)
	if !strings.Contains(err.Error(), outbound.BoundedDocumentsProfile) && !strings.Contains(err.Error(), wave48ApplicationWorkerProfile) && !strings.Contains(err.Error(), "OpenWorker") {
		test.Fatal("document-only root reached PostgreSQL validation instead of rejecting the full profile")
	}
}

func TestWave48WorkerProfilesRequirePostgresWithoutAuthenticationDependencies(test *testing.T) {
	for _, profile := range []string{outbound.BoundedDocumentsProfile, wave48ApplicationWorkerProfile} {
		test.Run(profile, func(test *testing.T) {
			data := wave48WorkerEnvironment(test)
			test.Setenv("CXT_AUTH", "invalid")
			test.Setenv("CXT_MEMORY_READ_REPOS", "invalid")
			test.Setenv("CXT_EXECUTION_PROFILE", profile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, wave48WorkerBudget(test, profile)))
			worker, err := OpenWorker(context.Background())
			wave48WorkerStartupError(test, worker, err, data, "CXT_POSTGRES_DSN", domain.ErrValidation)
		})
	}
}

func TestWave48RuntimeExternalWorkersIsFullProfileOnly(test *testing.T) {
	for _, profile := range []string{"", outbound.BoundedDocumentsProfile, wave48ApplicationWorkerProfile} {
		test.Run("profile="+profile, func(test *testing.T) {
			root := &Runtime{}
			if profile != "" {
				root.execution = &executionProfile{budget: executionprofile.Budget{Profile: profile}}
			}
			if root.ExternalWorkers() != (profile == wave48ApplicationWorkerProfile) {
				test.Fatal("external application ownership changed an existing execution profile")
			}
			if root.BoundedDocuments() != (profile != "") {
				test.Fatal("application ownership changed bounded document ownership")
			}
		})
	}
}

type wave48WorkerContextKey struct{}

type wave48WorkerClaim struct {
	name string
	ctx  context.Context
}

type wave48LifecycleStore struct {
	*store.FSStore
	outbound.DocRangeJobStore
	claims     chan wave48WorkerClaim
	closed     chan struct{}
	closeCalls atomic.Int32
	closeErr   error
	onClose    func()
}

func (storage *wave48LifecycleStore) claim(name string, ctx context.Context) {
	select {
	case storage.claims <- wave48WorkerClaim{name: name, ctx: ctx}:
	default:
	}
}

func (storage *wave48LifecycleStore) ClaimDocJob(ctx context.Context, _ domain.ContentHash, _ time.Time, _ time.Duration) (domain.DocFinalizationJob, error) {
	storage.claim("finalization", ctx)
	return domain.DocFinalizationJob{}, domain.ErrNotFound
}

func (storage *wave48LifecycleStore) ClaimDocRangeJob(ctx context.Context, _ time.Time, _ time.Duration) (domain.DocRangeIndexJob, error) {
	storage.claim("range", ctx)
	return domain.DocRangeIndexJob{}, domain.ErrNotFound
}

func (storage *wave48LifecycleStore) Close() error {
	storage.closeCalls.Add(1)
	storage.onClose()
	close(storage.closed)
	return storage.closeErr
}

type wave48LifecycleDocumentStore struct {
	outbound.DocFindFieldsStore
	outbound.DocDisplayStore
	storage *wave48LifecycleStore
}

func (storage wave48LifecycleDocumentStore) ClaimDocFindFields(ctx context.Context, _ time.Time, _ time.Duration) (domain.DocRangeIndexJob, error) {
	storage.storage.claim("find fields", ctx)
	return domain.DocRangeIndexJob{}, domain.ErrNotFound
}

func (storage wave48LifecycleDocumentStore) ClaimDocDisplay(ctx context.Context, _ time.Time, _ time.Duration) (domain.DocDisplayJob, error) {
	storage.storage.claim("display", ctx)
	return domain.DocDisplayJob{}, domain.ErrNotFound
}

type wave48LifecycleMaintenanceStore struct{ storage *wave48LifecycleStore }

func (storage wave48LifecycleMaintenanceStore) StepDocDisplayMaintenance(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
	storage.storage.claim("maintenance", ctx)
	return domain.DocDisplayMaintenanceStep{}, domain.ErrNotFound
}

type wave48LifecycleAdmission struct {
	*runtimeProfileAdmission
	checked    chan struct{}
	closeCalls atomic.Int32
	failure    error
	onClose    func()
}

func (admission *wave48LifecycleAdmission) Ready() bool {
	select {
	case admission.checked <- struct{}{}:
	default:
	}
	return admission.ready.Load()
}

func (admission *wave48LifecycleAdmission) Err() error {
	if admission.ctx.Err() != nil {
		return admission.failure
	}
	return nil
}

func (admission *wave48LifecycleAdmission) Close() error {
	admission.closeCalls.Add(1)
	admission.onClose()
	return admission.runtimeProfileAdmission.Close()
}

type wave48BindingProbe struct {
	name        string
	started     chan context.Context
	canceled    chan struct{}
	release     chan struct{}
	exited      chan struct{}
	runs        atomic.Int32
	releaseOnce sync.Once
}

func (probe *wave48BindingProbe) run(ctx context.Context) {
	probe.runs.Add(1)
	defer close(probe.exited)
	probe.started <- ctx
	<-ctx.Done()
	close(probe.canceled)
	<-probe.release
}

func (probe *wave48BindingProbe) unblock() {
	probe.releaseOnce.Do(func() { close(probe.release) })
}

type wave48WorkerFixture struct {
	worker      *WorkerRuntime
	storage     *wave48LifecycleStore
	admission   *wave48LifecycleAdmission
	probes      []*wave48BindingProbe
	loseControl context.CancelFunc
}

func wave48NewWorkerFixture(test *testing.T, profile string, additional int) *wave48WorkerFixture {
	test.Helper()
	storage := &wave48LifecycleStore{
		FSStore: store.NewFSStore(test.TempDir()), claims: make(chan wave48WorkerClaim, 32), closed: make(chan struct{}),
	}
	documents := wave48LifecycleDocumentStore{storage: storage}
	workers, err := app.NewDocumentWorkers(app.DocumentWorkersConfig{
		Metadata: storage, Blobs: storage, FindFields: documents, Display: documents,
		Verifier: &domain.CanonicalDocVerifier{}, Profile: documentquery.LegacyCompatibility,
	})
	if err != nil {
		test.Fatal("narrow document fixture construction failed", err)
	}
	maintenance, err := app.NewDisplayMaintenanceWorker(wave48LifecycleMaintenanceStore{storage: storage})
	if err != nil {
		test.Fatal(err)
	}
	control, cancel := context.WithCancel(context.Background())
	admission := &wave48LifecycleAdmission{
		runtimeProfileAdmission: &runtimeProfileAdmission{ctx: control, closed: make(chan struct{})},
		checked:                 make(chan struct{}, 1), failure: errors.New("wave48 admission control lost"),
	}
	execution := &executionProfile{
		budget: executionprofile.Budget{Profile: profile}, ctx: control, cancel: cancel, admission: admission,
	}
	infrastructure := &Infrastructure{Store: storage, ownedStore: storage, execution: execution}
	fixture := &wave48WorkerFixture{
		worker:  &WorkerRuntime{Store: storage, workers: workers, maintenance: maintenance, infrastructure: infrastructure, execution: execution},
		storage: storage, admission: admission, loseControl: cancel,
	}
	for _, name := range []string{"first application owner", "second application owner"}[:additional] {
		probe := &wave48BindingProbe{
			name: name, started: make(chan context.Context, 1), canceled: make(chan struct{}), release: make(chan struct{}), exited: make(chan struct{}),
		}
		fixture.probes = append(fixture.probes, probe)
		fixture.worker.additional = append(fixture.worker.additional, workerBinding{name: name, run: probe.run})
	}
	admission.onClose = func() {
		for _, probe := range fixture.probes {
			if probe.runs.Load() == 0 {
				continue
			}
			select {
			case <-probe.exited:
			default:
				test.Error("role registration closed before an additional owner joined")
			}
		}
	}
	storage.onClose = func() {
		select {
		case <-admission.closed:
		default:
			test.Error("owned infrastructure closed before role registration")
		}
		admission.onClose()
	}
	test.Cleanup(func() {
		for _, probe := range fixture.probes {
			probe.unblock()
		}
		cancel()
		closed := make(chan struct{})
		go func() { _ = fixture.worker.Close(); close(closed) }()
		wave48WaitWorkerSignal(test, closed, "worker fixture cleanup")
	})
	return fixture
}

func wave48WaitWorkerSignal(test *testing.T, signal <-chan struct{}, description string) {
	test.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		test.Fatalf("timed out waiting for %s", description)
	}
}

func wave48WorkerResult(test *testing.T, result <-chan error, description string) error {
	test.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		test.Fatalf("timed out waiting for %s", description)
		return nil
	}
}

func wave48WaitDocumentClaims(test *testing.T, fixture *wave48WorkerFixture) {
	test.Helper()
	pending := map[string]bool{"finalization": true, "range": true, "find fields": true, "display": true, "maintenance": true}
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for len(pending) != 0 {
		select {
		case claim := <-fixture.storage.claims:
			if claim.ctx.Value(wave48WorkerContextKey{}) != "caller-48" {
				test.Fatal("actual document owner lost the caller context", claim.name)
			}
			delete(pending, claim.name)
		case <-timer.C:
			test.Fatal("actual document worker loops did not all start", pending)
		}
	}
}

func wave48AssertWorkerNotStarted(test *testing.T, fixture *wave48WorkerFixture) {
	test.Helper()
	select {
	case <-fixture.storage.claims:
		test.Fatal("document owner started without Run")
	default:
	}
	wave48AssertAdditionalNotStarted(test, fixture)
}

func wave48AssertAdditionalNotStarted(test *testing.T, fixture *wave48WorkerFixture) {
	test.Helper()
	for _, probe := range fixture.probes {
		if probe.runs.Load() != 0 {
			test.Fatal("additional owner started before full-profile admission readiness", probe.name)
		}
	}
}

func wave48AssertWorkerRegistrationHeld(test *testing.T, fixture *wave48WorkerFixture) {
	test.Helper()
	for _, signal := range []<-chan struct{}{fixture.admission.closed, fixture.storage.closed} {
		select {
		case <-signal:
			test.Fatal("live worker released registration or infrastructure before joining")
		default:
		}
	}
}

func TestWave48WorkerBindingsRetainFiveDocumentOwnersAndAdditionalNames(test *testing.T) {
	fixture := wave48NewWorkerFixture(test, wave48ApplicationWorkerProfile, 2)
	bindings := fixture.worker.bindings()
	if len(bindings) != 7 {
		test.Fatal("worker bindings lost the original five document owners or additional owners", len(bindings))
	}
	names := make(map[string]bool)
	for _, binding := range bindings {
		if binding.name == "" || binding.run == nil || names[binding.name] {
			test.Fatal("worker bindings require unique names and explicit runners")
		}
		names[binding.name] = true
	}
	for _, probe := range fixture.probes {
		if !names[probe.name] {
			test.Fatal("additional owner binding was dropped", probe.name)
		}
	}
	wave48AssertWorkerNotStarted(test, fixture)
}

func TestWave48FullWorkerReadinessBarrierCancellationDoesNotStartAdditionalOwners(test *testing.T) {
	for _, trigger := range []string{"caller", "control", "close"} {
		test.Run(trigger, func(test *testing.T) {
			fixture := wave48NewWorkerFixture(test, wave48ApplicationWorkerProfile, 2)
			caller, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- fixture.worker.Run(caller) }()
			wave48WaitWorkerSignal(test, fixture.admission.checked, "full worker readiness barrier")
			wave48AssertAdditionalNotStarted(test, fixture)
			var want error
			switch trigger {
			case "caller":
				cancel()
			case "control":
				fixture.loseControl()
				want = fixture.admission.failure
			case "close":
				closed := make(chan error, 1)
				go func() { closed <- fixture.worker.Close() }()
				if err := wave48WorkerResult(test, closed, "close during readiness barrier"); err != nil {
					test.Fatal(err)
				}
			}
			if err := wave48WorkerResult(test, done, "canceled readiness barrier"); !errors.Is(err, want) {
				test.Fatal("readiness barrier changed cancellation or admission failure", err)
			}
			wave48AssertAdditionalNotStarted(test, fixture)
			if trigger != "caller" && caller.Err() != nil {
				test.Fatal("worker barrier canceled its caller context")
			}
		})
	}
}

func TestWave48FullWorkerRunJoinsAllBindingsBeforeRegistrationAndStoreClose(test *testing.T) {
	for _, trigger := range []string{"caller", "control", "close"} {
		test.Run(trigger, func(test *testing.T) {
			fixture := wave48NewWorkerFixture(test, wave48ApplicationWorkerProfile, 2)
			caller, cancel := context.WithCancel(context.WithValue(context.Background(), wave48WorkerContextKey{}, "caller-48"))
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- fixture.worker.Run(caller) }()
			wave48WaitWorkerSignal(test, fixture.admission.checked, "full worker readiness barrier")
			wave48AssertAdditionalNotStarted(test, fixture)
			fixture.admission.ready.Store(true)
			wave48WaitDocumentClaims(test, fixture)
			for _, probe := range fixture.probes {
				select {
				case current := <-probe.started:
					if current.Value(wave48WorkerContextKey{}) != "caller-48" {
						test.Fatal("additional binding lost its caller context")
					}
				case <-time.After(5 * time.Second):
					test.Fatal("ready full worker did not start an additional binding", probe.name)
				}
			}
			if !fixture.worker.Ready() {
				test.Fatal("complete fake admission roster did not make the worker ready")
			}
			if err := fixture.worker.Run(caller); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("worker allowed duplicate Run", err)
			}
			closed := make(chan error, 1)
			var want error
			switch trigger {
			case "caller":
				cancel()
			case "control":
				fixture.loseControl()
				want = fixture.admission.failure
			case "close":
				go func() { closed <- fixture.worker.Close() }()
			}
			for _, probe := range fixture.probes {
				wave48WaitWorkerSignal(test, probe.canceled, "additional binding cancellation")
			}
			wave48AssertWorkerRegistrationHeld(test, fixture)
			select {
			case <-done:
				test.Fatal("Run returned while additional owners were still live")
			default:
			}
			fixture.probes[0].unblock()
			wave48WaitWorkerSignal(test, fixture.probes[0].exited, "first additional owner join")
			wave48AssertWorkerRegistrationHeld(test, fixture)
			fixture.probes[1].unblock()
			if err := wave48WorkerResult(test, done, "all worker joins"); !errors.Is(err, want) {
				test.Fatal("worker changed caller cancellation or admission failure", err)
			}
			if trigger != "close" {
				wave48AssertWorkerRegistrationHeld(test, fixture)
				closed <- fixture.worker.Close()
			}
			if err := wave48WorkerResult(test, closed, "infrastructure close after joins"); err != nil {
				test.Fatal(err)
			}
			if err := fixture.worker.Close(); err != nil || fixture.storage.closeCalls.Load() != 1 || fixture.admission.closeCalls.Load() != 1 || fixture.worker.Ready() {
				test.Fatal("worker close lost idempotence, readiness, or owned registration")
			}
			for _, probe := range fixture.probes {
				if probe.runs.Load() != 1 {
					test.Fatal("worker started a binding more than once", probe.name)
				}
			}
			if trigger != "caller" && caller.Err() != nil {
				test.Fatal("worker canceled its caller's context")
			}
		})
	}
}

func TestWave48DocumentWorkerDoesNotGainFullProfileReadinessBarrier(test *testing.T) {
	fixture := wave48NewWorkerFixture(test, outbound.BoundedDocumentsProfile, 0)
	caller, cancel := context.WithCancel(context.WithValue(context.Background(), wave48WorkerContextKey{}, "caller-48"))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fixture.worker.Run(caller) }()
	wave48WaitDocumentClaims(test, fixture)
	if fixture.worker.Ready() {
		test.Fatal("partial fake admission roster advertised readiness")
	}
	cancel()
	if err := wave48WorkerResult(test, done, "document-only cancellation"); err != nil {
		test.Fatal(err)
	}
	wave48AssertWorkerRegistrationHeld(test, fixture)
}

func TestWave48WorkerCloseBeforeRunRetainsErrorAndPreventsStartup(test *testing.T) {
	fixture := wave48NewWorkerFixture(test, wave48ApplicationWorkerProfile, 2)
	failure := errors.New("wave48 owned store close failed")
	fixture.storage.closeErr = failure
	for attempt := 0; attempt < 3; attempt++ {
		if err := fixture.worker.Close(); !errors.Is(err, failure) {
			test.Fatal("worker close lost the original infrastructure error", err)
		}
	}
	if fixture.storage.closeCalls.Load() != 1 || fixture.admission.closeCalls.Load() != 1 {
		test.Fatal("worker close repeated registration or storage cleanup")
	}
	if err := fixture.worker.Run(context.Background()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("closed worker accepted Run", err)
	}
	wave48AssertWorkerNotStarted(test, fixture)
	var absent *WorkerRuntime
	if err := absent.Close(); err != nil {
		test.Fatal("nil worker close changed", err)
	}
}
