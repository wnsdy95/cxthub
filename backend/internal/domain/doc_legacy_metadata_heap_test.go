package domain

import (
	"context"
	"math"
	"reflect"
	"testing"
)

func rangeHashHeapOracle(hashes []ContentHash) docRangeHeaderHeapEstimate {
	estimate := docRangeHeaderHeapEstimate{}
	for _, hash := range hashes[:cap(hashes)] {
		estimate.allocation(int64(len(hash)), 1)
	}
	return estimate
}

func checkRangeDirectoryHeap(test testing.TB, work *docRangeDirectoryWork) {
	test.Helper()
	if work == nil {
		return
	}
	if want := rangeHashHeapOracle(work.hashes); !reflect.DeepEqual(work.hashHeap, want) {
		test.Fatal("directory input accounting differs from owned backing storage", work.hashHeap, want)
	}
	if want := rangeHashHeapOracle(work.parents); !reflect.DeepEqual(work.parentHeap, want) {
		test.Fatal("directory parent accounting differs from owned backing storage", work.parentHeap, want)
	}
}

func TestLegacyMetadataDirectoryAccountingMatchesOwnedState(test *testing.T) {
	proof, _ := legacyDocRangeGenerationFixture(test, DocRangePageEvents*DocRangeDirectoryFanout+1)
	state, plan, _ := legacyMetadataWorkFixture(test, proof)
	spec, _ := plan.Spec()
	for step := 0; step <= spec.Blocks+5; step++ {
		if want := rangeHashHeapOracle(state.pages.hashes); !reflect.DeepEqual(state.pages.hashHeap, want) {
			test.Fatal("source-page accounting differs from owned hash storage", step, state.pages.hashHeap, want)
		}
		objects := docRangeHeaderHeapEstimate{}
		for _, object := range state.metadata.objects[:cap(state.metadata.objects)] {
			objects.allocation(int64(len(object.hash)), 1)
		}
		if !reflect.DeepEqual(state.metadata.objectHeap, objects) {
			test.Fatal("receipt accounting differs from owned object storage", step, state.metadata.objectHeap, objects)
		}
		checkRangeDirectoryHeap(test, state.events)
		if chunks := state.chunks; chunks != nil {
			owned := docRangeHeaderHeapEstimate{}
			for _, chunk := range chunks.chunks[:cap(chunks.chunks)] {
				owned.allocation(int64(len(chunk.Hash)), 1)
			}
			if !reflect.DeepEqual(chunks.chunkHeap, owned) || !reflect.DeepEqual(chunks.pageHeap, rangeHashHeapOracle(chunks.pages)) {
				test.Fatal("chunk accounting differs from actual retained descriptors/pages", step)
			}
			checkRangeDirectoryHeap(test, chunks.directory)
		}
		if bytes, err := state.RetainedBytes(); err != nil || bytes <= 0 || bytes >= ConversationManifestChunkBytes/2 {
			test.Fatal("actual metadata state escaped the unchanged small-event bound", step, bytes, err)
		}
		if state.done {
			return
		}
		if _, err := state.Step(context.Background(), func(context.Context, DocRangeObject) error { return nil }); err != nil {
			test.Fatal(err)
		}
	}
	test.Fatal("accounted metadata did not terminate")
}

func TestRangeDirectoryAccountingIncludesUnusedBackingReferences(test *testing.T) {
	hashes := []ContentHash{HashContent([]byte("first")), HashContent([]byte("backing-only"))}
	work, err := newDocRangeDirectoryWork(hashes[:1])
	if err != nil {
		test.Fatal(err)
	}
	checkRangeDirectoryHeap(test, work)
	chunks := []ConversationManifestChunk{{Hash: hashes[0], Bytes: 1}, {Hash: hashes[1], Bytes: 1}}
	chunkWork, err := newDocRangeChunkDirectoryWork(chunks[:1])
	if err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(chunkWork.chunkHeap, work.hashHeap) {
		test.Fatal("unused descriptor backing storage escaped accounting")
	}
	for _, sizes := range [][2]int64{{0, 0}, {17, 23}, {math.MaxInt64, 1}, {-1, 0}, {0, -1}} {
		estimate := docRangeHeaderHeapEstimate{bytes: sizes[0]}
		estimate.include(docRangeHeaderHeapEstimate{bytes: sizes[1]})
		invalid := sizes[0] < 0 || sizes[1] < 0 || sizes[0] > math.MaxInt64-sizes[1]
		if (estimate.err != nil) != invalid || (!invalid && estimate.bytes != sizes[0]+sizes[1]) {
			test.Fatal("cached resource estimates bypassed integer overflow/negative checks", sizes, estimate)
		}
	}
}
