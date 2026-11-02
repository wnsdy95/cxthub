//go:build postgres && !windows

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const wave48APIProfileChildEnv = "CXT_TEST_WAVE48_API_PROFILE_CHILD"

func TestWave48APIProfileChild(test *testing.T) {
	if os.Getenv(wave48APIProfileChildEnv) != "1" {
		return
	}
	if os.Getenv("CXT_EXECUTION_PROFILE") != "bounded-workers-v1" || os.Getenv(serverLifecycleChildEnv) != "sigterm" {
		test.Fatal("API ownership child requires the explicit full-worker profile and original signal lifecycle")
	}
	TestServerLifecyclePGProcessHelper(test)
}

func TestWave48PGAPIExternalOwnershipReadyLeavesClaimableDocumentQueued(test *testing.T) {
	fixture := newServerLifecyclePGFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	test.Cleanup(cancel)
	budget := executionprofile.Budget{
		Version: 1, Profile: "bounded-workers-v1",
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
	apiConfig, err := budget.AdmissionConfig(outbound.ProcessAPI)
	if err != nil {
		test.Fatal(err)
	}
	apiFingerprint, err := store.ProcessAdmissionFingerprint(apiConfig.BudgetFingerprint)
	if err != nil {
		test.Fatal(err)
	}
	raw, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	budgetPath := filepath.Join(test.TempDir(), "budget.json")
	if err := os.WriteFile(budgetPath, raw, 0o600); err != nil {
		test.Fatal(err)
	}
	stores := make(map[outbound.ProcessRole]*store.PostgresStore, 2)
	admissions := make(map[outbound.ProcessRole]outbound.ProcessAdmission, 2)
	for _, role := range []outbound.ProcessRole{outbound.ProcessMCP, outbound.ProcessWorker} {
		roleBudget, err := budget.ForRole(role)
		if err != nil {
			test.Fatal(err)
		}
		storage := wave48APIProfileDataStore(test, ctx, fixture, roleBudget)
		config, err := budget.AdmissionConfig(role)
		if err != nil {
			test.Fatal(err)
		}
		admission, err := storage.OpenProcessAdmission(ctx, config)
		if err != nil {
			test.Fatalf("open real %s process admission without an application runtime: %v", role, err)
		}
		test.Cleanup(func() {
			if err := admission.Close(); err != nil {
				test.Errorf("close %s process admission: %v", role, err)
			}
		})
		stores[role], admissions[role] = storage, admission
	}
	storage := stores[outbound.ProcessWorker]
	setup, stopSetup := context.WithTimeout(ctx, 30*time.Second)
	_, err = storage.ApplyMigrations(setup, fixture.migrations)
	stopSetup()
	if err != nil {
		test.Fatal("migrate only the fresh local API ownership fixture", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		test.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		test.Fatal(err)
	}
	child := startWave48APIProfileProcess(test, fixture, address, budgetPath)
	transport := &http.Transport{DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	healthy := func() bool {
		probe, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		request, err := http.NewRequestWithContext(probe, http.MethodGet, "http://"+address+"/healthz", nil)
		if err != nil {
			test.Fatal(err)
		}
		response, err := client.Do(request)
		if err != nil {
			return false
		}
		defer response.Body.Close()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1024))
		return err == nil && response.StatusCode == http.StatusOK && string(body) == `{"status":"ok"}`
	}
	child.await(test, time.Minute, "actual API and compatible MCP/worker admissions ready", func() bool {
		return admissions[outbound.ProcessMCP].Ready() && admissions[outbound.ProcessWorker].Ready() && healthy()
	})
	seed, stopSeed := context.WithTimeout(ctx, 5*time.Second)
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(seed, domain.Repo{ID: repo}); err != nil {
		stopSeed()
		test.Fatal(err)
	}
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events:   []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "ready API leaves externally owned finalization queued"}}}},
	}
	canonical, err := domain.CanonicalBytes(document)
	if err != nil {
		stopSeed()
		test.Fatal(err)
	}
	plan, planned := domain.PlanDocChunks(canonical)
	if !planned {
		stopSeed()
		test.Fatal("valid fixture conversation did not produce real document chunks")
	}
	if _, _, err := storage.PutChunks(seed, repo, plan.Bodies); err != nil {
		stopSeed()
		test.Fatal(err)
	}
	job, err := domain.NewDocFinalizationJob(repo, domain.HashContent(canonical), plan.Manifest, time.Now().UTC())
	if err != nil {
		stopSeed()
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(seed, job); err != nil {
		stopSeed()
		test.Fatal(err)
	}
	stopSeed()
	until := time.Now().Add(5 * time.Second)
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	healthProbes := 0
	for {
		child.assertRunning(test)
		if !healthy() || !admissions[outbound.ProcessMCP].Ready() || !admissions[outbound.ProcessWorker].Ready() {
			test.Fatal("API ownership observation lost health or compatible process admission")
		}
		healthProbes++
		probe, stop := context.WithTimeout(ctx, time.Second)
		queued, err := storage.GetDocJob(probe, repo, job.ID)
		stop()
		if err != nil {
			test.Fatal(err)
		}
		if queued.State != "waiting" || queued.Attempts != 0 || queued.Version != 0 || !queued.Due(time.Now().UTC()) {
			test.Fatalf("ready external-worker API changed a due document job: state=%s attempts=%d version=%d", queued.State, queued.Attempts, queued.Version)
		}
		if !time.Now().Before(until) {
			break
		}
		select {
		case <-ctx.Done():
			test.Fatal("API ownership observation exceeded its deadline")
		case <-ticker.C:
		}
	}
	if healthProbes < 3 {
		test.Fatal("API ownership observation did not span repeated healthy probes")
	}
	func() {
		probe, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		permit, err := admissions[outbound.ProcessWorker].Acquire(probe, true, false)
		if err != nil {
			test.Fatal("compatible process admission did not permit real bulk work", err)
		}
		defer func() {
			if err := permit.Release(); err != nil {
				test.Error("release positive claim control admission", err)
			}
		}()
		claimed, err := storage.ClaimDocJob(probe, "", time.Now().UTC(), time.Minute)
		if err != nil || claimed.ID != job.ID || claimed.State != "running" || claimed.Attempts != 1 || claimed.Version != 1 {
			test.Fatalf("original queued job was not actually claimable: state=%s attempts=%d version=%d error=%v", claimed.State, claimed.Attempts, claimed.Version, err)
		}
	}()
	if !healthy() {
		test.Fatal("actual API lost readiness during the positive claim control")
	}
	if err := child.command.Process.Signal(syscall.SIGTERM); err != nil {
		test.Fatal(err)
	}
	child.awaitReturned(test, "sigterm", 15*time.Second)
	child.await(test, 5*time.Second, "bounded API data, bulk and control sessions released before child exit", func() bool {
		probe, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		var remaining int
		err := fixture.admin.QueryRow(probe, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=$1 AND application_name IN ($2,$3,$4)`, fixture.database,
			"cxthub/bounded-documents-v1/api/data", "cxthub/bounded-documents-v1/api/bulk",
			outbound.BoundedDocumentsControlPrefix+"api/"+apiFingerprint).Scan(&remaining)
		if err != nil {
			test.Fatal(err)
		}
		return remaining == 0
	})
	probe, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	connection, err := (&net.Dialer{}).DialContext(probe, "tcp", address)
	if err == nil {
		connection.Close()
		test.Fatal("API listener still accepts connections after serve returned")
	}
	child.finish(test)
}

func wave48APIProfileDataStore(test *testing.T, ctx context.Context, fixture *serverLifecyclePGFixture, role executionprofile.RoleBudget) *store.PostgresStore {
	test.Helper()
	name := "cxthub/bounded-documents-v1/" + string(role.Role) + "/data"
	dsn := fixture.dsn
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			test.Fatal("invalid isolated API ownership fixture URL")
		}
		query := parsed.Query()
		query.Set("application_name", name)
		query.Set("pool_max_conns", fmt.Sprint(role.DataPoolMax))
		query.Set("pool_min_conns", "0")
		query.Set("pool_min_idle_conns", "0")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += fmt.Sprintf(" application_name=%s pool_max_conns=%d pool_min_conns=0 pool_min_idle_conns=0", name, role.DataPoolMax)
	}
	open, stop := context.WithTimeout(ctx, 10*time.Second)
	defer stop()
	storage, err := store.NewPostgresStore(open, dsn)
	if err != nil {
		test.Fatalf("open capped, tagged %s data pool: %v", role.Role, err)
	}
	test.Cleanup(storage.Close)
	return storage
}

func startWave48APIProfileProcess(test *testing.T, fixture *serverLifecyclePGFixture, address, budgetPath string) *serverLifecycleProcess {
	test.Helper()
	executable, err := os.Executable()
	if err != nil {
		test.Fatal(err)
	}
	directory := test.TempDir()
	command := exec.Command(executable, "-test.run=^TestWave48APIProfileChild$", "-test.count=1", "-test.timeout=2m")
	command.Dir = directory
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		switch name {
		case "PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "TZ", "GOMAXPROCS", "SystemRoot", "WINDIR":
			command.Env = append(command.Env, entry)
		}
	}
	command.Env = append(command.Env,
		"HOME="+directory,
		wave48APIProfileChildEnv+"=1",
		serverLifecycleChildEnv+"=sigterm",
		"CXT_EXECUTION_PROFILE=bounded-workers-v1",
		"CXT_EXECUTION_BUDGET_FILE="+budgetPath,
		"CXT_AUTH=dev",
		"CXT_ADDR="+address,
		"CXT_DATA="+filepath.Join(directory, "data"),
		"CXT_PUBLIC_URL=http://"+address,
		"CXT_WEB_URL=http://"+address,
		"CXT_REQUIRE_POSTGRES=1",
		"CXT_POSTGRES_DSN="+fixture.dsn,
		"CXT_MIGRATIONS_DIR="+fixture.migrations,
	)
	logPath := filepath.Join(directory, "process.log")
	output, err := os.Create(logPath)
	if err != nil {
		test.Fatal(err)
	}
	defer output.Close()
	command.Stdout, command.Stderr = output, output
	input, err := command.StdinPipe()
	if err != nil {
		test.Fatal(err)
	}
	if err := command.Start(); err != nil {
		input.Close()
		test.Fatal(err)
	}
	child := &serverLifecycleProcess{command: command, input: input, logPath: logPath, done: make(chan struct{})}
	go func() {
		child.waitErr = command.Wait()
		close(child.done)
	}()
	test.Cleanup(func() {
		select {
		case <-child.done:
		default:
			_ = command.Process.Kill()
			select {
			case <-child.done:
			case <-time.After(5 * time.Second):
				test.Error("API ownership subprocess did not exit after cleanup kill")
			}
		}
		input.Close()
		if test.Failed() {
			output, err := os.ReadFile(logPath)
			if err != nil {
				test.Logf("read API ownership subprocess output: %v", err)
			} else {
				test.Logf("API ownership subprocess output:\n%s", output)
			}
		}
	})
	return child
}
