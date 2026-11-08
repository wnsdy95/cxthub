//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func checkpointInsertPGObserve(test *testing.T, ctx context.Context, storage *PostgresStore) {
	test.Helper()
	if _, err := storage.pool.Exec(ctx, `CREATE TABLE checkpoint_insert_accounting_calls (doc_hash text);
 CREATE FUNCTION observe_checkpoint_insert_accounting() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN INSERT INTO public.checkpoint_insert_accounting_calls VALUES(NEW.doc_hash); RETURN NEW; END $$;
 CREATE TRIGGER observe_checkpoint_insert_accounting AFTER UPDATE ON public.doc_legacy_byte_usage_v1
 FOR EACH ROW EXECUTE FUNCTION observe_checkpoint_insert_accounting()`); err != nil {
		test.Fatal(err)
	}
}

func checkpointInsertPGAssert(test *testing.T, ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, hash domain.ContentHash, objects, size, calls int64) {
	test.Helper()
	var actualObjects, actualBytes, physicalObjects, physicalBytes, actualCalls int64
	err := database.QueryRow(ctx, `SELECT checkpoint_objects,checkpoint_bytes,
 (SELECT count(*) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1),
 (SELECT coalesce(sum(octet_length(bytes)),0) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM checkpoint_insert_accounting_calls WHERE doc_hash=$1)
 FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, hash).
		Scan(&actualObjects, &actualBytes, &physicalObjects, &physicalBytes, &actualCalls)
	if err != nil || actualObjects != objects || actualBytes != size || physicalObjects != objects || physicalBytes != size || actualCalls != calls {
		test.Fatal("checkpoint physical rows, exact bytes, or usage UPDATE count differ", actualObjects, actualBytes, physicalObjects, physicalBytes, actualCalls, objects, size, calls, err)
	}
}

func TestLegacyCheckpointPGInsertAccountingBatchesExactUsageAndRollback(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	checkpointInsertPGObserve(test, ctx, storage)
	peer, trace := normalizationBatchPGPeer(test, storage, ctx)
	const count = 128
	objects := make([]legacyByteCheckpointObject, count)
	var total int64
	for ordinal := range objects {
		objects[ordinal] = legacyCheckpointPGObject(fmt.Sprintf("insert-accounting-object-%d", ordinal))
		total += int64(len(objects[ordinal].data))
	}
	for _, outcome := range []string{"rollback", "final-fence", "commit"} {
		work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
		bound, batch, err := work.beginBatch(ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(batch.transaction)
		before := trace.snapshot()
		for offset := 0; offset < count; offset += domain.DocLegacyByteBackfillBatchBlocks {
			if err := work.stageParserObjects(bound, objects[offset:offset+domain.DocLegacyByteBackfillBatchBlocks]); err != nil {
				test.Fatal(err)
			}
		}
		after := trace.snapshot()
		if after.objectInserts-before.objectInserts != count/legacyNormalizationCheckpointBatchObjects ||
			after.hashReads-before.hashReads != 2*count || after.bodyArguments-before.bodyArguments != count ||
			int64(after.bodyBytes-before.bodyBytes) != total || after.maxBodyArguments != legacyNormalizationCheckpointBatchObjects ||
			after.maxBodyBytes > domain.LegacyCanonicalEncodingBytes {
			test.Fatal("bounded INSERT lost per-object byte proofs, payload accounting or statement limits", before, after)
		}
		checkpointInsertPGAssert(test, bound, batch.transaction, job.DocHash, count, total, count/legacyNormalizationCheckpointBatchObjects)
		for offset := 0; offset < count; offset += domain.DocLegacyByteBackfillBatchBlocks {
			if err := work.stageParserObjects(bound, objects[offset:offset+domain.DocLegacyByteBackfillBatchBlocks]); err != nil {
				test.Fatal(err)
			}
		}
		reused := trace.snapshot()
		if reused.objectInserts != after.objectInserts || reused.bodyArguments != after.bodyArguments ||
			reused.bodyBytes != after.bodyBytes || reused.hashReads-after.hashReads != count {
			test.Fatal("duplicate batch resent payload, updated usage or skipped current byte proofs", after, reused)
		}
		checkpointInsertPGAssert(test, bound, batch.transaction, job.DocHash, count, total, count/legacyNormalizationCheckpointBatchObjects)
		var failure error
		if outcome == "rollback" {
			failure = domain.ErrIntegrity
		} else if outcome == "final-fence" {
			if _, err := batch.transaction.Exec(bound, `UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id=$1 AND doc_hash=$2`, job.RepoID, job.DocHash); err != nil {
				test.Fatal(err)
			}
		}
		err = batch.finish(bound, failure)
		if outcome == "commit" {
			if err != nil {
				test.Fatal(err)
			}
			checkpointInsertPGAssert(test, ctx, storage.pool, job.DocHash, count, total, count/legacyNormalizationCheckpointBatchObjects)
		} else {
			want := domain.ErrIntegrity
			if outcome == "final-fence" {
				want = domain.ErrConflict
			}
			if !errors.Is(err, want) {
				test.Fatal("failed batch did not reject its original failure or lost claim", outcome, err)
			}
			checkpointInsertPGAssert(test, ctx, storage.pool, job.DocHash, 0, 0, 0)
			if err := storage.confirmLegacyBytePreparation(ctx, job, state.stage); err != nil {
				test.Fatal("rollback changed the original source or claim", err)
			}
		}
	}
}

func TestLegacyCheckpointPGInsertAccountingCountsOnlyInsertedRows(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	checkpointInsertPGObserve(test, ctx, storage)
	transaction, _, err := storage.openLegacyByteCheckpoint(ctx, job, state.stage, &state, false)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
		test.Fatal(err)
	}
	first, second, third := legacyCheckpointPGObject("first"), legacyCheckpointPGObject("second"), legacyCheckpointPGObject("third")
	for ordinal, batch := range [][]legacyByteCheckpointObject{{first, first, second}, {first, second, second}, {first, third, third}, {}} {
		hashes, bodies := make([]string, len(batch)), make([][]byte, len(batch))
		for index, object := range batch {
			hashes[index], bodies[index] = string(object.hash), object.data
		}
		result, err := transaction.Exec(ctx, legacyNormalizationBatchObjectInsert, job.DocHash, state.stage.incarnation, hashes, bodies)
		if err != nil || result.RowsAffected() != []int64{2, 0, 1, 0}[ordinal] {
			test.Fatal("statement did not distinguish inserted and conflicting rows", ordinal, result, err)
		}
		objects, size, calls := int64(2), int64(len(first.data)+len(second.data)), int64(1)
		if ordinal >= 2 {
			objects, size, calls = 3, size+int64(len(third.data)), 2
		}
		checkpointInsertPGAssert(test, ctx, transaction, job.DocHash, objects, size, calls)
	}
	for _, damage := range []string{"protocol", "hash"} {
		savepoint, err := transaction.Begin(ctx)
		if err != nil {
			test.Fatal(err)
		}
		body := first.data
		if damage == "protocol" {
			if _, err := savepoint.Exec(ctx, `SET LOCAL cxt.legacy_byte_checkpoint_protocol='0'`); err != nil {
				test.Fatal(err)
			}
		} else {
			body = []byte("wrong duplicate bytes")
		}
		_, err = savepoint.Exec(ctx, legacyNormalizationBatchObjectInsert, job.DocHash, state.stage.incarnation, []string{string(first.hash)}, [][]byte{body})
		var rejected *pgconn.PgError
		code := "23514"
		if damage == "protocol" {
			code = "0A000"
		}
		if !errors.As(err, &rejected) || rejected.Code != code {
			test.Fatal("conflicting input bypassed its original row guard or hash constraint", damage, err)
		}
		if err := savepoint.Rollback(ctx); err != nil {
			test.Fatal(err)
		}
	}
	checkpointInsertPGAssert(test, ctx, transaction, job.DocHash, 3, int64(len(first.data)+len(second.data)+len(third.data)), 2)
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	checkpointInsertPGAssert(test, ctx, storage.pool, job.DocHash, 3, int64(len(first.data)+len(second.data)+len(third.data)), 2)
}

func TestLegacyCheckpointPGInsertAccountingQuotaAndAuthority(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	checkpointInsertPGObserve(test, ctx, storage)
	var definer, owner, private bool
	var settings []string
	if err := storage.pool.QueryRow(ctx, `SELECT accounting.prosecdef,accounting.proowner=usage.relowner,accounting.proconfig,
 NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(accounting.proacl,acldefault('f',accounting.proowner))) permission
 WHERE permission.grantee=0 AND permission.privilege_type='EXECUTE')
 FROM pg_proc accounting CROSS JOIN pg_class usage
 WHERE accounting.oid='public.cxt_account_legacy_checkpoint_insert()'::regprocedure
 AND usage.oid='public.doc_legacy_byte_usage_v1'::regclass`).Scan(&definer, &owner, &settings, &private); err != nil ||
		!definer || !owner || !private || !strings.Contains(strings.ReplaceAll(strings.Join(settings, ";"), " ", ""), "search_path=pg_catalog,public,pg_temp") {
		test.Fatal("insert accounting changed its owner, namespace or execute authority", definer, owner, settings, private, err)
	}
	for _, quota := range []string{"objects", "bytes"} {
		work := &legacyNormalizationCheckpointWork{store: storage, job: job, state: state}
		bound, batch, err := work.beginBatch(ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(batch.transaction)
		if _, err := batch.transaction.Exec(bound, `ALTER TABLE doc_legacy_byte_usage_v1 DISABLE TRIGGER guard_legacy_byte_usage`); err != nil {
			test.Fatal(err)
		}
		objects, size := int64(65535), int64(0)
		if quota == "bytes" {
			objects, size = 0, 2684420095
		}
		if _, err := batch.transaction.Exec(bound, `UPDATE doc_legacy_byte_usage_v1 SET checkpoint_objects=$2,checkpoint_bytes=$3 WHERE doc_hash=$1`, job.DocHash, objects, size); err != nil {
			test.Fatal(err)
		}
		if _, err := batch.transaction.Exec(bound, `ALTER TABLE doc_legacy_byte_usage_v1 ENABLE TRIGGER guard_legacy_byte_usage`); err != nil {
			test.Fatal(err)
		}
		if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{legacyCheckpointPGObject("quota-first"), legacyCheckpointPGObject("quota-second")}); err != nil {
			test.Fatal(err)
		}
		err = batch.finish(bound, nil)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "23514" {
			test.Fatal("statement INSERT escaped the original usage quota", quota, err)
		}
		checkpointInsertPGAssert(test, ctx, storage.pool, job.DocHash, 0, 0, 0)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_legacy_byte_usage_v1 SET checkpoint_objects=checkpoint_objects+1 WHERE doc_hash=$1`, job.DocHash); err == nil {
		test.Fatal("direct usage mutation bypassed the trusted accounting guard")
	}
	checkpointInsertPGAssert(test, ctx, storage.pool, job.DocHash, 0, 0, 0)
}

func TestLegacyCheckpointPGInsertAccountingKeepsDocumentGroupsSeparate(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	_, _, _, otherJob, otherState := legacyCheckpointPGStart(test)
	if job.DocHash == otherJob.DocHash {
		test.Fatal("fixture must have two distinct source documents")
	}
	checkpointInsertPGObserve(test, ctx, storage)
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
		test.Fatal(err)
	}
	first, second, third := legacyCheckpointPGObject("first group"), legacyCheckpointPGObject("second group"), legacyCheckpointPGObject("second group again")
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash,bytes)
 VALUES($1,$2::uuid,$3,$4),($5,$6::uuid,$7,$8),($5,$6::uuid,$9,$10)`,
		job.DocHash, state.stage.incarnation, first.hash, first.data,
		otherJob.DocHash, otherState.stage.incarnation, second.hash, second.data, third.hash, third.data); err != nil {
		test.Fatal(err)
	}
	checkpointInsertPGAssert(test, ctx, transaction, job.DocHash, 1, int64(len(first.data)), 1)
	checkpointInsertPGAssert(test, ctx, transaction, otherJob.DocHash, 2, int64(len(second.data)+len(third.data)), 1)
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	checkpointInsertPGAssert(test, ctx, storage.pool, job.DocHash, 0, 0, 0)
	checkpointInsertPGAssert(test, ctx, storage.pool, otherJob.DocHash, 0, 0, 0)
}

func TestNormalizationCheckpointArrayPayloadTraceCountsEveryBody(test *testing.T) {
	trace := &legacyNormalizationParserReusePGTrace{}
	trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: legacyNormalizationBatchObjectInsert,
		Args: []any{"doc", "stage", []string{"first", "second"}, [][]byte{[]byte("abc"), []byte("de")}}})
	trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: "SELECT $1", Args: []any{[]byte("f")}})
	if counts := trace.snapshot(); counts.bodyArguments != 3 || counts.bodyBytes != 6 || counts.maxBodyArguments != 2 || counts.maxBodyBytes != 5 || counts.objectInserts != 1 {
		test.Fatal("array payloads disappeared from byte or argument accounting", counts)
	}
	hash := domain.HashContent([]byte("decoded input"))
	repeat := &normalizationParserRepeatPGTrace{input: map[domain.ContentHash]struct{}{hash: {}}}
	repeat.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: legacyNormalizationBatchObjectInsert,
		Args: []any{"doc", "stage", []string{string(hash), "other"}, [][]byte{[]byte("decoded input"), []byte("control")}}})
	if repeat.inputUploads.Load() != 1 || repeat.objects.Load() != 1 {
		test.Fatal("array payload hid a repeated decoded-input upload", repeat.inputUploads.Load(), repeat.objects.Load())
	}
}
