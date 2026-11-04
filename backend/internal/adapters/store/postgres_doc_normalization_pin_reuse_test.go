//go:build postgres

package store

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizationCheckpointPGReuseDoesNotRewriteCurrentPin(test *testing.T) {
	for _, pipeline := range []bool{false, true} {
		name := "direct"
		if pipeline {
			name = "pipeline"
		}
		test.Run(name, func(test *testing.T) {
			storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
			object := legacyCheckpointPGObject("current-pin-needs-no-rewrite")
			peer, trace := normalizationBatchPGPeer(test, storage, ctx)
			stage := func() {
				test.Helper()
				if !pipeline {
					if err := peer.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
						test.Fatal(err)
					}
					return
				}
				work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
				bound, batch, err := work.beginBatch(ctx)
				if err != nil {
					test.Fatal(err)
				}
				if err := batch.finish(bound, work.stageParserObjects(bound, []legacyByteCheckpointObject{object})); err != nil {
					test.Fatal(err)
				}
			}
			pin := func() string {
				test.Helper()
				var tuple string
				var sequence int64
				if err := storage.pool.QueryRow(ctx, `SELECT ctid::text,last_sequence FROM doc_legacy_checkpoint_staging_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3`, job.DocHash, state.stage.incarnation, object.hash).Scan(&tuple, &sequence); err != nil || sequence != state.sequence {
					test.Fatal("pin did not retain the current sequence", sequence, state.sequence, err)
				}
				return tuple
			}
			stage()
			before, counts := pin(), trace.snapshot()
			stage()
			if after := pin(); after != before {
				test.Fatal("current pin was physically rewritten", before, after)
			}
			if after := trace.snapshot(); after.hashReads-counts.hashReads != 1 || after.bodyBytes != counts.bodyBytes {
				test.Fatal("reuse bypassed fresh verification or resent the body", counts, after)
			}
			root := legacyCheckpointPGObject("next-pin-generation")
			var err error
			state, err = storage.saveLegacyNormalizationCheckpoint(ctx, job, state, root.hash, len(root.data), []legacyByteCheckpointObject{root})
			if err != nil {
				test.Fatal(err)
			}
			stage()
			if pin() == before || domain.HashContent(object.data) != object.hash {
				test.Fatal("new sequence did not refresh the pin")
			}
		})
	}
}

func TestNormalizationCheckpointPGReusedStructureRejectsDamagedChild(test *testing.T) {
	for _, pipeline := range []bool{false, true} {
		name := "direct"
		if pipeline {
			name = "pipeline"
		}
		test.Run(name, func(test *testing.T) {
			storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
			object := legacyCheckpointPGObject("retained-structure-leaf")
			if err := storage.stageLegacyNormalizationParserObject(ctx, job, state, object.hash, object.data); err != nil {
				test.Fatal(err)
			}
			work := &legacyNormalizationCheckpointWork{store: storage, job: job, state: state}
			refs := []legacyNormalizationBufferRef{{Hash: object.hash, Bytes: len(object.data)}}
			_, err := work.parserPinNode(ctx, refs)
			if err != nil {
				test.Fatal(err)
			}
			legacyNormalizationParserReusePGDamage(test, storage, ctx, job, state, object.hash, object.data[:len(object.data)-1])
			bound := ctx
			var batch *legacyNormalizationCheckpointBatch
			if pipeline {
				bound, batch, err = work.beginBatch(ctx)
				if err != nil {
					test.Fatal(err)
				}
			}
			_, err = work.parserPinNode(bound, refs)
			if batch != nil {
				err = batch.finish(bound, err)
			}
			if err == nil {
				test.Fatal("reused structure accepted a corrupted child's stored length")
			}
		})
	}
}
