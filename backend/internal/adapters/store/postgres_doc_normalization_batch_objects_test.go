//go:build postgres

package store

import (
	"bytes"
	"context"
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
