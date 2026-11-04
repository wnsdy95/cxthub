package domain

import (
	"errors"
	"fmt"
	"math"
	"testing"
	"unsafe"
)

type legacyCanonicalAppendPlanSnapshot[Value comparable] struct {
	header legacyCanonicalPages[Value]
	single Value
	inline [legacyCanonicalInlineValues]Value
	nodes  map[*legacyCanonicalPageNode[Value]]legacyCanonicalPageNode[Value]
	pages  map[*[legacyCanonicalPageValues]Value][legacyCanonicalPageValues]Value
}

func legacyCanonicalAppendPlanCapture[Value comparable](values *legacyCanonicalPages[Value]) legacyCanonicalAppendPlanSnapshot[Value] {
	snapshot := legacyCanonicalAppendPlanSnapshot[Value]{
		header: *values,
		nodes:  make(map[*legacyCanonicalPageNode[Value]]legacyCanonicalPageNode[Value]),
		pages:  make(map[*[legacyCanonicalPageValues]Value][legacyCanonicalPageValues]Value),
	}
	if values.single != nil {
		snapshot.single = *values.single
	}
	if values.inline != nil {
		snapshot.inline = *values.inline
	}
	var visit func(*legacyCanonicalPageNode[Value])
	visit = func(node *legacyCanonicalPageNode[Value]) {
		if node == nil {
			return
		}
		if _, seen := snapshot.nodes[node]; seen {
			return
		}
		snapshot.nodes[node] = *node
		if node.values != nil {
			snapshot.pages[node.values] = *node.values
		}
		for _, child := range node.children {
			visit(child)
		}
	}
	visit(values.root)
	return snapshot
}

func (snapshot legacyCanonicalAppendPlanSnapshot[Value]) unchanged(test *testing.T, values *legacyCanonicalPages[Value]) {
	test.Helper()
	if *values != snapshot.header {
		test.Fatal("append preview changed metadata or backing pointer identity")
	}
	if values.single != nil && *values.single != snapshot.single {
		test.Fatal("append preview changed the retained singleton slot")
	}
	if values.inline != nil && *values.inline != snapshot.inline {
		test.Fatal("append preview changed live or unused inline slots")
	}
	for node, original := range snapshot.nodes {
		if *node != original {
			test.Fatal("append preview changed a retained node or child/page pointer")
		}
	}
	for page, original := range snapshot.pages {
		if *page != original {
			test.Fatal("append preview changed live or unused paged slots")
		}
	}
}

func legacyCanonicalAppendPlanCheck[Value comparable](test *testing.T, values *legacyCanonicalPages[Value], value Value) (int, docRangeHeaderHeapEstimate) {
	test.Helper()
	before := legacyCanonicalAppendPlanCapture(values)
	visible := make([]Value, values.length())
	for position := range visible {
		stored := values.at(position)
		if stored == nil {
			test.Fatal("append fixture has a missing live slot", position)
		}
		visible[position] = *stored
	}
	work, forecast, err := values.appendRequirements()
	if err != nil || forecast.err != nil || forecast.bytes < 0 || work < 0 || work > LegacyCanonicalEncodingBytes {
		test.Fatal("valid append forecast failed", values.length(), work, forecast, err)
	}
	before.unchanged(test, values)
	wrapped, err := values.appendBytes()
	if err != nil || wrapped != work {
		test.Fatal("appendBytes disagrees with the shared work forecast", wrapped, work, err)
	}
	before.unchanged(test, values)
	var repeatedWork int
	var repeatedHeap docRangeHeaderHeapEstimate
	var repeatedErr error
	allocations := testing.AllocsPerRun(16, func() {
		repeatedWork, repeatedHeap, repeatedErr = values.appendRequirements()
	})
	if allocations != 0 || repeatedErr != nil || repeatedHeap.err != nil || repeatedWork != work || repeatedHeap.bytes != forecast.bytes {
		test.Fatal("append forecast allocated or changed across previews", allocations, repeatedWork, repeatedHeap, repeatedErr)
	}
	before.unchanged(test, values)
	if err := values.append(value); err != nil {
		test.Fatal("forecasted append failed", err)
	}
	if values.length() != len(visible)+1 {
		test.Fatal("forecasted append changed the wrong number of slots", values.length(), len(visible))
	}
	for position, original := range visible {
		if stored := values.at(position); stored == nil || *stored != original {
			test.Fatal("forecasted append changed a previous value", position)
		}
	}
	if stored := values.at(len(visible)); stored == nil || *stored != value {
		test.Fatal("forecasted append lost its new value")
	}
	owned, err := values.ownedBytes()
	independent := legacyCanonicalIndependentPageHeap(test, values)
	if err != nil || independent.err != nil || forecast.bytes != owned || forecast.bytes != independent.bytes {
		test.Fatal("post-append heap forecast differs from reachable physical allocations", forecast, owned, independent, err)
	}
	return work, forecast
}

func legacyCanonicalAppendPlanReject[Value comparable](test *testing.T, values *legacyCanonicalPages[Value], value Value) {
	test.Helper()
	before := legacyCanonicalAppendPlanCapture(values)
	work, _, err := values.appendRequirements()
	if work != 0 || !errors.Is(err, ErrIntegrity) {
		test.Fatal("invalid append forecast was admitted", work, err)
	}
	before.unchanged(test, values)
	wrapped, err := values.appendBytes()
	if wrapped != 0 || !errors.Is(err, ErrIntegrity) {
		test.Fatal("appendBytes did not preserve forecast rejection", wrapped, err)
	}
	before.unchanged(test, values)
	var repeatedWork int
	var repeatedErr error
	allocations := testing.AllocsPerRun(16, func() {
		repeatedWork, _, repeatedErr = values.appendRequirements()
	})
	if allocations != 0 || repeatedWork != 0 || !errors.Is(repeatedErr, ErrIntegrity) {
		test.Fatal("rejected forecast allocated or changed its result", allocations, repeatedWork, repeatedErr)
	}
	before.unchanged(test, values)
	if err := values.append(value); !errors.Is(err, ErrIntegrity) {
		test.Fatal("rejected forecast was followed by an admitted append", err)
	}
	before.unchanged(test, values)
}

func TestLegacyCanonicalPagesAppendRequirementsGrowth(test *testing.T) {
	var values legacyCanonicalPages[uint64]
	for _, count := range []int{0, 1, 2, 7, 8, 9, 63, 64, 65, 4095, 4096, 4097, 8191, 8192, 8193, 262143, 262144, 262145} {
		for values.length() < count {
			if err := values.append(uint64(values.length() + 1)); err != nil {
				test.Fatal("growth fixture setup failed", count, err)
			}
		}
		test.Run(fmt.Sprintf("count-%d", count), func(test *testing.T) {
			legacyCanonicalAppendPlanCheck(test, &values, uint64(count+1))
		})
	}
	if values.depth < 3 {
		test.Fatal("growth fixture did not exercise a deeper root promotion", values.depth)
	}
	test.Run("zero-width", func(test *testing.T) {
		var empty legacyCanonicalPages[struct{}]
		for _, count := range []int{0, 1, 2, 7, 8, 9, 63, 64, 65, 4095, 4096, 4097} {
			for empty.length() < count {
				if err := empty.append(struct{}{}); err != nil {
					test.Fatal("zero-width fixture setup failed", count, err)
				}
			}
			legacyCanonicalAppendPlanCheck(test, &empty, struct{}{})
		}
	})
}

func TestLegacyCanonicalPagesAppendRequirementsRetainedReuse(test *testing.T) {
	for _, capacity := range []int{1, 8, 9, 65, 4097, 8193} {
		test.Run(fmt.Sprintf("capacity-%d", capacity), func(test *testing.T) {
			var values legacyCanonicalPages[uint64]
			for position := 0; position < capacity; position++ {
				if err := values.append(uint64(position + 1)); err != nil {
					test.Fatal("reuse fixture setup failed", position, err)
				}
			}
			retained, err := values.ownedBytes()
			if err != nil {
				test.Fatal(err)
			}
			for _, count := range []int{4097, 4096, 4095, 65, 64, 63, 9, 8, 7, 2, 1, 0} {
				if count >= capacity {
					continue
				}
				for values.length() > count {
					if err := values.pop(); err != nil {
						test.Fatal("reuse fixture pop failed", count, err)
					}
				}
				test.Run(fmt.Sprintf("count-%d", count), func(test *testing.T) {
					before := values
					work, forecast := legacyCanonicalAppendPlanCheck(test, &values, uint64(count+100))
					if work != 0 || forecast.bytes != retained {
						test.Fatal("reused capacity was reallocated or omitted from the forecast", work, forecast, retained)
					}
					if values.single != before.single || values.inline != before.inline || values.root != before.root ||
						values.nodes != before.nodes || values.pages != before.pages || values.depth != before.depth {
						test.Fatal("reused append replaced retained physical storage")
					}
				})
			}
		})
	}
	test.Run("large-initial-inline", func(test *testing.T) {
		var values legacyCanonicalPages[[8192]byte]
		value := [8192]byte{3}
		value[len(value)-1] = 7
		for position := 0; position < legacyCanonicalInlineValues; position++ {
			work, _ := legacyCanonicalAppendPlanCheck(test, &values, value)
			want := 0
			if position == 0 {
				want = int(unsafe.Sizeof([legacyCanonicalInlineValues][8192]byte{}))
			}
			if work != want || values.single != nil || values.inline == nil || values.root != nil {
				test.Fatal("supported large value lost its initial-inline allocation path", position, work, want)
			}
		}
		legacyCanonicalAppendPlanReject(test, &values, value)
		retained, err := values.ownedBytes()
		if err != nil {
			test.Fatal(err)
		}
		for _, count := range []int{7, 2, 1, 0} {
			for values.length() > count {
				if err := values.pop(); err != nil {
					test.Fatal(err)
				}
			}
			work, forecast := legacyCanonicalAppendPlanCheck(test, &values, value)
			if work != 0 || forecast.bytes != retained {
				test.Fatal("large popped inline storage was not fully reused and charged", count, work, forecast, retained)
			}
		}
	})
}

func TestLegacyCanonicalPagesAppendRequirementsRejections(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	maximumDepth := (int(unsafe.Sizeof(uintptr(0)))*8 - 1) / legacyCanonicalPageShift
	fixtures := []struct {
		name   string
		values legacyCanonicalPages[uint64]
	}{
		{name: "negative-count", values: legacyCanonicalPages[uint64]{count: -1}},
		{name: "negative-nodes", values: legacyCanonicalPages[uint64]{nodes: -1}},
		{name: "negative-pages", values: legacyCanonicalPages[uint64]{pages: -1}},
		{name: "negative-depth", values: legacyCanonicalPages[uint64]{depth: -1}},
		{name: "excessive-depth", values: legacyCanonicalPages[uint64]{depth: maximumDepth + 1}},
		{name: "missing-storage", values: legacyCanonicalPages[uint64]{count: 1}},
		{name: "nodes-without-root", values: legacyCanonicalPages[uint64]{nodes: 1}},
		{name: "single-inline-overlap", values: legacyCanonicalPages[uint64]{single: new(uint64), inline: new([legacyCanonicalInlineValues]uint64), count: 1}},
		{name: "single-root-overlap", values: legacyCanonicalPages[uint64]{single: new(uint64), root: &legacyCanonicalPageNode[uint64]{}, count: 1, nodes: 1, pages: 1}},
		{name: "single-count-overflow", values: legacyCanonicalPages[uint64]{single: new(uint64), count: 2}},
		{name: "single-nodes", values: legacyCanonicalPages[uint64]{single: new(uint64), count: 1, nodes: 1}},
		{name: "single-pages", values: legacyCanonicalPages[uint64]{single: new(uint64), count: 1, pages: 1}},
		{name: "single-depth", values: legacyCanonicalPages[uint64]{single: new(uint64), count: 1, depth: 1}},
		{name: "inline-root-overlap", values: legacyCanonicalPages[uint64]{inline: new([legacyCanonicalInlineValues]uint64), root: &legacyCanonicalPageNode[uint64]{}, count: 2, nodes: 1, pages: 1}},
		{name: "inline-count-overflow", values: legacyCanonicalPages[uint64]{inline: new([legacyCanonicalInlineValues]uint64), count: legacyCanonicalInlineValues + 1}},
		{name: "root-without-nodes", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, pages: 1}},
		{name: "root-without-pages", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, nodes: 1}},
		{name: "pages-exceed-nodes", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, nodes: 1, pages: 2}},
		{name: "count-outside-tree", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, count: 65, nodes: 1, pages: 1}},
		{name: "count-overflow", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, count: maximum, nodes: 1, pages: 1, depth: maximumDepth}},
		{name: "node-counter-overflow", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, count: 64, nodes: maximum, pages: 1}},
		{name: "page-counter-overflow", values: legacyCanonicalPages[uint64]{root: &legacyCanonicalPageNode[uint64]{}, count: 9, nodes: maximum, pages: maximum}},
	}
	nodeCharge := int64(64) + 2*int64(unsafe.Sizeof(legacyCanonicalPageNode[uint64]{}))
	pageCharge := int64(64) + 2*int64(unsafe.Sizeof([legacyCanonicalPageValues]uint64{}))
	for _, overflow := range []struct {
		name  string
		count int64
		pages int
	}{
		{name: "node-heap-overflow", count: math.MaxInt64/nodeCharge + 1, pages: 1},
		{name: "combined-heap-overflow", count: math.MaxInt64/(nodeCharge+pageCharge) + 1},
	} {
		if overflow.count <= int64(maximum) {
			pages := overflow.pages
			if pages == 0 {
				pages = int(overflow.count)
			}
			fixtures = append(fixtures, struct {
				name   string
				values legacyCanonicalPages[uint64]
			}{
				name: overflow.name,
				values: legacyCanonicalPages[uint64]{
					root:  &legacyCanonicalPageNode[uint64]{values: new([legacyCanonicalPageValues]uint64)},
					count: 9,
					nodes: int(overflow.count),
					pages: pages,
				},
			})
		}
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			values := fixture.values
			if values.single != nil {
				*values.single = 73
			}
			if values.inline != nil {
				values.inline[0], values.inline[legacyCanonicalInlineValues-1] = 89, 97
			}
			if values.root != nil && values.root.values != nil {
				values.root.values[0], values.root.values[legacyCanonicalPageValues-1] = 101, 103
			}
			legacyCanonicalAppendPlanReject(test, &values, uint64(107))
		})
	}
	test.Run("oversized-first-inline", func(test *testing.T) {
		var values legacyCanonicalPages[[8193]byte]
		legacyCanonicalAppendPlanReject(test, &values, [8193]byte{5})
	})
	test.Run("unsupported-single-promotion", func(test *testing.T) {
		stored := [8192]byte{7}
		values := legacyCanonicalPages[[8192]byte]{single: &stored, count: 1}
		legacyCanonicalAppendPlanReject(test, &values, [8192]byte{11})
	})
	test.Run("page-at-cap-plus-node-and-copy", func(test *testing.T) {
		var values legacyCanonicalPages[[1024]byte]
		for position := 0; position < legacyCanonicalInlineValues; position++ {
			if err := values.append([1024]byte{byte(position + 1)}); err != nil {
				test.Fatal(err)
			}
		}
		legacyCanonicalAppendPlanReject(test, &values, [1024]byte{13})
	})
}
