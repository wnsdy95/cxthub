//go:build postgres

package store

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The tables 0171 drops. Nothing that stays names them.
var captureLeftoverTables = []string{
	"context_history", "doc_legacy_byte_stage_checkpoint_objects_v1", "doc_legacy_checkpoint_edges_v1",
	"doc_legacy_checkpoint_staging_v1", "doc_legacy_checkpoint_structures_v1", "git_commit_deltas", "git_commit_trees",
	"git_context_heads", "git_contribution_admissions", "git_contribution_proofs", "git_contributions",
	"git_head_receipts", "git_ref_observations", "git_tree_nodes", "graph_projection_work", "memory_read_object_wakes",
	"memory_read_wakes", "pending_contexts", "pending_projection_work", "pr_contributions", "pr_history_checkpoints",
	"pr_participations", "reflog", "refs", "repository_catalog_changes", "repository_graph_source_changes",
	"repository_graph_source_state", "repository_memory_source_state", "repository_view_source_changes",
	"repository_view_source_state", "session_archives", "settings_objects", "snapshots", "unsync_contexts",
}

// The tables that stay whose rows 0171 changes on purpose. Each is checked on
// its own.
var captureLeftoverChanged = []string{
	"doc_legacy_byte_stages_v1", "doc_legacy_byte_usage_v1", "schema_migrations",
	"storage_accounts", "storage_usage_ledger", "storage_usage_objects",
}

// The rows of every table that stays and that 0171 must not change.
func captureLeftoverImage(test *testing.T, ctx context.Context, storage *PostgresStore) map[string]string {
	test.Helper()
	rows, err := storage.pool.Query(ctx, `SELECT relname FROM pg_class
 WHERE relnamespace='public'::regnamespace AND relkind='r' ORDER BY relname`)
	if err != nil {
		test.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		test.Fatal(err)
	}
	image := map[string]string{}
	for _, table := range tables {
		if slices.Contains(captureLeftoverTables, table) || slices.Contains(captureLeftoverChanged, table) {
			continue
		}
		var body string
		if err := storage.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(stored) ORDER BY to_jsonb(stored)::text),'[]'::jsonb)::text FROM `+
			pgx.Identifier{"public", table}.Sanitize()+` stored`).Scan(&body); err != nil {
			test.Fatal(table, err)
		}
		image[table] = body
	}
	return image
}

func captureLeftoverText(test *testing.T, ctx context.Context, storage *PostgresStore, query string, arguments ...any) string {
	test.Helper()
	var text string
	if err := storage.pool.QueryRow(ctx, query, arguments...).Scan(&text); err != nil {
		test.Fatalf("%s: %v", query, err)
	}
	return text
}

func TestPGDropCaptureLeftoversKeepsWhatStays(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	storage, err := NewPostgresStore(ctx, rootProjectionIsolatedDSN(test, ctx))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	const bundle = "../../../../schemas/db/migrations"
	earlier := test.TempDir()
	entries, err := os.ReadDir(bundle)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".sql" || entry.Name() > "0170_collector_reports.sql" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(bundle, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(earlier, entry.Name()), raw, 0600); err != nil {
			test.Fatal(err)
		}
	}
	if n, err := storage.ApplyMigrations(ctx, earlier); err != nil || n != 170 {
		test.Fatal("migrate a fresh database to 0170", n, err)
	}

	// What stays: a repository with a document, its settings and secrets.
	repo, namespace := rootPGRepo(test, ctx, storage)
	document := []byte(`{"document":"held by a capture and by its repository"}`)
	documentHash := string(domain.HashContent(document))
	exec := func(statement string, arguments ...any) {
		test.Helper()
		if _, err := storage.pool.Exec(ctx, statement, arguments...); err != nil {
			test.Fatalf("%s: %v", statement, err)
		}
	}
	exec(`INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, documentHash, document)
	exec(`INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo, documentHash)
	if err := storage.PutSettingsBundle(ctx, repo, domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "settings.json", ContentB64: "e30="}},
		UpdatedAt: time.Now().UTC().Truncate(time.Second)}); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSecretsEnvelope(ctx, repo, []byte(`{"version":1}`)); err != nil {
		test.Fatal(err)
	}

	// What goes: a capture of that document with its ref, history, archive and
	// settings object, and a legacy normalization checkpoint of the document.
	settingsHash := string(domain.HashContent([]byte("settings object of a capture")))
	exec(`INSERT INTO settings_objects(repo_id,hash,data) VALUES($1,$2,'{"settings":"of a capture"}')`, repo, settingsHash)
	exec(`INSERT INTO snapshots(id,repo_id,branch,doc_hash,provider,fidelity) VALUES($2,$1,'main',$2,'claude','full')`, repo, documentHash)
	exec(`INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'branch','main',$2)`, repo, documentHash)
	exec(`INSERT INTO reflog(repo_id,kind,name,new) VALUES($1,'branch','main',$2)`, repo, documentHash)
	exec(`INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,'{"kind":"birth"}')`, repo, strings.Repeat("a", 32))
	exec(`INSERT INTO session_archives(repo_id,key,snapshot_id,provider,session_id,archived_at,archived_by) VALUES($1,$2,$2,'claude','old',now(),'someone')`, repo, documentHash)
	exec(`INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,'pending','{}')`, repo)
	// The root is an index structure of the staged object, so that the root
	// also has a structure row and an edge.
	staged := []byte(`{"checkpoint":"staged"}`)
	root := []byte(`{"e":[{"h":"` + string(domain.HashContent(staged)) + `","n":` + strconv.Itoa(len(staged)) + `}],"v":"1"}`)
	checkpoint, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(checkpoint)
	for _, statement := range []struct {
		sql       string
		arguments []any
	}{
		{`SET LOCAL cxt.legacy_byte_stage_protocol='1'`, nil},
		{`SET LOCAL cxt.legacy_byte_checkpoint_protocol='1'`, nil},
		{`INSERT INTO doc_legacy_byte_stages_v1(doc_hash,source_id,source_version,next_block,complete)
 SELECT source.hash,incarnation.incarnation,source.doc_range_source_version,0,false
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, []any{documentHash}},
		{`INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash,bytes)
 SELECT stage.doc_hash,stage.stage_id,'sha256:'||encode(sha256(object.bytes),'hex'),object.bytes
 FROM doc_legacy_byte_stages_v1 stage CROSS JOIN unnest(ARRAY[$2::bytea,$3::bytea]) object(bytes) WHERE stage.doc_hash=$1`, []any{documentHash, root, staged}},
		{`INSERT INTO doc_legacy_checkpoint_structures_v1(doc_hash,stage_id,hash,kind)
 SELECT doc_hash,stage_id,'sha256:'||encode(sha256($2::bytea),'hex'),1 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, []any{documentHash, root}},
		{`UPDATE doc_legacy_byte_stages_v1 SET checkpoint_sequence=1,checkpoint_hash='sha256:'||encode(sha256($2::bytea),'hex'),checkpoint_bytes=octet_length($2::bytea)
 WHERE doc_hash=$1`, []any{documentHash, root}},
		{`INSERT INTO doc_legacy_checkpoint_staging_v1(doc_hash,stage_id,hash,last_sequence)
 SELECT doc_hash,stage_id,'sha256:'||encode(sha256($2::bytea),'hex'),1 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, []any{documentHash, staged}},
	} {
		if _, err := checkpoint.Exec(ctx, statement.sql, statement.arguments...); err != nil {
			test.Fatalf("%s: %v", statement.sql, err)
		}
	}
	if err := checkpoint.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if counted := captureLeftoverText(test, ctx, storage, `SELECT checkpoint_objects||' '||checkpoint_bytes||' '||(SELECT count(*) FROM doc_legacy_checkpoint_edges_v1)
 FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, documentHash); counted != "2 "+strconv.Itoa(len(root)+len(staged))+" 1" {
		test.Fatal("the checkpoint objects were not counted before the migration", counted)
	}
	settingsBytes := captureLeftoverText(test, ctx, storage, `SELECT bytes::text FROM storage_usage_objects WHERE namespace_id=$1 AND object_key='settings:'||$2`, namespace, settingsHash)
	usedBefore := captureLeftoverText(test, ctx, storage, `SELECT current_bytes::text FROM storage_accounts WHERE namespace_id=$1`, namespace)
	stageBefore := captureLeftoverText(test, ctx, storage, `SELECT (to_jsonb(stage)-'checkpoint_hash'-'checkpoint_bytes')::text FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1`, documentHash)
	usageBefore := captureLeftoverText(test, ctx, storage, `SELECT (to_jsonb(usage)-'checkpoint_objects'-'checkpoint_bytes')::text FROM doc_legacy_byte_usage_v1 usage WHERE doc_hash=$1`, documentHash)
	epochBefore := captureLeftoverText(test, ctx, storage, `SELECT epoch::text FROM repository_catalog_state WHERE repo_id=$1`, repo)
	ledgerBefore := captureLeftoverText(test, ctx, storage, `SELECT COALESCE(max(seq),0)::text FROM storage_usage_ledger WHERE namespace_id=$1`, namespace)
	before := captureLeftoverImage(test, ctx, storage)

	if n, err := storage.ApplyPublicMigrations(ctx, bundle); err != nil || n != 1 {
		test.Fatal("apply 0171 with the operator path", n, err)
	}
	if err := storage.VerifyMigrations(ctx, bundle); err != nil {
		test.Fatal("the migrated database must pass startup verification", err)
	}

	for _, table := range captureLeftoverTables {
		if gone := captureLeftoverText(test, ctx, storage, `SELECT (to_regclass('public.'||$1) IS NULL)::text`, table); gone != "true" {
			test.Errorf("table %s is still there", table)
		}
	}
	for _, object := range []string{"repository_view_source_images", "repository_graph_source_images", "graph_serving_fence",
		"memory_read_object_wake_fence_seq", "memory_read_wake_seq", "memory_read_work_fence_seq"} {
		if gone := captureLeftoverText(test, ctx, storage, `SELECT (to_regclass('public.'||$1) IS NULL)::text`, object); gone != "true" {
			test.Errorf("%s is still there", object)
		}
	}
	if left := captureLeftoverText(test, ctx, storage, `SELECT COALESCE(string_agg(proname,' ' ORDER BY proname),'')
 FROM pg_proc function WHERE pronamespace='public'::regnamespace
 AND NOT EXISTS(SELECT 1 FROM pg_depend WHERE classid='pg_proc'::regclass AND objid=function.oid AND deptype='e')
 AND NOT EXISTS(SELECT 1 FROM pg_trigger WHERE tgfoid=function.oid)
 AND proname NOT IN ('cxt_reset_catalog','cxt_storage_apply','cxt_storage_reconcile','cxt_storage_refresh_key','prune_doc_read_block_hashes_v3')`); left != "" {
		test.Fatal("functions that nothing calls are left", left)
	}
	after := captureLeftoverImage(test, ctx, storage)
	for table, rows := range before {
		if after[table] != rows {
			test.Errorf("the rows of %s changed:\n%s\n%s", table, rows, after[table])
		}
	}
	if len(after) != len(before) {
		test.Fatal("the tables that stay changed", len(before), len(after))
	}

	// Storage accounting released the settings object through its own trigger.
	if left := captureLeftoverText(test, ctx, storage, `SELECT count(*)::text FROM storage_usage_objects WHERE object_key LIKE 'settings:%'`); left != "0" {
		test.Fatal("settings objects are still counted", left)
	}
	if released := captureLeftoverText(test, ctx, storage, `SELECT string_agg(reason||' '||object_key||' '||(-delta_bytes)::text,',' ORDER BY seq)
 FROM storage_usage_ledger WHERE namespace_id=$1 AND seq>$2::bigint`, namespace, ledgerBefore); released != "payload.delete settings:"+settingsHash+" "+settingsBytes {
		test.Fatal("the release of the settings object was not recorded", released, settingsBytes)
	}
	if used := captureLeftoverText(test, ctx, storage, `SELECT (current_bytes+$2::bigint)::text FROM storage_accounts WHERE namespace_id=$1`, namespace, settingsBytes); used != usedBefore {
		test.Fatal("the namespace still counts the settings object", used, usedBefore)
	}
	if balanced := captureLeftoverText(test, ctx, storage, `SELECT (current_bytes=(SELECT COALESCE(sum(bytes),0) FROM storage_usage_objects WHERE namespace_id=$1))::text
 FROM storage_accounts WHERE namespace_id=$1`, namespace); balanced != "true" {
		test.Fatal("the namespace's usage does not add up after the migration")
	}

	// The legacy stage lost only its checkpoint root, and its usage only the
	// checkpoint objects.
	if stage := captureLeftoverText(test, ctx, storage, `SELECT (checkpoint_hash IS NULL AND checkpoint_bytes=0)::text||' '||(to_jsonb(stage)-'checkpoint_hash'-'checkpoint_bytes')::text
 FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1`, documentHash); stage != "true "+stageBefore {
		test.Fatal("the legacy stage changed beyond its checkpoint root", stage, stageBefore)
	}
	if usage := captureLeftoverText(test, ctx, storage, `SELECT (checkpoint_objects=0 AND checkpoint_bytes=0)::text||' '||(to_jsonb(usage)-'checkpoint_objects'-'checkpoint_bytes')::text
 FROM doc_legacy_byte_usage_v1 usage WHERE doc_hash=$1`, documentHash); usage != "true "+usageBefore {
		test.Fatal("the legacy usage changed beyond its checkpoint objects", usage, usageBefore)
	}

	// The triggers that stay still do their work.
	added, addedNamespace := rootPGRepo(test, ctx, storage)
	if state := captureLeftoverText(test, ctx, storage, `SELECT count(*)::text FROM repository_catalog_state WHERE repo_id=$1`, added); state != "1" {
		test.Fatal("a new repository has no catalog state", state)
	}
	fresh := []byte(`{"document":"stored after the migration"}`)
	freshHash := string(domain.HashContent(fresh))
	exec(`INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, freshHash, fresh)
	exec(`INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, added, freshHash)
	if incarnation := captureLeftoverText(test, ctx, storage, `SELECT count(*)::text FROM blob_source_incarnations WHERE hash=$1`, freshHash); incarnation != "1" {
		test.Fatal("a new blob has no source incarnation", incarnation)
	}
	if counted := captureLeftoverText(test, ctx, storage, `SELECT bytes::text FROM storage_usage_objects WHERE namespace_id=$1 AND object_key='blob:'||$2`, addedNamespace, freshHash); counted != strconv.Itoa(len(fresh)) {
		test.Fatal("a new document is not counted", counted)
	}
	if _, err := storage.pool.Exec(ctx, `TRUNCATE repo_blobs`); err == nil || !strings.Contains(err.Error(), "TRUNCATE is unsupported") {
		test.Fatal("TRUNCATE of repo_blobs was not refused", err)
	}
	exec(`SELECT cxt_storage_reconcile($1)`, namespace)
	if used := captureLeftoverText(test, ctx, storage, `SELECT (current_bytes+$2::bigint)::text FROM storage_accounts WHERE namespace_id=$1`, namespace, settingsBytes); used != usedBefore {
		test.Fatal("a reconcile changed the usage the migration left", used, usedBefore)
	}
	exec(`SELECT cxt_reset_catalog($1)`, repo)
	if state := captureLeftoverText(test, ctx, storage, `SELECT (epoch::text<>$2 AND head_seq=0 AND floor_seq=0 AND writer_xid IS NULL)::text FROM repository_catalog_state WHERE repo_id=$1`, repo, epochBefore); state != "true" {
		test.Fatal("the reset did not rotate the epoch", state)
	}
	// The document a capture held can be deleted now.
	if err := storage.DeleteDoc(ctx, repo, domain.ContentHash(documentHash)); err != nil {
		test.Fatal("delete the document the capture held", err)
	}
	if left := captureLeftoverText(test, ctx, storage, `SELECT (SELECT count(*) FROM blobs WHERE hash=$1)+(SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1)||''`, documentHash); left != "0" {
		test.Fatal("the document or its stage is still stored", left)
	}
}
