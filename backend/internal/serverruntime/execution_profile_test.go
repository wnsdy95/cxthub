package serverruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func runtimeExecutionBudget(test *testing.T) []byte {
	test.Helper()
	budget := executionprofile.Budget{
		Version: 1, Profile: outbound.BoundedDocumentsProfile,
		BaselineActive: 4, BaselineBulk: 2, BaselineConnections: 20, ReservedConnections: 7,
		BaselineCPU: 4, BaselineMemoryBytes: 800 << 20, BaselineSpoolBytes: 3 << 30,
	}
	for _, role := range []outbound.ProcessRole{outbound.ProcessAPI, outbound.ProcessMCP, outbound.ProcessWorker} {
		entry := executionprofile.RoleBudget{
			Role: role, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2,
			RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 1 << 30,
		}
		if role == outbound.ProcessAPI {
			entry.Active, entry.DataPoolMax = 2, 3
		}
		budget.Roles = append(budget.Roles, entry)
	}
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func runtimeExecutionBudgetFile(test *testing.T, raw []byte) string {
	test.Helper()
	path := filepath.Join(test.TempDir(), "budget.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		test.Fatal(err)
	}
	return path
}

func TestExecutionProfileEnvironmentIsExplicitAndBounded(test *testing.T) {
	for _, name := range []string{"", "embedded"} {
		test.Setenv("CXT_EXECUTION_PROFILE", name)
		test.Setenv("CXT_EXECUTION_BUDGET_FILE", "must-not-read")
		profile, err := executionProfileFromEnvironment(outbound.ProcessAPI, false)
		if err != nil || profile != nil {
			test.Fatal("embedded execution consumed an opt-in manifest", profile, err)
		}
		if _, err := executionProfileFromEnvironment(outbound.ProcessWorker, true); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("worker accepted embedded execution", err)
		}
	}
	test.Setenv("CXT_EXECUTION_PROFILE", "future-profile")
	if _, err := executionProfileFromEnvironment(outbound.ProcessAPI, false); !errors.Is(err, errors.ErrUnsupported) {
		test.Fatal("unknown execution profile did not fail closed", err)
	}
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
	for _, path := range []string{"", filepath.Join(test.TempDir(), "missing")} {
		test.Setenv("CXT_EXECUTION_BUDGET_FILE", path)
		if _, err := executionProfileFromEnvironment(outbound.ProcessAPI, false); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("missing execution budget accepted", err)
		}
	}
	raw := runtimeExecutionBudget(test)
	for _, length := range []int{len(raw), executionBudgetLimit, executionBudgetLimit + 1} {
		padded := append(bytes.Clone(raw), bytes.Repeat([]byte(" "), length-len(raw))...)
		test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, padded))
		profile, err := executionProfileFromEnvironment(outbound.ProcessMCP, false)
		if length > executionBudgetLimit {
			if profile != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("oversized execution budget accepted", err)
			}
			continue
		}
		if err != nil || profile == nil || profile.role.Role != outbound.ProcessMCP || profile.role.Active != 1 {
			test.Fatal("valid bounded role configuration rejected", profile, err)
		}
	}
}

func TestBoundedRootValidationPrecedesStoreEffects(test *testing.T) {
	for name, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"api": Open, "mcp": OpenMCP} {
		test.Run(name, func(test *testing.T) {
			infrastructureRuntimeEnvironment(test)
			test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, []byte(`{"version":1}`)))
			data := filepath.Join(test.TempDir(), "untouched")
			opened, err := open(context.Background(), "127.0.0.1:0", data, false)
			if opened != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("malformed budget reached storage", err)
			}
			if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
				test.Fatal("invalid bounded startup touched data storage", err)
			}
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, runtimeExecutionBudget(test)))
			if opened, err := open(context.Background(), "127.0.0.1:0", data, false); opened != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("bounded profile permitted filesystem fallback", err)
			}
			test.Setenv("CXT_AUTH", "invalid")
			test.Setenv("CXT_EXECUTION_PROFILE", "invalid")
			if _, err := open(context.Background(), "127.0.0.1:0", data, false); err == nil || err.Error() != "CXT_AUTH must be firebase or dev" {
				test.Fatal("profile validation displaced existing authentication ordering", err)
			}
			test.Setenv("CXT_AUTH", "dev")
			test.Setenv("CXT_PUBLIC_URL", "invalid")
			if _, err := open(context.Background(), "127.0.0.1:0", data, false); err == nil || !strings.HasPrefix(err.Error(), "CXT_PUBLIC_URL") {
				test.Fatal("profile validation displaced existing origin ordering", err)
			}
		})
	}
}

func TestExecutionProfileRejectsUnqualifiedCPUBeforeStoreOrGlobals(test *testing.T) {
	var budget executionprofile.Budget
	if err := json.Unmarshal(runtimeExecutionBudget(test), &budget); err != nil {
		test.Fatal(err)
	}
	budget.BaselineCPU = int64(^uint64(0) >> 1)
	budget.Roles[0].CPU = budget.BaselineCPU - 2
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := executionprofile.Parse(raw); err != nil {
		test.Fatal("huge CPU fixture must remain valid pure manifest input", err)
	}
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, raw))
	test.Setenv("CXT_POSTGRES_DSN", "invalid-dsn-must-not-be-parsed")
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	data := filepath.Join(test.TempDir(), "untouched")
	opened, err := Open(context.Background(), "127.0.0.1:0", data, true)
	if opened != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "execution CPU") {
		test.Fatal("unqualified CPU reached store initialization", err)
	}
	if runtime.GOMAXPROCS(0) != priorCPU || debug.SetMemoryLimit(-1) != priorMemory {
		test.Fatal("rejected CPU declaration mutated process runtime settings")
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("rejected CPU declaration touched storage", err)
	}
	budget.BaselineCPU = int64(runtime.NumCPU()) + 3
	budget.Roles[0].CPU = int64(runtime.NumCPU()) + 1
	raw, err = json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, raw))
	if _, err := executionProfileFromEnvironment(outbound.ProcessAPI, false); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("CPU declaration above available logical CPUs accepted", err)
	}
}

func TestDocumentWorkerRootRequiresProfileWithoutAuthenticationOrFilesystemFallback(test *testing.T) {
	test.Setenv("CXT_AUTH", "invalid")
	test.Setenv("CXT_PUBLIC_URL", "invalid")
	test.Setenv("CXT_MEMORY_READ_REPOS", "invalid")
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, runtimeExecutionBudget(test)))
	test.Setenv("CXT_POSTGRES_DSN", "")
	data := filepath.Join(test.TempDir(), "untouched")
	test.Setenv("CXT_DATA", data)
	worker, err := OpenDocumentWorker(context.Background())
	if worker != nil || !errors.Is(err, domain.ErrValidation) || !strings.Contains(err.Error(), "CXT_POSTGRES_DSN") {
		test.Fatal("worker constructed authentication/query dependencies or allowed local storage", err)
	}
	if _, err := os.Stat(data); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("worker touched local storage", err)
	}
	test.Setenv("CXT_EXECUTION_PROFILE", "")
	if worker, err := OpenDocumentWorker(context.Background()); worker != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("worker accepted an implicit embedded profile", err)
	}
}

func TestExecutionDataDSNRewritesOriginalWithoutLosingCredentials(test *testing.T) {
	role := executionprofile.RoleBudget{Role: outbound.ProcessAPI, DataPoolMax: 3}
	for _, raw := range []string{
		"postgres://role:p%40ss%27word@localhost/database?application_name=old&application_name=ignored&pool_max_conns=99&pool_max_conns=88&pool_min_conns=4&pool_min_idle_conns=2",
		`host=localhost user='role name' password='p a\'ss\\word' dbname='db name' application_name=first application_name='untrusted raw' pool_max_conns=99 pool_max_conns=88 pool_min_conns=4 pool_min_idle_conns=2`,
	} {
		original, err := pgxpool.ParseConfig(raw)
		if err != nil {
			test.Fatal("invalid credential fixture")
		}
		rewritten, err := executionDataDSN(raw, role)
		if err != nil {
			test.Fatal("bounded DSN rewrite rejected a valid original", err)
		}
		parsed, err := pgxpool.ParseConfig(rewritten)
		if err != nil {
			test.Fatal("rewritten DSN cannot be parsed")
		}
		if parsed.ConnConfig.User != original.ConnConfig.User || parsed.ConnConfig.Password != original.ConnConfig.Password || parsed.ConnConfig.Database != original.ConnConfig.Database {
			test.Fatal("bounded DSN rewrite altered credentials or database")
		}
		if parsed.MaxConns != 3 || parsed.MinConns != 0 || parsed.MinIdleConns != 0 || parsed.ConnConfig.RuntimeParams["application_name"] != "cxthub/bounded-documents-v1/api/data" {
			test.Fatal("bounded DSN settings were not serialized")
		}
	}
}

func TestExecutionDataDSNPreservesEnvironmentAndSanitizesErrors(test *testing.T) {
	for key, value := range map[string]string{
		"PGHOST": "localhost", "PGPORT": "5432", "PGUSER": "environment-user", "PGPASSWORD": "environment-secret",
		"PGDATABASE": "environment-database", "PGAPPNAME": "untrusted-application",
	} {
		test.Setenv(key, value)
	}
	role := executionprofile.RoleBudget{Role: outbound.ProcessWorker, DataPoolMax: 2}
	rewritten, err := executionDataDSN("connect_timeout=5", role)
	if err != nil {
		test.Fatal(err)
	}
	parsed, err := pgxpool.ParseConfig(rewritten)
	if err != nil || parsed.ConnConfig.User != "environment-user" || parsed.ConnConfig.Password != "environment-secret" || parsed.ConnConfig.Database != "environment-database" {
		test.Fatal("bounded DSN rewrite lost environment defaults")
	}
	if parsed.MaxConns != 2 || parsed.MinConns != 0 || parsed.MinIdleConns != 0 || parsed.ConnConfig.RuntimeParams["application_name"] != "cxthub/bounded-documents-v1/worker/data" {
		test.Fatal("environment overrode bounded data pool settings")
	}
	if _, err := executionDataDSN("postgres://user:credential-secret@localhost/db?pool_max_conns=invalid", role); !errors.Is(err, domain.ErrValidation) || strings.Contains(err.Error(), "credential-secret") {
		test.Fatal("invalid PostgreSQL settings leaked raw configuration")
	}
}

type runtimeProfileAdmission struct {
	ctx       context.Context
	ready     atomic.Bool
	closed    chan struct{}
	closeOnce sync.Once
}

func (admission *runtimeProfileAdmission) Acquire(context.Context, bool, bool) (outbound.ProcessAdmissionPermit, error) {
	return nil, domain.ErrResourceOverloaded
}

func (admission *runtimeProfileAdmission) Context() context.Context { return admission.ctx }
func (admission *runtimeProfileAdmission) Ready() bool              { return admission.ready.Load() }
func (admission *runtimeProfileAdmission) Err() error               { return nil }
func (admission *runtimeProfileAdmission) Close() error {
	admission.closeOnce.Do(func() { close(admission.closed) })
	return nil
}

type runtimeProfileAdmissionStore struct {
	admission outbound.ProcessAdmission
	opened    *context.Context
}

func (storage runtimeProfileAdmissionStore) ConfigureTransientSpoolBudget(limit int64) (func() error, error) {
	return (&store.FSStore{}).ConfigureTransientSpoolBudget(limit)
}

func (storage runtimeProfileAdmissionStore) TransientSpoolBudgetUsage() outbound.TransientSpoolBudgetUsage {
	return (&store.FSStore{}).TransientSpoolBudgetUsage()
}

func (storage runtimeProfileAdmissionStore) OpenProcessAdmission(ctx context.Context, config outbound.ProcessAdmissionConfig) (outbound.ProcessAdmission, error) {
	if storage.opened != nil {
		*storage.opened = ctx
		storage.admission.(*runtimeProfileAdmission).ctx = ctx
	}
	return storage.admission, nil
}

func TestExecutionProfileAppliesSoftRuntimeSettingsAndRestoresAfterDrain(test *testing.T) {
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, runtimeExecutionBudget(test)))
	profile, err := executionProfileFromEnvironment(outbound.ProcessMCP, false)
	if err != nil {
		test.Fatal(err)
	}
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	control, cancel := context.WithCancel(context.Background())
	defer cancel()
	admission := &runtimeProfileAdmission{ctx: control, closed: make(chan struct{})}
	admission.ready.Store(true)
	if err := profile.configure(control, runtimeProfileAdmissionStore{admission: admission}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = profile.close() })
	if runtime.GOMAXPROCS(0) != 1 || debug.SetMemoryLimit(-1) != 192<<20 {
		test.Fatal("bounded role runtime settings were not applied")
	}
	if err := profile.close(); err != nil {
		test.Fatal(err)
	}
	if runtime.GOMAXPROCS(0) != priorCPU || debug.SetMemoryLimit(-1) != priorMemory {
		test.Fatal("bounded role runtime settings were not restored")
	}
}

func TestExecutionProfileRetainsControlUntilCallerDrain(test *testing.T) {
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, runtimeExecutionBudget(test)))
	profile, err := executionProfileFromEnvironment(outbound.ProcessAPI, false)
	if err != nil {
		test.Fatal(err)
	}
	caller, cancelCaller := context.WithCancel(context.Background())
	defer cancelCaller()
	admission := &runtimeProfileAdmission{closed: make(chan struct{})}
	admission.ready.Store(true)
	var control context.Context
	if err := profile.configure(caller, runtimeProfileAdmissionStore{admission: admission, opened: &control}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = profile.close() })
	cancelCaller()
	if profile.ctx.Err() == nil || control.Err() != nil {
		test.Fatal("caller cancellation released control before owners could drain")
	}
	if err := profile.close(); err != nil {
		test.Fatal(err)
	}
	if control.Err() == nil {
		test.Fatal("closed profile retained its private control lifetime")
	}
}

func TestExecutionProfileReadinessControlLossAndRequestDrain(test *testing.T) {
	control, loseControl := context.WithCancel(context.Background())
	defer loseControl()
	current, cancel := context.WithCancel(control)
	admission := &runtimeProfileAdmission{ctx: control, closed: make(chan struct{})}
	profile := &executionProfile{ctx: current, cancel: cancel, admission: admission}
	test.Cleanup(func() { _ = profile.close() })
	started, canceled, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var finishOnce sync.Once
	release := func() { finishOnce.Do(func() { close(finish) }) }
	defer release()
	handler := (&Runtime{execution: profile}).HealthHandler(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
		close(canceled)
		<-finish
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != http.StatusServiceUnavailable {
		test.Fatal("partial role roster advertised readiness")
	}
	admission.ready.Store(true)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
	if response.Code != http.StatusOK {
		test.Fatal("complete role roster did not become ready")
	}
	done := make(chan struct{})
	go func() {
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/work", nil))
		close(done)
	}()
	<-started
	process := profile.processContext(context.Background())
	loseControl()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		test.Fatal("control loss did not cancel request")
	}
	select {
	case <-process.Done():
	case <-time.After(2 * time.Second):
		test.Fatal("control loss did not cancel runtime context")
	}
	closed := make(chan struct{})
	go func() { _ = profile.close(); close(closed) }()
	select {
	case <-admission.closed:
		test.Fatal("role released before live handler joined")
	default:
	}
	release()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		test.Fatal("drained profile did not close")
	}
	<-done
	select {
	case <-admission.closed:
	default:
		test.Fatal("drained profile retained role registration")
	}
}
