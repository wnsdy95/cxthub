package domain

import (
	"errors"
	"math"
	"testing"
	"unsafe"
)

func TestLegacyCanonicalNumberRegistryReviewSplitRetainedOverflowBoundary(test *testing.T) {
	var marker byte
	var registry legacyCanonicalNumberRegistry
	for size := 0; size <= legacyCanonicalNumberBucketValues; size++ {
		inserted, err := registry.insert(legacyCanonicalNumberIdentity{start: &marker, size: size})
		if err != nil || !inserted {
			test.Fatal("split registry fixture failed to register an exact identity", size, inserted, err)
		}
	}

	var expected docRangeHeaderHeapEstimate
	allocations := 0
	var visit func(*legacyCanonicalNumberRegistryNode, int)
	visit = func(node *legacyCanonicalNumberRegistryNode, depth int) {
		if node == nil {
			return
		}
		if depth > legacyCanonicalNumberDigits {
			test.Fatal("split registry exceeded its pointer and length radix", depth)
		}
		allocations++
		expected.allocation(1, int64(unsafe.Sizeof(*node)))
		for _, child := range node.children {
			visit(child, depth+1)
		}
	}
	visit(registry.root, 0)
	if expected.err != nil || allocations <= 1 || allocations != registry.nodes {
		test.Fatal("split fixture did not expose distinct fixed-node allocations", allocations, registry.nodes, expected)
	}

	before := registry
	for _, fixture := range []struct {
		name     string
		excess   int64
		overflow bool
	}{
		{name: "exact-fit"},
		{name: "one-byte-overflow", excess: 1, overflow: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			heap := docRangeHeaderHeapEstimate{bytes: math.MaxInt64 - expected.bytes + fixture.excess}
			registry.retained(&heap)
			if fixture.overflow {
				if !errors.Is(heap.err, ErrIntegrity) {
					test.Fatal("separately allocated registry nodes bypassed parent estimate overflow", heap)
				}
			} else if heap.err != nil || heap.bytes != math.MaxInt64 {
				test.Fatal("split registry did not charge each allocation at the exact parent limit", heap, expected)
			}
			if registry != before {
				test.Fatal("registry retained estimation mutated its owners or counters")
			}
		})
	}

	registry.clear()
	var released docRangeHeaderHeapEstimate
	registry.retained(&released)
	if released.err != nil || released.bytes != 0 || registry != (legacyCanonicalNumberRegistry{}) {
		test.Fatal("cleared split registry retained ownership or allocation overhead", released)
	}
	heap := docRangeHeaderHeapEstimate{bytes: math.MaxInt64}
	registry.retained(&heap)
	if heap.err != nil || heap.bytes != math.MaxInt64 {
		test.Fatal("released registry charged phantom allocation overhead", heap)
	}
}
