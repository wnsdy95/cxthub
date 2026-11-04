//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
)

func rootSourceFunctionDefinitionsPG() map[string]string {
	return map[string]string{
		"cxt_advance_doc_range_source_version": "bc2e3079d076794550de5983f8a8daae659c149011a2b2a549e60abaea69bad8",
		"cxt_assign_blob_incarnation":          "50dc341b9a190f43f8a43be1ddaaace338855be38a3ce453a0d25cf390a64431",
		"cxt_preserve_blob_incarnation":        "0b08f8b8736dfd21e394e6242fd5666ace199372317d86ade1b1a7e2e3cbc2bd",
		"cxt_initialize_blob_incarnation":      "f9b7a23c1f758c646574669e85cc6cbb87bb6b246cf049784be3d8fd247a00f1",
		"cxt_capture_catalog":                  "ee492eecd49cbe654e623ec09d187e3097711462ae66301ff0af80b0af6542a6",
	}
}

func verifyMigrationRootSources(ctx context.Context, database interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}) error {
	definitions := rootSourceFunctionDefinitionsPG()
	rows, err := database.Query(ctx, `WITH relations AS (
 SELECT count(*)=6 AND COALESCE(bool_and(relation.relkind='r'
  AND relation.relpersistence='p' AND NOT relation.relrowsecurity AND NOT relation.relforcerowsecurity
  AND relation.relowner=control.relowner),false) AS safe
 FROM pg_class relation CROSS JOIN pg_class control
 WHERE relation.relnamespace='public'::regnamespace
 AND relation.relname IN ('blobs','blob_source_incarnations','repo_blobs','repository_catalog_state','doc_finalization_jobs','repos')
 AND control.oid='public.schema_migrations'::regclass AND control.relkind='r'
 AND control.relpersistence='p' AND NOT control.relrowsecurity AND NOT control.relforcerowsecurity
), columns AS (
 SELECT count(attribute.attnum)=9 AND COALESCE(bool_and(attribute.attnotnull AND NOT attribute.attisdropped
  AND attribute.atttypid=expected.kind::regtype),false) AS safe
 FROM (VALUES ('blobs','hash','text'),('blobs','bytes','bytea'),('blobs','doc_range_source_version','int8'),
  ('blob_source_incarnations','hash','text'),('blob_source_incarnations','incarnation','uuid'),
  ('repo_blobs','hash','text'),('repository_catalog_state','repo_id','text'),
  ('repository_catalog_state','epoch','uuid'),('doc_finalization_jobs','repo_id','text')) expected(relation_name,attribute_name,kind)
 LEFT JOIN pg_class relation ON relation.relnamespace='public'::regnamespace AND relation.relname=expected.relation_name
 LEFT JOIN pg_attribute attribute ON attribute.attrelid=relation.oid AND attribute.attname=expected.attribute_name
), keys AS (
 SELECT COALESCE(bool_and(EXISTS(SELECT 1 FROM pg_constraint constraint_row
  JOIN pg_class relation ON relation.oid=constraint_row.conrelid
  JOIN pg_attribute attribute ON attribute.attrelid=relation.oid AND attribute.attname=expected.attribute_name
  WHERE relation.relnamespace='public'::regnamespace AND relation.relname=expected.relation_name
  AND constraint_row.contype='p' AND constraint_row.convalidated AND NOT constraint_row.condeferrable
  AND constraint_row.conkey=ARRAY[attribute.attnum])),false)
  AND EXISTS(SELECT 1 FROM pg_class relation
   JOIN pg_attribute attribute ON attribute.attrelid=relation.oid AND attribute.attname='epoch'
   JOIN pg_attrdef defaults ON defaults.adrelid=relation.oid AND defaults.adnum=attribute.attnum
   WHERE relation.relnamespace='public'::regnamespace AND relation.relname='repository_catalog_state'
   AND pg_get_expr(defaults.adbin,defaults.adrelid)='gen_random_uuid()') AS safe
 FROM (VALUES ('blobs','hash'),('blob_source_incarnations','hash'),('repository_catalog_state','repo_id')) expected(relation_name,attribute_name)
), lifetimes AS (
 SELECT COALESCE(bool_and(EXISTS(SELECT 1 FROM pg_constraint constraint_row
  JOIN pg_class relation ON relation.oid=constraint_row.conrelid
  JOIN pg_class target ON target.oid=constraint_row.confrelid
  JOIN pg_attribute source_column ON source_column.attrelid=relation.oid AND source_column.attname=expected.attribute_name
  JOIN pg_attribute target_column ON target_column.attrelid=target.oid AND target_column.attname=expected.target_attribute
  WHERE constraint_row.contype='f' AND constraint_row.convalidated AND NOT constraint_row.condeferrable
  AND relation.relnamespace='public'::regnamespace AND relation.relname=expected.relation_name
  AND target.relnamespace='public'::regnamespace AND target.relname=expected.target_name
  AND constraint_row.conkey=ARRAY[source_column.attnum] AND constraint_row.confkey=ARRAY[target_column.attnum]
  AND constraint_row.confdeltype=expected.deletion::"char")),false) AS safe
 FROM (VALUES ('blob_source_incarnations','hash','blobs','hash','c'),('repo_blobs','hash','blobs','hash','a'),
  ('repository_catalog_state','repo_id','repos','id','c'),('doc_finalization_jobs','repo_id','repos','id','c'))
  expected(relation_name,attribute_name,target_name,target_attribute,deletion)
)
 SELECT expected.function_name,COALESCE(function.prosrc,''),COALESCE(
  relations.safe AND columns.safe AND keys.safe AND lifetimes.safe
  AND current_schemas(true)=ARRAY['pg_catalog','public']::name[] AND pg_my_temp_schema()=0
  AND trigger.tgenabled IN ('O','A') AND NOT trigger.tgisinternal AND trigger.tgtype=expected.events
  AND trigger.tgnargs=expected.arguments AND trigger.tgargs=expected.argument_bytes
  AND trigger.tgqual IS NULL AND cardinality(trigger.tgattr::smallint[])=0
  AND function.pronamespace='public'::regnamespace AND function.proname=expected.function_name
  AND function.pronargs=0 AND function.prorettype='trigger'::regtype AND function.prokind='f'
  AND function.prolang=(SELECT oid FROM pg_language WHERE lanname='plpgsql')
  AND function.proowner=relation.relowner AND NOT function.prosecdef AND NOT function.proretset
  AND NOT function.proisstrict AND function.provolatile='v' AND function.proparallel='u'
  AND function.proconfig IS NULL,false)
 FROM (VALUES ('blobs','advance_doc_range_source_version','cxt_advance_doc_range_source_version',19,0,''::bytea),
  ('blobs','initialize_blob_incarnation','cxt_initialize_blob_incarnation',5,0,''::bytea),
  ('blob_source_incarnations','assign_blob_incarnation','cxt_assign_blob_incarnation',7,0,''::bytea),
  ('blob_source_incarnations','preserve_blob_incarnation','cxt_preserve_blob_incarnation',19,0,''::bytea),
  ('repos','catalog_protocol','cxt_capture_catalog',21,1,decode('70726f746f636f6c00','hex')))
  expected(relation_name,trigger_name,function_name,events,arguments,argument_bytes)
 CROSS JOIN relations CROSS JOIN columns CROSS JOIN keys CROSS JOIN lifetimes
 LEFT JOIN pg_class relation ON relation.relnamespace='public'::regnamespace AND relation.relname=expected.relation_name
 LEFT JOIN pg_trigger trigger ON trigger.tgrelid=relation.oid AND trigger.tgname=expected.trigger_name
 LEFT JOIN pg_proc function ON function.oid=trigger.tgfoid
 ORDER BY expected.function_name`)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var name, definition string
		var safe bool
		if err := rows.Scan(&name, &definition, &safe); err != nil {
			return err
		}
		digest := sha256.Sum256([]byte(definition))
		expected, exists := definitions[name]
		if !safe || !exists || hex.EncodeToString(digest[:]) != expected {
			return fmt.Errorf("%w: root source lifetime contract %q is missing or incompatible", ErrSchemaMismatch, name)
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(definitions) {
		return fmt.Errorf("%w: root source lifetime contracts are incomplete", ErrSchemaMismatch)
	}
	return nil
}
