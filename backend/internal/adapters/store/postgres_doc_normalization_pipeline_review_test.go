//go:build postgres

package store

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestNormalizationCheckpointPGPipelineByteBoundaryReadsOwnWrites(test *testing.T) {
	storage, ctx, _, job, state := legacyNormalizationParserReusePGStart(test)
	peer, trace := normalizationBatchPGPeer(test, storage, ctx)
	work := &legacyNormalizationCheckpointWork{store: peer, job: job, state: state}
	bound, batch, err := work.beginBatch(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(batch.transaction)
	first := bytes.Repeat([]byte("a"), domain.LegacyCanonicalEncodingBytes/2+1)
	second := bytes.Repeat([]byte("b"), domain.LegacyCanonicalEncodingBytes/2)
	for ordinal, body := range [][]byte{first, second} {
		object := legacyByteCheckpointObject{hash: domain.HashContent(body), data: body}
		if err := work.stageParserObjects(bound, []legacyByteCheckpointObject{object}); err != nil {
			test.Fatal(err)
		}
		if len(batch.objects) != 1 || batch.bytes != len(body) || trace.snapshot().objectInserts != ordinal {
			test.Fatal("byte boundary did not flush before retaining the next owned object", ordinal, batch.bytes, trace.snapshot())
		}
	}
	actual, err := work.readParserObject(bound, domain.HashContent(second), len(second))
	if err != nil || !bytes.Equal(actual, second) || len(batch.objects) != 0 || batch.bytes != 0 {
		test.Fatal("same-connection read did not flush pending writes", err)
	}
	if err := batch.finish(bound, nil); err != nil {
		test.Fatal(err)
	}
	for _, body := range [][]byte{first, second} {
		actual, err := work.readParserObject(ctx, domain.HashContent(body), len(body))
		if err != nil || !bytes.Equal(actual, body) {
			test.Fatal("byte-boundary flush lost an object", err)
		}
	}
}

func TestNormalizationCheckpointPGPipelineCallerReservesHeap(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	rangeSpoolTempDir(test)
	_, work, job, stored := normalizationParserRestartPGStart(test, storage, ctx, false)
	defer work.Close()
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		return checkpoint.capture != nil && checkpoint.position == 14 && checkpoint.next.Parser != nil &&
			checkpoint.next.Parser.Input.Tree.DataBytes == len(stored)
	})
	if _, err := work.Step(ctx, job); err != nil {
		test.Fatal(err)
	}
	budget := reflect.ValueOf(work.checkpoint.capture).Elem().FieldByName("parserBudget").Int()
	if budget != legacyNormalizationParserHeapBudget-legacyNormalizationParserBatchHeap || legacyNormalizationParserBatchHeap != 128<<10 {
		test.Fatal("integrated parser caller did not reserve bounded pipeline ownership", budget)
	}
	resources, err := work.RetainedResources()
	if err != nil || resources.HeapBytes+legacyNormalizationParserBatchHeap > legacyNormalizationParserHeapBudget {
		test.Fatal("integrated capture exceeded the combined heap bound", resources, err)
	}
	test.Logf("caller_budget=%d reserved=%d retained=%d", budget, legacyNormalizationParserBatchHeap, resources.HeapBytes)
}
