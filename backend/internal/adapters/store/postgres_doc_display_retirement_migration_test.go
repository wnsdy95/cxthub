//go:build postgres

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func docDisplayRetirementPredecessorPG(test *testing.T, fixture docDisplayPGFixture) []byte {
	test.Helper()
	requireDocToolCatalogPGIsolation(test, fixture)
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0143_doc_display_catalog_retirement.sql")
	if err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != "7ed4e22782f9906693515a671088467dfc265e604cbab576e9c13f4a65441431" {
		test.Fatal("retirement handoff must apply the exact frozen migration")
	}
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	for _, table := range docCatalogWriterTablesPG {
		if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER `+table+`_a_catalog_delete ON `+table); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER doc_display_catalog_lifecycle_v1_restart_complete ON doc_display_catalog_lifecycle_v1;
 DROP FUNCTION cxthub_verify_doc_catalog_restart();
 DROP FUNCTION cxthub_guard_doc_tool_catalog_delete();
 DROP FUNCTION cxthub_restart_doc_catalog(text,uuid,text,bytea);
 DROP FUNCTION cxthub_finish_doc_catalog_retirement(text,uuid);
 DROP FUNCTION cxthub_begin_doc_catalog_retirement(text,uuid);
 DROP FUNCTION cxthub_require_doc_catalog_collector(text);
 DROP FUNCTION cxthub_bind_doc_catalog_collector(text,uuid);
 DROP FUNCTION cxthub_doc_catalog_clear(text,uuid,uuid);
 DROP FUNCTION cxthub_lock_doc_catalog_retirement(text,uuid);
 DROP FUNCTION cxthub_doc_catalog_empty(text);
 DROP INDEX doc_tool_key_members_v1_first;
 DROP INDEX doc_tool_key_members_v1_last;
 DROP INDEX doc_tool_key_observations_v1_tree_node;
 DROP INDEX doc_tool_order_plans_v1_tree_parent;
 DROP INDEX doc_tool_key_sources_v1_tree_scope;
 ALTER TABLE doc_display_catalog_lifecycle_v1 DROP CONSTRAINT doc_display_catalog_lifecycle_v1_retirement,
 DROP COLUMN retire_generation,DROP COLUMN retire_phase,DROP COLUMN retire_scope`); err != nil {
		test.Fatal("reconstruct the actual epoch-aware predecessor without retirement authority", err)
	}
	for _, previous := range []struct{ file, function string }{
		{"0142_doc_display_catalog_lifecycle.sql", "cxthub_guard_doc_catalog_lifecycle"},
		{"0131_doc_tool_key_map.sql", "cxthub_guard_doc_tool_key_row"},
		{"0132_doc_tool_orders.sql", "cxthub_guard_doc_tool_order_plan"},
	} {
		body, err := os.ReadFile("../../../../schemas/db/migrations/" + previous.file)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(fixture.ctx, docDisplayLifecycleHistoricalStatementPG(test, string(body), "function", previous.function)); err != nil {
			test.Fatal("restore the actual predecessor function body", previous.function, err)
		}
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("predecessor reconstruction changed original content, jobs or private data")
	}
	return raw
}

func TestDocDisplayRetirementPGMigrationBusyRollbackAndCompatibleWriter(test *testing.T) {
	for _, blocked := range []struct{ name, query string }{
		{"job", `SELECT repo_id FROM doc_display_jobs_v1 WHERE repo_id=$1 FOR UPDATE NOWAIT`},
		{"source", `SELECT source_hash FROM doc_display_sources_v1 WHERE source_hash=$1 FOR NO KEY UPDATE NOWAIT`},
		{"private", `SELECT scope FROM doc_tool_key_members_v1 ORDER BY scope,parent_from,digest LIMIT 1 FOR UPDATE NOWAIT`},
	} {
		test.Run(blocked.name, func(test *testing.T) {
			fixture, job, prepared := docCatalogProtocolFixturePG(test)
			step := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
			migration := docDisplayRetirementPredecessorPG(test, fixture)
			before := docCatalogUseImagePG(test, fixture)
			blocker := docCatalogUseBeginPG(test, fixture)
			var selected string
			source, _ := prepared.Source.Hash()
			arguments := []any{}
			if blocked.name == "job" {
				arguments = []any{job.RepoID}
			} else if blocked.name == "source" {
				arguments = []any{source}
			}
			if err := blocker.QueryRow(fixture.ctx, blocked.query, arguments...).Scan(&selected); err != nil || selected == "" {
				test.Fatal("hold an actual predecessor row before migration", err)
			}
			attempt := docCatalogUseBeginPG(test, fixture)
			_, err := attempt.Exec(fixture.ctx, string(migration))
			var failure *pgconn.PgError
			if !errors.As(err, &failure) || failure.Code != "55P03" {
				test.Fatal("busy predecessor owner did not reject retirement cutover atomically", err)
			}
			rollbackPG(attempt)
			rollbackPG(blocker)
			var absent bool
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT to_regprocedure('cxthub_begin_doc_catalog_retirement(text,uuid)') IS NULL
 AND NOT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_name='doc_display_catalog_lifecycle_v1' AND column_name='retire_phase')`).Scan(&absent); err != nil || !absent || docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("failed cutover leaked schema or data changes", absent, err)
			}
			retry := docCatalogUseBeginPG(test, fixture)
			if _, err := retry.Exec(fixture.ctx, string(migration)); err != nil {
				test.Fatal("retry the same frozen migration after the actual row owner releases", err)
			}
			if err := retry.Commit(fixture.ctx); err != nil || docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("compatible cutover rewrote content, checkpoint or private rows", err)
			}
			if saved, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, step); err != nil || !reflect.DeepEqual(saved, step.Preparation()) {
				test.Fatal("compatible epoch-aware work could not continue after retirement cutover", err)
			}
		})
	}
}
