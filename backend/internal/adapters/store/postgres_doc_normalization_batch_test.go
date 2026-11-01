//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestNormalizationCheckpointPGBatchRejectsBorrowedAndClosedAuthority(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	work := &legacyNormalizationCheckpointWork{store: storage, job: job, state: state}
	bound, batch, err := work.beginBatch(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(batch.transaction)
	object := legacyCheckpointPGObject("bounded-parser-batch")
	if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil {
		test.Fatal(err)
	}
	if body, err := work.readParserObject(bound, object.hash, len(object.data)); err != nil || !bytes.Equal(body, object.data) {
		test.Fatal("batch lost its bounded current object", err)
	}
	foreign := *work
	if _, err := foreign.readParserObject(bound, object.hash, len(object.data)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("batch admitted a different checkpoint owner", err)
	}
	work.state.sequence++
	if _, err := work.readParserObject(bound, object.hash, len(object.data)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("batch admitted a changed checkpoint sequence", err)
	}
	work.state = state
	if err := batch.finish(bound, nil); err != nil {
		test.Fatal(err)
	}
	if _, err := work.readParserObject(bound, object.hash, len(object.data)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("closed batch retained authority", err)
	}
	if body, err := work.readParserObject(ctx, object.hash, len(object.data)); err != nil || !bytes.Equal(body, object.data) {
		test.Fatal("freshly admitted read lost the committed object", err)
	}
}

func TestNormalizationCheckpointPGBatchFinalFenceRollsBackObjects(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	work := &legacyNormalizationCheckpointWork{store: storage, job: job, state: state}
	bound, batch, err := work.beginBatch(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(batch.transaction)
	object := legacyCheckpointPGObject("parser-batch-final-fence")
	if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil {
		test.Fatal(err)
	}
	if _, err := batch.transaction.Exec(bound, `UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id=$1 AND doc_hash=$2`, job.RepoID, job.DocHash); err != nil {
		test.Fatal(err)
	}
	if err := batch.finish(bound, nil); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("batch committed after losing its claim", err)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, state.stage.incarnation, object.hash).Scan(&count); err != nil || count != 0 {
		test.Fatal("failed final fence retained provisional objects", count, err)
	}
	if err := storage.confirmLegacyBytePreparation(ctx, job, state.stage); err != nil {
		test.Fatal("rollback changed the original live claim", err)
	}
}

func TestLegacyNormalizationPrefixPGBatchesTreeReadAdmission(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	const chunk = domain.LegacyCanonicalEncodingBytes
	body := bytes.Repeat([]byte("tree-read"), (34*chunk+8)/9)
	var prefix legacyNormalizationPrefix
	var err error
	for prefix.Tree.DataBytes < 34*chunk {
		prefix, err = storage.stageLegacyNormalizationPrefix(ctx, job, state, prefix, 1, chunk, 34*chunk,
			func(_ context.Context, offset, length int) ([]byte, error) { return body[offset : offset+length], nil })
		if err != nil {
			test.Fatal(err)
		}
	}
	peer, trace := legacyNormalizationParserReusePGPeer(test, storage, ctx)
	actual, err := peer.readLegacyNormalizationPrefix(ctx, job, state, prefix, 33*chunk, 17)
	if err != nil || !bytes.Equal(actual, body[33*chunk:33*chunk+17]) || trace.snapshot().queries != 10 {
		test.Fatal("three-node read repeated source/stage/job admission", trace.snapshot(), err)
	}
}

func TestNormalizationCheckpointPGBatchCanceledFinishClosesOwner(test *testing.T) {
	for _, mode := range []string{"capture", "restore"} {
		test.Run(mode, func(test *testing.T) {
			storage, ctx := rootProjectionIsolatedPG(test)
			rangeSpoolTempDir(test)
			_, work, job, stored := normalizationParserRestartPGStart(test, storage, ctx, false)
			defer func() { work.Close() }()
			if mode == "capture" {
				normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
					return checkpoint.capture != nil && checkpoint.position == 14 && checkpoint.next.Parser != nil &&
						checkpoint.next.Parser.Input.Tree.DataBytes == len(stored)
				})
			} else {
				normalizationParserRestartPGAwaitRoot(test, work, ctx, job, 0)
				peer := work.store
				if err := work.Close(); err != nil {
					test.Fatal(err)
				}
				job = normalizationCheckpointPGReclaim(test, peer, ctx, job)
				prepared, err := peer.PrepareDocRangeWork(ctx, job)
				if err != nil {
					test.Fatal(err)
				}
				work = prepared.(*preparedLegacyCaptureWork)
				normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
					return checkpoint.restoring && checkpoint.restore != nil
				})
			}
			if work.store.pool.Config().MaxConns != 1 {
				test.Fatal("batch callbacks must share one available connection")
			}
			trace := work.store.pool.Config().ConnConfig.Tracer.(*docCaptureWorkPGTrace)
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			committed := false
			trace.commit.Store(&docCaptureCommitHook{run: func() { committed = true; cancel() }})
			progress, err := work.Step(selected, job)
			for step := 1; mode == "capture" && err == nil && !committed && step < normalizationCheckpointPGStepLimit; step++ {
				if progress != outbound.DocRangeWorkPending || work.checkpoint.capture == nil || work.checkpoint.position != 14 {
					test.Fatal("capture passed its first actual I/O batch", step, progress)
				}
				progress, err = work.Step(selected, job)
			}
			trace.commit.Store(nil)
			if !committed || progress != 0 || !errors.Is(err, context.Canceled) {
				test.Fatal("batch ignored cancellation at commit", progress, err)
			}
			if mode == "capture" {
				if _, err := work.checkpoint.capture.Control(ctx); err == nil {
					test.Fatal("failed batch retained its frozen capture")
				}
			} else if _, err := work.checkpoint.restore.Result(); err == nil || errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("failed batch retained its restorable owner", err)
			}
			if err := work.store.confirmLegacyBytePreparation(ctx, job, work.witness.stage); err != nil {
				test.Fatal("fixture must retain current authority independently of the failed owner", err)
			}
			if progress, err := work.Step(ctx, job); err == nil || progress == outbound.DocRangeWorkPublishable {
				test.Fatal("failed batch owner resumed checkpoint work", progress, err)
			}
		})
	}
}
