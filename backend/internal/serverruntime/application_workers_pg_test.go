//go:build postgres

package serverruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/webhookdelivery"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const wave48PGProfile = "bounded-workers-v1"

type wave48PGFixture struct {
	dsn         string
	database    string
	migrations  string
	budget      executionprofile.Budget
	fingerprint string
	observer    *pgx.Conn
	storage     *store.PostgresStore
	repo        domain.ContentHash
}

type wave48PGCommand struct {
	Operation string
	Hash      domain.ContentHash
}

type wave48PGEvent struct {
	State    string
	Code     string
	Ready    bool
	Bindings int
	Text     string
	Canceled bool
	Joined   bool
	Closed   bool
	Restored bool
}

type wave48PGChild struct {
	role    outbound.ProcessRole
	command *exec.Cmd
	input   io.WriteCloser
	events  chan wave48PGEvent
	done    chan struct{}
	waitErr error
}

func wave48PGSystemContext(ctx context.Context) context.Context {
	identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	return outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), identities), identities, identities)
}

func newWave48PGFixture(test *testing.T) *wave48PGFixture {
	test.Helper()
	dsn, observer := executionRootPostgresFixture(test)
	executionRootPostgresEnvironment(test, dsn)
	budget, err := executionprofile.Parse(runtimeExecutionBudget(test))
	if err != nil {
		test.Fatal(err)
	}
	budget.Profile = wave48PGProfile
	encoded, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	budgetPath := runtimeExecutionBudgetFile(test, encoded)
	test.Setenv("CXT_EXECUTION_PROFILE", wave48PGProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", budgetPath)
	for _, name := range []string{
		"CXT_PENDING_PROJECTION_REPOS", "CXT_GRAPH_PROJECTION_REPOS", "CXT_GRAPH_SHADOW_REPOS", "CXT_MEMORY_PREPARATION_REPOS",
		"CXT_GITHUB_APP_ID", "CXT_GITHUB_APP_CLIENT_ID", "CXT_GITHUB_APP_CLIENT_SECRET", "CXT_GITHUB_APP_PRIVATE_KEY",
		"CXT_GITHUB_APP_SLUG", "CXT_GITHUB_APP_WEBHOOK_SECRET", "CXT_GITHUB_TOKEN",
		"RESEND_API_KEY", "RESEND_FROM", "CXT_WEB_URL", "CXT_IDENTITY_ENCRYPTION_KEY", "CXT_IDENTITY_ENCRYPTION_KEYRING",
	} {
		test.Setenv(name, "")
	}
	budgetFingerprint, err := budget.Fingerprint()
	if err != nil {
		test.Fatal("full worker manifest was not accepted", err)
	}
	fingerprint, err := store.ProcessAdmissionFingerprint(budgetFingerprint)
	if err != nil {
		test.Fatal("full worker admission fingerprint was not accepted", err)
	}
	role, err := budget.ForRole(outbound.ProcessWorker)
	if err != nil {
		test.Fatal(err)
	}
	seedDSN, err := executionDataDSN(dsn, role)
	if err != nil {
		test.Fatal(err)
	}
	seedDSN = infrastructureNamedPostgresDSN(test, seedDSN, "cxthub/bounded-documents-v1/observer")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	storage, err := store.NewPostgresStore(ctx, seedDSN)
	if err != nil {
		test.Fatal("open only the owned local fixture seed store", err)
	}
	test.Cleanup(storage.Close)
	migrations, err := filepath.Abs("../../../schemas/db/migrations")
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ApplyMigrations(ctx, migrations); err != nil {
		test.Fatal("initialize only the fresh owned fixture database", err)
	}
	configuration, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal(err)
	}
	repo := domain.HashContent([]byte("wave48 full worker durable repository"))
	ctx = wave48PGSystemContext(ctx)
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal("seed content repository through the production port", err)
	}
	if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal("retain the actual repository document identity fence", err)
	}
	return &wave48PGFixture{dsn: dsn, database: configuration.ConnConfig.Database, migrations: migrations,
		budget: budget, fingerprint: fingerprint, observer: observer, storage: storage, repo: repo}
}

func wave48PGAssertFullRoot(test *testing.T, worker *WorkerRuntime) {
	test.Helper()
	if worker == nil || worker.infrastructure == nil || worker.Store != worker.infrastructure.Store || worker.Store != worker.infrastructure.ownedStore ||
		worker.execution == nil || worker.execution.budget.Profile != wave48PGProfile || worker.execution.role.Role != outbound.ProcessWorker ||
		worker.infrastructure.DocumentProfile != documentquery.VerifiedRoots {
		test.Fatal("public full worker root lost its one-store infrastructure, role or verified document profile")
	}
	if _, postgres := worker.Store.(*store.PostgresStore); !postgres {
		test.Fatal("full worker root did not use the actual PostgreSQL production adapter")
	}
	if err := app.ValidateProductionStore(worker.Store); err != nil {
		test.Fatal("actual full root lost required production ports", err)
	}
	if worker.workers == nil || worker.workers.Finalization == nil || worker.workers.Range == nil ||
		worker.workers.FindFields == nil || worker.workers.Display == nil || worker.maintenance == nil {
		test.Fatal("full worker root omitted an existing document owner")
	}
	wave48PGAssertBindings(test, worker.additional)
	if runtime.GOMAXPROCS(0) != int(worker.execution.role.CPU) || debug.SetMemoryLimit(-1) != worker.execution.role.MemoryBytes {
		test.Fatal("full worker root did not apply the same manifest CPU and soft-memory envelopes")
	}
}

func wave48PGAssertBindings(test *testing.T, bindings []workerBinding) {
	test.Helper()
	if len(bindings) != 12 {
		test.Fatal("full worker root must add twelve owners to the retained five document owners", len(bindings))
	}
	seen := make(map[string]bool, len(bindings))
	for _, binding := range bindings {
		if strings.TrimSpace(binding.name) == "" || binding.run == nil || seen[binding.name] {
			test.Fatal("full worker root contains an absent or duplicate additional binding", binding.name)
		}
		seen[binding.name] = true
	}
}

func wave48PGOpenWorker(ctx context.Context) (*WorkerRuntime, error) {
	if os.Getenv("CXT_EXECUTION_PROFILE") != wave48PGProfile {
		return OpenWorker(ctx)
	}
	return OpenWorker(ctx, func(storage store.Store) (ApplicationWorkerConfig, error) {
		configuration := ApplicationWorkerConfig{
			GitReader: gitevidence.NewGitHub(func() string { return "" }), NotificationSender: webhookdelivery.New(),
		}
		for _, selection := range []struct {
			name     string
			selected *[]domain.ContentHash
		}{
			{"CXT_PENDING_PROJECTION_REPOS", &configuration.PendingRepositories},
			{"CXT_GRAPH_PROJECTION_REPOS", &configuration.GraphRepositories},
			{"CXT_GRAPH_SHADOW_REPOS", &configuration.QualificationRepositories},
			{"CXT_MEMORY_PREPARATION_REPOS", &configuration.MemoryRepositories},
		} {
			repositories, err := WorkerRepositories(selection.name, os.Getenv(selection.name))
			if err != nil {
				return ApplicationWorkerConfig{}, err
			}
			*selection.selected = repositories
		}
		return configuration, nil
	})
}

func TestWave48PGFullRootCompleteProductionPortsAndClose(test *testing.T) {
	fixture := newWave48PGFixture(test)
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	worker, err := wave48PGOpenWorker(context.Background())
	if err != nil {
		test.Fatal("actual public full worker composition root", err)
	}
	test.Cleanup(func() { _ = worker.Close() })
	wave48PGAssertFullRoot(test, worker)
	if worker.Ready() {
		test.Fatal("one full worker role became ready without API and MCP peers")
	}
	bindings, err := newApplicationWorkers(worker.Store, ApplicationWorkerConfig{
		PendingRepositories: []domain.ContentHash{fixture.repo}, GraphRepositories: []domain.ContentHash{fixture.repo},
		QualificationRepositories: []domain.ContentHash{fixture.repo}, MemoryRepositories: []domain.ContentHash{fixture.repo},
		GitReader:          gitevidence.NewGitHub(func() string { return "" }),
		NotificationSender: webhookdelivery.New(),
	})
	if err != nil {
		test.Fatal("enabled Complete worker constructors rejected the current production PostgreSQL ports", err)
	}
	wave48PGAssertBindings(test, bindings)
	if documentOnly, err := OpenDocumentWorker(context.Background()); documentOnly != nil || !errors.Is(err, domain.ErrValidation) {
		if documentOnly != nil {
			_ = documentOnly.Close()
		}
		test.Fatal("document-only selector did not reject the full profile before role registration", err)
	}
	current, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(current) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			test.Fatal("full root waiting for its compatible roster did not cancel cleanly", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("full root did not join its pre-readiness Run")
	}
	if err := worker.Close(); err != nil {
		test.Fatal("full root close", err)
	}
	if err := worker.Close(); err != nil {
		test.Fatal("repeated full root close", err)
	}
	if err := worker.Run(context.Background()); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("closed full root accepted Run", err)
	}
	if worker.Ready() || worker.Store.(*store.PostgresStore).Ping(context.Background()) == nil ||
		runtime.GOMAXPROCS(0) != priorCPU || debug.SetMemoryLimit(-1) != priorMemory {
		test.Fatal("closed full root retained its pool, readiness or process runtime settings")
	}
	fixture.waitRoleGone(test, outbound.ProcessWorker)
	test.Log("actual full root and enabled Complete ports constructed; this test does not claim job processing")
}

type wave48PGDormantGitHub struct {
	outbound.GitHubApp
}

func TestWave48PGFullRootOptionalGitHubUsesOneExplicitOwner(test *testing.T) {
	newWave48PGFixture(test)
	worker, err := OpenWorker(context.Background(), func(storage store.Store) (ApplicationWorkerConfig, error) {
		teams, ok := storage.(outbound.TeamStore)
		if !ok {
			return ApplicationWorkerConfig{}, domain.ErrCatalogUnsupported
		}
		transactions, ok := storage.(outbound.IdentityTransactions)
		if !ok {
			return ApplicationWorkerConfig{}, domain.ErrCatalogUnsupported
		}
		github, err := app.NewGitHubWorker(app.GitHubWorkerConfig{
			Profile: app.GitHubWorkerComplete, Store: storage, Remote: &wave48PGDormantGitHub{},
			Metadata: storage, Blobs: storage, Repositories: storage, Organizations: storage,
			Teams: teams, IdentityTransactions: transactions,
		})
		if err != nil {
			return ApplicationWorkerConfig{}, err
		}
		return ApplicationWorkerConfig{
			GitReader:    gitevidence.NewGitHub(func() string { return "" }),
			GitHubWorker: github, NotificationSender: webhookdelivery.New(),
		}, nil
	})
	if err != nil {
		test.Fatal("optional Complete owner assembly on actual PostgreSQL", err)
	}
	test.Cleanup(func() { _ = worker.Close() })
	bindings := worker.bindings()
	if len(bindings) != 18 || worker.Store != worker.infrastructure.Store {
		test.Fatal("optional full root lost one-store ownership or the existing worker set", len(bindings))
	}
	github := 0
	for _, binding := range bindings {
		if binding.name == "github" {
			github++
			if binding.run == nil {
				test.Fatal("optional GitHub owner has no explicit runner")
			}
		}
	}
	if github != 1 || worker.Ready() {
		test.Fatal("optional owner duplicated or bypassed compatible-peer readiness", github)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			test.Fatal("optional owner did not join before readiness", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("optional full worker could not cancel before readiness")
	}
}

func wave48PGErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrConflict):
		return "conflict"
	case errors.Is(err, domain.ErrValidation):
		return "validation"
	case errors.Is(err, domain.ErrResourceOverloaded):
		return "overloaded"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "unexpected"
	}
}

func TestWave48PGChild(test *testing.T) {
	role := outbound.ProcessRole(os.Getenv("CXT_WAVE48_PROCESS_ROLE"))
	if role == "" {
		test.Skip("entered only by owned full worker PostgreSQL process proofs")
	}
	if role != outbound.ProcessAPI && role != outbound.ProcessMCP && role != outbound.ProcessWorker {
		test.Fatal("invalid private full worker fixture role")
	}
	configuration, err := pgxpool.ParseConfig(os.Getenv("CXT_POSTGRES_DSN"))
	if err != nil {
		test.Fatal("invalid private full worker fixture DSN")
	}
	local := func(host string) bool {
		address := net.ParseIP(host)
		return host == "localhost" || filepath.IsAbs(host) || address != nil && address.IsLoopback()
	}
	database := os.Getenv("CXT_WAVE48_PROCESS_DATABASE")
	if !strings.HasPrefix(database, "cxt_execution_root_") || database != configuration.ConnConfig.Database || !local(configuration.ConnConfig.Host) {
		test.Fatal("child requires its parent's fresh owned local execution-root database")
	}
	for _, fallback := range configuration.ConnConfig.Fallbacks {
		if !local(fallback.Host) {
			test.Fatal("private full worker fixture has a nonlocal fallback")
		}
	}
	lifetime, stopLifetime := context.WithTimeout(context.Background(), 90*time.Second)
	defer stopLifetime()
	current, cancel := context.WithCancel(lifetime)
	defer cancel()
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	emit := func(event wave48PGEvent) {
		encoded, err := json.Marshal(event)
		if err != nil {
			test.Fatal("encode bounded private child event", err)
		}
		if _, err := fmt.Fprintf(os.Stdout, "CXT_W48 %s\n", encoded); err != nil {
			test.Fatal("write bounded private child event", err)
		}
	}
	var worker *WorkerRuntime
	var root *Runtime
	switch role {
	case outbound.ProcessWorker:
		worker, err = wave48PGOpenWorker(current)
	case outbound.ProcessAPI:
		root, err = Open(current, "127.0.0.1:0", os.Getenv("HOME"), true)
	case outbound.ProcessMCP:
		root, err = OpenMCP(current, "127.0.0.1:0", os.Getenv("HOME"), true)
	}
	if err != nil {
		emit(wave48PGEvent{State: "refused", Code: wave48PGErrorCode(err)})
		return
	}
	closeRoot := func() error {
		if worker != nil {
			return worker.Close()
		}
		root.Close()
		return nil
	}
	defer func() { _ = closeRoot() }()
	process := current
	bindings := 0
	if worker != nil {
		process = worker.ProcessContext(current)
		if os.Getenv("CXT_EXECUTION_PROFILE") == wave48PGProfile {
			wave48PGAssertFullRoot(test, worker)
			bindings = 5 + len(worker.additional)
		} else if worker.workers == nil || worker.maintenance == nil || len(worker.additional) != 0 {
			test.Fatal("document-only worker changed ownership")
		} else {
			bindings = 5
		}
	} else {
		process = root.ProcessContext(current)
		if !root.BoundedDocuments() || root.ExternalWorkers() != (os.Getenv("CXT_EXECUTION_PROFILE") == wave48PGProfile) ||
			root.execution.role.Role != role || root.Store != root.infrastructure.Store || root.DocumentQueries == nil || root.Identity == nil ||
			root.DocumentQueryProfile != documentquery.VerifiedRoots || (root.Context != nil) != (role == outbound.ProcessAPI) {
			test.Fatal("actual API/MCP root lost its command, query or external-worker boundary")
		}
	}
	emit(wave48PGEvent{State: "opened", Bindings: bindings})
	commands := make(chan wave48PGCommand)
	go func() {
		defer close(commands)
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 32<<10))
		for decoded := 0; decoded < 512; decoded++ {
			var command wave48PGCommand
			if decoder.Decode(&command) != nil {
				return
			}
			select {
			case commands <- command:
			case <-lifetime.Done():
				return
			}
		}
	}()
	var running chan error
	closing := false
	for !closing {
		select {
		case <-process.Done():
			closing = true
		case command, present := <-commands:
			if !present {
				cancel()
				closing = true
				continue
			}
			switch command.Operation {
			case "ready":
				ready := worker != nil && worker.Ready()
				if root != nil {
					response := httptest.NewRecorder()
					root.HealthHandler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/healthz", nil).WithContext(process))
					ready = response.Code == http.StatusOK
				}
				emit(wave48PGEvent{State: "ready", Ready: ready})
			case "probe":
				probe, stop := context.WithTimeout(process, 2*time.Second)
				_, release, err := app.AcquireResources(probe, app.ResourceBulk)
				if err == nil {
					release()
				}
				stop()
				emit(wave48PGEvent{State: "probe", Code: wave48PGErrorCode(err)})
			case "read":
				if root == nil {
					test.Fatal("worker root cannot impersonate public document query ownership")
				}
				read, stop := context.WithTimeout(wave48PGSystemContext(process), 5*time.Second)
				page, err := root.DocumentQueries.ReadDocEvents(read, domain.ContentHash(os.Getenv("CXT_WAVE48_REPOSITORY")), command.Hash, "", 0, 8)
				if err != nil {
					fmt.Fprintf(os.Stderr, "wave48 private read error (%s): %T %v\n", role, err, err)
				}
				stop()
				text := ""
				if err == nil {
					if page.Hash != command.Hash || page.Total != 1 || len(page.Events) != 1 {
						test.Fatal("actual API/MCP query returned a different durable document")
					}
					text = domain.SearchableEventText(page.Events[0])
				}
				emit(wave48PGEvent{State: "read", Code: wave48PGErrorCode(err), Text: text})
			case "run":
				if worker == nil || running != nil {
					test.Fatal("only the actual worker root may receive one Run request")
				}
				running = make(chan error, 1)
				go func() { running <- worker.Run(process) }()
				emit(wave48PGEvent{State: "run-requested"})
			case "cancel":
				cancel()
				closing = true
			case "close":
				closing = true
			default:
				test.Fatal("unsupported bounded private child command")
			}
		}
	}
	if err := closeRoot(); err != nil {
		test.Fatal("actual root close failed", err)
	}
	if running != nil {
		select {
		case err := <-running:
			if err != nil {
				test.Fatal("actual worker Run did not cancel and join cleanly", err)
			}
		case <-time.After(5 * time.Second):
			test.Fatal("actual worker Close returned without joining Run")
		}
	}
	if err := closeRoot(); err != nil {
		test.Fatal("repeated actual root close failed", err)
	}
	emit(wave48PGEvent{State: "closed", Canceled: process.Err() != nil, Joined: true, Closed: true,
		Restored: runtime.GOMAXPROCS(0) == priorCPU && debug.SetMemoryLimit(-1) == priorMemory})
	select {
	case command, present := <-commands:
		if !present || command.Operation != "exit" {
			test.Fatal("parent must acknowledge pool closure while the child is still alive")
		}
	case <-lifetime.Done():
		test.Fatal("bounded child close acknowledgement timed out")
	}
}

func (fixture *wave48PGFixture) start(test *testing.T, ctx context.Context, role outbound.ProcessRole, budget executionprofile.Budget) *wave48PGChild {
	test.Helper()
	encoded, err := json.Marshal(budget)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := executionprofile.Parse(encoded); err != nil {
		test.Fatal("private child budget is not valid", err)
	}
	budgetPath := runtimeExecutionBudgetFile(test, encoded)
	executable, err := os.Executable()
	if err != nil {
		test.Fatal("locate only the already built test executable", err)
	}
	directory := test.TempDir()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestWave48PGChild$", "-test.count=1", "-test.timeout=100s")
	command.Dir = directory
	command.Env = []string{"HOME=" + directory, "TMPDIR=" + directory, "LANG=C", "TZ=UTC",
		"CXT_WAVE48_PROCESS_ROLE=" + string(role), "CXT_WAVE48_PROCESS_DATABASE=" + fixture.database,
		"CXT_WAVE48_REPOSITORY=" + string(fixture.repo), "CXT_EXECUTION_PROFILE=" + budget.Profile,
		"CXT_EXECUTION_BUDGET_FILE=" + budgetPath, "CXT_POSTGRES_DSN=" + fixture.dsn,
		"CXT_MIGRATIONS_DIR=" + fixture.migrations, "CXT_AUTH=dev", "CXT_PUBLIC_URL=https://wave48.example.test",
		"CXT_CONVERSATION_ROOT_PUBLICATION=true", "CXT_PENDING_PROJECTION_REPOS=" + string(fixture.repo)}
	for _, name := range []string{"SystemRoot", "WINDIR"} {
		if value, present := os.LookupEnv(name); present {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	input, err := command.StdinPipe()
	if err != nil {
		test.Fatal("create private full worker command pipe", err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		test.Fatal("create private full worker event pipe", err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		test.Fatal("start only the existing test executable", err)
	}
	child := &wave48PGChild{role: role, command: command, input: input, events: make(chan wave48PGEvent, 128), done: make(chan struct{})}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(child.events)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "CXT_W48 ") {
				continue
			}
			var event wave48PGEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "CXT_W48 ")), &event) != nil {
				event = wave48PGEvent{State: "protocol-error"}
			}
			child.events <- event
		}
	}()
	go func() {
		<-drained
		child.waitErr = command.Wait()
		close(child.done)
	}()
	test.Cleanup(func() {
		_ = child.input.Close()
		select {
		case <-child.done:
		default:
			_ = child.command.Process.Kill()
			select {
			case <-child.done:
			case <-time.After(5 * time.Second):
				test.Error("owned full worker child was not reaped before fixture database cleanup")
			}
		}
	})
	return child
}

func (child *wave48PGChild) send(test *testing.T, operation string) {
	test.Helper()
	if err := json.NewEncoder(child.input).Encode(wave48PGCommand{Operation: operation}); err != nil {
		test.Fatal("send bounded child operation", child.role, operation, err)
	}
}

func (child *wave48PGChild) expect(test *testing.T, ctx context.Context, state string) wave48PGEvent {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case event, present := <-child.events:
		if !present || event.State != state {
			test.Fatal("actual isolated root returned a different event", child.role, state, event)
		}
		return event
	case <-deadline.Done():
		test.Fatal("bounded wait for actual isolated root", child.role, state)
		return wave48PGEvent{}
	}
}

func (child *wave48PGChild) join(test *testing.T, ctx context.Context) {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case <-child.done:
		if child.waitErr != nil {
			test.Fatal("actual isolated root failed", child.role, child.waitErr)
		}
	case <-deadline.Done():
		test.Fatal("actual isolated root did not exit after close acknowledgement", child.role)
	}
}

func (fixture *wave48PGFixture) waitRoleGone(test *testing.T, role outbound.ProcessRole) {
	test.Helper()
	executionRootWait(test, func(ctx context.Context) bool {
		var count int
		err := fixture.observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database()
 AND (application_name LIKE $1 OR application_name LIKE $2)`, "cxthub/bounded-documents-v1/"+string(role)+"/%", outbound.BoundedDocumentsControlPrefix+string(role)+"/%").Scan(&count)
		return err == nil && count == 0
	})
}

func (fixture *wave48PGFixture) stop(test *testing.T, ctx context.Context, child *wave48PGChild, operation string) {
	test.Helper()
	child.send(test, operation)
	event := child.expect(test, ctx, "closed")
	if !event.Canceled || !event.Joined || !event.Closed || !event.Restored {
		test.Fatal("actual child did not cancel, join, close and restore its process settings", child.role, event)
	}
	if err := child.command.Process.Signal(syscall.Signal(0)); err != nil {
		test.Fatal("pool cleanup must be witnessed before child process exit", child.role, err)
	}
	fixture.waitRoleGone(test, child.role)
	child.send(test, "exit")
	child.join(test, ctx)
}

func wave48PGWaitReady(test *testing.T, ctx context.Context, child *wave48PGChild, want bool) {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		child.send(test, "ready")
		if child.expect(test, deadline, "ready").Ready == want {
			return
		}
		select {
		case <-deadline.Done():
			test.Fatal("actual root readiness did not converge", child.role, want)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (fixture *wave48PGFixture) enqueue(test *testing.T, marker string) domain.DocFinalizationJob {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 5*time.Second)
	defer cancel()
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(fixture.repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: marker}}}}}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal(err)
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal(err)
	}
	root, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal(err)
	}
	if _, _, err := fixture.storage.PutChunks(ctx, fixture.repo, bodies); err != nil {
		test.Fatal("upload original useful conversation chunks", err)
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(fixture.repo,
		domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: root}, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	if _, err := fixture.storage.EnqueueDocJob(ctx, job); err != nil {
		test.Fatal("enqueue original durable job through the current production port", err)
	}
	return job
}

func (fixture *wave48PGFixture) assertQueued(test *testing.T, job domain.DocFinalizationJob) {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 2*time.Second)
	defer cancel()
	until := time.Now().Add(250 * time.Millisecond)
	for {
		current, err := fixture.storage.GetDocJob(ctx, fixture.repo, job.ID)
		if err != nil || current.Attempts != 0 || current.State != job.State || current.DocumentRef() != job.DocumentRef() {
			test.Fatal("assembly or an incompatible roster executed the durable job", current, err)
		}
		if present, err := fixture.storage.HasDocs(ctx, fixture.repo, []domain.ContentHash{job.DocHash}); err != nil || len(present) != 0 {
			test.Fatal("useful document was published before the compatible full worker Run", err)
		}
		if time.Now().After(until) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (fixture *wave48PGFixture) waitCompleted(test *testing.T, job domain.DocFinalizationJob) domain.DocFinalizationJob {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 15*time.Second)
	defer cancel()
	for {
		current, err := fixture.storage.GetDocJob(ctx, fixture.repo, job.ID)
		if err != nil {
			test.Fatal("read original durable receipt", err)
		}
		if current.State == "completed" {
			if current.Attempts < 1 || current.Version < 1 || current.DocumentRef() != job.DocumentRef() {
				test.Fatal("actual full worker completion lost the original identity or lease/version fence", current)
			}
			if present, err := fixture.storage.HasDocs(ctx, fixture.repo, []domain.ContentHash{job.DocHash}); err != nil || len(present) != 1 {
				test.Fatal("completed full worker receipt has no useful durable publication", err)
			}
			return current
		}
		if current.State == "rejected" {
			test.Fatal("actual full worker rejected the useful fixture document", current)
		}
		select {
		case <-ctx.Done():
			test.Fatal("actual compatible full worker Run did not process its durable job", current.State)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (fixture *wave48PGFixture) assertRead(test *testing.T, ctx context.Context, child *wave48PGChild, job domain.DocFinalizationJob, marker string) {
	test.Helper()
	if err := json.NewEncoder(child.input).Encode(wave48PGCommand{Operation: "read", Hash: job.DocHash}); err != nil {
		test.Fatal(err)
	}
	event := child.expect(test, ctx, "read")
	if event.Code != "" || event.Text != marker {
		test.Fatal("actual API/MCP query did not preserve durable document contents", child.role, event)
	}
}

func (fixture *wave48PGFixture) enqueuePendingProjection(test *testing.T, job domain.DocFinalizationJob) domain.Pending {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 5*time.Second)
	defer cancel()
	pending := domain.Pending{RepoID: fixture.repo, SessionID: "wave48-durable-pending", Target: job.DocHash,
		Provider: domain.ProviderCodex, Branch: "main", UpdatedAt: time.Now().UTC()}
	fixture.publishSnapshot(test, job)
	if err := fixture.storage.PutPending(ctx, fixture.repo, pending); err != nil {
		test.Fatal("persist useful non-document work while the full worker is stopped", err)
	}
	if err := fixture.storage.ConfigurePendingProjection(ctx, fixture.repo, true); err != nil {
		test.Fatal("retain the existing pending projection work queue", err)
	}
	return pending
}

func (fixture *wave48PGFixture) publishSnapshot(test *testing.T, job domain.DocFinalizationJob) {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 5*time.Second)
	defer cancel()
	if err := fixture.storage.PutSnapshot(ctx, domain.Snapshot{ID: job.DocHash, RepoID: fixture.repo, DocHash: job.DocHash,
		DocIdentity: job.DocIdentity, SessionID: "wave48-durable-pending", Branch: "main",
		Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: job.CreatedAt}); err != nil {
		test.Fatal("link completed document to its original context snapshot through the production port", err)
	}
}

func (fixture *wave48PGFixture) waitPendingProjection(test *testing.T, pending domain.Pending) {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 15*time.Second)
	defer cancel()
	for {
		projection, err := fixture.storage.ReadPendingProjection(ctx, fixture.repo)
		if err != nil {
			test.Fatal("read original durable pending projection", err)
		}
		if projection.NeedsAttention {
			test.Fatal("actual full worker rejected the useful pending projection")
		}
		if projection.Ready {
			view, err := domain.DecodePendingProjection(ctx, fixture.repo, projection.Parts)
			if err != nil || !projection.Enabled || projection.Dataset == "" || len(view.Pending) != 1 ||
				view.Pending[0].SessionID != pending.SessionID || view.Pending[0].Target != pending.Target {
				test.Fatal("actual additional full worker published different durable pending contents", view, err)
			}
			test.Log("actual additional full worker published the original durable pending projection after restart")
			return
		}
		select {
		case <-ctx.Done():
			test.Fatal("actual additional full worker did not process its durable pending projection")
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (fixture *wave48PGFixture) assertPendingQueued(test *testing.T) {
	test.Helper()
	ctx, cancel := context.WithTimeout(wave48PGSystemContext(context.Background()), 5*time.Second)
	defer cancel()
	projection, err := fixture.storage.ReadPendingProjection(ctx, fixture.repo)
	if err != nil || !projection.Enabled || projection.Ready || projection.NeedsAttention {
		test.Fatal("assembly executed or rejected the pending projection before full worker Run", projection, err)
	}
}

func TestWave48PGFullRootDurableProcessingAcrossRestart(test *testing.T) {
	fixture := newWave48PGFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	firstMarker := "wave48 original full worker publication 한🙂"
	first := fixture.enqueue(test, firstMarker)
	initial := fixture.start(test, ctx, outbound.ProcessWorker, fixture.budget)
	if event := initial.expect(test, ctx, "opened"); event.Bindings != 17 {
		test.Fatal("public full worker root did not construct all seventeen actual default owners", event)
	}
	initial.send(test, "run")
	initial.expect(test, ctx, "run-requested")
	wave48PGWaitReady(test, ctx, initial, false)
	fixture.assertQueued(test, first)
	fixture.stop(test, ctx, initial, "cancel")
	api := fixture.start(test, ctx, outbound.ProcessAPI, fixture.budget)
	api.expect(test, ctx, "opened")
	mcp := fixture.start(test, ctx, outbound.ProcessMCP, fixture.budget)
	mcp.expect(test, ctx, "opened")
	worker := fixture.start(test, ctx, outbound.ProcessWorker, fixture.budget)
	worker.expect(test, ctx, "opened")
	for _, child := range []*wave48PGChild{api, mcp, worker} {
		wave48PGWaitReady(test, ctx, child, true)
	}
	fixture.assertQueued(test, first)
	worker.send(test, "run")
	worker.expect(test, ctx, "run-requested")
	completed := fixture.waitCompleted(test, first)
	fixture.publishSnapshot(test, first)
	fixture.assertRead(test, ctx, api, first, firstMarker)
	fixture.assertRead(test, ctx, mcp, first, firstMarker)
	fixture.stop(test, ctx, worker, "close")
	wave48PGWaitReady(test, ctx, api, false)
	pending := fixture.enqueuePendingProjection(test, first)
	fixture.assertPendingQueued(test)
	secondMarker := "wave48 useful publication after clean full worker restart"
	second := fixture.enqueue(test, secondMarker)
	fixture.assertQueued(test, second)
	restarted := fixture.start(test, ctx, outbound.ProcessWorker, fixture.budget)
	restarted.expect(test, ctx, "opened")
	for _, child := range []*wave48PGChild{api, mcp, restarted} {
		wave48PGWaitReady(test, ctx, child, true)
	}
	fixture.assertQueued(test, second)
	fixture.assertPendingQueued(test)
	restarted.send(test, "run")
	restarted.expect(test, ctx, "run-requested")
	fixture.waitCompleted(test, second)
	fixture.publishSnapshot(test, second)
	fixture.waitPendingProjection(test, pending)
	fixture.assertRead(test, ctx, api, first, firstMarker)
	fixture.assertRead(test, ctx, mcp, second, secondMarker)
	current, err := fixture.storage.GetDocJob(wave48PGSystemContext(ctx), fixture.repo, first.ID)
	if err != nil || current.State != "completed" || current.Attempts != completed.Attempts || current.Version != completed.Version || current.DocumentRef() != completed.DocumentRef() {
		test.Fatal("full worker restart replayed or replaced an already completed durable receipt", current, err)
	}
	fixture.stop(test, ctx, restarted, "cancel")
	fixture.stop(test, ctx, mcp, "close")
	fixture.stop(test, ctx, api, "close")
	test.Logf("actual public full worker Run completed two original durable jobs across restart; first attempts=%d version=%d", completed.Attempts, completed.Version)
}

func (fixture *wave48PGFixture) assertAdmissionFamily(test *testing.T, fingerprints map[outbound.ProcessRole]string) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var controls, roles, outside int
	if err := fixture.observer.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=ANY($1::text[])),
 (SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database())
  AND classid::bigint=1129862210 AND objid IN (10,11,12) AND objsubid=2 AND granted),
 (SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend'
  AND application_name NOT LIKE 'cxthub/bounded-documents-v1/%' AND application_name NOT LIKE 'cxthub/bd1/%')`, []string{
		outbound.BoundedDocumentsControlPrefix + "api/" + fingerprints[outbound.ProcessAPI],
		outbound.BoundedDocumentsControlPrefix + "mcp/" + fingerprints[outbound.ProcessMCP],
		outbound.BoundedDocumentsControlPrefix + "worker/" + fingerprints[outbound.ProcessWorker],
	}).Scan(&controls, &roles, &outside); err != nil || controls != 3 || roles != 3 || outside != 0 {
		test.Fatal("full or mixed profiles escaped the existing three-role PostgreSQL admission/session family", controls, roles, outside, err)
	}
}

func TestWave48PGMixedProfilesFingerprintsAndRoleCollisionFailClosed(test *testing.T) {
	fixture := newWave48PGFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	job := fixture.enqueue(test, "wave48 mixed deployments must not execute this durable job")
	worker := fixture.start(test, ctx, outbound.ProcessWorker, fixture.budget)
	worker.expect(test, ctx, "opened")
	worker.send(test, "run")
	worker.expect(test, ctx, "run-requested")
	documents := fixture.budget
	documents.Profile = outbound.BoundedDocumentsProfile
	for _, budget := range []executionprofile.Budget{fixture.budget, documents} {
		duplicate := fixture.start(test, ctx, outbound.ProcessWorker, budget)
		if event := duplicate.expect(test, ctx, "refused"); event.Code != "conflict" {
			test.Fatal("full/document profile duplicate worker escaped the shared role collision fence", event)
		}
		duplicate.join(test, ctx)
	}
	api := fixture.start(test, ctx, outbound.ProcessAPI, documents)
	api.expect(test, ctx, "opened")
	mcp := fixture.start(test, ctx, outbound.ProcessMCP, documents)
	mcp.expect(test, ctx, "opened")
	documentBudgetFingerprint, err := documents.Fingerprint()
	if err != nil {
		test.Fatal("worker profile did not participate in the admission fingerprint", err)
	}
	documentFingerprint, err := store.ProcessAdmissionFingerprint(documentBudgetFingerprint)
	if err != nil || documentFingerprint == fixture.fingerprint {
		test.Fatal("worker profile did not participate in the admission fingerprint", err)
	}
	fixture.assertAdmissionFamily(test, map[outbound.ProcessRole]string{
		outbound.ProcessAPI: documentFingerprint, outbound.ProcessMCP: documentFingerprint, outbound.ProcessWorker: fixture.fingerprint,
	})
	for _, child := range []*wave48PGChild{api, mcp, worker} {
		wave48PGWaitReady(test, ctx, child, false)
		child.send(test, "probe")
		if event := child.expect(test, ctx, "probe"); event.Code != "overloaded" {
			test.Fatal("mixed document/full profile roster admitted heavy work", child.role, event)
		}
	}
	fixture.assertQueued(test, job)
	fixture.stop(test, ctx, mcp, "close")
	fixture.stop(test, ctx, api, "close")
	different := fixture.budget
	different.BaselineSpoolBytes++
	api = fixture.start(test, ctx, outbound.ProcessAPI, different)
	api.expect(test, ctx, "opened")
	mcp = fixture.start(test, ctx, outbound.ProcessMCP, fixture.budget)
	mcp.expect(test, ctx, "opened")
	differentBudgetFingerprint, err := different.Fingerprint()
	if err != nil {
		test.Fatal("different explicit resource manifest retained the same fingerprint", err)
	}
	differentFingerprint, err := store.ProcessAdmissionFingerprint(differentBudgetFingerprint)
	if err != nil || differentFingerprint == fixture.fingerprint {
		test.Fatal("different explicit resource manifest retained the same fingerprint", err)
	}
	fixture.assertAdmissionFamily(test, map[outbound.ProcessRole]string{
		outbound.ProcessAPI: differentFingerprint, outbound.ProcessMCP: fixture.fingerprint, outbound.ProcessWorker: fixture.fingerprint,
	})
	for _, child := range []*wave48PGChild{api, mcp, worker} {
		wave48PGWaitReady(test, ctx, child, false)
		child.send(test, "probe")
		if event := child.expect(test, ctx, "probe"); event.Code != "overloaded" {
			test.Fatal("different full profile fingerprints admitted heavy work", child.role, event)
		}
	}
	fixture.assertQueued(test, job)
	fixture.stop(test, ctx, worker, "cancel")
	fixture.stop(test, ctx, mcp, "close")
	fixture.stop(test, ctx, api, "close")
}
