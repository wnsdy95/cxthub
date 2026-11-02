//go:build postgres

package store

import (
	"fmt"
	"testing"
)

func TestNormalizationParserPinsPGDeduplicateOnlyRetentionEdges(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	peer, trace := normalizationBatchPGPeer(test, storage, ctx)
	work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
	work.next.Parser = &legacyNormalizationParserCheckpoint{}
	objects := make([]legacyByteCheckpointObject, 8)
	for ordinal := range objects {
		objects[ordinal] = legacyCheckpointPGObject(fmt.Sprintf("repeated-retention-%d", ordinal))
	}
	for cycle := 0; cycle < 8; cycle++ {
		for _, object := range objects {
			if err := work.putParserObject(ctx, object.hash, object.data); err != nil {
				test.Fatal(err)
			}
		}
	}
	if work.parserPinCounts[0] != uint8(len(objects)) {
		test.Fatal("retention index repeated references already in its bounded frontier", work.parserPinCounts)
	}
	for _, count := range work.parserPinCounts[1:] {
		if count != 0 {
			test.Fatal("repeated references unnecessarily filled an index page", work.parserPinCounts)
		}
	}
	if trace.snapshot().hashReads != 9*len(objects) {
		test.Fatal("deduplicated retention skipped fresh byte verification", trace.snapshot())
	}
	for ordinal := 0; ordinal < 2*legacyNormalizationPrefixFanout; ordinal++ {
		object := legacyCheckpointPGObject(fmt.Sprintf("unique-retention-%d", ordinal))
		objects = append(objects, object)
		for _, current := range []legacyByteCheckpointObject{object, objects[0]} {
			if err := work.putParserObject(ctx, current.hash, current.data); err != nil {
				test.Fatal(err)
			}
		}
	}
	if work.parserPinCounts[0] != legacyNormalizationPrefixFanout || trace.snapshot().hashReads != 72+6*legacyNormalizationPrefixFanout+2 {
		test.Fatal("stable pins were evicted or a repeated object bypassed verification", work.parserPinCounts, trace.snapshot())
	}
	if err := work.finishParserPins(ctx); err != nil {
		test.Fatal(err)
	}
	var edges int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_checkpoint_edges_v1
 WHERE doc_hash=$1 AND stage_id=$2::uuid AND child_hash=$3`, job.DocHash, state.stage.incarnation, objects[0].hash).Scan(&edges); err != nil || edges != 1 {
		test.Fatal("stable pin was duplicated across completed retention pages", edges, err)
	}
	for _, object := range objects {
		var retained bool
		if err := storage.pool.QueryRow(ctx, `WITH RECURSIVE retained(hash) AS (
 SELECT $3::text UNION SELECT edge.child_hash FROM doc_legacy_checkpoint_edges_v1 edge JOIN retained ON edge.parent_hash=retained.hash
 WHERE edge.doc_hash=$1 AND edge.stage_id=$2::uuid
 ) SELECT EXISTS(SELECT 1 FROM retained WHERE hash=$4)`, job.DocHash, state.stage.incarnation, work.next.Parser.Pins.Hash, object.hash).Scan(&retained); err != nil || !retained {
			test.Fatal("deduplicated retention lost a referenced object", object.hash, retained, err)
		}
	}
	object := objects[0]
	legacyNormalizationParserReusePGDamage(test, storage, ctx, job, state, object.hash, object.data[:len(object.data)-1])
	if err := work.putParserObject(ctx, object.hash, object.data); err == nil {
		test.Fatal("retained reference bypassed current object integrity")
	}
}
