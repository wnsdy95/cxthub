//go:build postgres

package serverruntime

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const schemaReadonlyPGStartupPrefix = "verify schema (run cxt-admin migrate --apply before serving): "

func schemaReadonlyPGOpenAndCloseRoot(ctx context.Context, name, data string) (bool, error) {
	switch name {
	case "infrastructure":
		root, err := OpenInfrastructure(ctx, InfrastructureConfig{
			DSN: os.Getenv("CXT_POSTGRES_DSN"), DataDir: data,
			MigrationsDirectory: os.Getenv("CXT_MIGRATIONS_DIR"),
		})
		if root != nil {
			return true, errors.Join(err, root.Close())
		}
		return false, err
	case "api", "mcp":
		open := Open
		if name == "mcp" {
			open = OpenMCP
		}
		root, err := open(ctx, "127.0.0.1:0", data, false)
		if root != nil {
			root.Close()
		}
		return root != nil, err
	case "worker":
		root, err := wave48PGOpenWorker(ctx)
		if root != nil {
			return true, errors.Join(err, root.Close())
		}
		return false, err
	default:
		return false, fmt.Errorf("unknown schema startup fixture root %q", name)
	}
}

func schemaReadonlyPGNoClientSessions(test *testing.T, observer *pgx.Conn) {
	test.Helper()
	executionRootWait(test, func(ctx context.Context) bool {
		var sessions int
		err := observer.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
 WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend'`).Scan(&sessions)
		return err == nil && sessions == 0
	})
}

func TestSchemaReadonlyPGActualRootsRefuseBlankDatabaseWithoutDDL(test *testing.T) {
	dsn, observer := executionRootPostgresFixture(test)
	if fullProfileSchemaPGCatalog(test, observer) != "{}" {
		test.Fatal("blank-schema refusal requires a fresh owned database without operator migrations")
	}
	for _, profileName := range []string{"embedded", outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile} {
		test.Run(profileName, func(test *testing.T) {
			data := wave48WorkerEnvironment(test)
			test.Setenv("CXT_POSTGRES_DSN", dsn)
			test.Setenv("CXT_MIGRATIONS_DIR", "../../../schemas/db/migrations")
			test.Setenv("CXT_EXECUTION_PROFILE", profileName)
			test.Setenv("CXT_EXECUTION_BUDGET_FILE", fullRuntimeProfileBudgetFile(test, fullRuntimeProfileBudget(test, profileName)))
			priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
			names := []string{"infrastructure", "api", "mcp"}
			if profileName != "embedded" {
				names = append(names, "worker")
			}
			for _, name := range names {
				test.Run(name, func(test *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					opened, err := schemaReadonlyPGOpenAndCloseRoot(ctx, name, data)
					if opened || !errors.Is(err, store.ErrSchemaMismatch) || !strings.HasPrefix(err.Error(), schemaReadonlyPGStartupPrefix) {
						test.Fatal("production PostgreSQL root did not require an explicit operator migration", opened, err)
					}
					if fullProfileSchemaPGCatalog(test, observer) != "{}" {
						test.Fatal("refused startup initialized migration history or business schema")
					}
					schemaReadonlyPGNoClientSessions(test, observer)
					fullProfileSchemaPGNoAdmission(test, observer)
					requireFullProfileNoStoreOrRuntimeEffects(test, data, priorCPU, priorMemory)
				})
			}
		})
	}
}

func TestSchemaReadonlyPGEmbeddedActualRootsRejectBlankChecksumWithoutRepair(test *testing.T) {
	dsn, observer := executionRootPostgresFixture(test)
	executionRootPostgresApplyMigrations(test, dsn)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if result, err := observer.Exec(ctx, `UPDATE public.schema_migrations SET checksum=''
 WHERE version=(SELECT max(version) FROM public.schema_migrations)`); err != nil || result.RowsAffected() != 1 {
		test.Fatal("blank exactly one applied checksum in the owned negative fixture", err)
	}
	migrationHistory := func(test *testing.T) string {
		test.Helper()
		var history string
		if err := observer.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(history) ORDER BY version)::text
 FROM public.schema_migrations history`).Scan(&history); err != nil {
			test.Fatal("observe incomplete migration history without normalizing it", err)
		}
		return history
	}
	history, catalog := migrationHistory(test), fullProfileSchemaPGCatalog(test, observer)
	data := wave48WorkerEnvironment(test)
	test.Setenv("CXT_POSTGRES_DSN", dsn)
	test.Setenv("CXT_MIGRATIONS_DIR", "../../../schemas/db/migrations")
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	for _, name := range []string{"infrastructure", "api", "mcp"} {
		test.Run(name, func(test *testing.T) {
			opened, err := schemaReadonlyPGOpenAndCloseRoot(ctx, name, data)
			if opened || !errors.Is(err, store.ErrSchemaMismatch) || !strings.HasPrefix(err.Error(), schemaReadonlyPGStartupPrefix) {
				test.Fatal("actual embedded root accepted an unverified historical checksum", opened, err)
			}
			if migrationHistory(test) != history || fullProfileSchemaPGCatalog(test, observer) != catalog {
				test.Fatal("refused startup repaired migration history or changed business schema")
			}
			schemaReadonlyPGNoClientSessions(test, observer)
			fullProfileSchemaPGNoAdmission(test, observer)
			requireFullProfileNoStoreOrRuntimeEffects(test, data, priorCPU, priorMemory)
		})
	}
}

func TestSchemaReadonlyPGEmbeddedActualRootsWithRestrictedDMLStartupRole(test *testing.T) {
	dsn, observer := executionRootPostgresFixture(test)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var superuser bool
	if err := observer.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname=session_user`).Scan(&superuser); err != nil {
		test.Fatal("inspect the owned local fixture's role capability", err)
	}
	if !superuser {
		test.Skip("restricted startup SET ROLE requires an owned local superuser fixture")
	}
	executionRootPostgresApplyMigrations(test, dsn)
	role := fmt.Sprintf("cxt_schema_runtime_%d", time.Now().UnixNano())
	quoted := pgx.Identifier{role}.Sanitize()
	if _, err := observer.Exec(ctx, "CREATE ROLE "+quoted+" NOLOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS"); err != nil {
		test.Fatal("create only the owned restricted startup role", err)
	}
	test.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		if _, err := observer.Exec(cleanup, "DROP OWNED BY "+quoted); err != nil {
			test.Error("revoke only the fixture role's grants", err)
		}
		if _, err := observer.Exec(cleanup, "DROP ROLE "+quoted); err != nil {
			test.Error("drop only the fixture startup role", err)
		}
	})
	for _, statement := range []string{
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT USAGE ON SCHEMA public TO " + quoted,
		"GRANT SELECT ON public.schema_migrations, public.ownership_migrations TO " + quoted,
		"GRANT SELECT, INSERT, UPDATE, DELETE ON public.repos TO " + quoted,
	} {
		if _, err := observer.Exec(ctx, statement); err != nil {
			test.Fatal("grant limited runtime DML without schema ownership or accounting authority", err)
		}
	}
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			test.Fatal(err)
		}
		query := parsed.Query()
		query.Set("role", role)
		query.Set("default_transaction_read_only", "on")
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " role=" + role + " default_transaction_read_only=on"
	}
	configuration, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		test.Fatal("parse the restricted startup configuration")
	}
	connection, err := pgx.ConnectConfig(ctx, configuration.ConnConfig)
	if err != nil {
		test.Fatal("connect using the restricted startup role", err)
	}
	test.Cleanup(func() { _ = connection.Close(context.Background()) })
	var restricted bool
	if err := connection.QueryRow(ctx, `SELECT current_user=$1 AND current_user<>session_user
 AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolbypassrls
 AND current_setting('transaction_read_only')='on'
 AND NOT has_schema_privilege(current_user,'public','CREATE')
 AND NOT has_database_privilege(current_user,current_database(),'CREATE')
 AND has_table_privilege(current_user,'public.repos','SELECT,INSERT,UPDATE,DELETE')
 AND NOT has_table_privilege(current_user,'public.doc_legacy_byte_usage_v1','SELECT,INSERT,UPDATE,DELETE')
 AND NOT has_function_privilege(current_user,'public.cxt_account_legacy_byte_storage()','EXECUTE')
 AND NOT has_function_privilege(current_user,'public.cxt_guard_legacy_byte_usage()','EXECUTE')
 AND to_regprocedure('public.cxt_backfill_legacy_byte_header_usage()') IS NULL
 FROM pg_roles WHERE rolname=current_user`, role).Scan(&restricted); err != nil || !restricted {
		test.Fatal("startup role retained DDL, owner, accounting or write-transaction authority", restricted, err)
	}
	if err := connection.Close(ctx); err != nil {
		test.Fatal(err)
	}
	data := wave48WorkerEnvironment(test)
	test.Setenv("CXT_POSTGRES_DSN", dsn)
	test.Setenv("CXT_MIGRATIONS_DIR", "../../../schemas/db/migrations")
	test.Setenv("CXT_EXECUTION_PROFILE", "embedded")
	test.Setenv("CXT_EXECUTION_BUDGET_FILE", "")
	history := fullProfileSchemaPGHistory(test, observer, "../../../schemas/db/migrations")
	catalog := fullProfileSchemaPGCatalog(test, observer)
	var receipt string
	if err := observer.QueryRow(ctx, `SELECT row_to_json(receipt)::text FROM public.ownership_migrations receipt
 WHERE version='repository-ownership-v1'`).Scan(&receipt); err != nil {
		test.Fatal("retain the operator's original repository ownership receipt", err)
	}
	priorCPU, priorMemory := runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)
	for _, name := range []string{"infrastructure", "api", "mcp"} {
		test.Run(name, func(test *testing.T) {
			opened, err := schemaReadonlyPGOpenAndCloseRoot(ctx, name, data)
			if !opened || err != nil {
				test.Fatal("actual embedded root required migration-owner or DDL privileges to serve a current schema", opened, err)
			}
			var currentReceipt string
			if err := observer.QueryRow(ctx, `SELECT row_to_json(receipt)::text FROM public.ownership_migrations receipt
 WHERE version='repository-ownership-v1'`).Scan(&currentReceipt); err != nil || currentReceipt != receipt {
				test.Fatal("read-only startup changed the operator's repository ownership receipt", err)
			}
			if fullProfileSchemaPGCatalog(test, observer) != catalog ||
				!reflect.DeepEqual(history, fullProfileSchemaPGHistory(test, observer, "../../../schemas/db/migrations")) {
				test.Fatal("read-only startup changed schema, migration checksums or application timestamps")
			}
			schemaReadonlyPGNoClientSessions(test, observer)
			fullProfileSchemaPGNoAdmission(test, observer)
			requireFullProfileNoStoreOrRuntimeEffects(test, data, priorCPU, priorMemory)
		})
	}
	test.Log("sequential Infrastructure/API/MCP startup under limited-DML SET ROLE and read-only transactions; independent login, request DML and bounded admission are not established")
}
