//go:build postgres

package serverruntime_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const executionProcessObserverTag = "cxthub/bounded-documents-v1/observer"

type executionProcessFixture struct {
	dsn         string
	database    string
	migrations  string
	budgetPath  string
	fingerprint string
	profile     string
	storage     *store.PostgresStore
	observer    *pgx.Conn
	repo        domain.ContentHash
	foreign     domain.ContentHash
	job         domain.DocFinalizationJob
	session     string
	mcpToken    string
	marker      string
}

type executionProcessChild struct {
	role    outbound.ProcessRole
	command *exec.Cmd
	input   io.WriteCloser
	events  chan executionProcessEvent
	done    chan struct{}
	waitErr error
	pending []executionProcessEvent
	url     string
}

func executionProcessBaseContext(ctx context.Context) func(net.Listener) context.Context {
	return func(net.Listener) context.Context { return ctx }
}

func executionProcessLocalConfig(test *testing.T, dsn string) *pgxpool.Config {
	test.Helper()
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("cannot parse private-fixture PostgreSQL configuration")
	}
	local := func(host string) bool {
		if filepath.IsAbs(host) || host == "localhost" {
			return true
		}
		address := net.ParseIP(host)
		return address != nil && address.IsLoopback()
	}
	if !local(config.ConnConfig.Host) {
		test.Fatal("bounded process fixture refuses non-local PostgreSQL")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		if !local(fallback.Host) {
			test.Fatal("bounded process fixture refuses non-local PostgreSQL fallbacks")
		}
	}
	config.MaxConns = 2
	config.MinConns = 0
	config.ConnConfig.ConnectTimeout = 3 * time.Second
	for key := range config.ConnConfig.RuntimeParams {
		if strings.HasPrefix(key, "pool_") {
			delete(config.ConnConfig.RuntimeParams, key)
		}
	}
	config.ConnConfig.RuntimeParams["application_name"] = executionProcessObserverTag
	return config
}

func executionProcessDatabaseDSN(test *testing.T, dsn, database string) string {
	test.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			test.Fatal("cannot parse private-fixture PostgreSQL URL")
		}
		parsed.Path = "/" + database
		parsed.RawPath = ""
		query := parsed.Query()
		query.Set("dbname", database)
		query.Set("application_name", executionProcessObserverTag)
		query.Set("pool_max_conns", "2")
		query.Set("pool_min_conns", "0")
		query.Set("pool_min_idle_conns", "0")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " dbname='" + database + "' application_name='" + executionProcessObserverTag + "' pool_max_conns=2 pool_min_conns=0 pool_min_idle_conns=0"
	}
	if executionProcessLocalConfig(test, dsn).ConnConfig.Database != database {
		test.Fatal("private-fixture DSN did not select its newly owned database")
	}
	return dsn
}

func newExecutionProcessFixture(test *testing.T, ctx context.Context) *executionProcessFixture {
	test.Helper()
	return newExecutionProcessFixtureWithMigrations(test, ctx, "")
}

func newExecutionProcessFixtureWithMigrations(test *testing.T, ctx context.Context, migrations string) *executionProcessFixture {
	test.Helper()
	dsn := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset; no owned local process fixture created")
	}
	configuration := executionProcessLocalConfig(test, dsn)
	admin, err := pgx.ConnectConfig(ctx, configuration.ConnConfig.Copy())
	if err != nil {
		test.Fatal("cannot connect to the explicitly selected local fixture administrator")
	}
	database := domain.NewID("bounded_process_")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		_ = admin.Close(context.Background())
		test.Fatal("cannot create the fresh owned process database")
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		var sessions int
		if err := admin.QueryRow(cleanup, `SELECT count(*) FROM pg_stat_activity WHERE datname=$1`, database).Scan(&sessions); err != nil || sessions != 0 {
			test.Error("owned fixture retained sessions after child reaping and pool close", sessions)
		}
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			test.Error("cannot drop the owned process database after reaping children")
		}
		if err := admin.Close(cleanup); err != nil {
			test.Error("close local fixture administrator")
		}
	})
	dsn = executionProcessDatabaseDSN(test, dsn, database)
	config := executionProcessLocalConfig(test, dsn)
	observer, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal("cannot connect the tagged private-database observer")
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(cleanup); err != nil {
			test.Error("close private-database observer")
		}
	})
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal("cannot open the tagged fixture seed store")
	}
	test.Cleanup(storage.Close)
	if migrations == "" {
		migrations, err = filepath.Abs("../../../schemas/db/migrations")
		if err != nil {
			test.Fatal("resolve explicit fixture migration directory")
		}
	}
	if _, err := storage.ApplyMigrations(ctx, migrations); err != nil {
		test.Fatal("apply actual schema to only the fresh fixture database")
	}
	budget := executionprofile.Budget{
		Version: 1, Profile: outbound.BoundedDocumentsProfile,
		BaselineActive: 4, BaselineBulk: 2, BaselineConnections: 20, ReservedConnections: 7,
		BaselineCPU: 3, BaselineMemoryBytes: 800 << 20, BaselineSpoolBytes: 64 << 20,
		Roles: []executionprofile.RoleBudget{
			{Role: outbound.ProcessAPI, Instances: 1, Active: 2, Bulk: 1, DataPoolMax: 3, CoordinationPoolMax: 2, RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 8 << 20},
			{Role: outbound.ProcessMCP, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2, RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 8 << 20},
			{Role: outbound.ProcessWorker, Instances: 1, Active: 1, Bulk: 1, DataPoolMax: 2, CoordinationPoolMax: 2, RenewalReserve: 1, Queue: 4, CPU: 1, MemoryBytes: 192 << 20, SpoolBytes: 8 << 20},
		},
	}
	encoded, err := json.Marshal(budget)
	if err != nil {
		test.Fatal("encode explicit fixture-only budget")
	}
	if _, err := executionprofile.Parse(encoded); err != nil {
		test.Fatal("fixture-only declared budget failed manifest validation")
	}
	budgetFingerprint, err := budget.Fingerprint()
	if err != nil {
		test.Fatal(err)
	}
	fingerprint, err := store.ProcessAdmissionFingerprint(budgetFingerprint)
	if err != nil {
		test.Fatal(err)
	}
	budgetPath := filepath.Join(test.TempDir(), "bounded-documents-fixture.json")
	if err := os.WriteFile(budgetPath, encoded, 0600); err != nil {
		test.Fatal("write explicit private fixture budget")
	}
	fixture := &executionProcessFixture{dsn: dsn, database: database, migrations: migrations, budgetPath: budgetPath, fingerprint: fingerprint, storage: storage, observer: observer, marker: "wave39 durable worker content 한🙂"}
	fixture.seed(test, ctx)
	return fixture
}

func executionProcessSystemContext(ctx context.Context) context.Context {
	identities := []domain.DocumentIdentity{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1}
	return outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), identities), identities, identities)
}

func (fixture *executionProcessFixture) seed(test *testing.T, ctx context.Context) {
	test.Helper()
	ctx = executionProcessSystemContext(ctx)
	identity := app.NewIdentityService(auth.NewDevVerifier(), fixture.storage)
	owner, session, err := identity.Login(ctx, "dev:"+domain.NewID("bounded-owner-")+"@example.test", "owned process fixture")
	if err != nil {
		test.Fatal("create actual fixture owner and session")
	}
	foreignOwner, _, err := identity.Login(ctx, "dev:"+domain.NewID("bounded-foreign-")+"@example.test", "foreign fixture owner")
	if err != nil {
		test.Fatal("create actual unrelated fixture owner")
	}
	for index, user := range []domain.User{owner, foreignOwner} {
		record, err := identity.CreateRepository(ctx, user, "Bounded-documents")
		if err != nil {
			test.Fatal("create actual private repository authority")
		}
		repo := domain.HashContent([]byte(record.ID))
		if _, err := fixture.storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: record.ID, DefaultBranch: "main"}); err != nil {
			test.Fatal("bind fixture content repository to its real authority")
		}
		if index == 0 {
			fixture.repo = repo
		} else {
			fixture.foreign = repo
		}
	}
	fixture.session = session.Token
	pair, err := identity.IssueMCPTokenPair(ctx, owner.ID, domain.NewID("bounded-mcp-"), "https://bounded.example.test/mcp")
	if err != nil {
		test.Fatal("issue actual fixture-scoped MCP access token")
	}
	fixture.mcpToken = pair.AccessToken
	if err := fixture.storage.RequireDocumentIdentity(ctx, fixture.repo, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal("declare actual root document identity")
	}
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(fixture.repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events:   []domain.CIREvent{{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: fixture.marker}}}},
	}
	manifest, bodies, err := domain.ConversationManifestForCIR(document)
	if err != nil {
		test.Fatal("construct useful document root")
	}
	hash, err := domain.ConversationManifestHash(manifest)
	if err != nil {
		test.Fatal("hash useful document root")
	}
	root, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		test.Fatal("encode useful document root")
	}
	if _, _, err := fixture.storage.PutChunks(ctx, fixture.repo, bodies); err != nil {
		test.Fatal("upload only original document chunks")
	}
	job, err := domain.NewDocFinalizationJobForRepresentation(fixture.repo, domain.DocumentRepresentation{Hash: hash, Identity: domain.DocumentIdentityRootV1, RootManifest: root}, time.Now().UTC())
	if err != nil {
		test.Fatal("construct original durable document job")
	}
	if _, err := fixture.storage.EnqueueDocJob(ctx, job); err != nil {
		test.Fatal("enqueue original durable document job")
	}
	fixture.job = job
	if have, err := fixture.storage.HasDocs(ctx, fixture.repo, []domain.ContentHash{hash}); err != nil || len(have) != 0 {
		test.Fatal("fixture published content without executing an actual worker")
	}
}

func (fixture *executionProcessFixture) start(test *testing.T, ctx context.Context, role outbound.ProcessRole) *executionProcessChild {
	test.Helper()
	executable, err := os.Executable()
	if err != nil {
		test.Fatal("locate the already built test executable")
	}
	return fixture.startExecutable(test, ctx, role, executable)
}

func (fixture *executionProcessFixture) startExecutable(test *testing.T, ctx context.Context, role outbound.ProcessRole, executable string) *executionProcessChild {
	test.Helper()
	return fixture.startExecutableChild(test, ctx, role, executable, "TestBoundedExecutionProcessPGChild")
}

func (fixture *executionProcessFixture) startExecutableChild(test *testing.T, ctx context.Context, role outbound.ProcessRole, executable, childTest string) *executionProcessChild {
	test.Helper()
	profile := fixture.profile
	if profile == "" {
		profile = outbound.BoundedDocumentsProfile
	}
	directory := test.TempDir()
	command := exec.CommandContext(ctx, executable, "-test.run=^"+childTest+"$", "-test.count=1", "-test.timeout=40s")
	command.Dir = directory
	command.Env = []string{"HOME=" + directory, "TMPDIR=" + directory, "LANG=C", "TZ=UTC"}
	for _, name := range []string{"SystemRoot", "WINDIR"} {
		if value, present := os.LookupEnv(name); present {
			command.Env = append(command.Env, name+"="+value)
		}
	}
	command.Env = append(command.Env,
		"CXT_WAVE39_PROCESS_ROLE="+string(role),
		"CXT_WAVE39_PROCESS_DATABASE="+fixture.database,
		"CXT_EXECUTION_PROFILE="+profile,
		"CXT_EXECUTION_BUDGET_FILE="+fixture.budgetPath,
		"CXT_POSTGRES_DSN="+fixture.dsn,
		"CXT_MIGRATIONS_DIR="+fixture.migrations,
		"CXT_AUTH=dev",
		"CXT_PUBLIC_URL=https://bounded.example.test",
		"CXT_CONVERSATION_ROOT_PUBLICATION=true",
	)
	command.Stderr = io.Discard
	input, err := command.StdinPipe()
	if err != nil {
		test.Fatal("create private child command pipe")
	}
	output, err := command.StdoutPipe()
	if err != nil {
		_ = input.Close()
		test.Fatal("create private child event pipe")
	}
	if err := command.Start(); err != nil {
		_ = input.Close()
		_ = output.Close()
		test.Fatal("start only the existing test executable")
	}
	child := &executionProcessChild{role: role, command: command, input: input, events: make(chan executionProcessEvent, 128), done: make(chan struct{})}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		defer close(child.events)
		scanner := bufio.NewScanner(output)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "CXT_W39 ") {
				continue
			}
			var event executionProcessEvent
			if json.Unmarshal([]byte(strings.TrimPrefix(line, "CXT_W39 ")), &event) != nil {
				event = executionProcessEvent{State: "protocol-error"}
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
				test.Error("owned process child was not reaped before database cleanup")
			}
		}
	})
	return child
}

func (child *executionProcessChild) send(test *testing.T, command executionProcessCommand) {
	test.Helper()
	if err := json.NewEncoder(child.input).Encode(command); err != nil {
		test.Fatal("send private owned-child command", child.role, command.Operation)
	}
}

func (child *executionProcessChild) expect(test *testing.T, ctx context.Context, state, token string) executionProcessEvent {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		for index, event := range child.pending {
			if event.State == state && event.Token == token {
				child.pending = append(child.pending[:index], child.pending[index+1:]...)
				return event
			}
		}
		select {
		case event, present := <-child.events:
			if !present {
				test.Fatal("owned child exited before the required event", child.role, state, token)
			}
			if event.State == "opened" {
				child.url = event.URL
			}
			if event.PID != child.command.Process.Pid {
				test.Fatal("child event came from a different OS process")
			}
			if event.State == state && event.Token == token {
				return event
			}
			if event.State == "refused" || event.State == "protocol-error" || event.Token == token && (event.State == "acquired" && state == "rejected" || event.State == "rejected" && state == "acquired") {
				test.Fatal("unexpected owned-child outcome", child.role, state, event.State, event.Code)
			}
			child.pending = append(child.pending, event)
		case <-deadline.Done():
			test.Fatal("timed out awaiting owned-child evidence", child.role, state, token)
		}
	}
}

func (child *executionProcessChild) assertReady(test *testing.T, ctx context.Context, want bool) {
	test.Helper()
	token := domain.NewID("ready-")
	child.send(test, executionProcessCommand{Operation: "ready", Token: token})
	if event := child.expect(test, ctx, "ready", token); event.Ready != want {
		test.Fatal("real root readiness did not reflect the exact role roster", child.role, event.Ready, want)
	}
}

func (child *executionProcessChild) waitReady(test *testing.T, ctx context.Context, want bool) {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		token := domain.NewID("ready-")
		child.send(test, executionProcessCommand{Operation: "ready", Token: token})
		if event := child.expect(test, deadline, "ready", token); event.Ready == want {
			return
		}
		select {
		case <-deadline.Done():
			test.Fatal("real root did not observe the role roster transition", child.role, want)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (child *executionProcessChild) hold(test *testing.T, ctx context.Context, token string, bulk bool) {
	test.Helper()
	child.send(test, executionProcessCommand{Operation: "hold", Token: token, Bulk: bulk, TimeoutMS: 10000})
	child.expect(test, ctx, "acquired", token)
}

func (child *executionProcessChild) reject(test *testing.T, ctx context.Context, token string, bulk bool) {
	test.Helper()
	child.send(test, executionProcessCommand{Operation: "hold", Token: token, Bulk: bulk, TimeoutMS: 250})
	event := child.expect(test, ctx, "rejected", token)
	if event.Code != "overloaded" && event.Code != "canceled" {
		test.Fatal("unavailable admission lost the supported error convention", child.role, event.Code)
	}
}

func (child *executionProcessChild) release(test *testing.T, ctx context.Context, token string) {
	test.Helper()
	child.send(test, executionProcessCommand{Operation: "release", Token: token})
	child.expect(test, ctx, "released", token)
}

func (child *executionProcessChild) join(test *testing.T, ctx context.Context, killed bool) {
	test.Helper()
	select {
	case <-child.done:
		if (child.waitErr != nil) != killed {
			test.Fatal("owned child exit did not match graceful/forced termination", child.role)
		}
		test.Logf("reaped actual %s root pid=%d user_cpu=%s system_cpu=%s", child.role, child.command.Process.Pid, child.command.ProcessState.UserTime(), child.command.ProcessState.SystemTime())
	case <-ctx.Done():
		test.Fatal("owned child failed to reap within the scoped proof deadline", child.role)
	}
}

func (child *executionProcessChild) terminateAndAcknowledge(test *testing.T, ctx context.Context, fixture *executionProcessFixture) {
	test.Helper()
	if err := child.command.Process.Signal(syscall.SIGTERM); err != nil {
		test.Fatal("send SIGTERM only to the owned role child", child.role)
	}
	child.acknowledgeClosed(test, ctx, fixture)
}

func (child *executionProcessChild) acknowledgeClosed(test *testing.T, ctx context.Context, fixture *executionProcessFixture) {
	test.Helper()
	event := child.expect(test, ctx, "closed", "")
	if !event.Canceled || !event.Joined || !event.Closed {
		test.Fatal("child acknowledged before runtime cancellation, joins and pool close", child.role)
	}
	if err := child.command.Process.Signal(syscall.Signal(0)); err != nil {
		test.Fatal("close evidence was not emitted while the child remained alive", child.role)
	}
	fixture.waitRoleSessionsGone(test, ctx, child.role)
	child.send(test, executionProcessCommand{Operation: "exit"})
	child.join(test, ctx, false)
}

func (fixture *executionProcessFixture) controlName(role outbound.ProcessRole) string {
	return outbound.BoundedDocumentsControlPrefix + string(role) + "/" + fixture.fingerprint
}

func (fixture *executionProcessFixture) waitRoleSessionsGone(test *testing.T, ctx context.Context, role outbound.ProcessRole) {
	test.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var sessions int
		if err := fixture.observer.QueryRow(deadline, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND (application_name LIKE $1 OR application_name=$2)`,
			"cxthub/bounded-documents-v1/"+string(role)+"/%", fixture.controlName(role)).Scan(&sessions); err != nil {
			test.Fatal("observe only owned role session cleanup")
		}
		if sessions == 0 {
			return
		}
		select {
		case <-deadline.Done():
			test.Fatal("closed or killed role retained a PostgreSQL session", role, sessions)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (fixture *executionProcessFixture) assertSessions(test *testing.T, ctx context.Context, roles, bulk int) {
	test.Helper()
	rows, err := fixture.observer.Query(ctx, `SELECT application_name,count(*) FROM pg_stat_activity WHERE datname=current_database() AND backend_type='client backend' GROUP BY application_name`)
	if err != nil {
		test.Fatal("observe owned role data/control/bulk connections")
	}
	counts := make(map[string]int)
	total := 0
	for rows.Next() {
		var tag string
		var count int
		if err := rows.Scan(&tag, &count); err != nil {
			rows.Close()
			test.Fatal("decode owned session counts")
		}
		counts[tag] = count
		total += count
	}
	rows.Close()
	if rows.Err() != nil {
		test.Fatal("read owned session counts")
	}
	controls := 0
	for tag, count := range counts {
		if tag == executionProcessObserverTag {
			if count > 7 {
				test.Fatal("fixture observers exceeded the explicit operator reserve", count)
			}
			continue
		}
		parts := strings.Split(tag, "/")
		if len(parts) != 4 || parts[0] != "cxthub" {
			test.Fatal("fresh fixture contains an unaccounted client session")
		}
		kind := parts[3]
		if parts[1] == "bd1" && tag == fixture.controlName(outbound.ProcessRole(parts[2])) {
			kind = "control"
		} else if parts[1] != outbound.BoundedDocumentsProfile || (kind != "data" && kind != "bulk") {
			test.Fatal("fresh fixture contains an unqualified budget/session witness")
		}
		limit := 2
		if parts[2] == "api" {
			limit = 3
		} else if parts[2] != "mcp" && parts[2] != "worker" {
			test.Fatal("fresh fixture contains an unregistered role tag")
		}
		switch kind {
		case "data":
			if count > limit {
				test.Fatal("actual role data pool exceeded its explicit cap", parts[2], count, limit)
			}
		case "control":
			controls += count
			if count != 1 {
				test.Fatal("role retained more than one control session", parts[2], count)
			}
		case "bulk":
			if count > 1 {
				test.Fatal("role retained more than one bulk session", parts[2], count)
			}
		default:
			test.Fatal("fresh fixture contains an unknown coordination session")
		}
		if counts[fixture.controlName(outbound.ProcessRole(parts[2]))]+counts["cxthub/bounded-documents-v1/"+parts[2]+"/bulk"] > 2 {
			test.Fatal("actual role exceeded control-plus-one-bulk coordination cap", parts[2])
		}
	}
	if controls != roles || total > 20 {
		test.Fatal("actual roster or connection budget differs from the explicit manifest", controls, roles, total)
	}
	locks, err := fixture.observer.Query(ctx, `SELECT objid::bigint,count(*) FROM pg_locks WHERE locktype='advisory' AND database=(SELECT oid FROM pg_database WHERE datname=current_database()) AND classid::bigint=1129862210 AND objsubid=2 AND granted GROUP BY objid`)
	if err != nil {
		test.Fatal("observe actual database-scoped process session locks")
	}
	roleLocks, bulkLocks := 0, 0
	for locks.Next() {
		var identifier int64
		var count int
		if err := locks.Scan(&identifier, &count); err != nil {
			locks.Close()
			test.Fatal("decode actual process session locks")
		}
		if count != 1 {
			locks.Close()
			test.Fatal("role or shared bulk session lock was granted more than once")
		}
		switch identifier {
		case 10, 11, 12:
			roleLocks += count
		case 20, 21:
			bulkLocks += count
		default:
			locks.Close()
			test.Fatal("unexpected bounded-profile session lock")
		}
	}
	locks.Close()
	if locks.Err() != nil || roleLocks != roles || bulkLocks != bulk {
		test.Fatal("actual role/shared-bulk lock evidence differs", roleLocks, roles, bulkLocks, bulk)
	}
	test.Logf("owned PG sample roles=%d bulk_session_locks=%d client_sessions=%d declared_connection_budget=20", roleLocks, bulkLocks, total)
}

func (fixture *executionProcessFixture) terminateControl(test *testing.T, ctx context.Context, role outbound.ProcessRole) {
	test.Helper()
	var terminated bool
	if err := fixture.observer.QueryRow(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1`, fixture.controlName(role)).Scan(&terminated); err != nil || !terminated {
		test.Fatal("terminate exactly the owned child control session", role)
	}
}

func (fixture *executionProcessFixture) waitDocumentJob(test *testing.T, ctx context.Context) {
	test.Helper()
	deadline, cancel := context.WithTimeout(executionProcessSystemContext(ctx), 10*time.Second)
	defer cancel()
	for {
		job, err := fixture.storage.GetDocJob(deadline, fixture.repo, fixture.job.ID)
		if err != nil {
			test.Fatal("read original durable document receipt")
		}
		if job.State == "completed" {
			if job.Attempts < 1 || job.Version < 1 || job.DocumentRef() != fixture.job.DocumentRef() {
				test.Fatal("worker publication lost original identity or lease/version receipt")
			}
			have, err := fixture.storage.HasDocs(deadline, fixture.repo, []domain.ContentHash{fixture.job.DocHash})
			if err != nil || len(have) != 1 {
				test.Fatal("actual worker completed without useful durable publication")
			}
			test.Logf("actual worker root completed original document receipt attempts=%d version=%d", job.Attempts, job.Version)
			return
		}
		if job.State == "rejected" {
			test.Fatal("actual worker rejected the useful fixture document")
		}
		select {
		case <-deadline.Done():
			test.Fatal("actual worker Run did not process its durable document job", job.State)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (fixture *executionProcessFixture) publishSnapshot(test *testing.T, ctx context.Context) {
	test.Helper()
	ctx = executionProcessSystemContext(ctx)
	hash := fixture.job.DocHash
	if err := fixture.storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: fixture.repo, DocHash: hash, DocIdentity: domain.DocumentIdentityRootV1,
		Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull, CreatedAt: time.Now().UTC()}); err != nil {
		test.Fatal("publish original snapshot only after worker receipt")
	}
	if err := fixture.storage.CompareAndSwapRef(ctx, fixture.repo, domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: hash}, ""); err != nil {
		test.Fatal("publish original ref through its unchanged CAS fence")
	}
}

func (fixture *executionProcessFixture) assertDocumentReads(test *testing.T, ctx context.Context, api, mcp *executionProcessChild) {
	test.Helper()
	client := &http.Client{
		Timeout: 3 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	for _, foreign := range []bool{false, true, false} {
		repo := fixture.repo
		if foreign {
			repo = fixture.foreign
		}
		path := api.url + "/api/v1/repos/" + string(repo) + "/docs/" + string(fixture.job.DocHash) + "/events?offset=0&limit=1"
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, path, nil)
		if err != nil {
			test.Fatal("construct actual API document request")
		}
		request.Header.Set("Authorization", "Bearer "+fixture.session)
		request.Header.Set("X-Cxt-Doc-Identities", string(domain.DocumentIdentityRootV1))
		status, body := executionProcessHTTP(test, client, request)
		if foreign {
			if status != http.StatusForbidden || bytes.Contains(body, []byte(fixture.marker)) {
				test.Fatal("actual API foreign authority exposed document content", status)
			}
		} else {
			if status != http.StatusOK || !bytes.Contains(body, []byte(fixture.marker)) {
				test.Fatal("actual API current authorized document read failed", status)
			}
		}
		encoded, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 39, "method": "tools/call", "params": map[string]any{
			"name": "context_fetch", "arguments": map[string]any{"repository": string(repo), "ref": string(fixture.job.DocHash), "branch": "main", "limit": 1},
		}})
		if err != nil {
			test.Fatal("construct actual MCP document tool call")
		}
		request, err = http.NewRequestWithContext(ctx, http.MethodPost, mcp.url+"/mcp", bytes.NewReader(encoded))
		if err != nil {
			test.Fatal("construct actual MCP HTTP request")
		}
		request.Header.Set("Authorization", "Bearer "+fixture.mcpToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		status, body = executionProcessHTTP(test, client, request)
		var rpc struct {
			Result struct {
				IsError bool `json:"isError"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"result"`
		}
		if status != http.StatusOK || json.Unmarshal(body, &rpc) != nil || rpc.Result.IsError != foreign || len(rpc.Result.Content) == 0 {
			test.Fatal("actual MCP current/foreign authority result changed", status, foreign)
		}
		content := ""
		for _, item := range rpc.Result.Content {
			content += item.Text
		}
		if strings.Contains(content, fixture.marker) == foreign {
			test.Fatal("actual MCP lost useful content or exposed it across authority", foreign)
		}
	}
}

func executionProcessHTTP(test *testing.T, client *http.Client, request *http.Request) (int, []byte) {
	test.Helper()
	parsed := request.URL
	host := net.ParseIP(parsed.Hostname())
	if parsed.Scheme != "http" || host == nil || !host.IsLoopback() {
		test.Fatal("fixture refuses non-loopback HTTP requests")
	}
	if _, err := strconv.Atoi(parsed.Port()); err != nil {
		test.Fatal("fixture HTTP endpoint has no explicit port")
	}
	response, err := client.Do(request)
	if err != nil {
		test.Fatal("actual loopback process request failed")
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 128<<10))
	if err != nil {
		test.Fatal("read bounded actual process response")
	}
	return response.StatusCode, body
}
