//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func (storage *PostgresStore) VerifyMigrations(ctx context.Context, directory string) error {
	manifest, err := migrationManifest(ctx, directory)
	if err != nil {
		return err
	}
	if err := verifyMigrationCatalog(manifest); err != nil {
		return err
	}
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted, AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SELECT pg_advisory_xact_lock_shared(hashtextextended('cxt-schema-migrations',0))`); err != nil {
		return err
	}
	if err := verifyMigrationNamespace(ctx, transaction); err != nil {
		return err
	}
	names := []string{"schema_migrations"}
	if manifestContains(manifest, "0052_repository_organizations.sql") {
		names = append(names, "ownership_migrations")
	}
	if manifestContains(manifest, "0148_legacy_byte_accounting_authority.sql") {
		names = append(names, "doc_legacy_byte_usage_v1")
	}
	var safe bool
	if err := transaction.QueryRow(ctx, `SELECT count(*)=cardinality($1::text[])
 AND count(DISTINCT relowner)=1 AND COALESCE(bool_and(relkind='r' AND NOT relrowsecurity AND NOT relforcerowsecurity),false)
 FROM pg_class WHERE relnamespace='public'::regnamespace AND relname=ANY($1::text[])`, names).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("%w: migration control tables or their ownership are missing or unsafe", ErrSchemaMismatch)
	}
	applied, err := readMigrationHistory(ctx, transaction, "public.schema_migrations")
	if err != nil {
		return err
	}
	if err := migrationHistoryMatches(manifest, applied, true); err != nil {
		return err
	}
	if manifestContains(manifest, "0052_repository_organizations.sql") {
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.ownership_migrations WHERE version='repository-ownership-v1')`).Scan(&safe); err != nil {
			return err
		}
		if !safe {
			return fmt.Errorf("%w: repository ownership migration is not complete", ErrSchemaMismatch)
		}
	}
	if manifestContains(manifest, "0148_legacy_byte_accounting_authority.sql") {
		if err := verifyMigrationAccounting(ctx, transaction, manifest); err != nil {
			return err
		}
	}
	if manifestContains(manifest, "0152_settings_codex.sql") {
		if err := verifyMigrationSettingsKinds(ctx, transaction); err != nil {
			return err
		}
	}
	if manifestContains(manifest, "0109_blob_source_incarnation.sql") {
		if err := verifyMigrationRootSources(ctx, transaction); err != nil {
			return err
		}
	}
	return transaction.Commit(ctx)
}

func verifyMigrationSettingsKinds(ctx context.Context, transaction pgx.Tx) error {
	var safe bool
	if err := transaction.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM pg_constraint constraint_row
 JOIN pg_class relation ON relation.oid=constraint_row.conrelid
 JOIN pg_attribute attribute ON attribute.attrelid=relation.oid AND attribute.attname='kind'
 WHERE relation.relnamespace='public'::regnamespace AND relation.relname='repo_settings'
 AND relation.relkind='r' AND NOT relation.relrowsecurity AND NOT relation.relforcerowsecurity
 AND attribute.attnotnull AND NOT attribute.attisdropped AND attribute.atttypid='text'::regtype
 AND constraint_row.conname='repo_settings_kind_check' AND constraint_row.contype='c'
 AND constraint_row.convalidated AND constraint_row.conislocal AND constraint_row.coninhcount=0
 AND constraint_row.conkey=ARRAY[attribute.attnum]
 AND pg_get_constraintdef(constraint_row.oid,false)=
 'CHECK ((kind = ANY (ARRAY[''claude''::text, ''agents''::text, ''codex''::text])))'
)`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("%w: repository settings kind constraint is missing or incompatible", ErrSchemaMismatch)
	}
	return nil
}

func verifyMigrationNamespace(ctx context.Context, query interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) error {
	var safe bool
	if err := query.QueryRow(ctx, `SELECT current_schemas(true)=ARRAY['pg_catalog','public']::name[] AND pg_my_temp_schema()=0`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("%w: migration target must resolve only pg_catalog then public without a temporary schema", ErrSchemaMismatch)
	}
	return nil
}

func readMigrationHistory(ctx context.Context, query interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, relation string) (map[string]string, error) {
	rows, err := query.Query(ctx, `SELECT version,checksum FROM `+relation+` ORDER BY version`)
	if err != nil {
		return nil, migrationHistoryReadError(err)
	}
	defer rows.Close()
	applied := make(map[string]string)
	for rows.Next() {
		var name, checksum string
		if err := rows.Scan(&name, &checksum); err != nil {
			return nil, err
		}
		applied[name] = checksum
	}
	return applied, migrationHistoryReadError(rows.Err())
}

func migrationHistoryReadError(err error) error {
	var failure *pgconn.PgError
	if errors.As(err, &failure) && (failure.Code == "42P01" || failure.Code == "42703" || failure.Code == "42809") {
		return fmt.Errorf("%w: migration history structure is incomplete: %w", ErrSchemaMismatch, err)
	}
	return err
}

func verifyMigrationAccounting(ctx context.Context, transaction pgx.Tx, manifest []schemaMigration) error {
	var safe bool
	if err := transaction.QueryRow(ctx, `SELECT count(*)=2 AND COALESCE(bool_and(COALESCE(
 function.pronargs=0 AND function.prorettype='trigger'::regtype AND function.proowner=usage.relowner
 AND function.prosecdef=(function.proname='cxt_account_legacy_byte_storage')
 AND array_length(function.proconfig,1)=1
 AND replace(function.proconfig[1],' ','')='search_path=pg_catalog,public,pg_temp'
 AND NOT EXISTS(SELECT 1 FROM aclexplode(COALESCE(function.proacl,acldefault('f',function.proowner))) permission
  WHERE permission.grantee=0 AND permission.privilege_type='EXECUTE')
	 AND NOT EXISTS(SELECT 1 FROM aclexplode(COALESCE(usage.relacl,acldefault('r',usage.relowner))) permission WHERE permission.grantee=0),false)),false)
 FROM pg_proc function CROSS JOIN pg_class usage
 WHERE function.pronamespace='public'::regnamespace
 AND function.proname IN ('cxt_account_legacy_byte_storage','cxt_guard_legacy_byte_usage')
 AND usage.oid='public.doc_legacy_byte_usage_v1'::regclass`).Scan(&safe); err != nil {
		return err
	}
	if !safe {
		return fmt.Errorf("%w: legacy byte accounting authority is unsafe", ErrSchemaMismatch)
	}
	type accountingBinding struct {
		relation, name, function string
		events                   int16
	}
	bindings := []accountingBinding{
		{"doc_legacy_byte_usage_v1", "guard_legacy_byte_usage", "cxt_guard_legacy_byte_usage", 31},
		{"doc_legacy_byte_stages_v1", "account_legacy_byte_source", "cxt_account_legacy_byte_storage", 5},
		{"doc_legacy_byte_stage_objects_v1", "account_legacy_byte_objects", "cxt_account_legacy_byte_storage", 13},
		{"doc_legacy_byte_stage_parts_v1", "account_legacy_byte_parts", "cxt_account_legacy_byte_storage", 13},
	}
	if manifestContains(manifest, "0151_legacy_byte_header_accounting.sql") {
		bindings = append(bindings, accountingBinding{"doc_legacy_byte_stage_headers_v1", "account_legacy_byte_headers", "cxt_account_legacy_byte_storage", 13})
	}
	for _, binding := range bindings {
		if err := transaction.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_trigger trigger
 JOIN pg_class relation ON relation.oid=trigger.tgrelid JOIN pg_proc function ON function.oid=trigger.tgfoid
 WHERE relation.relnamespace='public'::regnamespace AND relation.relname=$1
 AND trigger.tgname=$2 AND trigger.tgenabled IN ('O','A') AND NOT trigger.tgisinternal
	 AND trigger.tgtype=$4 AND trigger.tgnargs=0 AND trigger.tgqual IS NULL
	 AND function.pronamespace='public'::regnamespace AND function.proname=$3 AND function.pronargs=0)`, binding.relation, binding.name, binding.function, binding.events).Scan(&safe); err != nil {
			return err
		}
		if !safe {
			return fmt.Errorf("%w: legacy byte accounting trigger %q is missing or unsafe", ErrSchemaMismatch, binding.name)
		}
	}
	return nil
}
