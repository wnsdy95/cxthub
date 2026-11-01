//go:build postgres

package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyByteUsageMigrationPGImage struct {
	source     []byte
	original   string
	job        string
	generation string
	stage      string
	parts      string
	objects    map[domain.ContentHash][]byte
}

func legacyByteUsageMigrationPGRead(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, prefix int) legacyByteUsageMigrationPGImage {
	test.Helper()
	var image legacyByteUsageMigrationPGImage
	if err := storage.pool.QueryRow(ctx, `SELECT source.bytes,jsonb_build_object(
 'source',jsonb_build_array(source.hash,source.doc_range_source_version,incarnation.incarnation::text),
 'owner',(SELECT jsonb_build_array(replica.id,replica.repository_id,repository.id,repository.owner_namespace_id,replica.created_at)
 FROM repos replica JOIN repositories repository ON repository.id=replica.repository_id WHERE replica.id=$1),
 'ownership',(SELECT jsonb_agg(jsonb_build_array(repo_id,kind,hash) ORDER BY repo_id,kind,hash)
 FROM repo_blobs WHERE repo_id=$1 OR hash=$2),
 'snapshots',(SELECT jsonb_agg(jsonb_build_array(id,repo_id,branch,parents,doc_hash,doc_identity,memory_hash,
 claude_settings,agents_settings,codex_settings,provider,fidelity,message,author_name,author_email,author_team,created_at,
 grafted,session_id,models,compaction_count,graft_parents,graft_seq) ORDER BY repo_id,id)
 FROM snapshots WHERE repo_id=$1 OR doc_hash=$2))::text,
 (SELECT jsonb_build_array(repo_id,doc_hash,base_hash,state,reason,attempts,version,created_at,next_attempt,
 lease_until,minimum_range_version,byte_source_version)::text FROM doc_range_jobs_v1
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''),
 jsonb_build_object(
 'generation',(SELECT jsonb_build_array(doc_hash,source_version,generation_hash,encode(descriptor,'hex'),
 generation_id::text,range_layout_version,range_stream_bytes,legacy_byte_plan_hash)
 FROM doc_range_generations_v2 WHERE doc_hash=$2),
 'objects',(SELECT jsonb_agg(jsonb_build_array(hash,encode(bytes,'hex')) ORDER BY hash)
 FROM doc_range_objects_v2 WHERE doc_hash=$2))::text,
 (SELECT jsonb_build_array(doc_hash,source_id::text,source_version,plan_hash,encode(spec,'hex'),blocks,next_block,complete)::text
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$2),
 (SELECT jsonb_agg(jsonb_build_array(doc_hash,ordinal,hash,byte_count) ORDER BY ordinal)::text
 FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$2 AND ordinal<$3)
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$2`,
		fixture.repo, fixture.hash, prefix).
		Scan(&image.source, &image.original, &image.job, &image.generation, &image.stage, &image.parts); err != nil {
		test.Fatal(err)
	}
	rows, err := storage.pool.Query(ctx, `SELECT object.hash,object.bytes FROM doc_legacy_byte_stage_objects_v1 object
 WHERE object.doc_hash=$1 AND EXISTS(SELECT 1 FROM doc_legacy_byte_stage_parts_v1 part
 WHERE part.doc_hash=object.doc_hash AND part.hash=object.hash AND part.ordinal<$2) ORDER BY object.hash`, fixture.hash, prefix)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	image.objects = make(map[domain.ContentHash][]byte)
	for rows.Next() {
		var hash domain.ContentHash
		var body []byte
		if err := rows.Scan(&hash, &body); err != nil {
			test.Fatal(err)
		}
		image.objects[hash] = body
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	return image
}

func TestLegacyByteUsagePGMigrationPreservesPopulatedStage(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	dsn := rootProjectionIsolatedDSN(test, ctx)
	storage, err := NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	directory := "../../../../schemas/db/migrations"
	legacy := test.TempDir()
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	var upgrades []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if entry.Name() >= "0147" {
			upgrades = append(upgrades, entry.Name())
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, entry.Name()), raw, 0600); err != nil {
			test.Fatal(err)
		}
	}
	if _, err := storage.ApplyMigrations(ctx, legacy); err != nil {
		test.Fatal(err)
	}
	var historical bool
	if err := storage.pool.QueryRow(ctx, `SELECT to_regclass('public.doc_legacy_byte_usage_v1') IS NULL
 AND NOT EXISTS(SELECT 1 FROM pg_attribute WHERE NOT attisdropped AND (
 (attrelid='public.doc_range_jobs_v1'::regclass AND attname='legacy_validation_revision')
 OR (attrelid='public.doc_legacy_byte_stages_v1'::regclass AND attname IN ('stage_id','header_version'))))
 AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version>='0147')`).Scan(&historical); err != nil || !historical {
		test.Fatal("fixture did not start at the populated pre-accounting schema", historical, err)
	}
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	var verifier domain.CanonicalDocVerifier
	proof, err := verifier.Verify(ctx, fixture.hash, fixture.raw)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := domain.NewLegacyByteBackfill(ctx, proof)
	if err != nil {
		test.Fatal(err)
	}
	spec, err := plan.Spec()
	const partialBlocks = 8
	if err != nil || spec.Blocks <= partialBlocks || spec.StreamBytes != int64(len(fixture.stream)) {
		test.Fatal("pre-migration plan must span more than one historical batch", spec, err)
	}
	encoded, err := spec.Encode()
	if err != nil {
		test.Fatal(err)
	}
	planHash, err := plan.Key()
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.doc_range_worker_protocol='4'; SET LOCAL cxt.legacy_byte_stage_protocol='1'`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	var sourceID string
	var sourceVersion int64
	if err := transaction.QueryRow(ctx, `SELECT incarnation.incarnation::text,source.doc_range_source_version
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE source.hash=$1 FOR SHARE OF source,incarnation`, fixture.hash).Scan(&sourceID, &sourceVersion); err != nil || sourceVersion <= 0 {
		test.Fatal("historical fixture lost its current nonzero source version", sourceID, sourceVersion, err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_range_jobs_v1
 (repo_id,doc_hash,base_hash,state,reason,attempts,version,created_at,next_attempt,minimum_range_version,byte_source_version)
 VALUES($1,$2,'','waiting','historical byte copy',2,6,clock_timestamp()-interval '1 hour',
 clock_timestamp()-interval '1 minute',4,$3)`, fixture.repo, fixture.hash, sourceVersion); err != nil {
		test.Fatal(err)
	}
	job, err := scanDocRangeJob(transaction.QueryRow(ctx, `UPDATE doc_range_jobs_v1
 SET state='running',reason='',attempts=attempts+1,version=version+1,lease_until=clock_timestamp()+interval '2 minutes'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='waiting' AND version=6
 AND minimum_range_version=4 AND byte_source_version=$3
 RETURNING repo_id,doc_hash,base_hash,state,reason,attempts,version,created_at,next_attempt,lease_until`,
		fixture.repo, fixture.hash, sourceVersion))
	if err != nil || job.State != "running" || job.Version != 7 || job.Attempts != 3 || !job.LeaseUntil.After(time.Now().UTC()) {
		test.Fatal("historical protocol-4 claim did not acquire a live CAS lease", job, err)
	}
	if err := transaction.QueryRow(ctx, `SELECT epoch::text FROM repository_catalog_state WHERE repo_id=$1 FOR SHARE`, fixture.repo).Scan(&job.RepositoryEpoch); err != nil {
		test.Fatal("historical fixture did not retain its current repository lifetime", err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete) VALUES($1,$2::uuid,$3,$4,$5,$6,0,false)`,
		fixture.hash, sourceID, sourceVersion, planHash, encoded, spec.Blocks); err != nil {
		test.Fatal(err)
	}
	expectedObjects := make(map[domain.ContentHash][]byte)
	var expectedPayloadBytes int64
	for ordinal := 0; ordinal < partialBlocks; ordinal++ {
		block, err := plan.Block(ctx, ordinal)
		if err != nil {
			test.Fatal(err)
		}
		body, descriptor := legacyByteStagePGBlock(fixture, ordinal)
		if block.Descriptor() != descriptor || !bytes.Equal(block.Bytes(), body) {
			test.Fatal("historical plan differs from the exact canonical stream", ordinal)
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_objects_v1(doc_hash,hash,bytes)
 VALUES($1,$2,$3) ON CONFLICT(doc_hash,hash) DO NOTHING`, fixture.hash, descriptor.Hash, body); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_parts_v1(doc_hash,ordinal,hash,byte_count)
 VALUES($1,$2,$3,$4)`, fixture.hash, ordinal, descriptor.Hash, descriptor.Bytes); err != nil {
			test.Fatal(err)
		}
		if _, exists := expectedObjects[descriptor.Hash]; !exists {
			expectedObjects[descriptor.Hash] = bytes.Clone(body)
			expectedPayloadBytes += int64(len(body))
		}
	}
	advanced, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1 SET next_block=$2
 WHERE doc_hash=$1 AND next_block=0 AND NOT complete AND source_id=$3::uuid AND source_version=$4 AND plan_hash=$5`,
		fixture.hash, partialBlocks, sourceID, sourceVersion, planHash)
	if err != nil || advanced.RowsAffected() != 1 {
		test.Fatal("historical stage did not atomically commit its first eight parts", advanced, err)
	}
	if err := liveLegacyByteJob(ctx, transaction, job); err != nil {
		test.Fatal("historical partial stage has no live parent claim", err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	before := legacyByteUsageMigrationPGRead(test, storage, ctx, fixture, partialBlocks)
	if !bytes.Equal(before.source, fixture.raw) || !reflect.DeepEqual(before.objects, expectedObjects) {
		test.Fatal("historical SQL fixture did not retain exact source and deduplicated stream payloads")
	}
	for _, required := range []string{"0147_legacy_byte_storage_accounting.sql", "0150_legacy_byte_stage_headers.sql", "0151_legacy_byte_header_accounting.sql"} {
		if !containsMigration(upgrades, required) {
			test.Fatal("full historical accounting upgrade is missing", required)
		}
	}
	if applied, err := storage.ApplyMigrations(ctx, directory); err != nil || applied != len(upgrades) {
		test.Fatal("accounting migrations did not upgrade the populated pre-0147 predecessor", applied, len(upgrades), err)
	}
	var recorded int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM schema_migrations WHERE version=ANY($1::text[])`, upgrades).
		Scan(&recorded); err != nil || recorded != len(upgrades) {
		test.Fatal("historical upgrade did not record every pending migration", recorded, len(upgrades), err)
	}
	if after := legacyByteUsageMigrationPGRead(test, storage, ctx, fixture, partialBlocks); !reflect.DeepEqual(after, before) {
		test.Fatal("historical upgrade changed logical source, ownership, snapshot, generation, lease, CAS, plan or exact persisted payloads")
	}
	var stageID string
	var undeclared, unvalidated bool
	if err := storage.pool.QueryRow(ctx, `SELECT stage_id::text,
 header_version=0 AND header_bytes IS NULL AND header_next=0 AND NOT header_complete,
 (SELECT legacy_validation_revision=0 FROM doc_range_jobs_v1 WHERE repo_id=$2 AND doc_hash=$1 AND base_hash='')
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash, fixture.repo).
		Scan(&stageID, &undeclared, &unvalidated); err != nil || stageID == "" || stageID == "00000000-0000-0000-0000-000000000000" ||
		stageID == sourceID || !undeclared || !unvalidated {
		test.Fatal("historical stage did not acquire a fresh birth ID with unchanged legacy header and validation defaults", stageID, undeclared, unvalidated, err)
	}
	var definer, trustedOwner, privateExecution bool
	var settings []string
	if err := storage.pool.QueryRow(ctx, `SELECT function.prosecdef,function.proowner=relation.relowner,function.proconfig,
 NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(function.proacl,acldefault('f',function.proowner))) permission
 WHERE permission.grantee=0 AND permission.privilege_type='EXECUTE')
 FROM pg_proc function CROSS JOIN pg_class relation
 WHERE function.oid='public.cxt_account_legacy_byte_storage()'::regprocedure
 AND relation.oid='public.doc_legacy_byte_usage_v1'::regclass`).
		Scan(&definer, &trustedOwner, &settings, &privateExecution); err != nil || !definer || !trustedOwner || !privateExecution ||
		!strings.Contains(strings.ReplaceAll(strings.Join(settings, ";"), " ", ""), "search_path=pg_catalog,public,pg_temp") {
		test.Fatal("accounting migration lost its owner, execution or namespace boundary", definer, trustedOwner, settings, privateExecution, err)
	}
	usage, err := storage.ReadLegacyDerivedByteStorageUsage(ctx)
	if err != nil || usage.Sources != 1 || usage.CompleteSources != 0 || usage.Parts != partialBlocks ||
		usage.HeaderBytes != 0 || usage.HeaderParts != 0 || usage.SpecBytes != int64(len(encoded)) {
		test.Fatal("existing partial stage was not metered", usage, err)
	}
	var objects, payloadBytes int64
	if err := storage.pool.QueryRow(ctx, `SELECT count(*),coalesce(sum(octet_length(bytes)),0)::bigint
 FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&objects, &payloadBytes); err != nil ||
		usage.Objects != objects || usage.PayloadBytes != payloadBytes || objects != int64(len(expectedObjects)) ||
		payloadBytes != expectedPayloadBytes || usage.Objects >= usage.Parts {
		test.Fatal("migration lost deduplicated object accounting", usage, objects, payloadBytes, err)
	}
	if applied, err := storage.ApplyMigrations(ctx, directory); err != nil || applied != 0 {
		test.Fatal("historical accounting upgrade replay was not idempotent", applied, err)
	}
	if after := legacyByteUsageMigrationPGRead(test, storage, ctx, fixture, partialBlocks); !reflect.DeepEqual(after, before) {
		test.Fatal("accounting upgrade replay changed historical logical state or exact payloads")
	}
	storage.ConfigureLegacyByteRangePublication(true)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := storage.BuildDocRangeJob(ctx, job); err != nil {
		test.Fatal("upgraded partial stage could not resume through normal publication", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	completed, err := storage.ReadLegacyDerivedByteStorageUsage(ctx)
	if err != nil || completed.Sources != 1 || completed.CompleteSources != 1 || completed.Parts != int64(spec.Blocks) ||
		completed.SpecBytes != usage.SpecBytes || completed.PayloadBytes < usage.PayloadBytes {
		test.Fatal("resumed migration lost incremental usage", completed, usage, err)
	}
	after := legacyByteUsageMigrationPGRead(test, storage, ctx, fixture, partialBlocks)
	if !bytes.Equal(after.source, before.source) || after.original != before.original || after.parts != before.parts ||
		!reflect.DeepEqual(after.objects, before.objects) {
		test.Fatal("resumed upgrade changed original state or recopied the historical stream prefix")
	}
	var completedStageID string
	var retainedPlan domain.ContentHash
	var retainedSpec []byte
	var finishedNext, finishedBlocks int
	var finished bool
	if err := storage.pool.QueryRow(ctx, `SELECT stage_id::text,plan_hash,spec,next_block,blocks,complete
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&completedStageID, &retainedPlan, &retainedSpec, &finishedNext, &finishedBlocks, &finished); err != nil ||
		completedStageID != stageID || retainedPlan != planHash || !bytes.Equal(retainedSpec, encoded) ||
		!finished || finishedNext != spec.Blocks || finishedBlocks != spec.Blocks {
		test.Fatal("upgraded partial stage was replaced instead of resumed under its new birth ID", completedStageID, retainedPlan, finishedNext, finishedBlocks, finished, err)
	}
}
