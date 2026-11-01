package domain

import (
	"testing"
	"unsafe"
)

func TestLegacyCanonicalSingletonHeapReviewZeroWidthOracle(test *testing.T) {
	var values legacyCanonicalPages[struct{}]
	checkpoints := map[int]int{1: 0, 2: 0, 9: 1, 65: 3}
	for count := 1; count <= 65; count++ {
		if err := values.append(struct{}{}); err != nil {
			test.Fatal("zero-width storage append failed", count, err)
		}
		nodes, checkpoint := checkpoints[count]
		if !checkpoint {
			continue
		}
		owned, err := values.ownedBytes()
		want := int64(nodes) * (64 + 2*int64(unsafe.Sizeof(legacyCanonicalPageNode[struct{}]{})))
		if err != nil || owned != want {
			test.Fatal("zero-width storage charged value bytes or lost actual nodes", count, owned, want, err)
		}
		independent := legacyCanonicalIndependentPageHeap(test, &values)
		if independent.err != nil || independent.bytes != owned {
			test.Fatal("independent zero-width backing differs from retained storage", count, independent, owned)
		}
	}
	for values.length() != 0 {
		if err := values.pop(); err != nil {
			test.Fatal("zero-width storage could not release a value", err)
		}
	}
	legacyCanonicalSingletonStorage(test, &values)
	values.clear()
	if independent := legacyCanonicalIndependentPageHeap(test, &values); independent.err != nil || independent.bytes != 0 {
		test.Fatal("cleared zero-width storage retained backing", independent)
	}
}
