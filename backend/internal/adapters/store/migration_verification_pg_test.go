//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const migrationVerificationBundlePG = "../../../../schemas/db/migrations"

type migrationVerificationTracePG struct {
	readOnlyTransaction     bool
	sharedLockInTransaction bool
}

func (trace *migrationVerificationTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	if (strings.HasPrefix(statement, "begin") || strings.HasPrefix(statement, "start transaction") ||
		strings.HasPrefix(statement, "set transaction")) && strings.Contains(statement, "read only") {
		trace.readOnlyTransaction = true
	}
	if strings.Contains(statement, "pg_advisory_xact_lock_shared") && connection.PgConn().TxStatus() == 'T' {
		trace.sharedLockInTransaction = true
	}
	return ctx
}

func (*migrationVerificationTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func migrationVerificationStorePG(test *testing.T) (*processAdmissionPGFixture, *PostgresStore) {
	test.Helper()
	fixture := processAdmissionPG(test)
	ctx, cancel := context.WithTimeout(test.Context(), 60*time.Second)
	test.Cleanup(cancel)
	fixture.ctx = ctx
	config := fixture.config.Copy()
	config.ConnConfig.RuntimeParams["search_path"] = "public"
	delete(config.ConnConfig.RuntimeParams, "role")
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal("could not open the fresh local migration test database", err)
	}
	storage := &PostgresStore{pool: pool}
	test.Cleanup(storage.Close)
	return fixture, storage
}

func migrationVerificationReaderPG(test *testing.T, fixture *processAdmissionPGFixture, trace pgx.QueryTracer) *PostgresStore {
	test.Helper()
	role := domain.NewID("cxt_migration_reader_")
	quotedRole := pgx.Identifier{role}.Sanitize()
	password := domain.NewID("verification_")
	if _, err := fixture.observer.Exec(fixture.ctx, "CREATE ROLE "+quotedRole+
		" LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS PASSWORD '"+password+"'"); err != nil {
		test.Fatal("could not create the owned local verification LOGIN role", err)
	}
	var reader *PostgresStore
	test.Cleanup(func() {
		if reader != nil {
			reader.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.observer.Exec(cleanup, "DROP OWNED BY "+quotedRole); err != nil {
			test.Error("verification role privileges were not cleaned up", err)
		}
		if _, err := fixture.observer.Exec(cleanup, "DROP ROLE "+quotedRole); err != nil {
			test.Error("verification LOGIN role was not cleaned up", err)
		}
	})
	quotedDatabase := pgx.Identifier{fixture.config.ConnConfig.Database}.Sanitize()
	for _, statement := range []string{
		"REVOKE CREATE, TEMPORARY ON DATABASE " + quotedDatabase + " FROM PUBLIC",
		"REVOKE CREATE ON SCHEMA public FROM PUBLIC",
		"GRANT CONNECT ON DATABASE " + quotedDatabase + " TO " + quotedRole,
		"GRANT USAGE ON SCHEMA public TO " + quotedRole,
		"GRANT SELECT ON public.schema_migrations, public.ownership_migrations TO " + quotedRole,
	} {
		if _, err := fixture.observer.Exec(fixture.ctx, statement); err != nil {
			test.Fatal("could not grant the verification-only role its required reads", err)
		}
	}
	config := fixture.config.Copy()
	config.ConnConfig.User = role
	config.ConnConfig.Password = password
	config.ConnConfig.RuntimeParams["search_path"] = "public"
	config.ConnConfig.RuntimeParams["default_transaction_read_only"] = "on"
	config.ConnConfig.Tracer = trace
	delete(config.ConnConfig.RuntimeParams, "role")
	pool, err := pgxpool.NewWithConfig(fixture.ctx, config)
	if err != nil {
		test.Fatal("could not open the verification LOGIN connection", err)
	}
	reader = &PostgresStore{pool: pool}
	if err := reader.Ping(fixture.ctx); err != nil {
		test.Fatal("the restricted verification role could not log in", err)
	}
	return reader
}

func migrationVerificationStatePG(test *testing.T, fixture *processAdmissionPGFixture) [3]string {
	test.Helper()
	var state [3]string
	if err := fixture.observer.QueryRow(fixture.ctx, `WITH application_namespaces AS (
 SELECT oid FROM pg_namespace WHERE nspname !~ '^pg_' AND nspname<>'information_schema'
), application_relations AS (
 SELECT * FROM pg_class WHERE relnamespace IN (SELECT oid FROM application_namespaces)
)
SELECT jsonb_build_object(
 'database',(SELECT jsonb_build_array(datdba,datacl) FROM pg_database WHERE datname=current_database()),
 'namespaces',(SELECT jsonb_agg(jsonb_build_array(oid,nspname,nspowner,nspacl) ORDER BY oid)
   FROM pg_namespace WHERE oid IN (SELECT oid FROM application_namespaces)),
 'relations',(SELECT jsonb_agg(jsonb_build_array(oid,relnamespace,relname,relkind,relowner,relacl,
   reloptions,relrowsecurity,relforcerowsecurity) ORDER BY oid) FROM application_relations),
 'columns',(SELECT jsonb_agg(jsonb_build_array(attrelid,attnum,attname,atttypid,atttypmod,
   attnotnull,attidentity,attgenerated,attacl) ORDER BY attrelid,attnum) FROM pg_attribute
   WHERE attrelid IN (SELECT oid FROM application_relations)),
 'defaults',(SELECT jsonb_agg(jsonb_build_array(adrelid,adnum,pg_get_expr(adbin,adrelid)) ORDER BY adrelid,adnum)
   FROM pg_attrdef WHERE adrelid IN (SELECT oid FROM application_relations)),
 'constraints',(SELECT jsonb_agg(jsonb_build_array(oid,conrelid,conname,convalidated,pg_get_constraintdef(oid)) ORDER BY oid)
   FROM pg_constraint WHERE connamespace IN (SELECT oid FROM application_namespaces)),
 'indexes',(SELECT jsonb_agg(jsonb_build_array(indexrelid,indisvalid,indisready,pg_get_indexdef(indexrelid)) ORDER BY indexrelid)
   FROM pg_index WHERE indrelid IN (SELECT oid FROM application_relations)),
 'functions',(SELECT jsonb_agg(jsonb_build_array(oid,pronamespace,proowner,proacl,proconfig,pg_get_functiondef(oid)) ORDER BY oid)
   FROM pg_proc WHERE pronamespace IN (SELECT oid FROM application_namespaces) AND prokind='f'),
 'triggers',(SELECT jsonb_agg(jsonb_build_array(oid,tgrelid,tgname,tgenabled,pg_get_triggerdef(oid)) ORDER BY oid)
   FROM pg_trigger WHERE tgrelid IN (SELECT oid FROM application_relations))
)::text`).Scan(&state[0]); err != nil {
		test.Fatal("could not capture the migration database catalog", err)
	}
	for position, relation := range []string{"schema_migrations", "ownership_migrations"} {
		var present bool
		if err := fixture.observer.QueryRow(fixture.ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+relation).Scan(&present); err != nil {
			test.Fatal(err)
		}
		if present {
			statement := "SELECT coalesce(jsonb_agg(to_jsonb(history) ORDER BY version),'[]'::jsonb)::text FROM " +
				pgx.Identifier{"public", relation}.Sanitize() + " history"
			if err := fixture.observer.QueryRow(fixture.ctx, statement).Scan(&state[position+1]); err != nil {
				test.Fatal("could not capture the original migration receipts", err)
			}
		}
	}
	return state
}

func TestPGVerifyMigrationsNoDDLLogin(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	entries, err := os.ReadDir(migrationVerificationBundlePG)
	if err != nil {
		test.Fatal(err)
	}
	checksums := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(migrationVerificationBundlePG, entry.Name()))
		if err != nil || len(strings.TrimSpace(string(contents))) == 0 {
			test.Fatal("production migration is unreadable or empty", entry.Name(), err)
		}
		digest := sha256.Sum256(contents)
		checksums[entry.Name()] = hex.EncodeToString(digest[:])
	}
	if len(checksums) == 0 {
		test.Fatal("the production migration bundle is empty")
	}
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil || applied != len(checksums) {
		test.Fatal("the explicit public owner did not apply the complete production bundle", applied, len(checksums), err)
	}
	rows, err := storage.pool.Query(fixture.ctx, `SELECT version,checksum FROM public.schema_migrations ORDER BY version`)
	if err != nil {
		test.Fatal(err)
	}
	seen := 0
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			rows.Close()
			test.Fatal(err)
		}
		if expected, present := checksums[version]; !present || checksum == "" || checksum != expected {
			rows.Close()
			test.Fatal("applied history does not match the raw production file", version)
		}
		seen++
	}
	rows.Close()
	if err := rows.Err(); err != nil || seen != len(checksums) {
		test.Fatal("the production migration history is incomplete", seen, len(checksums), err)
	}
	trace := &migrationVerificationTracePG{}
	reader := migrationVerificationReaderPG(test, fixture, trace)
	var restricted bool
	if err := reader.pool.QueryRow(fixture.ctx, `SELECT current_user=session_user AND rolcanlogin
 AND NOT (rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR rolinherit)
 AND NOT has_schema_privilege('public','CREATE')
 AND NOT has_database_privilege(current_database(),'CREATE,TEMPORARY')
 AND NOT has_table_privilege('public.schema_migrations','INSERT,UPDATE,DELETE,TRUNCATE,REFERENCES,TRIGGER')
 AND NOT EXISTS(SELECT 1 FROM pg_class WHERE relowner=pg_roles.oid)
 AND current_setting('transaction_read_only')='on'
 FROM pg_roles WHERE rolname=current_user`).Scan(&restricted); err != nil || !restricted {
		test.Fatal("verification did not use a genuinely restricted read-only LOGIN", err)
	}
	before := migrationVerificationStatePG(test, fixture)
	var runtimeStore Store = reader
	for attempt := 0; attempt < 2; attempt++ {
		trace.readOnlyTransaction, trace.sharedLockInTransaction = false, false
		if err := runtimeStore.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
			test.Fatal("the complete migrated database rejected verification-only startup", err)
		}
		if !trace.readOnlyTransaction || !trace.sharedLockInTransaction {
			test.Fatal("verification did not explicitly use a read-only transaction and its shared transaction migration lock")
		}
		if reader.pool.Stat().AcquiredConns() != 0 {
			test.Fatal("successful verification retained a pool connection")
		}
	}
	if _, err := reader.pool.Exec(fixture.ctx, `SET default_transaction_read_only=off`); err != nil {
		test.Fatal(err)
	}
	trace.readOnlyTransaction, trace.sharedLockInTransaction = false, false
	if err := runtimeStore.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("verification failed when its LOGIN did not default to read-only transactions", err)
	}
	if !trace.readOnlyTransaction || !trace.sharedLockInTransaction {
		test.Fatal("verification depended on the LOGIN default instead of its own read-only transaction")
	}
	for _, statement := range []string{
		`CREATE TABLE public.verification_ddl_probe(identity integer)`,
		`CREATE TEMP TABLE verification_ddl_probe(identity integer)`,
		`ALTER TABLE public.schema_migrations ADD COLUMN verification_ddl_probe integer`,
	} {
		_, err := reader.pool.Exec(fixture.ctx, statement)
		var denied *pgconn.PgError
		if !errors.As(err, &denied) || denied.Code != "42501" {
			test.Fatal("the no-DDL LOGIN was not denied by actual PostgreSQL privileges", statement, err)
		}
	}
	if after := migrationVerificationStatePG(test, fixture); after != before {
		test.Fatal("read-only startup or its denied DDL controls changed the catalog or migration receipts")
	}
}

func TestPGVerifyMigrationsBundleHistoryMismatch(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	test.Run("fresh-database-requires-explicit-migration", func(test *testing.T) {
		before := migrationVerificationStatePG(test, fixture)
		if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); !errors.Is(err, ErrSchemaMismatch) {
			test.Fatal("an unmigrated database did not fail closed", err)
		}
		if after := migrationVerificationStatePG(test, fixture); after != before {
			test.Fatal("verification created schema or migration history on a fresh database")
		}
	})
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal(err)
	}
	if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("the unmodified full-bundle positive control failed", err)
	}
	for _, mutation := range []string{"missing-directory", "empty-directory", "missing-file", "empty-file", "whitespace-file", "changed-file", "unapplied-file"} {
		test.Run(mutation, func(test *testing.T) {
			directory := test.TempDir()
			if mutation == "missing-directory" {
				directory = filepath.Join(directory, "not-present")
			} else if mutation != "empty-directory" {
				entries, err := os.ReadDir(migrationVerificationBundlePG)
				if err != nil {
					test.Fatal(err)
				}
				for _, entry := range entries {
					if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
						continue
					}
					contents, err := os.ReadFile(filepath.Join(migrationVerificationBundlePG, entry.Name()))
					if err != nil {
						test.Fatal(err)
					}
					if entry.Name() == "0001_init.sql" {
						switch mutation {
						case "missing-file":
							continue
						case "empty-file":
							contents = nil
						case "whitespace-file":
							contents = []byte(" \n\t\n")
						case "changed-file":
							contents = append(contents, []byte("\nSELECT 1;\n")...)
						}
					}
					if err := os.WriteFile(filepath.Join(directory, entry.Name()), contents, 0600); err != nil {
						test.Fatal(err)
					}
				}
				if mutation == "unapplied-file" {
					if err := os.WriteFile(filepath.Join(directory, "9999_unapplied_verification.sql"), []byte("SELECT 1;\n"), 0600); err != nil {
						test.Fatal(err)
					}
				}
			}
			before := migrationVerificationStatePG(test, fixture)
			if err := storage.VerifyMigrations(fixture.ctx, directory); !errors.Is(err, ErrSchemaMismatch) {
				test.Fatal("an incomplete or altered migration bundle did not fail closed", err)
			}
			if after := migrationVerificationStatePG(test, fixture); after != before {
				test.Fatal("bundle verification changed the catalog or migration history")
			}
		})
	}
	var history string
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT jsonb_agg(to_jsonb(history) ORDER BY version)::text FROM public.schema_migrations history`).Scan(&history); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name      string
		statement string
		repair    string
	}{
		{"missing-history-table", `ALTER TABLE public.schema_migrations RENAME TO verification_missing_history`, `ALTER TABLE public.verification_missing_history RENAME TO schema_migrations`},
		{"missing-checksum-column", `ALTER TABLE public.schema_migrations RENAME COLUMN checksum TO verification_missing_checksum`, `ALTER TABLE public.schema_migrations RENAME COLUMN verification_missing_checksum TO checksum`},
		{"empty-history", `DELETE FROM public.schema_migrations`, ""},
		{"missing-history-row", `DELETE FROM public.schema_migrations WHERE version='0001_init.sql'`, ""},
		{"changed-history-checksum", `UPDATE public.schema_migrations SET checksum=repeat('0',64) WHERE version='0001_init.sql'`, ""},
		{"blank-history-checksum", `UPDATE public.schema_migrations SET checksum='' WHERE version='0001_init.sql'`, ""},
		{"blank-history-version", `UPDATE public.schema_migrations SET version='' WHERE version='0001_init.sql'`, ""},
		{"unknown-newer-history", `INSERT INTO public.schema_migrations(version,checksum) VALUES('9999_unknown_verification.sql',repeat('0',64))`, ""},
		{"unknown-older-history", `INSERT INTO public.schema_migrations(version,checksum) VALUES('0000_unknown_verification.sql',repeat('0',64))`, ""},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if _, err := fixture.observer.Exec(fixture.ctx, scenario.statement); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if scenario.repair != "" {
					if _, err := fixture.observer.Exec(cleanup, scenario.repair); err != nil {
						test.Error(err)
					}
					return
				}
				if _, err := fixture.observer.Exec(cleanup, `DELETE FROM public.schema_migrations`); err != nil {
					test.Error(err)
					return
				}
				if _, err := fixture.observer.Exec(cleanup, `INSERT INTO public.schema_migrations SELECT * FROM jsonb_populate_recordset(NULL::public.schema_migrations,$1::jsonb)`, history); err != nil {
					test.Error(err)
				}
			})
			before := migrationVerificationStatePG(test, fixture)
			if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); !errors.Is(err, ErrSchemaMismatch) {
				test.Fatal("invalid migration history did not fail closed", err)
			}
			switch scenario.name {
			case "changed-history-checksum", "blank-history-checksum", "blank-history-version", "unknown-newer-history", "unknown-older-history":
				if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); applied != 0 || !errors.Is(err, ErrSchemaMismatch) {
					test.Fatal("the explicit public operator accepted incompatible existing history", applied, err)
				}
			}
			if after := migrationVerificationStatePG(test, fixture); after != before {
				test.Fatal("verification repaired, rewrote, or advanced invalid migration history")
			}
			if storage.pool.Stat().AcquiredConns() != 0 {
				test.Fatal("failed history verification retained a pool connection")
			}
		})
		if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
			test.Fatal("restoring exact original history did not restore the positive control", scenario.name, err)
		}
	}
}

func TestPGVerifyMigrationsNamespaceAndAuthorityMismatch(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal(err)
	}
	reader := migrationVerificationReaderPG(test, fixture, nil)
	mismatchOwner := domain.NewID("cxt_migration_mismatch_")
	quotedMismatchOwner := pgx.Identifier{mismatchOwner}.Sanitize()
	if _, err := fixture.observer.Exec(fixture.ctx, "CREATE ROLE "+quotedMismatchOwner+" NOLOGIN NOINHERIT"); err != nil {
		test.Fatal("could not create the owned mismatched-owner control", err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := fixture.observer.Exec(cleanup, "DROP OWNED BY "+quotedMismatchOwner); err != nil {
			test.Error(err)
		}
		if _, err := fixture.observer.Exec(cleanup, "DROP ROLE "+quotedMismatchOwner); err != nil {
			test.Error(err)
		}
	})
	var originalOwner, ownershipReceipt string
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT pg_get_userbyid(relowner) FROM pg_class WHERE oid='public.schema_migrations'::regclass`).Scan(&originalOwner); err != nil {
		test.Fatal(err)
	}
	quotedOwner := pgx.Identifier{originalOwner}.Sanitize()
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT to_jsonb(receipt)::text FROM public.ownership_migrations receipt WHERE version='repository-ownership-v1'`).Scan(&ownershipReceipt); err != nil {
		test.Fatal("the original repository-ownership receipt is required", err)
	}
	for _, scenario := range []struct {
		name      string
		statement string
		repair    string
	}{
		{"missing-public-namespace", `ALTER SCHEMA public RENAME TO verification_private`, `ALTER SCHEMA verification_private RENAME TO public`},
		{"missing-ownership-table", `ALTER TABLE public.ownership_migrations RENAME TO verification_missing_ownership`, `ALTER TABLE public.verification_missing_ownership RENAME TO ownership_migrations`},
		{"null-accounting-configuration", `ALTER FUNCTION public.cxt_account_legacy_byte_storage() RESET ALL`, `ALTER FUNCTION public.cxt_account_legacy_byte_storage() SET search_path=pg_catalog,public,pg_temp`},
		{"null-guard-configuration", `ALTER FUNCTION public.cxt_guard_legacy_byte_usage() RESET ALL`, `ALTER FUNCTION public.cxt_guard_legacy_byte_usage() SET search_path=pg_catalog,public,pg_temp`},
		{"history-row-security", `ALTER TABLE public.schema_migrations ENABLE ROW LEVEL SECURITY; ALTER TABLE public.schema_migrations FORCE ROW LEVEL SECURITY`, `ALTER TABLE public.schema_migrations NO FORCE ROW LEVEL SECURITY; ALTER TABLE public.schema_migrations DISABLE ROW LEVEL SECURITY`},
		{"missing-ownership-receipt", `DELETE FROM public.ownership_migrations WHERE version='repository-ownership-v1'`, ""},
		{"wrong-ownership-receipt", `UPDATE public.ownership_migrations SET version='repository-ownership-v2' WHERE version='repository-ownership-v1'`, `UPDATE public.ownership_migrations SET version='repository-ownership-v1' WHERE version='repository-ownership-v2'`},
		{"history-owner-mismatch", "ALTER TABLE public.schema_migrations OWNER TO " + quotedMismatchOwner, "ALTER TABLE public.schema_migrations OWNER TO " + quotedOwner},
		{"receipt-owner-mismatch", "ALTER TABLE public.ownership_migrations OWNER TO " + quotedMismatchOwner, "ALTER TABLE public.ownership_migrations OWNER TO " + quotedOwner},
		{"usage-owner-mismatch", "ALTER TABLE public.doc_legacy_byte_usage_v1 OWNER TO " + quotedMismatchOwner, "ALTER TABLE public.doc_legacy_byte_usage_v1 OWNER TO " + quotedOwner},
		{"accounting-owner-mismatch", "ALTER FUNCTION public.cxt_account_legacy_byte_storage() OWNER TO " + quotedMismatchOwner, "ALTER FUNCTION public.cxt_account_legacy_byte_storage() OWNER TO " + quotedOwner},
		{"missing-accounting-function", `ALTER FUNCTION public.cxt_account_legacy_byte_storage() RENAME TO verification_missing_accounting`, `ALTER FUNCTION public.verification_missing_accounting() RENAME TO cxt_account_legacy_byte_storage`},
		{"missing-usage-relation", `ALTER TABLE public.doc_legacy_byte_usage_v1 RENAME TO verification_missing_usage`, `ALTER TABLE public.verification_missing_usage RENAME TO doc_legacy_byte_usage_v1`},
		{"accounting-invoker", `ALTER FUNCTION public.cxt_account_legacy_byte_storage() SECURITY INVOKER`, `ALTER FUNCTION public.cxt_account_legacy_byte_storage() SECURITY DEFINER`},
		{"guard-definer", `ALTER FUNCTION public.cxt_guard_legacy_byte_usage() SECURITY DEFINER`, `ALTER FUNCTION public.cxt_guard_legacy_byte_usage() SECURITY INVOKER`},
		{"accounting-unset-search-path", `ALTER FUNCTION public.cxt_account_legacy_byte_storage() RESET search_path`, `ALTER FUNCTION public.cxt_account_legacy_byte_storage() SET search_path=pg_catalog,public,pg_temp`},
		{"accounting-unsafe-search-path", `ALTER FUNCTION public.cxt_account_legacy_byte_storage() SET search_path=public,pg_catalog,pg_temp`, `ALTER FUNCTION public.cxt_account_legacy_byte_storage() SET search_path=pg_catalog,public,pg_temp`},
		{"guard-unsafe-search-path", `ALTER FUNCTION public.cxt_guard_legacy_byte_usage() SET search_path=public,pg_catalog,pg_temp`, `ALTER FUNCTION public.cxt_guard_legacy_byte_usage() SET search_path=pg_catalog,public,pg_temp`},
		{"public-accounting-execution", `GRANT EXECUTE ON FUNCTION public.cxt_account_legacy_byte_storage() TO PUBLIC`, `REVOKE EXECUTE ON FUNCTION public.cxt_account_legacy_byte_storage() FROM PUBLIC`},
		{"public-guard-execution", `GRANT EXECUTE ON FUNCTION public.cxt_guard_legacy_byte_usage() TO PUBLIC`, `REVOKE EXECUTE ON FUNCTION public.cxt_guard_legacy_byte_usage() FROM PUBLIC`},
		{"public-usage-read", `GRANT SELECT ON public.doc_legacy_byte_usage_v1 TO PUBLIC`, `REVOKE SELECT ON public.doc_legacy_byte_usage_v1 FROM PUBLIC`},
		{"public-usage-write", `GRANT UPDATE ON public.doc_legacy_byte_usage_v1 TO PUBLIC`, `REVOKE UPDATE ON public.doc_legacy_byte_usage_v1 FROM PUBLIC`},
		{"disabled-usage-guard", `ALTER TABLE public.doc_legacy_byte_usage_v1 DISABLE TRIGGER guard_legacy_byte_usage`, `ALTER TABLE public.doc_legacy_byte_usage_v1 ENABLE TRIGGER guard_legacy_byte_usage`},
		{"disabled-source-accounting", `ALTER TABLE public.doc_legacy_byte_stages_v1 DISABLE TRIGGER account_legacy_byte_source`, `ALTER TABLE public.doc_legacy_byte_stages_v1 ENABLE TRIGGER account_legacy_byte_source`},
		{"disabled-object-accounting", `ALTER TABLE public.doc_legacy_byte_stage_objects_v1 DISABLE TRIGGER account_legacy_byte_objects`, `ALTER TABLE public.doc_legacy_byte_stage_objects_v1 ENABLE TRIGGER account_legacy_byte_objects`},
		{"disabled-part-accounting", `ALTER TABLE public.doc_legacy_byte_stage_parts_v1 DISABLE TRIGGER account_legacy_byte_parts`, `ALTER TABLE public.doc_legacy_byte_stage_parts_v1 ENABLE TRIGGER account_legacy_byte_parts`},
		{"disabled-header-accounting", `ALTER TABLE public.doc_legacy_byte_stage_headers_v1 DISABLE TRIGGER account_legacy_byte_headers`, `ALTER TABLE public.doc_legacy_byte_stage_headers_v1 ENABLE TRIGGER account_legacy_byte_headers`},
		{"header-accounting-missing-delete-event", `DROP TRIGGER account_legacy_byte_headers ON public.doc_legacy_byte_stage_headers_v1;
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT ON public.doc_legacy_byte_stage_headers_v1 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`,
			`DROP TRIGGER account_legacy_byte_headers ON public.doc_legacy_byte_stage_headers_v1;
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_headers_v1 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`},
		{"header-accounting-skipped-condition", `DROP TRIGGER account_legacy_byte_headers ON public.doc_legacy_byte_stage_headers_v1;
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_headers_v1 FOR EACH ROW WHEN (false) EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`,
			`DROP TRIGGER account_legacy_byte_headers ON public.doc_legacy_byte_stage_headers_v1;
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_headers_v1 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`},
		{"wrong-header-trigger-binding", `DROP TRIGGER account_legacy_byte_headers ON public.doc_legacy_byte_stage_headers_v1;
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_headers_v1 FOR EACH ROW EXECUTE FUNCTION public.cxt_guard_legacy_byte_usage()`,
			`DROP TRIGGER account_legacy_byte_headers ON public.doc_legacy_byte_stage_headers_v1;
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT OR DELETE ON public.doc_legacy_byte_stage_headers_v1 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if _, err := fixture.observer.Exec(fixture.ctx, scenario.statement); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if scenario.repair != "" {
					if _, err := fixture.observer.Exec(cleanup, scenario.repair); err != nil {
						test.Error(err)
					}
				} else if _, err := fixture.observer.Exec(cleanup, `INSERT INTO public.ownership_migrations SELECT * FROM jsonb_populate_record(NULL::public.ownership_migrations,$1::jsonb)`, ownershipReceipt); err != nil {
					test.Error(err)
				}
			})
			before := migrationVerificationStatePG(test, fixture)
			if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); !errors.Is(err, ErrSchemaMismatch) {
				test.Fatal("namespace or original ownership/accounting authority did not fail closed", err)
			}
			if scenario.name == "history-row-security" {
				if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); applied != 0 || !errors.Is(err, ErrSchemaMismatch) {
					test.Fatal("public operator accepted history that row security could hide", applied, err)
				}
			}
			if after := migrationVerificationStatePG(test, fixture); after != before {
				test.Fatal("verification repaired or changed the original ownership/accounting boundary")
			}
			if storage.pool.Stat().AcquiredConns() != 0 {
				test.Fatal("authority verification retained a pool connection")
			}
		})
		if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
			test.Fatal("restoring the original authority did not restore the positive control", scenario.name, err)
		}
	}
	test.Run("shadow-schema-with-valid-history", func(test *testing.T) {
		if _, err := fixture.observer.Exec(fixture.ctx, `CREATE SCHEMA verification_shadow;
 CREATE TABLE verification_shadow.schema_migrations (LIKE public.schema_migrations INCLUDING ALL);
 INSERT INTO verification_shadow.schema_migrations SELECT * FROM public.schema_migrations;
 CREATE TABLE verification_shadow.ownership_migrations (LIKE public.ownership_migrations INCLUDING ALL);
 INSERT INTO verification_shadow.ownership_migrations SELECT * FROM public.ownership_migrations`); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := storage.pool.Exec(cleanup, `SET search_path=public`); err != nil {
				test.Error(err)
			}
			if _, err := fixture.observer.Exec(cleanup, `DROP SCHEMA verification_shadow CASCADE`); err != nil {
				test.Error(err)
			}
		})
		if _, err := storage.pool.Exec(fixture.ctx, `SET search_path=verification_shadow,public`); err != nil {
			test.Fatal(err)
		}
		before := migrationVerificationStatePG(test, fixture)
		if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); !errors.Is(err, ErrSchemaMismatch) {
			test.Fatal("valid shadow history authorized a non-public startup namespace", err)
		}
		if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); applied != 0 || !errors.Is(err, ErrSchemaMismatch) {
			test.Fatal("the explicit public operator accepted a shadow startup namespace", applied, err)
		}
		if after := migrationVerificationStatePG(test, fixture); after != before {
			test.Fatal("shadow-schema verification changed the catalog or receipts")
		}
	})
	if err := reader.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("restoring all original boundaries broke restricted-login verification", err)
	}
}

func TestPGVerifyMigrationsSharedLockCancellation(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal(err)
	}
	reader := migrationVerificationReaderPG(test, fixture, nil)
	before := migrationVerificationStatePG(test, fixture)
	test.Run("existing-shared-migration-lock-is-compatible", func(test *testing.T) {
		if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_lock_shared(hashtextextended('cxt-schema-migrations',0))`); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := fixture.observer.Exec(cleanup, `SELECT pg_advisory_unlock_shared(hashtextextended('cxt-schema-migrations',0))`); err != nil {
				test.Error(err)
			}
		})
		compatible, cancel := context.WithTimeout(fixture.ctx, 3*time.Second)
		defer cancel()
		if err := reader.VerifyMigrations(compatible, migrationVerificationBundlePG); err != nil {
			test.Fatal("verification did not share the existing migration advisory lock", err)
		}
	})
	var released bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT pg_try_advisory_lock(hashtextextended('cxt-schema-migrations',0))`).Scan(&released); err != nil || !released {
		test.Fatal("successful shared verification retained a migration lock", err)
	}
	if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
		test.Fatal(err)
	}
	if reader.pool.Stat().AcquiredConns() != 0 {
		test.Fatal("successful shared verification retained a pool connection")
	}
	for _, mode := range []string{"canceled", "deadline"} {
		test.Run(mode, func(test *testing.T) {
			if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_lock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := fixture.observer.Exec(cleanup, `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
					test.Error(err)
				}
			})
			waiting, cancel := context.WithCancel(fixture.ctx)
			expected := context.Canceled
			if mode == "deadline" {
				cancel()
				waiting, cancel = context.WithTimeout(fixture.ctx, 5*time.Second)
				expected = context.DeadlineExceeded
			}
			defer cancel()
			completed := make(chan error, 1)
			go func() { completed <- reader.VerifyMigrations(waiting, migrationVerificationBundlePG) }()
			observed := false
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			observation, stopObservation := context.WithTimeout(fixture.ctx, 3*time.Second)
			defer stopObservation()
			for !observed {
				if err := fixture.observer.QueryRow(observation, `SELECT EXISTS(
 SELECT 1 FROM pg_locks held JOIN pg_locks waiting
 USING(locktype,database,classid,objid,objsubid)
 JOIN pg_stat_activity activity ON activity.pid=waiting.pid
 WHERE held.pid=pg_backend_pid() AND held.locktype='advisory' AND held.granted
 AND held.mode='ExclusiveLock' AND waiting.mode='ShareLock' AND NOT waiting.granted
 AND waiting.database=(SELECT oid FROM pg_database WHERE datname=current_database())
 AND pg_backend_pid()=ANY(pg_blocking_pids(waiting.pid)) AND activity.xact_start IS NOT NULL)`).Scan(&observed); err != nil {
					test.Fatal("could not observe verification waiting in its transaction on the existing migration lock", err)
				}
				if observed {
					break
				}
				select {
				case err := <-completed:
					test.Fatal("verification crossed the held exclusive migration lock", err)
				case <-observation.Done():
					test.Fatal("verification never reached the existing shared transaction migration lock", observation.Err())
				case <-ticker.C:
				}
			}
			if mode == "canceled" {
				cancel()
			}
			select {
			case err := <-completed:
				if !errors.Is(err, expected) || errors.Is(err, ErrSchemaMismatch) {
					test.Fatal("causal lock cancellation lost its context error or became a schema mismatch", err)
				}
			case <-fixture.ctx.Done():
				test.Fatal("verification retained its lock wait after context cancellation", fixture.ctx.Err())
			}
			released, stopReleased := context.WithTimeout(fixture.ctx, 3*time.Second)
			defer stopReleased()
			for reader.pool.Stat().AcquiredConns() != 0 {
				select {
				case <-released.Done():
					test.Fatal("canceled verification retained its only pool connection", released.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
		})
		released, stopReleased := context.WithTimeout(fixture.ctx, 3*time.Second)
		defer stopReleased()
		for {
			var unlocked bool
			if err := fixture.observer.QueryRow(released, `SELECT pg_try_advisory_lock(hashtextextended('cxt-schema-migrations',0))`).Scan(&unlocked); err != nil {
				test.Fatal("verification retained a migration lock after cancellation", err)
			}
			if unlocked {
				break
			}
			select {
			case <-released.Done():
				test.Fatal("verification retained a migration lock after cancellation", released.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
		if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
			test.Fatal(err)
		}
		if err := reader.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
			test.Fatal("the one-connection reader could not verify after lock cancellation", err)
		}
	}
	if reader.pool.Stat().AcquiredConns() != 0 {
		test.Fatal("recovered verification retained a pool connection")
	}
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT pg_try_advisory_lock(hashtextextended('cxt-schema-migrations',0))`).Scan(&released); err != nil || !released {
		test.Fatal("recovered verification retained a migration lock", err)
	}
	if _, err := fixture.observer.Exec(fixture.ctx, `SELECT pg_advisory_unlock(hashtextextended('cxt-schema-migrations',0))`); err != nil {
		test.Fatal(err)
	}
	if after := migrationVerificationStatePG(test, fixture); after != before {
		test.Fatal("shared locking or causal cancellation changed the catalog or migration receipts")
	}
}
