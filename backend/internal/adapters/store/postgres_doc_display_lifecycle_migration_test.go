//go:build postgres

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docDisplayLifecycleHistoricalStatementPG(test *testing.T, raw, kind, name string) string {
	test.Helper()
	marker, terminator := "CREATE TRIGGER "+name+"\n", ";"
	if kind == "function" {
		marker, terminator = "CREATE FUNCTION "+name+"(", "\n$$;"
		if !strings.Contains(raw, marker) {
			marker = "CREATE OR REPLACE FUNCTION " + name + "("
		}
	}
	start := strings.Index(raw, marker)
	if start < 0 || strings.Count(raw, marker) != 1 {
		test.Fatal("historical migration must contain exactly the requested declaration", kind, name)
	}
	end := strings.Index(raw[start:], terminator)
	if end < 0 {
		test.Fatal("historical declaration is unterminated", kind, name)
	}
	return strings.Replace(raw[start:start+end+len(terminator)], "CREATE FUNCTION", "CREATE OR REPLACE FUNCTION", 1)
}

func docDisplayLifecyclePredecessorPG(test *testing.T, fixture docDisplayPGFixture) []byte {
	test.Helper()
	if _, owned := docDisplayOwnedFixturesPG.Load(fixture.storage.pool.Config().ConnConfig.Database); !owned {
		test.Fatal("historical lifecycle migration image requires an owned isolated database")
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0142_doc_display_catalog_lifecycle.sql")
	if err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != "3aaedae620c5cdf0ec4ce065b5242a75d913135c0506595bc973d51686ce9120" {
		test.Fatal("lifecycle handoff must apply the exact frozen migration")
	}
	before := docDisplayLifecycleMigrationContentPG(test, fixture)
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	var owned bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_database()=$1`, fixture.storage.pool.Config().ConnConfig.Database).Scan(&owned); err != nil || !owned {
		test.Fatal("historical reconstruction transaction escaped its owned database", owned, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `DROP TRIGGER doc_display_sources_v1_catalog_lifecycle ON doc_display_sources_v1;
 DROP FUNCTION cxthub_seed_doc_catalog_lifecycle();
 DROP FUNCTION cxthub_bind_doc_catalog_calculation(text,text,integer,text,bigint,text,uuid,uuid,text);
 DROP TABLE doc_display_catalog_lifecycle_v1;
 DROP FUNCTION cxthub_guard_doc_catalog_lifecycle();
 ALTER TABLE doc_display_jobs_v1 DROP COLUMN catalog_epoch CASCADE;
 ALTER TABLE doc_display_catalog_census_v1 DROP COLUMN catalog_epoch`); err != nil {
		test.Fatal("remove only lifecycle migration additions in the owned historical image", err)
	}
	for _, historical := range []struct {
		file      string
		functions []string
		triggers  []string
	}{
		{"0133_doc_display_references.sql", []string{"cxthub_queue_doc_display_maintenance"}, nil},
		{"0136_doc_display_catalog_admission.sql", []string{"cxthub_record_doc_catalog_job_admission"}, []string{"doc_display_jobs_v1_z_catalog_admission"}},
		{"0137_doc_display_catalog_census.sql", []string{"cxthub_queue_doc_display_job_roots"}, []string{"doc_display_jobs_v1_queue_roots"}},
		{"0139_doc_display_catalog_enqueue.sql", []string{"cxthub_guard_doc_catalog_admission_order"}, nil},
		{"0141_doc_display_catalog_calculation.sql", []string{"cxthub_bind_doc_catalog_writer", "cxthub_bind_doc_catalog_control", "cxthub_bind_doc_catalog_calculation", "cxthub_require_doc_catalog_writer", "cxthub_guard_doc_catalog_claim", "cxthub_guard_doc_catalog_job_write"}, []string{"guard_doc_display_zz_catalog_write"}},
	} {
		previous, err := os.ReadFile("../../../../schemas/db/migrations/" + historical.file)
		if err != nil {
			test.Fatal(err)
		}
		for _, name := range historical.functions {
			if _, err := transaction.Exec(fixture.ctx, docDisplayLifecycleHistoricalStatementPG(test, string(previous), "function", name)); err != nil {
				test.Fatal("restore the actual predecessor function", historical.file, name, err)
			}
		}
		for _, name := range historical.triggers {
			if _, err := transaction.Exec(fixture.ctx, docDisplayLifecycleHistoricalStatementPG(test, string(previous), "trigger", name)); err != nil {
				test.Fatal("restore the actual predecessor trigger layout", historical.file, name, err)
			}
		}
	}
	var predecessor bool
	if err := transaction.QueryRow(fixture.ctx, `SELECT
 to_regclass('doc_display_catalog_lifecycle_v1') IS NULL
 AND to_regprocedure('cxthub_bind_doc_catalog_calculation(text,text,integer,text,bigint,text,uuid,uuid,text)') IS NULL
 AND to_regprocedure('cxthub_guard_doc_catalog_lifecycle()') IS NULL
 AND to_regprocedure('cxthub_seed_doc_catalog_lifecycle()') IS NULL
 AND NOT EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid IN ('doc_display_jobs_v1'::regclass,'doc_display_catalog_census_v1'::regclass)
   AND attname='catalog_epoch' AND NOT attisdropped)
 AND (SELECT bool_and(tgenabled='O') FROM pg_trigger WHERE tgrelid IN
   ('doc_display_jobs_v1'::regclass,'doc_display_sources_v1'::regclass,
    'doc_tool_tree_sources_v1'::regclass,'doc_tool_tree_nodes_v1'::regclass,'doc_tool_key_sources_v1'::regclass,
    'doc_tool_key_observations_v1'::regclass,'doc_tool_key_members_v1'::regclass,
    'doc_tool_order_sources_v1'::regclass,'doc_tool_order_plans_v1'::regclass))
 AND (SELECT count(*)=7 FROM pg_trigger WHERE tgname IN
   ('doc_tool_tree_sources_v1_a_catalog_writer','doc_tool_tree_nodes_v1_a_catalog_writer','doc_tool_key_sources_v1_a_catalog_writer',
    'doc_tool_key_observations_v1_a_catalog_writer','doc_tool_key_members_v1_a_catalog_writer',
    'doc_tool_order_sources_v1_a_catalog_writer','doc_tool_order_plans_v1_a_catalog_writer'))
 AND position('protocol 2' IN (SELECT prosrc FROM pg_proc WHERE oid='cxthub_guard_doc_catalog_claim()'::regprocedure))>0`).Scan(&predecessor); err != nil || !predecessor {
		test.Fatal("reconstruction did not retain the guarded actual protocol-2 predecessor", predecessor, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `INSERT INTO doc_display_catalog_census_v1(source_hash,generation,admission_revision,source_revision,phase)
 SELECT source.source_hash,admission.generation,admission.admission_revision,source.reference_revision,'native'
 FROM doc_display_sources_v1 source JOIN doc_display_catalog_admissions_v1 admission
 ON admission.event_hash=source.event_hash AND admission.profile=source.profile AND admission.source_hash=source.source_hash`); err != nil {
		test.Fatal("populate the actual predecessor census with its current Source evidence", err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if docDisplayLifecycleMigrationContentPG(test, fixture) != before {
		test.Fatal("predecessor reconstruction changed canonical preparation, Source evidence, or private content")
	}
	return raw
}

func docDisplayLifecycleMigrationContentPG(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var image map[string]any
	if err := json.Unmarshal([]byte(docCatalogProtocolImmutableImagePG(test, fixture)), &image); err != nil {
		test.Fatal(err)
	}
	display := image["display"].(map[string]any)
	for _, row := range display["jobs"].([]any) {
		delete(row.(map[string]any), "catalog_epoch")
	}
	for _, row := range display["sources"].([]any) {
		delete(row.(map[string]any), "reference_revision")
	}
	delete(image, "census")
	raw, err := json.Marshal(image)
	if err != nil {
		test.Fatal(err)
	}
	return string(raw)
}

func docDisplayLifecycleMigrationSchemaPG(test *testing.T, fixture docDisplayPGFixture) string {
	test.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'functions',(SELECT jsonb_agg(pg_get_functiondef(oid) ORDER BY oid::regprocedure::text)
   FROM pg_proc WHERE pronamespace=current_schema()::regnamespace AND proname LIKE 'cxthub_%' AND prokind='f'),
 'relations',(SELECT jsonb_agg(jsonb_build_array(relname,relkind::text) ORDER BY relname)
   FROM pg_class WHERE relnamespace=current_schema()::regnamespace AND (relname LIKE 'doc_display_%' OR relname LIKE 'doc_tool_%')),
 'columns',(SELECT jsonb_agg(jsonb_build_array(attrelid::regclass::text,attnum,attname,atttypid::regtype::text,attnotnull,attisdropped,
   pg_get_expr(defaults.adbin,defaults.adrelid)) ORDER BY attrelid::regclass::text,attnum)
   FROM pg_attribute attribute JOIN pg_class relation ON relation.oid=attribute.attrelid
   LEFT JOIN pg_attrdef defaults ON defaults.adrelid=attribute.attrelid AND defaults.adnum=attribute.attnum
   WHERE relation.relnamespace=current_schema()::regnamespace AND attnum>0
     AND (relation.relname LIKE 'doc_display_%' OR relation.relname LIKE 'doc_tool_%')),
 'constraints',(SELECT jsonb_agg(jsonb_build_array(conrelid::regclass::text,conname,pg_get_constraintdef(oid))
   ORDER BY conrelid::regclass::text,conname) FROM pg_constraint WHERE connamespace=current_schema()::regnamespace),
 'triggers',(SELECT jsonb_agg(jsonb_build_array(tgrelid::regclass::text,tgname,tgenabled::text,pg_get_triggerdef(catalog_trigger.oid))
   ORDER BY tgrelid::regclass::text,tgname) FROM pg_trigger catalog_trigger JOIN pg_class relation ON relation.oid=catalog_trigger.tgrelid
   WHERE relation.relnamespace=current_schema()::regnamespace))::text`).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func docDisplayLifecycleProtocolTwoReplayPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob) {
	test.Helper()
	before := docCatalogUseImagePG(test, fixture)
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if err := docCatalogProtocolControlPG(fixture, transaction, job); err != nil {
		test.Fatal("actual protocol-2 control must own the old live token", err)
	}
	var protocol, calculation string
	if err := transaction.QueryRow(fixture.ctx, `SELECT current_setting('cxthub.doc_catalog_protocol'),current_setting('cxthub.doc_catalog_use')`).Scan(&protocol, &calculation); err != nil || protocol != "2" || calculation != "" {
		test.Fatal("old control unexpectedly acquired current protocol or calculation rights", protocol, calculation, err)
	}
	if _, err := transaction.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, admission.source, admission.generation); err != nil {
		test.Fatal("actual protocol-2 calculation must remain live before handoff", err)
	}
	for _, table := range docCatalogWriterTablesPG {
		for _, replay := range docCatalogProtocolReplaysPG(table) {
			if tag, err := transaction.Exec(fixture.ctx, replay.statement); err != nil || tag.RowsAffected() != replay.rows {
				test.Fatal("actual old calculation must replay every private table before handoff", table, replay.operation, tag, err)
			}
		}
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	if docCatalogUseImagePG(test, fixture) != before {
		test.Fatal("exact protocol-2 replays changed predecessor content")
	}
}

func applyDocDisplayLifecycleMigrationPG(test *testing.T, fixture docDisplayPGFixture, job domain.DocDisplayJob, raw []byte) {
	test.Helper()
	before := docDisplayLifecycleMigrationContentPG(test, fixture)
	admission := docCatalogAdmissionFactPG(test, fixture.ctx, fixture.storage.pool, job.Target.Event.Hash, job.Target.DisplayProfile())
	var revision int64
	var census int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT reference_revision,(SELECT count(*) FROM doc_display_catalog_census_v1)
 FROM doc_display_sources_v1 WHERE source_hash=$1`, admission.source).Scan(&revision, &census); err != nil || census == 0 {
		test.Fatal("handoff requires actual Source evidence and a populated predecessor census", census, err)
	}
	transaction := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(fixture.ctx, string(raw)); err != nil {
		test.Fatal("apply exact lifecycle migration to the populated protocol-2 predecessor", err)
	}
	if err := transaction.Commit(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	var state, reason string
	var version, worker, nextRevision int64
	var released, due, complete bool
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT state,version,reason,worker_protocol,lease_until IS NULL,next_attempt<=clock_timestamp()
 FROM doc_display_jobs_v1 WHERE `+docDisplayKey(job.Target), job.RepoID, job.DocHash, job.Target.Event.Index).
		Scan(&state, &version, &reason, &worker, &released, &due); err != nil || state != "waiting" || version != job.Version+1 || reason != "" || worker != 5 || !released || !due {
		test.Fatal("lifecycle handoff did not drain exactly the running token", state, version, reason, worker, released, due, err)
	}
	fact := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	if fact.epoch != fact.jobEpoch || fact.generation != admission.generation || string(fact.source) != admission.source {
		test.Fatal("new live epoch failed to bind the preserved Source/checkpoint incarnation", fact, admission)
	}
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT source.reference_revision,
 NOT EXISTS(SELECT 1 FROM doc_display_sources_v1 source LEFT JOIN doc_display_catalog_lifecycle_v1 lifecycle USING(source_hash)
   WHERE lifecycle.catalog_epoch IS NULL OR lifecycle.state<>'live') AND NOT EXISTS(SELECT 1 FROM doc_display_catalog_census_v1)
 FROM doc_display_sources_v1 source WHERE source.source_hash=$1`, admission.source).Scan(&nextRevision, &complete); err != nil || nextRevision != revision+1 || !complete {
		test.Fatal("migration failed to queue the Source exactly once or atomically replace its census epoch", revision, nextRevision, complete, err)
	}
	if docDisplayLifecycleMigrationContentPG(test, fixture) != before {
		test.Fatal("lifecycle handoff changed canonical preparation, formatter/content hashes, Source UUID, or private bytes")
	}
}

func TestDocDisplayLifecyclePGMigrationDrainsPopulatedProtocolTwo(test *testing.T) {
	fixture, job, prepared := docCatalogProtocolFixturePG(test)
	calculated := prepareDocDisplayLifecycleStepPG(test, fixture, job, prepared, nil)
	use, sealed := calculated.CatalogUse()
	if !sealed || use.Validate() != nil {
		test.Fatal("migration fixture lacks its actual original sealed calculation", use)
	}
	raw := docDisplayLifecyclePredecessorPG(test, fixture)
	docDisplayLifecycleProtocolTwoReplayPG(test, fixture, job)
	applyDocDisplayLifecycleMigrationPG(test, fixture, job, raw)
	fact := readDocDisplayLifecycleFactPG(test, fixture.ctx, fixture.storage.pool, job)
	if fact.checkpoint != calculated.Prior() || fact.epoch == use.Epoch || fact.generation != use.Generation {
		test.Fatal("handoff relabeled calculation content or replaced Source admission", fact, use)
	}
	before := docDisplayLifecycleImagePG(test, fixture)
	legacy := docCatalogUseBeginPG(test, fixture)
	defer rollbackPG(legacy)
	_, err := legacy.Exec(fixture.ctx, `SELECT cxthub_bind_doc_catalog_calculation($1,$2,$3,$4,$5,$6,$7::uuid)`,
		job.RepoID, job.DocHash, job.Target.Event.Index, job.Target.DisplayProfile(), job.Version, use.Source, use.Generation)
	docCatalogProtocolDeniedPG(test, err, "0A000", "document catalog calculations require writer protocol 3")
	if err := legacy.Rollback(fixture.ctx); err != nil {
		test.Fatal(err)
	}
	result, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, job, calculated)
	if !errors.Is(err, domain.ErrConflict) || !reflect.DeepEqual(result, domain.DocEventDisplayPreparation{}) || docDisplayLifecycleImagePG(test, fixture) != before {
		test.Fatal("old running-token calculation changed state after lifecycle handoff", result, err)
	}
	freshJob := fixture.claim(test)
	if !sameDocDisplayTarget(freshJob.Target, job.Target) || freshJob.Version != job.Version+2 {
		test.Fatal("current claimant did not take the newly drained version", freshJob)
	}
	freshPreparation, err := fixture.storage.LoadDocEventDisplay(fixture.ctx, freshJob)
	if err != nil || !reflect.DeepEqual(freshPreparation, prepared) {
		test.Fatal("current initialization did not preserve the migrated canonical checkpoint", err)
	}
	fresh := prepareDocDisplayLifecycleStepPG(test, fixture, freshJob, freshPreparation, nil)
	freshUse, sealed := fresh.CatalogUse()
	if !sealed || freshUse.Epoch != fact.epoch || freshUse.Source != use.Source || freshUse.Generation != use.Generation || !reflect.DeepEqual(fresh.Preparation(), calculated.Preparation()) {
		test.Fatal("fresh calculation changed content identity or reused the old lifecycle witness", freshUse, use)
	}
	saved, err := fixture.storage.SaveDocEventDisplay(fixture.ctx, freshJob, fresh)
	if err != nil || !reflect.DeepEqual(saved, fresh.Preparation()) {
		test.Fatal("current claim/read/calculation/save could not continue the preserved private checkpoint", err)
	}
}

func TestDocDisplayLifecyclePGMigrationRefusesRowOwnersAtomically(test *testing.T) {
	for _, holder := range []struct {
		name, table, lock string
	}{
		{"job", "doc_display_jobs_v1", "FOR UPDATE"},
		{"source", "doc_display_sources_v1", "FOR NO KEY UPDATE"},
		{"private", "doc_tool_tree_nodes_v1", "FOR UPDATE"},
	} {
		test.Run(holder.name, func(test *testing.T) {
			fixture, job, _ := docCatalogProtocolFixturePG(test)
			raw := docDisplayLifecyclePredecessorPG(test, fixture)
			docDisplayLifecycleProtocolTwoReplayPG(test, fixture, job)
			before, schema := docCatalogUseImagePG(test, fixture), docDisplayLifecycleMigrationSchemaPG(test, fixture)
			blocker := docCatalogUseBeginPG(test, fixture)
			defer rollbackPG(blocker)
			var locked int
			if err := blocker.QueryRow(fixture.ctx, `SELECT 1 FROM `+pgx.Identifier{holder.table}.Sanitize()+` LIMIT 1 `+holder.lock).Scan(&locked); err != nil || locked != 1 {
				test.Fatal("actual predecessor row owner was not established", holder.name, err)
			}
			attempt := docCatalogUseBeginPG(test, fixture)
			defer rollbackPG(attempt)
			_, err := attempt.Exec(fixture.ctx, string(raw))
			docCatalogProtocolDeniedPG(test, err, "55P03", `could not obtain lock on relation "`+holder.table+`"`)
			if err := attempt.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			if docCatalogUseImagePG(test, fixture) != before || docDisplayLifecycleMigrationSchemaPG(test, fixture) != schema {
				test.Fatal("busy lifecycle migration partially changed data, guards, claim protocol, or epoch DDL", holder.name)
			}
			if err := blocker.Rollback(fixture.ctx); err != nil {
				test.Fatal(err)
			}
			docDisplayLifecycleProtocolTwoReplayPG(test, fixture, job)
			applyDocDisplayLifecycleMigrationPG(test, fixture, job, raw)
		})
	}
}
