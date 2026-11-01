package domain

import (
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyCanonicalPreparedKeyRefsOwnBoundedKeys(test *testing.T) {
	var refs legacyCanonicalPreparedKeyRefs
	backing := strings.Repeat("large-backing", 10000)
	key := backing[:64]
	ref := legacyNativeCheckpointRef{Hash: HashContent([]byte("key-ref")), Bytes: 7}
	refs.remember(key, ref)
	if refs.find(key) != ref || unsafe.StringData(refs.entries[0].key) == unsafe.StringData(key) {
		test.Fatal("bounded key reference borrowed its source backing")
	}
	refs.remember(backing, ref)
	if refs.find(backing).Bytes != 0 || refs.next != 1 {
		test.Fatal("key references retained an unbounded key")
	}
	for ordinal := 0; ordinal < 100; ordinal++ {
		refs.remember(strings.Repeat("x", ordinal%65), ref)
	}
	var heap docRangeHeaderHeapEstimate
	refs.retained(&heap)
	var maximum docRangeHeaderHeapEstimate
	for range refs.entries {
		maximum.allocation(int64(64+len(ref.Hash)), 1)
	}
	if heap.err != nil || maximum.err != nil || refs.next < 0 || refs.next >= len(refs.entries) || heap.bytes > maximum.bytes {
		test.Fatal("key reference ownership exceeded its fixed capacity", refs.next, heap)
	}
}

func TestLegacyNormalizationStageFailureReleasesKeyRefs(test *testing.T) {
	work, capture, buffer, store := legacyNormalizationStageFenceStart(test, true)
	binding := HashContent(buffer.body)
	want := errors.New("checkpoint key-reference write failure")
	var refs *legacyCanonicalPreparedKeyRefs
	put := store.Put
	store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		if refs != nil {
			return want
		}
		return put(ctx, hash, body)
	}
	for range legacyDecoderCheckpointSteps {
		if capture.stage != nil && capture.stage.child != nil && capture.stage.child.keyRefs.entries[0].ref.Bytes != 0 {
			refs = &capture.stage.child.keyRefs
		}
		_, ready, err := capture.StageStep(context.Background(), binding, store, legacyNormalizationStageFenceBudget)
		if errors.Is(err, want) {
			if ready || refs == nil || *refs != (legacyCanonicalPreparedKeyRefs{}) {
				test.Fatal("failed checkpoint retained provisional key authority")
			}
			legacyNormalizationStageFenceReleased(test, work, capture)
			return
		}
		if err != nil || ready {
			test.Fatal("checkpoint missed its populated-reference failure boundary", ready, err)
		}
	}
	test.Fatal("checkpoint never failed after retaining a key reference")
}
