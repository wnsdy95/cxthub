//go:build postgres

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type rootSourceIntegritySchemaTracePG struct {
	migrationVerificationTracePG
	test            *testing.T
	statement       string
	catalogCheck    string
	armed           bool
	pending         bool
	rolledBack      bool
	attemptedWrite  bool
	transaction     pgx.Tx
	tamperedCatalog [3]string
	verifiedCatalog [3]string
}

func (trace *rootSourceIntegritySchemaTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	if trace.armed && strings.HasPrefix(statement, "begin") {
		trace.armed = false
		transaction, err := connection.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			trace.test.Fatal("begin rollback-only source-integrity drift fixture", err)
		}
		trace.transaction = transaction
		if _, err := transaction.Exec(ctx, trace.statement); err != nil {
			trace.test.Fatal("tamper with the owned source-integrity catalog", err)
		}
		var changed bool
		if err := transaction.QueryRow(ctx, trace.catalogCheck).Scan(&changed); err != nil || !changed {
			trace.test.Fatal("source-integrity drift was not present in the exact public catalog", changed, err)
		}
		if _, err := transaction.Exec(ctx, `SET TRANSACTION READ ONLY`); err != nil {
			trace.test.Fatal("make drift verification read-only before its first catalog read", err)
		}
		var readOnly bool
		if err := transaction.QueryRow(ctx, `SELECT current_setting('transaction_read_only')='on'
 AND current_setting('transaction_isolation')='read committed'
 AND current_schemas(true)=ARRAY['pg_catalog','public']::name[] AND pg_my_temp_schema()=0`).Scan(&readOnly); err != nil || !readOnly {
			trace.test.Fatal("source-integrity verification requires the exact namespace and read-only transaction", err)
		}
		local := &processAdmissionPGFixture{ctx: ctx, observer: connection}
		trace.tamperedCatalog = migrationVerificationStatePG(trace.test, local)
		trace.pending = true
	}
	if trace.pending {
		for _, prefix := range []string{"alter ", "create ", "drop ", "truncate ", "insert ", "update ", "delete "} {
			if strings.HasPrefix(statement, prefix) {
				trace.attemptedWrite = true
			}
		}
		if statement == "commit" || statement == "rollback" {
			trace.pending = false
			local := &processAdmissionPGFixture{ctx: ctx, observer: connection}
			trace.verifiedCatalog = migrationVerificationStatePG(trace.test, local)
			if err := trace.transaction.Rollback(ctx); err != nil {
				trace.test.Fatal("roll back source-integrity drift instead of committing fixture DDL", err)
			}
			trace.rolledBack = true
		}
	}
	return trace.migrationVerificationTracePG.TraceQueryStart(ctx, connection, data)
}

func TestRootSourceIntegritySchemaPGVerifyMigrationsRejectsDrift(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	if _, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("install the unchanged production migration bundle in the owned database", err)
	}
	var controlsPresent bool
	if err := storage.pool.QueryRow(fixture.ctx, `SELECT count(*)=4 AND bool_and(COALESCE(
 relation.relkind='r' AND trigger.tgenabled='O' AND NOT trigger.tgisinternal
 AND trigger.tgtype=expected.events AND trigger.tgnargs=0 AND trigger.tgqual IS NULL
 AND cardinality(trigger.tgattr::smallint[])=0
 AND function.pronamespace='public'::regnamespace AND function.proname=expected.function_name
 AND function.pronargs=0 AND function.prorettype='trigger'::regtype,false))
 FROM (VALUES
 ('blobs','advance_doc_range_source_version','cxt_advance_doc_range_source_version',19),
 ('blobs','initialize_blob_incarnation','cxt_initialize_blob_incarnation',5),
 ('blob_source_incarnations','assign_blob_incarnation','cxt_assign_blob_incarnation',7),
 ('blob_source_incarnations','preserve_blob_incarnation','cxt_preserve_blob_incarnation',19)
 ) expected(relation_name,trigger_name,function_name,events)
 LEFT JOIN pg_class relation ON relation.relnamespace='public'::regnamespace AND relation.relname=expected.relation_name
 LEFT JOIN pg_trigger trigger ON trigger.tgrelid=relation.oid AND trigger.tgname=expected.trigger_name
 LEFT JOIN pg_proc function ON function.oid=trigger.tgfoid`).Scan(&controlsPresent); err != nil || !controlsPresent {
		test.Fatal("the production fixture lacks the exact source-generation and incarnation trigger bindings", err)
	}
	baseline := migrationVerificationStatePG(test, fixture)
	if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("the unchanged complete production schema must pass startup verification", err)
	}
	if after := migrationVerificationStatePG(test, fixture); after != baseline {
		test.Fatal("positive startup verification changed the catalog or migration receipts")
	}
	for _, scenario := range []struct {
		name         string
		statement    string
		catalogCheck string
	}{
		{
			name:      "advance-trigger-disabled",
			statement: `ALTER TABLE public.blobs DISABLE TRIGGER advance_doc_range_source_version`,
			catalogCheck: `SELECT EXISTS(SELECT 1 FROM pg_trigger
 WHERE tgrelid='public.blobs'::regclass AND tgname='advance_doc_range_source_version' AND tgenabled='D')`,
		},
		{
			name:      "advance-trigger-dropped",
			statement: `DROP TRIGGER advance_doc_range_source_version ON public.blobs`,
			catalogCheck: `SELECT NOT EXISTS(SELECT 1 FROM pg_trigger
 WHERE tgrelid='public.blobs'::regclass AND tgname='advance_doc_range_source_version')`,
		},
		{
			name: "advance-trigger-rebound",
			statement: `DROP TRIGGER advance_doc_range_source_version ON public.blobs;
 CREATE TRIGGER advance_doc_range_source_version BEFORE UPDATE ON public.blobs
 FOR EACH ROW EXECUTE FUNCTION public.cxt_preserve_blob_incarnation()`,
			catalogCheck: `SELECT EXISTS(SELECT 1 FROM pg_trigger
 WHERE tgrelid='public.blobs'::regclass AND tgname='advance_doc_range_source_version'
 AND tgenabled='O' AND tgtype=19 AND tgfoid='public.cxt_preserve_blob_incarnation()'::regprocedure)`,
		},
		{
			name:      "initialize-incarnation-trigger-disabled",
			statement: `ALTER TABLE public.blobs DISABLE TRIGGER initialize_blob_incarnation`,
			catalogCheck: `SELECT EXISTS(SELECT 1 FROM pg_trigger
 WHERE tgrelid='public.blobs'::regclass AND tgname='initialize_blob_incarnation' AND tgenabled='D')`,
		},
		{
			name:      "assign-incarnation-trigger-dropped",
			statement: `DROP TRIGGER assign_blob_incarnation ON public.blob_source_incarnations`,
			catalogCheck: `SELECT NOT EXISTS(SELECT 1 FROM pg_trigger
 WHERE tgrelid='public.blob_source_incarnations'::regclass AND tgname='assign_blob_incarnation')`,
		},
		{
			name: "preserve-incarnation-trigger-rebound",
			statement: `DROP TRIGGER preserve_blob_incarnation ON public.blob_source_incarnations;
 CREATE TRIGGER preserve_blob_incarnation BEFORE UPDATE ON public.blob_source_incarnations
 FOR EACH ROW EXECUTE FUNCTION public.cxt_assign_blob_incarnation()`,
			catalogCheck: `SELECT EXISTS(SELECT 1 FROM pg_trigger
 WHERE tgrelid='public.blob_source_incarnations'::regclass AND tgname='preserve_blob_incarnation'
 AND tgenabled='O' AND tgtype=19 AND tgfoid='public.cxt_assign_blob_incarnation()'::regprocedure)`,
		},
		{
			name: "advance-function-body-altered",
			statement: `CREATE OR REPLACE FUNCTION public.cxt_advance_doc_range_source_version()
 RETURNS trigger LANGUAGE plpgsql AS $drift$BEGIN RETURN NEW; END;$drift$`,
			catalogCheck: `SELECT EXISTS(SELECT 1 FROM pg_proc
 WHERE oid='public.cxt_advance_doc_range_source_version()'::regprocedure
 AND pronamespace='public'::regnamespace AND prosrc='BEGIN RETURN NEW; END;')`,
		},
		{
			name: "initialize-incarnation-function-body-altered",
			statement: `CREATE OR REPLACE FUNCTION public.cxt_initialize_blob_incarnation()
 RETURNS trigger LANGUAGE plpgsql AS $drift$BEGIN RETURN NULL; END;$drift$`,
			catalogCheck: `SELECT EXISTS(SELECT 1 FROM pg_proc
 WHERE oid='public.cxt_initialize_blob_incarnation()'::regprocedure
 AND pronamespace='public'::regnamespace AND prosrc='BEGIN RETURN NULL; END;')`,
		},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithTimeout(fixture.ctx, 5*time.Second)
			defer cancel()
			trace := &rootSourceIntegritySchemaTracePG{
				test: test, statement: scenario.statement, catalogCheck: scenario.catalogCheck, armed: true,
			}
			config := fixture.config.Copy()
			config.ConnConfig.RuntimeParams["search_path"] = "public"
			config.ConnConfig.RuntimeParams["statement_timeout"] = "3000"
			config.ConnConfig.RuntimeParams["lock_timeout"] = "1000"
			config.ConnConfig.Tracer = trace
			delete(config.ConnConfig.RuntimeParams, "role")
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				test.Fatal("open the rollback-only startup-verification connection", err)
			}
			defer pool.Close()
			reader := &PostgresStore{pool: pool}
			verificationErr := reader.VerifyMigrations(ctx, migrationVerificationBundlePG)
			if !trace.rolledBack || trace.armed || trace.pending {
				test.Fatal("startup verification did not finish the rollback-only drift transaction", verificationErr)
			}
			if !trace.readOnlyTransaction || !trace.sharedLockInTransaction || trace.attemptedWrite {
				test.Error("startup verification must request a read-only transaction and shared migration lock without DDL or repair")
			}
			if trace.tamperedCatalog == baseline {
				test.Error("the drift fixture did not change the source-integrity catalog")
			}
			if trace.verifiedCatalog != trace.tamperedCatalog {
				test.Error("verification repaired source-integrity drift or changed the catalog or migration receipts")
			}
			if after := migrationVerificationStatePG(test, fixture); after != baseline {
				test.Error("rollback-only source-integrity tampering escaped into the owned database")
			}
			if pool.Stat().AcquiredConns() != 0 {
				test.Error("source-integrity verification retained a pool connection")
			}
			if !errors.Is(verificationErr, ErrSchemaMismatch) {
				test.Error("startup accepted source-generation or incarnation schema drift instead of ErrSchemaMismatch", verificationErr)
			}
		})
	}
}
