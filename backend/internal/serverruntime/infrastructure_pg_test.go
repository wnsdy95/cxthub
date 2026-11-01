//go:build postgres

package serverruntime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func infrastructurePostgresDSN(test *testing.T) string {
	test.Helper()
	dsn := strings.TrimSpace(os.Getenv("CXT_TEST_DSN"))
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset")
	}
	return dsn
}

func infrastructureNamedPostgresDSN(test *testing.T, dsn, name string) string {
	test.Helper()
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn + " application_name=" + name
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		test.Fatal(err)
	}
	query := parsed.Query()
	query.Set("application_name", name)
	parsed.RawQuery = query.Encode()
	return parsed.String()
}

func infrastructurePostgresObserver(test *testing.T, dsn string) *pgxpool.Conn {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	observer, err := pool.Acquire(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(observer.Release)
	return observer
}

func assertInfrastructurePostgresPoolClosed(test *testing.T, observer *pgxpool.Conn, name string) {
	test.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var connections int
		if err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE datname=current_database() AND application_name=$1`, name).Scan(&connections); err != nil {
			test.Fatal(err)
		}
		if connections == 0 {
			return
		}
		select {
		case <-ctx.Done():
			test.Fatal("infrastructure retained PostgreSQL connections after initialization failed", connections)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestInfrastructurePostgresProfileAndPublicationPolicy(test *testing.T) {
	dsn := infrastructurePostgresDSN(test)
	executionRootPostgresApplyMigrations(test, dsn)
	ctx := context.Background()
	for _, scenario := range []struct {
		name     string
		required bool
		byte     bool
		derived  bool
	}{
		{name: "DSN selects production"},
		{name: "required production", required: true},
		{name: "byte publication", required: true, byte: true},
		{name: "derived publication", required: true, byte: true, derived: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			infrastructure, err := OpenInfrastructure(ctx, InfrastructureConfig{
				DSN:                          " \t" + dsn + "\n",
				DataDir:                      test.TempDir(),
				RequirePostgres:              scenario.required,
				MigrationsDirectory:          "../../../schemas/db/migrations",
				LegacyByteRangePublication:   scenario.byte,
				LegacyDerivedBytePublication: scenario.derived,
			})
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() { _ = infrastructure.Close() })
			storage, postgres := infrastructure.Store.(*store.PostgresStore)
			if !postgres || infrastructure.Store != infrastructure.ownedStore || infrastructure.DocumentProfile != documentquery.VerifiedRoots {
				test.Fatal("production infrastructure lost its store or document profile")
			}
			if err := app.ValidateProductionStore(storage); err != nil {
				test.Fatal(err)
			}
			if storage.LegacyByteRangePublicationEnabled() != scenario.byte {
				test.Fatal("byte publication policy changed")
			}
			if _, supported := infrastructure.Store.(outbound.LegacyDerivedByteRangePublicationStore); !supported {
				test.Fatal("derived publication capability missing")
			}
			if _, supported := infrastructure.Store.(outbound.LegacyDerivedByteStorageAccounting); !supported {
				test.Fatal("derived storage accounting missing")
			}
			if err := storage.VerifyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
				test.Fatal("infrastructure accepted an unverified schema", err)
			}
			if err := infrastructure.Close(); err != nil {
				test.Fatal(err)
			}
			if err := storage.Ping(ctx); err == nil {
				test.Fatal("infrastructure did not close its returned PostgreSQL pool")
			}
			if err := infrastructure.Close(); err != nil {
				test.Fatal(err)
			}
		})
	}
}

func TestInfrastructurePostgresInitializationErrorsCloseOwnedPool(test *testing.T) {
	dsn := infrastructurePostgresDSN(test)
	executionRootPostgresApplyMigrations(test, dsn)
	ctx := context.Background()
	observer := infrastructurePostgresObserver(test, dsn)
	for _, scenario := range []struct {
		name      string
		directory string
		want      string
	}{
		{name: "verification before publication", directory: filepath.Join(test.TempDir(), "missing"), want: schemaReadonlyPGStartupPrefix},
		{name: "publication after verification", directory: "../../../schemas/db/migrations", want: "CXT_LEGACY_DERIVED_BYTE_PUBLICATION requires CXT_LEGACY_BYTE_RANGE_PUBLICATION"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			name := fmt.Sprintf("cxt-infrastructure-%d", time.Now().UnixNano())
			infrastructure, err := OpenInfrastructure(ctx, InfrastructureConfig{
				DSN:                          infrastructureNamedPostgresDSN(test, dsn, name),
				RequirePostgres:              true,
				MigrationsDirectory:          scenario.directory,
				LegacyDerivedBytePublication: true,
			})
			if infrastructure != nil {
				test.Cleanup(func() { _ = infrastructure.Close() })
			}
			if infrastructure != nil || err == nil || !strings.HasPrefix(err.Error(), scenario.want) {
				test.Fatal("infrastructure initialization error changed", infrastructure, err)
			}
			if scenario.name == "verification before publication" && !errors.Is(err, os.ErrNotExist) {
				test.Fatal("schema bundle read cause was lost", err)
			}
			assertInfrastructurePostgresPoolClosed(test, observer, name)
		})
	}
}

func TestInfrastructurePostgresVerificationAdvisoryLockCancellation(test *testing.T) {
	dsn := infrastructurePostgresDSN(test)
	executionRootPostgresApplyMigrations(test, dsn)
	observer := infrastructurePostgresObserver(test, dsn)
	if _, err := observer.Exec(context.Background(), `SELECT pg_advisory_lock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		_, _ = observer.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	name := fmt.Sprintf("cxt-infrastructure-%d", time.Now().UnixNano())
	infrastructure, err := OpenInfrastructure(ctx, InfrastructureConfig{
		DSN:                 infrastructureNamedPostgresDSN(test, dsn, name),
		RequirePostgres:     true,
		MigrationsDirectory: "../../../schemas/db/migrations",
	})
	if infrastructure != nil {
		test.Cleanup(func() { _ = infrastructure.Close() })
	}
	if infrastructure != nil || !errors.Is(err, context.DeadlineExceeded) || !strings.HasPrefix(err.Error(), schemaReadonlyPGStartupPrefix) {
		test.Fatal("schema verification ignored the migration advisory lock or cancellation", infrastructure, err)
	}
	assertInfrastructurePostgresPoolClosed(test, observer, name)
}

func TestRuntimePostgresCallersReuseInfrastructurePool(test *testing.T) {
	dsn := infrastructurePostgresDSN(test)
	executionRootPostgresApplyMigrations(test, dsn)
	for name, open := range map[string]func(context.Context, string, string, bool) (*Runtime, error){"API": Open, "MCP": OpenMCP} {
		test.Run(name, func(test *testing.T) {
			infrastructureRuntimeEnvironment(test)
			test.Setenv("CXT_POSTGRES_DSN", dsn)
			test.Setenv("CXT_MIGRATIONS_DIR", "../../../schemas/db/migrations")
			runtime, err := open(context.Background(), "127.0.0.1:0", test.TempDir(), true)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(runtime.Close)
			storage, postgres := runtime.Store.(*store.PostgresStore)
			if !postgres || runtime.infrastructure == nil || runtime.infrastructure.Store != runtime.Store || runtime.infrastructure.ownedStore != runtime.Store {
				test.Fatal("runtime did not retain the one infrastructure pool")
			}
			if runtime.DocumentQueryProfile != documentquery.VerifiedRoots || (runtime.Context != nil) != (name == "API") {
				test.Fatal("runtime changed its production query profile or command boundary")
			}
			runtime.Close()
			runtime.Close()
			if err := storage.Ping(context.Background()); err == nil {
				test.Fatal("runtime did not close its infrastructure pool")
			}
		})
	}
}
