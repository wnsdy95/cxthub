//go:build postgres

package store

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyCheckpointDiscardPGCounts(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) (objects, edges, staging, structures, usageObjects, usageBytes int64) {
	test.Helper()
	if err := storage.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_checkpoint_edges_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_checkpoint_staging_v1 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_legacy_checkpoint_structures_v1 WHERE doc_hash=$1),
 COALESCE((SELECT checkpoint_objects FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1),0),
 COALESCE((SELECT checkpoint_bytes FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1),0)`, hash).
		Scan(&objects, &edges, &staging, &structures, &usageObjects, &usageBytes); err != nil {
		test.Fatal(err)
	}
	return objects, edges, staging, structures, usageObjects, usageBytes
}

// A large checkpoint discard must not hold the job row lock in one unbounded
// transaction, or the worker's own lease renewal times out and cancels the job.
func TestLegacyNormalizationCheckpointPGDiscardCommitsBoundedPasses(test *testing.T) {
	storage, ctx, _, job, initial := legacyCheckpointPGStart(test)
	const chunk = domain.LegacyCanonicalEncodingBytes
	body := bytes.Repeat([]byte("d"), 3*chunk)
	var prefix legacyNormalizationPrefix
	var err error
	for prefix.Tree.DataBytes < len(body) {
		prefix, err = storage.stageLegacyNormalizationPrefix(ctx, job, initial, prefix, 1, chunk, len(body),
			func(ctx context.Context, offset, length int) ([]byte, error) {
				return body[offset : offset+length], ctx.Err()
			})
		if err != nil {
			test.Fatal(err)
		}
	}
	state := legacyNormalizationGCPGRoot(test, storage, ctx, job, initial, prefix)
	const plain = 2*legacyCheckpointDiscardObjects + 100
	for offset := 0; offset < plain; offset += domain.DocLegacyByteBackfillBatchBlocks {
		objects := make([]legacyByteCheckpointObject, 0, domain.DocLegacyByteBackfillBatchBlocks)
		for ordinal := offset; ordinal < min(offset+domain.DocLegacyByteBackfillBatchBlocks, plain); ordinal++ {
			objects = append(objects, legacyCheckpointPGObject(fmt.Sprintf("bounded-discard-%05d", ordinal)))
		}
		if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, state, objects); err != nil {
			test.Fatal(err)
		}
	}
	if objects, edges, _, _, _, _ := legacyCheckpointDiscardPGCounts(test, storage, ctx, job.DocHash); objects <= plain || edges == 0 {
		test.Fatal("fixture did not retain a referenced structure and more than two bounded passes of objects", objects, edges)
	}
	unpinned, err := storage.unpinLegacyNormalizationCheckpoint(ctx, job, state)
	if err != nil || unpinned.sequence != state.sequence+1 || unpinned.hash != "" || unpinned.bytes != 0 {
		test.Fatal("unpin did not clear the root and advance the sequence once", unpinned, err)
	}
	if loaded, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job); err != nil || found || loaded != unpinned {
		test.Fatal("unpinned state is not the durable checkpoint state", loaded, found, err)
	}
	cursor := domain.ContentHash("")
	transactions, passes, waited := 0, 0, false
	for {
		scanned, removed, last, err := storage.drainLegacyNormalizationCheckpoint(ctx, job, unpinned, cursor)
		if err != nil || scanned > legacyCheckpointDiscardObjects || removed > scanned || scanned != 0 && last <= cursor {
			test.Fatal("drain exceeded its bound or did not advance its cursor", scanned, removed, last, cursor, err)
		}
		transactions++
		waited = waited || removed < scanned
		renewal, cancel := context.WithTimeout(ctx, time.Second)
		err = storage.RenewDocRangeJob(renewal, job, time.Now().UTC(), 2*time.Minute)
		cancel()
		if err != nil {
			test.Fatal("lease renewal could not run between bounded discard transactions", err)
		}
		if scanned == legacyCheckpointDiscardObjects {
			cursor = last
			continue
		}
		cursor = ""
		if objects, _, _, _, _, _ := legacyCheckpointDiscardPGCounts(test, storage, ctx, job.DocHash); objects == 0 {
			break
		}
		if passes++; passes > 8 {
			test.Fatal("bounded discard did not converge")
		}
	}
	if transactions < 3 || !waited {
		test.Fatal("discard did not split into bounded transactions or delete parents before children", transactions, waited)
	}
	if objects, edges, staging, structures, usageObjects, usageBytes := legacyCheckpointDiscardPGCounts(test, storage, ctx, job.DocHash); objects != 0 || edges != 0 || staging != 0 || structures != 0 || usageObjects != 0 || usageBytes != 0 {
		test.Fatal("bounded discard left checkpoint rows or usage behind", objects, edges, staging, structures, usageObjects, usageBytes)
	}
	if loaded, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job); err != nil || found || loaded != unpinned {
		test.Fatal("draining changed the unpinned checkpoint state", loaded, found, err)
	}
}
