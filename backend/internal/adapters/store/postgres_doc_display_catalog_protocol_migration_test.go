//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docCatalogProtocolLegacyImagePG(test *testing.T, fixture docDisplayPGFixture) []byte {
	test.Helper()
	legacy, err := os.ReadFile("../../../../schemas/db/migrations/0138_doc_display_catalog_writer.sql")
	if err != nil {
		test.Fatal(err)
	}
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	for _, name := range []string{"cxthub_guard_doc_catalog_claim", "cxthub_bind_doc_catalog_writer", "cxthub_require_doc_catalog_writer", "cxthub_guard_doc_catalog_job_write", "cxthub_guard_doc_tool_catalog_write"} {
		start := strings.Index(string(legacy), "CREATE FUNCTION "+name+"(")
		if start < 0 {
			test.Fatal("missing actual historical writer function", name)
		}
		end := strings.Index(string(legacy[start:]), "\n$$;")
		if end < 0 {
			test.Fatal("unterminated actual historical writer function", name)
		}
		body := strings.Replace(string(legacy[start:start+end+len("\n$$;")]), "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
		if _, err := transaction.Exec(fixture.ctx, body); err != nil {
			test.Fatal("restore protocol-1 function body in owned fixture", name, err)
		}
	}
	if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER guard_doc_display_zz_catalog_write ON doc_display_jobs_v1;
 CREATE TRIGGER guard_doc_display_zz_catalog_write
 BEFORE UPDATE OF source_hash,checkpoint_hash,checkpoint,preparation_hash,preparation,discovery_hash,discovery,state ON doc_display_jobs_v1
 FOR EACH ROW EXECUTE FUNCTION cxthub_guard_doc_catalog_job_write();
 DROP FUNCTION IF EXISTS cxthub_bind_doc_catalog_calculation(text,text,integer,text,bigint,text,uuid);
 DROP FUNCTION IF EXISTS cxthub_bind_doc_catalog_control(text,text,integer,text,bigint)`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0141_doc_display_catalog_calculation.sql")
	if err != nil {
		test.Fatal(err)
	}
	return raw
}

func docCatalogProtocolImmutableImagePG(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var image map[string]any
	if err := json.Unmarshal([]byte(docCatalogUseImagePG(test, fixture)), &image); err != nil {
		test.Fatal(err)
	}
	for _, row := range image["display"].(map[string]any)["jobs"].([]any) {
		job := row.(map[string]any)
		for _, field := range []string{"state", "version", "lease_until", "reason", "next_attempt"} {
			delete(job, field)
		}
	}
	raw, err := json.Marshal(image)
	if err != nil {
		test.Fatal(err)
	}
	return string(raw)
}

func docCatalogProtocolLegacyReplayPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) {
	test.Helper()
	before := docCatalogUseImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_writer($1,$2,$3,$4,$5)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version); err != nil {
		test.Fatal("actual old binder must work before cutover", err)
	}
	for _, table := range docCatalogWriterTablesPG {
		for _, replay := range docCatalogProtocolReplaysPG(table) {
			if tag, err := transaction.Exec(fixture.ctx, replay.statement); err != nil || tag.RowsAffected() != replay.rows {
				test.Fatal("actual old private replay must work before cutover", table, replay.operation, tag, err)
			}
		}
	}
	if err := transaction.Commit(fixture.ctx); err != nil || docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("old exact replay must commit without changing content", err)
	}
}

func docCatalogProtocolCutoverPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, raw []byte) {
	test.Helper()
	before := docCatalogProtocolImmutableImagePG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, string(raw)); err != nil {
		test.Fatal("cut over populated actual protocol-1 function image", err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	var state, reason string
	var version int64
	var released, due bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,version,reason,lease_until IS NULL,next_attempt<=clock_timestamp()
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).
		Scan(&state, &version, &reason, &released, &due); err != nil || state != "waiting" || version != job.Version+1 || reason != "" || !released || !due {
		test.Fatal("cutover failed to invalidate and requeue the live token", state, version, reason, released, due, err)
	}
	if docCatalogProtocolImmutableImagePG(test, fixture) != before {
		test.Fatal("cutover changed preparation, hashes, formatter, Source or private content")
	}
}

func TestDocDisplayCatalogProtocolPGMigrationDrainsPopulatedLegacy(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	requested := 0
	calculated := docToolValuesPGStep(test, fixture, job, prepared, &requested)
	raw := docCatalogProtocolLegacyImagePG(test, fixture)
	docCatalogProtocolLegacyReplayPG(test, fixture, job)
	docCatalogProtocolCutoverPG(test, fixture, job, raw)
	before := docCatalogUseImagePG(test, fixture)
	if result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, calculated); !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) || docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("already-calculated legacy token wrote after cutover", err)
	}
	freshJob := claimDocCatalogHistoricalProtocolPG(test, fixture, job, "2")
	if freshJob.Version <= job.Version+1 {
		test.Fatal("new claimant reused a drained version")
	}
	fresh := docToolValuesPGStep(test, fixture, freshJob, prepared, &requested)
	_, _, hasTree := fresh.ToolTreeScan()
	_, _, hasKeys := fresh.ToolKeyMap()
	_, _, hasOrders := fresh.ToolOrders()
	if hasTree || hasKeys || hasOrders {
		test.Fatal("historical continuation must be the actual post-order CAS-only step")
	}
	use, sealed := fresh.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("historical continuation lacks its actual Source read witness", use)
	}
	err := fixture.storage.docDisplayWrite(fixture.ctx, freshJob, time.Time{}, func(bound context.Context, transaction pgx.Tx) error {
		if _, err := transaction.Exec(bound, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid)`,
			freshJob.RepoID, freshJob.DocHash, freshJob.Target.Event.Index, freshJob.Target.DisplayProfile(), freshJob.Version, use.Source, use.Generation); err != nil {
			return err
		}
		if err := putDocDisplayObjects(bound, transaction, use.Source, fresh.Objects()); err != nil {
			return err
		}
		encoded, err := fresh.Preparation().Encode()
		if err != nil {
			return err
		}
		tag, err := transaction.Exec(bound, `UPDATE doc_display_jobs_v1 SET preparation_hash=$4,preparation=$5
 WHERE `+docDisplayKey(freshJob.Target)+` AND preparation_hash=$6 AND version=$7 AND state='running' AND lease_until>clock_timestamp()`,
			freshJob.RepoID, freshJob.DocHash, freshJob.Target.Event.Index, domain.HashContent(encoded), encoded, fresh.Prior(), freshJob.Version)
		if err == nil && tag.RowsAffected() != 1 {
			return domain.ErrConflict
		}
		return err
	})
	if err != nil {
		test.Fatal("historical protocol2 failed to continue the preserved checkpoint", err)
	}
	if result := docCatalogProgressLoadPG(test, fixture, freshJob); !reflect.DeepEqual(result, fresh.Preparation()) {
		test.Fatal("historical protocol2 did not retain the actual advancing checkpoint")
	}
}

func TestDocDisplayCatalogProtocolPGMigrationRefusesRowOwnersAtomically(test *testing.T) {
	for _, owner := range []string{"job", "source", "admission", "private"} {
		test.Run(owner, func(test *testing.T) {
			fixture, job, _ := docCatalogProtocolFixturePG(test)
			raw := docCatalogProtocolLegacyImagePG(test, fixture)
			docCatalogProtocolLegacyReplayPG(test, fixture, job)
			before := docCatalogUseImagePG(test, fixture)
			blocker := docCatalogUseBeginPG(test, fixture)
			defer rollbackPG(blocker)
			statement := map[string]string{
				"job":       `SELECT 1 FROM doc_display_jobs_v1 LIMIT 1 FOR UPDATE`,
				"source":    `SELECT 1 FROM doc_display_sources_v1 LIMIT 1 FOR NO KEY UPDATE`,
				"admission": `SELECT 1 FROM doc_display_catalog_admissions_v1 LIMIT 1 FOR UPDATE`,
				"private":   `SELECT 1 FROM doc_tool_tree_nodes_v1 LIMIT 1 FOR UPDATE`,
			}[owner]
			var locked int
			if err := blocker.QueryRow(fixture.ctx, statement).Scan(&locked); err != nil || locked != 1 {
				test.Fatal("actual row owner not established", owner, err)
			}
			attempt := docCatalogUseBeginPG(test, fixture)
			_, err := attempt.Exec(fixture.ctx, string(raw))
			docCatalogWriterCodePG(test, err, "55P03")
			rollbackPG(attempt)
			var legacy bool
			if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT
 to_regprocedure('cxthub_bind_doc_catalog_control(text,text,integer,text,bigint)') IS NULL
 AND to_regprocedure('cxthub_bind_doc_catalog_calculation(text,text,integer,text,bigint,text,uuid)') IS NULL
 AND position('protocol 1' IN (SELECT prosrc FROM pg_proc WHERE oid='cxthub_guard_doc_catalog_claim()'::regprocedure))>0`).Scan(&legacy); err != nil || !legacy || docCatalogUseImagePG(test, fixture) != before {
				test.Fatal("busy migration partially installed or changed legacy state", owner, legacy, err)
			}
			if err := blocker.Rollback(context.WithoutCancel(fixture.ctx)); err != nil {
				test.Fatal(err)
			}
			docCatalogProtocolCutoverPG(test, fixture, job, raw)
		})
	}
}
