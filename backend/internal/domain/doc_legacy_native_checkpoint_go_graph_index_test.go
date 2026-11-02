package domain

import (
	"context"
	"errors"
	"math/bits"
	"testing"
	"unsafe"
)

func legacyNativeGoGraphSparseInsertion(test *testing.T, registry *legacyNativeCheckpointGoGraphRegistry, key legacyNativeCheckpointGoGraphKey) *legacyNativeCheckpointGoGraph {
	test.Helper()
	previous, err := registry.lookup(key)
	if err != nil || previous != nil && previous.key == key {
		test.Fatal("invalid sparse insertion fixture", previous, key, err)
	}
	entry := &legacyNativeCheckpointGoGraphEntry{key: key}
	split := -1
	if previous != nil {
		split = key.difference(previous.key)
	}
	if registry.last == nil {
		registry.first, registry.root.entry = entry, entry
	} else {
		registry.last.next = entry
	}
	registry.last = entry
	registry.entries++
	heap := legacyNativeCheckpointGoGraphFrameHeap()
	frame := &legacyNativeCheckpointGoGraphFrame{key: key, entry: entry, phase: legacyNativeCheckpointGoGraphIndex,
		indexNode: &registry.root, indexLevel: split, indexLimit: legacyNativeCheckpointGoGraphKeyBits, charge: heap.bytes}
	work := &legacyNativeCheckpointGoGraph{registry: registry, frame: frame, depth: 1, frameBytes: heap.bytes, budget: legacyDecoderCheckpointBudget}
	registry.active = work
	return work
}

func legacyNativeGoGraphSparseFinish(test *testing.T, work *legacyNativeCheckpointGoGraph) {
	test.Helper()
	for attempt := 0; attempt <= legacyNativeCheckpointGoGraphKeyBits; attempt++ {
		if ready, err := work.step(context.Background()); ready || err != nil {
			test.Fatal("sparse index failed before processing source values", ready, err)
		}
		if work.frame.phase == legacyNativeCheckpointGoGraphNext {
			if work.frame.indexNode != nil {
				test.Fatal("completed index retained its insertion cursor")
			}
			return
		}
	}
	test.Fatal("sparse index exceeded its finite key depth")
}

func legacyNativeGoGraphSparseAudit(test *testing.T, registry *legacyNativeCheckpointGoGraphRegistry) {
	test.Helper()
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(*registry)))
	seen := make(map[*legacyNativeCheckpointGoGraphEntry]bool)
	var nodes int64
	var visit func(legacyNativeCheckpointGoGraphLink, int)
	visit = func(link legacyNativeCheckpointGoGraphLink, limit int) {
		if link.branch != nil {
			if link.entry != nil || link.branch.bit < 0 || link.branch.bit >= limit {
				test.Fatal("invalid sparse discriminator", link, limit)
			}
			nodes++
			expected.allocation(1, int64(unsafe.Sizeof(*link.branch)))
			for _, child := range link.branch.children {
				visit(child, link.branch.bit)
			}
			return
		}
		if link.entry == nil || seen[link.entry] {
			test.Fatal("sparse tree lost or duplicated a key", link.entry)
		}
		seen[link.entry] = true
		expected.allocation(1, int64(unsafe.Sizeof(*link.entry)))
		found, err := registry.lookup(link.entry.key)
		if err != nil || found != link.entry {
			test.Fatal("sparse branch routes a key to another entry", link.entry.key, err)
		}
	}
	visit(registry.root, legacyNativeCheckpointGoGraphKeyBits)
	retained, err := registry.retainedBytes()
	if nodes != registry.nodes || nodes != registry.entries-1 || int64(len(seen)) != registry.entries || expected.err != nil || err != nil || retained != expected.bytes {
		test.Fatal("sparse registry ledger differs from actual allocations", nodes, registry.nodes, registry.entries, len(seen), retained, expected, err)
	}
}

func TestLegacyNativeGoGraphSparseIdentityAndAccounting(test *testing.T) {
	registry := newLegacyNativeCheckpointGoGraphRegistry()
	defer registry.close()
	keys := []legacyNativeCheckpointGoGraphKey{
		{address: 8, length: 1, capacity: 8, kind: legacyNativeCheckpointArray},
		{address: 12, length: 1, capacity: 8, kind: legacyNativeCheckpointArray},
		{address: 10, length: 1, capacity: 8, kind: legacyNativeCheckpointArray},
		{address: 9, length: 1, capacity: 8, kind: legacyNativeCheckpointArray},
		{address: 8, length: 2, capacity: 8, kind: legacyNativeCheckpointArray},
		{address: 8, length: 1, capacity: 16, kind: legacyNativeCheckpointArray},
		{address: 8, length: 1, capacity: 8, kind: legacyNativeCheckpointObject},
		{address: 8, length: 1, capacity: 8, kind: 128},
		{address: ^uintptr(0), length: int(^uint(0) >> 1), capacity: int(^uint(0) >> 1), kind: 255},
	}
	for _, key := range keys {
		work := legacyNativeGoGraphSparseInsertion(test, registry, key)
		legacyNativeGoGraphSparseFinish(test, work)
		legacyNativeGoGraphSparseAudit(test, registry)
	}
	missing := legacyNativeCheckpointGoGraphKey{address: 11, length: 1, capacity: 8, kind: legacyNativeCheckpointArray}
	found, err := registry.lookup(missing)
	if err != nil || found == nil || found.key == missing || missing.difference(missing) != -1 {
		test.Fatal("sparse candidate was confused with an exact match", found, err)
	}
}

func TestLegacyNativeGoGraphSparseStepBound(test *testing.T) {
	registry := newLegacyNativeCheckpointGoGraphRegistry()
	defer registry.close()
	legacyNativeGoGraphSparseFinish(test, legacyNativeGoGraphSparseInsertion(test, registry, legacyNativeCheckpointGoGraphKey{kind: 1}))
	for position := 0; position < bits.UintSize-1; position++ {
		key := legacyNativeCheckpointGoGraphKey{address: uintptr(1) << uint(position), kind: 1}
		legacyNativeGoGraphSparseFinish(test, legacyNativeGoGraphSparseInsertion(test, registry, key))
	}
	work := legacyNativeGoGraphSparseInsertion(test, registry, legacyNativeCheckpointGoGraphKey{address: 3, kind: 1})
	beforeNodes := registry.nodes
	if ready, err := work.step(context.Background()); ready || err != nil || work.frame.phase != legacyNativeCheckpointGoGraphIndex || registry.nodes != beforeNodes {
		test.Fatal("one index step drained a deep insertion", ready, err, work.frame.phase, registry.nodes)
	}
	expectedLimit := bits.UintSize - 1 - legacyNativeCheckpointGoGraphIndexActions
	if work.frame.indexLimit != expectedLimit {
		test.Fatal("outer step repeated the bounded index helper", work.frame.indexLimit, expectedLimit)
	}
	legacyNativeGoGraphSparseFinish(test, work)
	legacyNativeGoGraphSparseAudit(test, registry)
}

func TestLegacyNativeGoGraphSparseAdmissionAndCorruption(test *testing.T) {
	for _, scenario := range []string{"budget", "cancel", "cycle", "mixed-link", "empty-child"} {
		test.Run(scenario, func(test *testing.T) {
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			legacyNativeGoGraphSparseFinish(test, legacyNativeGoGraphSparseInsertion(test, registry, legacyNativeCheckpointGoGraphKey{address: 1, kind: 1}))
			legacyNativeGoGraphSparseFinish(test, legacyNativeGoGraphSparseInsertion(test, registry, legacyNativeCheckpointGoGraphKey{address: 2, kind: 1}))
			if scenario == "budget" || scenario == "cancel" {
				work := legacyNativeGoGraphSparseInsertion(test, registry, legacyNativeCheckpointGoGraphKey{address: 4, kind: 1})
				before, nodes := registry.root, registry.nodes
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				expected := error(context.Canceled)
				if scenario == "cancel" {
					cancel()
				} else {
					own, ownErr := work.retainedBytes()
					shared, sharedErr := registry.retainedBytes()
					if ownErr != nil || sharedErr != nil {
						test.Fatal(ownErr, sharedErr)
					}
					work.budget, expected = own+shared, ErrDocumentResourceLimit
				}
				if err := work.index(ctx, work.frame); !errors.Is(err, expected) || registry.root != before || registry.nodes != nodes || work.frame.phase != legacyNativeCheckpointGoGraphIndex {
					test.Fatal("rejected index insertion modified the tree", err, expected)
				}
				return
			}
			key := legacyNativeCheckpointGoGraphKey{address: 1, kind: 1}
			position := key.bit(registry.root.branch.bit)
			switch scenario {
			case "cycle":
				registry.root.branch.children[position] = registry.root
			case "mixed-link":
				registry.root.entry = registry.first
			case "empty-child":
				registry.root.branch.children[1-position] = legacyNativeCheckpointGoGraphLink{}
			}
			if _, err := registry.lookup(key); !errors.Is(err, ErrIntegrity) {
				test.Fatal("malformed sparse index was not rejected", err)
			}
		})
	}
}
