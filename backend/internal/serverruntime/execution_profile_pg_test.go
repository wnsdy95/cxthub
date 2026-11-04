//go:build postgres

package serverruntime

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/executionprofile"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func executionRootPostgresFixture(test *testing.T) (string, *pgx.Conn) {
	test.Helper()
	raw := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if raw == "" {
		test.Skip("CXT_TEST_DSN unset")
	}
	config, err := pgxpool.ParseConfig(raw)
	if err != nil {
		test.Fatal("invalid local PostgreSQL test configuration")
	}
	host := config.ConnConfig.Host
	address := net.ParseIP(host)
	if host != "localhost" && !strings.HasPrefix(host, "/") && (address == nil || !address.IsLoopback()) {
		test.Fatal("execution root fixtures require an explicitly local PostgreSQL server")
	}
	for _, fallback := range config.ConnConfig.Fallbacks {
		address := net.ParseIP(fallback.Host)
		if fallback.Host != "localhost" && !strings.HasPrefix(fallback.Host, "/") && (address == nil || !address.IsLoopback()) {
			test.Fatal("execution root fixture contains a nonlocal fallback")
		}
	}
	config.ConnConfig.RuntimeParams["application_name"] = "cxthub/bounded-documents-v1/observer"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	if err != nil {
		test.Fatal("cannot connect local PostgreSQL fixture")
	}
	test.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := fmt.Sprintf("cxt_execution_root_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		test.Fatal("cannot create owned execution root fixture database", err)
	}
	test.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		if _, err := admin.Exec(cleanup, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			test.Error("cannot drop owned execution root fixture database", err)
		}
	})
	config.ConnConfig.Database = name
	observer, err := pgx.ConnectConfig(ctx, config.ConnConfig)
	if err != nil {
		test.Fatal("cannot connect owned execution root observer")
	}
	test.Cleanup(func() { _ = observer.Close(context.Background()) })
	var dsn string
	if strings.HasPrefix(raw, "postgres://") || strings.HasPrefix(raw, "postgresql://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			test.Fatal("cannot parse local fixture DSN")
		}
		parsed.Path, parsed.RawPath = "/"+name, ""
		query := parsed.Query()
		query.Set("dbname", name)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn = raw + " dbname=" + name
	}
	return dsn, observer
}

func executionRootPostgresApplyMigrations(test *testing.T, dsn string) {
	test.Helper()
	budget, err := executionprofile.Parse(runtimeExecutionBudget(test))
	if err != nil {
		test.Fatal(err)
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
		test.Fatal("open the bounded operator fixture store before startup", err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		test.Fatal("explicitly migrate the fixture before starting a production root", err)
	}
}

func executionRootPostgresEnvironment(test *testing.T, dsn string) {
	test.Helper()
	infrastructureRuntimeEnvironment(test)
	test.Setenv("CXT_EXECUTION_PROFILE", outbound.BoundedDocumentsProfile)
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", runtimeExecutionBudgetFile(test, runtimeExecutionBudget(test)))
	test.Setenv("CXT_POSTGRES_DSN", dsn)
	test.Setenv("CXT_MIGRATIONS_DIR", "../../../schemas/db/migrations")
	executionRootPostgresApplyMigrations(test, dsn)
}

func executionRootWait(test *testing.T, condition func(context.Context) bool) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for !condition(ctx) {
		select {
		case <-ctx.Done():
			test.Fatal("execution root condition did not converge")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestExecutionProfilePostgresActualAPIMCPRootsAndControlLoss(test *testing.T) {
	for name, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"api": Open, "mcp": OpenMCP} {
		test.Run(name, func(test *testing.T) {
			dsn, observer := executionRootPostgresFixture(test)
			executionRootPostgresEnvironment(test, dsn)
			priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			root, err := open(context.Background(), "127.0.0.1:0", "", true)
			if err != nil {
				test.Fatal("actual bounded composition root", err)
			}
			test.Cleanup(root.Close)
			if !root.BoundedDocuments() || root.execution.role.Role != outbound.ProcessRole(name) || root.infrastructure.Store != root.Store || root.DocumentQueryProfile != documentquery.VerifiedRoots {
				test.Fatal("bounded root lost role, one-store ownership or verified schema policy")
			}
			if _, ok := root.Store.(*store.PostgresStore); !ok || (root.Context != nil) != (name == "api") || root.DocumentQueries == nil || root.Identity == nil {
				test.Fatal("bounded root changed API/MCP functional ownership")
			}
			response := httptest.NewRecorder()
			root.HealthHandler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest("GET", "/healthz", nil))
			if response.Code != http.StatusServiceUnavailable {
				test.Fatal("single role root became ready without its peers")
			}
			config, err := root.execution.budget.AdmissionConfig(outbound.ProcessRole(name))
			if err != nil {
				test.Fatal(err)
			}
			fingerprint, err := store.ProcessAdmissionFingerprint(config.BudgetFingerprint)
			if err != nil {
				test.Fatal(err)
			}
			controlName := outbound.BoundedDocumentsControlPrefix + name + "/" + fingerprint
			var terminated int
			if err := observer.QueryRow(context.Background(), `SELECT count(*) FROM (SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1) terminated`, controlName).Scan(&terminated); err != nil || terminated != 1 {
				test.Fatal("cannot terminate exactly the owned control session", terminated, err)
			}
			process := root.ProcessContext(context.Background())
			select {
			case <-process.Done():
			case <-time.After(5 * time.Second):
				test.Fatal("actual root did not fail closed on control loss")
			}
			root.Close()
			if runtime.GOMAXPROCS(0) != priorCPU || debug.SetMemoryLimit(-1) != priorMemory {
				test.Fatal("actual root did not restore soft runtime settings after drainage")
			}
			executionRootWait(test, func(ctx context.Context) bool {
				var sessions int
				err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND (application_name LIKE $1 OR application_name=$2)`,
					"cxthub/bounded-documents-v1/"+name+"/%", controlName).Scan(&sessions)
				return err == nil && sessions == 0
			})
		})
	}
}

func TestExecutionProfilePostgresActualWorkerRootRunAndClose(test *testing.T) {
	dsn, observer := executionRootPostgresFixture(test)
	executionRootPostgresEnvironment(test, dsn)
	test.Setenv("CXT_AUTH", "invalid")
	test.Setenv("CXT_PUBLIC_URL", "invalid")
	test.Setenv("CXT_MEMORY_READ_REPOS", "invalid")
	worker, err := OpenDocumentWorker(context.Background())
	if err != nil {
		test.Fatal("worker root unnecessarily required authentication or broad queries", err)
	}
	test.Cleanup(func() { _ = worker.Close() })
	if worker.workers == nil || worker.workers.Finalization == nil || worker.workers.Range == nil || worker.workers.FindFields == nil || worker.workers.Display == nil || worker.maintenance == nil || worker.Store != worker.infrastructure.Store || worker.Ready() {
		test.Fatal("worker root did not own exactly its document workers and maintenance with one shared store")
	}
	current, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- worker.Run(current) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			test.Fatal("cooperative worker cancellation failed", err)
		}
	case <-time.After(5 * time.Second):
		test.Fatal("worker Run did not join all document loops")
	}
	if err := worker.Close(); err != nil {
		test.Fatal("worker root close", err)
	}
	budgetFingerprint, err := worker.execution.budget.Fingerprint()
	if err != nil {
		test.Fatal(err)
	}
	fingerprint, err := store.ProcessAdmissionFingerprint(budgetFingerprint)
	if err != nil {
		test.Fatal(err)
	}
	executionRootWait(test, func(ctx context.Context) bool {
		var sessions int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND (application_name LIKE 'cxthub/bounded-documents-v1/worker/%' OR application_name=$1)`,
			outbound.BoundedDocumentsControlPrefix+"worker/"+fingerprint).Scan(&sessions)
		return err == nil && sessions == 0
	})
}
