//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyByteUsagePGRead(test *testing.T, storage *PostgresStore, ctx context.Context) domain.LegacyDerivedByteStorageUsage {
	test.Helper()
	usage, err := storage.ReadLegacyDerivedByteStorageUsage(ctx)
	if err != nil {
		test.Fatal(err)
	}
	if usage.TableBytes < 0 || usage.IndexBytes < 0 {
		test.Fatal("negative physical allocation", usage)
	}
	usage.TableBytes, usage.IndexBytes = 0, 0
	return usage
}

func legacyByteUsagePGAssertDelta(test *testing.T, storage *PostgresStore, ctx context.Context, initial, expected domain.LegacyDerivedByteStorageUsage) {
	test.Helper()
	actual := legacyByteUsagePGRead(test, storage, ctx)
	actual.Sources -= initial.Sources
	actual.CompleteSources -= initial.CompleteSources
	actual.Objects -= initial.Objects
	actual.Parts -= initial.Parts
	actual.PayloadBytes -= initial.PayloadBytes
	actual.SpecBytes -= initial.SpecBytes
	actual.HeaderBytes -= initial.HeaderBytes
	actual.HeaderParts -= initial.HeaderParts
	if actual != expected {
		test.Fatalf("logical usage delta: got=%+v want=%+v", actual, expected)
	}
}

func legacyByteUsagePGExpected(fixture legacyByteStagePGFixture, stage legacyByteStage, specBytes int) domain.LegacyDerivedByteStorageUsage {
	usage := domain.LegacyDerivedByteStorageUsage{Sources: 1, Parts: int64(stage.next), SpecBytes: int64(specBytes)}
	if stage.complete {
		usage.CompleteSources = 1
	}
	objects := make(map[domain.ContentHash]bool)
	for ordinal := 0; ordinal < stage.next; ordinal++ {
		body, descriptor := legacyByteStagePGBlock(fixture, ordinal)
		if !objects[descriptor.Hash] {
			objects[descriptor.Hash] = true
			usage.Objects++
			usage.PayloadBytes += int64(len(body))
		}
	}
	return usage
}

func TestLegacyDerivedByteUsagePGLifecycleDedupAndBilling(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	initial := legacyByteUsagePGRead(test, storage, ctx)
	fixture := legacyByteStagePGSource(test, storage, ctx, 3<<20)
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	var namespace string
	if err := storage.pool.QueryRow(ctx, `SELECT repository.owner_namespace_id FROM repos replica
 JOIN repositories repository ON repository.id=replica.repository_id WHERE replica.id=$1`, fixture.repo).Scan(&namespace); err != nil {
		test.Fatal(err)
	}
	periodEnd := time.Now().UTC()
	readBilling := func() domain.StorageUsage {
		usage, err := storage.ReadStorageUsage(ctx, namespace, periodEnd.Add(-time.Hour), periodEnd)
		if err != nil {
			test.Fatal(err)
		}
		return usage
	}
	billing := readBilling()
	if billing.CurrentBytes <= 0 {
		test.Fatal("fixture has no billable archival bytes", billing)
	}
	partial, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || partial.next != 1 || partial.complete {
		test.Fatal("first copy did not leave a partial source", partial, err)
	}
	expectedPartial := legacyByteUsagePGExpected(fixture, partial, len(session.encoded))
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedPartial)
	job.State, job.Reason, job.NextAttempt = "retrying", "usage restart", time.Now().UTC().Add(-time.Minute)
	if err := storage.FinishDocRangeJob(ctx, job, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedPartial)
	job = docRangeJobPGClaim(test, storage, ctx, fixture.repo, fixture.hash)
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedPartial)
	complete, err := storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || !complete.complete || complete.next != complete.blocks {
		test.Fatal("resumed copy did not complete", complete, err)
	}
	expectedComplete := legacyByteUsagePGExpected(fixture, complete, len(session.encoded))
	if expectedComplete.Parts <= expectedComplete.Objects || expectedComplete.PayloadBytes >= int64(len(fixture.stream)) {
		test.Fatal("fixture does not exercise repeated identical CAS blocks", expectedComplete)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedComplete)
	var objectBytes, objectCount, partCount, oracleBytes, oracleObjects, oracleParts int64
	if err := storage.pool.QueryRow(ctx, `SELECT usage.object_bytes,usage.object_count,usage.part_count,
 (SELECT coalesce(sum(octet_length(bytes)),0) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)
 FROM doc_legacy_byte_usage_v1 usage WHERE usage.doc_hash=$1`, fixture.hash).
		Scan(&objectBytes, &objectCount, &partCount, &oracleBytes, &oracleObjects, &oracleParts); err != nil {
		test.Fatal(err)
	}
	if objectBytes != oracleBytes || objectCount != oracleObjects || partCount != oracleParts ||
		objectBytes != expectedComplete.PayloadBytes || objectCount != expectedComplete.Objects || partCount != expectedComplete.Parts {
		test.Fatal("source-local counters differ from unique stored objects or ordered parts", objectBytes, objectCount, partCount, oracleBytes, oracleObjects, oracleParts)
	}
	for range 2 {
		repeated, err := storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
		if err != nil || repeated != complete {
			test.Fatal("duplicate resume changed completed stage", repeated, err)
		}
		legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedComplete)
	}
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	if err := storage.publishLegacyDerivedByteRange(ctx, job, session); err != nil {
		test.Fatal(err)
	}
	expectedPublished := expectedComplete
	expectedPublished.HeaderBytes = int64(len(fixture.envelope))
	expectedPublished.HeaderParts = int64((len(fixture.envelope) + legacyByteHeaderPartBytes - 1) / legacyByteHeaderPartBytes)
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedPublished)
	if after := readBilling(); !reflect.DeepEqual(after, billing) {
		test.Fatalf("derived copies or publication changed namespace billing: before=%+v after=%+v", billing, after)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	if after := readBilling(); !reflect.DeepEqual(after, billing) {
		test.Fatal("retiring derived storage changed namespace billing", after)
	}
	job = legacyByteStagePGClaim(test, storage, ctx, fixture)
	if rebuilt, err := storage.stageLegacyByteBackfill(ctx, job, domain.DocLegacyByteBackfillBatchBlocks); err != nil || !rebuilt.complete {
		test.Fatal("retired source could not rebuild its counters", rebuilt, err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, expectedComplete)
	for _, statement := range []string{
		`DELETE FROM snapshots WHERE doc_hash=$1`,
		`DELETE FROM repo_blobs WHERE hash=$1`,
		`DELETE FROM blobs WHERE hash=$1`,
	} {
		if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
			test.Fatal(err)
		}
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	var remaining int64
	if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)+
 (SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)`, fixture.hash).Scan(&remaining); err != nil || remaining != 0 {
		test.Fatal("deleted source retained derived storage or counters", remaining, err)
	}
}

func TestLegacyDerivedByteUsagePGCounterGuardsAndRollback(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	initial := legacyByteUsagePGRead(test, storage, ctx)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete) VALUES($1,$2::uuid,$3,$4,$5,$6,0,false)`,
		fixture.hash, session.stage.sourceID, session.stage.sourceVersion, session.stage.plan, session.encoded, session.stage.blocks); err != nil {
		test.Fatal(err)
	}
	var objectBytes, objectCount, partCount int64
	if err := transaction.QueryRow(ctx, `SELECT object_bytes,object_count,part_count FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&objectBytes, &objectCount, &partCount); err != nil || objectBytes != 0 || objectCount != 0 || partCount != 0 {
		test.Fatal("new stage did not atomically create zero counters", objectBytes, objectCount, partCount, err)
	}
	body, descriptor := legacyByteStagePGBlock(fixture, 0)
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_objects_v1(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, descriptor.Hash, body); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_parts_v1(doc_hash,ordinal,hash,byte_count) VALUES($1,0,$2,$3)`, fixture.hash, descriptor.Hash, descriptor.Bytes); err != nil {
		test.Fatal(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT object_bytes,object_count,part_count FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&objectBytes, &objectCount, &partCount); err != nil || objectBytes != int64(len(body)) || objectCount != 1 || partCount != 1 {
		test.Fatal("copy transaction did not update its own counters", objectBytes, objectCount, partCount, err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	partial, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || partial.next != 1 || partial.complete {
		test.Fatal(partial, err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, legacyByteUsagePGExpected(fixture, partial, len(session.encoded)))
	for _, statement := range []string{
		`INSERT INTO doc_legacy_byte_usage_v1(doc_hash) VALUES($1) ON CONFLICT DO NOTHING`,
		`UPDATE doc_legacy_byte_usage_v1 SET object_bytes=object_bytes+1 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_usage_v1 SET object_count=object_count+1 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_usage_v1 SET part_count=part_count+1 WHERE doc_hash=$1`,
		`DELETE FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`,
	} {
		attempt, err := storage.pool.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := attempt.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'`); err != nil {
			rollbackPG(attempt)
			test.Fatal(err)
		}
		_, err = attempt.Exec(ctx, statement, fixture.hash)
		rollbackPG(attempt)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "23514" {
			test.Fatal("direct counter mutation bypassed the trigger-depth guard", statement, err)
		}
	}
	before := legacyByteUsagePGRead(test, storage, ctx)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	name := pgx.Identifier{domain.NewID("legacy_usage_fence_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER INSERT ON doc_legacy_byte_stage_parts_v1 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	drop := `DROP TRIGGER IF EXISTS ` + name + ` ON doc_legacy_byte_stage_parts_v1; DROP FUNCTION IF EXISTS ` + name + `()`
	defer func() {
		if _, err := storage.pool.Exec(context.WithoutCancel(ctx), drop); err != nil {
			test.Error(err)
		}
	}()
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if !errors.Is(err, domain.ErrConflict) || stage != (legacyByteStage{}) {
		test.Fatal("final lease fault committed the copied batch", stage, err)
	}
	if after := legacyByteUsagePGRead(test, storage, ctx); after != before {
		test.Fatal("rolled-back final lease fault changed counters", before, after)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("rolled-back final lease fault changed staged storage")
	}
	if _, err := storage.pool.Exec(ctx, drop); err != nil {
		test.Fatal(err)
	}
	retried, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || retried.next != partial.next+1 {
		test.Fatal("rolled-back batch could not resume once", retried, err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, legacyByteUsagePGExpected(fixture, retried, len(session.encoded)))
}

func TestLegacyDerivedByteUsagePGIndependentWorkersAndReadSnapshot(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	initial := legacyByteUsagePGRead(test, storage, ctx)
	first := legacyByteStagePGSource(test, storage, ctx, 2<<20)
	firstJob := legacyByteStagePGClaim(test, storage, ctx, first)
	firstSession, err := storage.prepareLegacyByteBackfill(ctx, firstJob)
	if err != nil {
		test.Fatal(err)
	}
	firstPartial, err := storage.stageLegacyByteSession(ctx, firstJob, firstSession, 1)
	if err != nil || firstPartial.next != 1 || firstPartial.complete {
		test.Fatal(firstPartial, err)
	}
	second := legacyByteStagePGSource(test, storage, ctx, 2<<20)
	secondJob := legacyByteStagePGClaim(test, storage, ctx, second)
	secondSession, err := storage.prepareLegacyByteBackfill(ctx, secondJob)
	if err != nil {
		test.Fatal(err)
	}
	secondPartial, err := storage.stageLegacyByteSession(ctx, secondJob, secondSession, 1)
	if err != nil || secondPartial.next != 1 || secondPartial.complete {
		test.Fatal(secondPartial, err)
	}
	legacyByteUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{
		Sources: 2, Objects: 2, Parts: 2, PayloadBytes: 2 * domain.ConversationManifestChunkBytes,
		SpecBytes: int64(len(firstSession.encoded) + len(secondSession.encoded)),
	})
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "INSERT INTO doc_legacy_byte_stage_parts_v1")
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		snapshot := legacyByteUsagePGRead(test, storage, read)
		worker, cancel := context.WithTimeout(ctx, 10*time.Second)
		finished := make(chan struct{})
		var workerError error
		go func() {
			defer close(finished)
			stage, err := peer.stageLegacyByteSession(worker, firstJob, firstSession, domain.DocLegacyByteBackfillBatchBlocks)
			workerError = err
			if err == nil && !stage.complete {
				workerError = fmt.Errorf("first worker left an incomplete stage: %+v", stage)
			}
		}()
		defer func() {
			release()
			cancel()
			<-finished
		}()
		select {
		case <-reached:
		case <-finished:
			test.Fatal("first worker skipped the uncommitted counter barrier", workerError)
		case <-worker.Done():
			test.Fatal(worker.Err())
		}
		if actual := legacyByteUsagePGRead(test, storage, ctx); actual != snapshot {
			test.Fatal("uncommitted source-local counters escaped the copy transaction", actual, snapshot)
		}
		independent, stop := context.WithTimeout(ctx, 3*time.Second)
		secondComplete, err := storage.stageLegacyByteSession(independent, secondJob, secondSession, domain.DocLegacyByteBackfillBatchBlocks)
		stop()
		if err != nil || !secondComplete.complete {
			test.Fatal("unrelated source worker blocked behind another source's counter transaction", secondComplete, err)
		}
		secondDelta := legacyByteUsagePGExpected(second, secondComplete, len(secondSession.encoded))
		secondDelta.Sources, secondDelta.SpecBytes = 0, 0
		secondDelta.Objects--
		secondDelta.Parts--
		secondDelta.PayloadBytes -= domain.ConversationManifestChunkBytes
		legacyByteUsagePGAssertDelta(test, storage, ctx, snapshot, secondDelta)
		if actual := legacyByteUsagePGRead(test, storage, read); actual != snapshot {
			test.Fatal("repeatable-read usage changed after another source committed", actual, snapshot)
		}
		beforeRelease := legacyByteUsagePGRead(test, storage, ctx)
		release()
		select {
		case <-finished:
			if workerError != nil {
				test.Fatal("first worker did not commit after release", workerError)
			}
		case <-worker.Done():
			test.Fatal(worker.Err())
		}
		firstComplete := firstPartial
		firstComplete.next, firstComplete.complete = firstComplete.blocks, true
		firstDelta := legacyByteUsagePGExpected(first, firstComplete, len(firstSession.encoded))
		firstDelta.Sources, firstDelta.SpecBytes = 0, 0
		firstDelta.Objects--
		firstDelta.Parts--
		firstDelta.PayloadBytes -= domain.ConversationManifestChunkBytes
		legacyByteUsagePGAssertDelta(test, storage, ctx, beforeRelease, firstDelta)
		if actual := legacyByteUsagePGRead(test, storage, read); actual != snapshot {
			test.Fatal("repeatable-read usage mixed partial and completed source snapshots", actual, snapshot)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}
