//go:build postgres

package store

import (
	"fmt"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestLegacyCheckpointPGDeleteAccountingAggregatesAndRollsBack(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	const count = 128
	var total int64
	for offset := 0; offset < count; offset += domain.DocLegacyByteBackfillBatchBlocks {
		objects := make([]legacyByteCheckpointObject, domain.DocLegacyByteBackfillBatchBlocks)
		for ordinal := range objects {
			objects[ordinal] = legacyCheckpointPGObject(fmt.Sprintf("delete-accounting-object-%d", offset+ordinal))
			total += int64(len(objects[ordinal].data))
		}
		if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, state, objects); err != nil {
			test.Fatal(err)
		}
	}
	for _, commit := range []bool{false, true} {
		transaction, _, err := storage.openLegacyByteCheckpoint(ctx, job, state.stage, &state, false)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(transaction)
		if err := legacyByteCheckpointProtocols(ctx, transaction); err != nil {
			test.Fatal(err)
		}
		if _, err := transaction.Exec(ctx, `CREATE TEMP TABLE checkpoint_accounting_calls (doc_hash text) ON COMMIT DROP;
 CREATE FUNCTION pg_temp.observe_checkpoint_accounting() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN INSERT INTO pg_temp.checkpoint_accounting_calls VALUES(NEW.doc_hash); RETURN NEW; END $$;
 CREATE TRIGGER observe_checkpoint_accounting AFTER UPDATE ON public.doc_legacy_byte_usage_v1
 FOR EACH ROW EXECUTE FUNCTION pg_temp.observe_checkpoint_accounting()`); err != nil {
			test.Fatal(err)
		}
		result, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid`, job.DocHash, state.stage.incarnation)
		if err != nil || result.RowsAffected() != count {
			test.Fatal("delete did not remove the exact checkpoint objects", result, err)
		}
		var calls, objects, size int64
		if err := transaction.QueryRow(ctx, `SELECT checkpoint_objects,checkpoint_bytes FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&objects, &size); err != nil || objects != 0 || size != 0 {
			test.Fatal("statement accounting did not match deleted bytes and objects", objects, size, err)
		}
		if _, err := transaction.Exec(ctx, `DELETE FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid`, job.DocHash, state.stage.incarnation); err != nil {
			test.Fatal(err)
		}
		if err := transaction.QueryRow(ctx, `SELECT count(*) FROM pg_temp.checkpoint_accounting_calls`).Scan(&calls); err != nil || calls != 1 {
			test.Fatal("delete updated usage per object or for an empty statement", calls, err)
		}
		if _, err := transaction.Exec(ctx, `DROP TRIGGER observe_checkpoint_accounting ON public.doc_legacy_byte_usage_v1; DROP FUNCTION pg_temp.observe_checkpoint_accounting()`); err != nil {
			test.Fatal(err)
		}
		if commit {
			err = transaction.Commit(ctx)
		} else {
			err = transaction.Rollback(ctx)
		}
		if err != nil {
			test.Fatal(err)
		}
		if err := storage.pool.QueryRow(ctx, `SELECT checkpoint_objects,checkpoint_bytes FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&objects, &size); err != nil {
			test.Fatal(err)
		}
		if commit && (objects != 0 || size != 0) || !commit && (objects != count || size != total) {
			test.Fatal("checkpoint objects and accounting did not commit or roll back together", commit, objects, size)
		}
		var actual int64
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&actual); err != nil || actual != objects {
			test.Fatal("physical objects differ from accounting", actual, objects, err)
		}
	}
}
