//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type normalizationBatchPGTrace struct {
	legacyNormalizationParserReusePGTrace
	maximum int
}

func (trace *normalizationBatchPGTrace) TraceBatchStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	trace.mutex.Lock()
	trace.maximum = max(trace.maximum, data.Batch.Len())
	trace.mutex.Unlock()
	for _, query := range data.Batch.QueuedQueries {
		trace.TraceQueryStart(ctx, connection, pgx.TraceQueryStartData{SQL: query.SQL, Args: query.Arguments})
	}
	return ctx
}

func (*normalizationBatchPGTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (*normalizationBatchPGTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

func normalizationBatchPGPeer(test *testing.T, storage *PostgresStore, ctx context.Context) (*PostgresStore, *normalizationBatchPGTrace) {
	test.Helper()
	trace := &normalizationBatchPGTrace{}
	config := storage.pool.Config()
	config.MaxConns, config.MinConns = 1, 0
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	return &PostgresStore{pool: pool}, trace
}

func TestNormalizationCheckpointPGBatchPipelinesBoundedOwnedObjects(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	peer, trace := normalizationBatchPGPeer(test, storage, ctx)
	work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
	objects := make([]legacyByteCheckpointObject, domain.DocLegacyByteBackfillBatchBlocks)
	for ordinal := range objects {
		objects[ordinal] = legacyCheckpointPGObject(fmt.Sprintf("bounded-pipeline-%d", ordinal))
	}
	for _, reuse := range []bool{false, true} {
		before := trace.snapshot()
		bound, batch, err := work.beginBatch(ctx)
		if err != nil {
			test.Fatal(err)
		}
		for _, object := range objects {
			borrowed := object
			borrowed.data = bytes.Clone(object.data)
			if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{borrowed}); err != nil {
				test.Fatal(err)
			}
			borrowed.data[0] ^= 1
			if len(batch.objects) > domain.DocLegacyByteBackfillBatchBlocks || batch.bytes > domain.LegacyCanonicalEncodingBytes {
				test.Fatal("pipeline exceeded its original object bound", len(batch.objects))
			}
		}
		if err := batch.finish(bound, nil); err != nil || batch.transaction != nil || batch.objects != nil {
			test.Fatal("pipeline did not commit and release its bounded ownership", err)
		}
		after := trace.snapshot()
		if reuse && (after.bodyArguments != before.bodyArguments || after.objectInserts != before.objectInserts || after.hashReads-before.hashReads != len(objects)) {
			test.Fatal("pipelined reuse resent bodies or skipped fresh hash proofs", before, after)
		}
		for _, object := range objects {
			body, err := work.readParserObject(ctx, object.hash, len(object.data))
			if err != nil || !bytes.Equal(body, object.data) {
				test.Fatal("pipelined object borrowed caller bytes", err)
			}
		}
	}
	trace.mutex.Lock()
	maximum := trace.maximum
	trace.mutex.Unlock()
	if maximum != domain.DocLegacyByteBackfillBatchBlocks {
		test.Fatal("pipeline did not use exactly its existing eight-object bound", maximum)
	}
}

func TestNormalizationCheckpointPGBatchDeferredCorruptionFailsClosed(test *testing.T) {
	for _, damage := range []string{"hash", "length"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx, fixture, job, state := legacyNormalizationParserReusePGStart(test)
			object := legacyCheckpointPGObject("buffered-hit-must-be-freshly-verified")
			if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
				test.Fatal(err)
			}
			corrupt := bytes.Clone(object.data)
			if damage == "hash" {
				corrupt[0] ^= 1
			} else {
				corrupt = corrupt[:len(corrupt)-1]
			}
			legacyNormalizationParserReusePGDamage(test, storage, ctx, job, state, object.hash, corrupt)
			peer, trace := normalizationBatchPGPeer(test, storage, ctx)
			work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			bound, batch, err := work.beginBatch(ctx)
			if err != nil {
				test.Fatal(err)
			}
			if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil || len(batch.objects) != 1 {
				test.Fatal("fixture did not retain a deferred bounded object", err)
			}
			if err := batch.finish(bound, nil); !errors.Is(err, domain.ErrPreparedDocRangeObject) || batch.transaction != nil || batch.objects != nil {
				test.Fatal("deferred corruption retained a reusable batch", err)
			}
			if counts := trace.snapshot(); counts.bodyArguments != 0 || counts.objectInserts != 0 || counts.hashReads != 1 {
				test.Fatal("deferred corrupt hit overwrote bytes or omitted proof", counts)
			}
			if _, err := work.readParserObject(bound, object.hash, len(object.data)); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("failed pipeline retained authority", err)
			}
			var actual []byte
			if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`,
				job.DocHash, state.stage.incarnation, object.hash).Scan(&actual); err != nil || !bytes.Equal(actual, corrupt) || public != legacyByteStagePGPublicImage(test, storage, ctx, fixture) {
				test.Fatal("failed pipeline changed immutable or public state", err)
			}
		})
	}
}

func TestNormalizationCheckpointPGBatchBufferedDuplicatesPreserveFlushBounds(test *testing.T) {
	for _, size := range []int{64, domain.LegacyCanonicalEncodingBytes / 2} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
			peer, trace := normalizationBatchPGPeer(test, storage, ctx)
			work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
			body := bytes.Repeat([]byte("d"), size)
			object := legacyByteCheckpointObject{hash: domain.HashContent(body), data: body}
			bound, batch, err := work.beginBatch(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(batch.transaction)
			limit := min(domain.DocLegacyByteBackfillBatchBlocks, domain.LegacyCanonicalEncodingBytes/size)
			for window := 0; window < 2; window++ {
				before := trace.snapshot()
				for ordinal := 1; ordinal <= limit; ordinal++ {
					borrowed := object
					borrowed.data = bytes.Clone(body)
					if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{borrowed}); err != nil {
						test.Fatal(err)
					}
					borrowed.data[0] ^= 1
					if ordinal < limit && (len(batch.objects) != ordinal || batch.bytes != ordinal*size ||
						!bytes.Equal(batch.objects[0].data, body) || trace.snapshot().hashReads != before.hashReads) {
						test.Fatal("duplicate changed owned bytes or the original flush boundary", ordinal, len(batch.objects), batch.bytes)
					}
				}
				if len(batch.objects) != 0 || batch.bytes != 0 {
					test.Fatal("duplicate window did not flush at its original count/byte cap")
				}
				after := trace.snapshot()
				proofs, inserts := 2, 1
				if window != 0 {
					proofs, inserts = 1, 0
				}
				if after.hashReads-before.hashReads != proofs || after.objectInserts-before.objectInserts != inserts ||
					after.bodyArguments-before.bodyArguments != inserts || after.bodyBytes-before.bodyBytes != inserts*size {
					test.Fatal("buffered duplicates skipped fresh proof, resent reuse bytes, or repeated SQL", before, after)
				}
			}
			if err := batch.finish(bound, nil); err != nil {
				test.Fatal(err)
			}
			actual, err := work.readParserObject(ctx, object.hash, size)
			if err != nil || !bytes.Equal(actual, body) {
				test.Fatal("duplicate admission borrowed caller bytes", err)
			}
		})
	}
}

func TestNormalizationCheckpointPGBatchDuplicateValidatesEveryInput(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	work := &legacyNormalizationCheckpointWork{store: storage, job: job, state: state}
	object := legacyCheckpointPGObject("duplicate-must-hash-every-owned-input")
	bound, batch, err := work.beginBatch(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(batch.transaction)
	if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil {
		test.Fatal(err)
	}
	for _, invalid := range []legacyByteCheckpointObject{
		{hash: object.hash, data: bytes.Repeat([]byte("x"), len(object.data))},
		{hash: object.hash, data: object.data, structure: 3},
	} {
		if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{invalid}); err == nil {
			test.Fatal("buffered hash bypassed input validation")
		}
	}
	if len(batch.objects) != 1 || batch.bytes != len(object.data) {
		test.Fatal("rejected duplicate changed pending ownership")
	}
	if err := batch.finish(bound, domain.ErrIntegrity); !errors.Is(err, domain.ErrIntegrity) || batch.transaction != nil || batch.objects != nil {
		test.Fatal("rejected duplicate retained a usable batch", err)
	}
}

func TestNormalizationCheckpointPGBatchDuplicatePreservesConflictingStructure(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	child := legacyCheckpointPGObject("bounded-duplicate-structure-child")
	if err := storage.stageLegacyNormalizationCheckpointObjects(ctx, job, state, []legacyByteCheckpointObject{child}); err != nil {
		test.Fatal(err)
	}
	body, err := json.Marshal(legacyNormalizationPrefixNode{Version: 1, Children: []legacyNormalizationPrefixTree{
		{Hash: child.hash, Bytes: len(child.data), DataBytes: len(child.data), Leaves: 1},
	}})
	if err != nil {
		test.Fatal(err)
	}
	object := legacyByteCheckpointObject{hash: domain.HashContent(body), data: body, structure: 1}
	work := &legacyNormalizationCheckpointWork{store: storage, job: job, state: state}
	bound, batch, err := work.beginBatch(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(batch.transaction)
	for _, structure := range []uint8{1, 2} {
		object.structure = structure
		if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil {
			test.Fatal(err)
		}
	}
	if len(batch.objects) != 2 || batch.bytes != 2*len(body) {
		test.Fatal("same hash suppressed conflicting structure validation")
	}
	if err := batch.finish(bound, nil); err == nil {
		test.Fatal("conflicting structure was accepted")
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`,
		job.DocHash, state.stage.incarnation, object.hash).Scan(&count); err != nil || count != 0 {
		test.Fatal("conflicting structure escaped rollback", count, err)
	}
}

func TestNormalizationCheckpointPGBatchBufferedDuplicateCorruptionFailsFresh(test *testing.T) {
	for _, damage := range []string{"hash", "length"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
			object := legacyCheckpointPGObject("buffered-duplicate-still-needs-current-bytes")
			if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
				test.Fatal(err)
			}
			corrupt := bytes.Clone(object.data)
			if damage == "hash" {
				corrupt[0] ^= 1
			} else {
				corrupt = corrupt[:len(corrupt)-1]
			}
			legacyNormalizationParserReusePGDamage(test, storage, ctx, job, state, object.hash, corrupt)
			peer, trace := normalizationBatchPGPeer(test, storage, ctx)
			work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
			bound, batch, err := work.beginBatch(ctx)
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(batch.transaction)
			for ordinal := 0; ordinal < 3; ordinal++ {
				if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil {
					test.Fatal(err)
				}
			}
			if len(batch.objects) != 3 || batch.bytes != 3*len(object.data) {
				test.Fatal("fixture did not retain the original bounded duplicate window")
			}
			if err := batch.finish(bound, nil); !errors.Is(err, domain.ErrPreparedDocRangeObject) || batch.transaction != nil || batch.objects != nil {
				test.Fatal("buffered duplicate accepted stored corruption", err)
			}
			if counts := trace.snapshot(); counts.hashReads != 1 || counts.bodyArguments != 0 || counts.objectInserts != 0 {
				test.Fatal("corrupt duplicate omitted fresh proof or overwrote stored bytes", counts)
			}
		})
	}
}
