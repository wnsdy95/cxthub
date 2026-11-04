package domain

import (
	"errors"
	"testing"
	"unsafe"
)

type legacyCanonicalSingletonTestValue struct {
	ordinal   int
	reference *int
	text      string
}

func legacyCanonicalSingletonStorage[Value any](test testing.TB, values *legacyCanonicalPages[Value]) docRangeHeaderHeapEstimate {
	test.Helper()
	actual := legacyCanonicalIndependentPageHeap(test, values)
	var retained docRangeHeaderHeapEstimate
	values.retained(&retained)
	owned, err := values.ownedBytes()
	if !values.valid() || actual.err != nil || retained.err != nil || err != nil || retained.bytes != actual.bytes || owned != actual.bytes {
		test.Fatal("singleton storage differs from independent reachable allocations", actual, retained, owned, err)
	}
	if values.length() != values.count || values.at(-1) != nil || values.at(values.length()) != nil {
		test.Fatal("singleton storage changed length or out-of-range access", values.length(), values.count)
	}
	return actual
}

func TestLegacyCanonicalSingletonPagesPromotionAllocationAndCopy(test *testing.T) {
	var values legacyCanonicalPages[legacyCanonicalSingletonTestValue]
	firstReference := 100
	expected := []legacyCanonicalSingletonTestValue{{ordinal: 0, reference: &firstReference, text: "first"}}
	if err := values.append(expected[0]); err != nil {
		test.Fatal(err)
	}
	if values.single == nil || values.inline != nil || values.root != nil || values.count != 1 || values.nodes != 0 || values.pages != 0 || values.depth != 0 || values.at(0) != values.single {
		test.Fatal("first value did not use exclusive singleton storage")
	}
	legacyCanonicalSingletonStorage(test, &values)
	singleAddress := values.single
	var inlineAddress, pageAddress *legacyCanonicalSingletonTestValue
	valueBytes := int(unsafe.Sizeof(*values.single))
	inlineBytes := int(unsafe.Sizeof([legacyCanonicalInlineValues]legacyCanonicalSingletonTestValue{}))
	nodeBytes := int(unsafe.Sizeof(legacyCanonicalPageNode[legacyCanonicalSingletonTestValue]{}))
	pageBytes := int(unsafe.Sizeof([legacyCanonicalPageValues]legacyCanonicalSingletonTestValue{}))
	for position := 1; position <= legacyCanonicalPageValues; position++ {
		before := values
		planned, err := values.appendBytes()
		want := 0
		switch position {
		case 1:
			want = inlineBytes + valueBytes
		case legacyCanonicalInlineValues:
			want = nodeBytes + pageBytes + inlineBytes
		case legacyCanonicalPageValues:
			want = 2*nodeBytes + pageBytes
		}
		if err != nil || planned != want || planned < 0 || planned > 64<<10 || values != before {
			test.Fatal("append reservation omitted actual allocation or promotion copy work", position, planned, want, err)
		}
		if repeated, err := values.appendBytes(); err != nil || repeated != planned || values != before {
			test.Fatal("append reservation changed on a read-only retry", position, repeated, planned, err)
		}
		reference := position + 100
		value := legacyCanonicalSingletonTestValue{ordinal: position, reference: &reference, text: "retained"}
		expected = append(expected, value)
		if err := values.append(value); err != nil {
			test.Fatal("bounded singleton promotion failed", position, err)
		}
		legacyCanonicalSingletonStorage(test, &values)
		actualWork := (values.nodes-before.nodes)*nodeBytes + (values.pages-before.pages)*pageBytes
		if before.inline == nil && values.inline != nil {
			actualWork += int(unsafe.Sizeof(*values.inline))
		}
		if before.single != nil && values.inline != nil {
			actualWork += int(unsafe.Sizeof(*before.single))
		}
		if before.inline != nil && values.root != nil {
			actualWork += int(unsafe.Sizeof(*before.inline))
		}
		if planned != actualWork || values.length() != position+1 || values.single != nil {
			test.Fatal("actual storage transitions disagree with reserved work or length", position, planned, actualWork, values.length())
		}
		if position < legacyCanonicalInlineValues {
			if values.inline == nil || values.root != nil || values.nodes != 0 || values.pages != 0 || values.depth != 0 {
				test.Fatal("singleton promotion did not use only the original inline array", position)
			}
			if position == 1 {
				inlineAddress = values.at(0)
				if inlineAddress == singleAddress || inlineAddress != &values.inline[0] {
					test.Fatal("singleton was not copied into independent inline storage")
				}
			} else if values.at(0) != inlineAddress {
				test.Fatal("inline append moved an existing value", position)
			}
		} else {
			if values.inline != nil || values.root == nil {
				test.Fatal("page promotion retained obsolete small-list storage", position)
			}
			if position == legacyCanonicalInlineValues {
				pageAddress = values.at(0)
				if values.nodes != 1 || values.pages != 1 || values.depth != 0 || values.root.values == nil || pageAddress != &values.root.values[0] || pageAddress == inlineAddress {
					test.Fatal("inline promotion did not allocate one independent fixed value page")
				}
			} else if values.at(0) != pageAddress {
				test.Fatal("tree growth moved an existing page value", position)
			}
		}
		for storedPosition, wantValue := range expected {
			stored := values.at(storedPosition)
			if stored == nil || *stored != wantValue {
				test.Fatal("promotion lost value order or original references", position, storedPosition, stored, wantValue)
			}
		}
	}
	if values.nodes != 3 || values.pages != 2 || values.depth != 1 || values.root.children[0] == nil || values.root.children[1] == nil {
		test.Fatal("the sixty-fifth value did not retain two pages under one tree root")
	}
	*values.at(0), *values.at(legacyCanonicalPageValues) = *values.at(legacyCanonicalPageValues), *values.at(0)
	if *values.at(0) != expected[legacyCanonicalPageValues] || *values.at(legacyCanonicalPageValues) != expected[0] {
		test.Fatal("mutable cross-page access changed after singleton promotion")
	}
	values.clear()
	if values != (legacyCanonicalPages[legacyCanonicalSingletonTestValue]{}) || legacyCanonicalSingletonStorage(test, &values).bytes != 0 {
		test.Fatal("clear retained promoted storage")
	}
}

func TestLegacyCanonicalSingletonPagesPopZeroReuseAndClear(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		count int
	}{
		{name: "single", count: 1},
		{name: "inline", count: legacyCanonicalInlineValues},
		{name: "tree", count: legacyCanonicalPageValues + 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var values legacyCanonicalPages[legacyCanonicalSingletonTestValue]
			for position := 0; position < fixture.count; position++ {
				reference := position + 100
				if err := values.append(legacyCanonicalSingletonTestValue{ordinal: position, reference: &reference, text: "release"}); err != nil {
					test.Fatal(err)
				}
			}
			retained := legacyCanonicalSingletonStorage(test, &values)
			backing := values
			addresses := make([]*legacyCanonicalSingletonTestValue, fixture.count)
			for position := range addresses {
				addresses[position] = values.at(position)
			}
			for remaining := fixture.count; remaining > 0; remaining-- {
				last := addresses[remaining-1]
				if err := values.pop(); err != nil || *last != (legacyCanonicalSingletonTestValue{}) || values.length() != remaining-1 || values.at(remaining-1) != nil {
					test.Fatal("pop did not zero released references or hide the last slot", remaining, err)
				}
				if actual := legacyCanonicalSingletonStorage(test, &values); actual.bytes != retained.bytes {
					test.Fatal("pop discarded or failed to charge retained capacity", remaining, actual, retained)
				}
			}
			backing.count = 0
			if values != backing {
				test.Fatal("popping to zero changed reusable storage or its allocation counters")
			}
			if err := values.pop(); !errors.Is(err, ErrIntegrity) || values != backing {
				test.Fatal("empty pop changed retained storage", err)
			}
			for position := 0; position < fixture.count; position++ {
				before := values
				if planned, err := values.appendBytes(); err != nil || planned != 0 || values != before {
					test.Fatal("reused capacity requested a new allocation or copy", position, planned, err)
				}
				reference := position + 200
				value := legacyCanonicalSingletonTestValue{ordinal: -position, reference: &reference, text: "reused"}
				if err := values.append(value); err != nil || values.at(position) != addresses[position] || *values.at(position) != value {
					test.Fatal("reuse moved retained storage or exposed a stale value", position, err)
				}
				if actual := legacyCanonicalSingletonStorage(test, &values); actual.bytes != retained.bytes {
					test.Fatal("reused storage changed its independent retained ledger", position, actual, retained)
				}
			}
			values.clear()
			if values != (legacyCanonicalPages[legacyCanonicalSingletonTestValue]{}) || legacyCanonicalSingletonStorage(test, &values).bytes != 0 {
				test.Fatal("clear did not release all small-list or tree storage")
			}
			if err := values.append(legacyCanonicalSingletonTestValue{text: "restart"}); err != nil || values.single == nil || values.inline != nil || values.root != nil {
				test.Fatal("cleared storage did not restart with an exclusive singleton", err)
			}
			legacyCanonicalSingletonStorage(test, &values)
		})
	}
}

func TestLegacyCanonicalSingletonPagesRejectOverlappingAndOverflowStates(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	maximumDepth := (int(unsafe.Sizeof(uintptr(0)))*8 - 1) / legacyCanonicalPageShift
	for _, fixture := range []struct {
		name   string
		values legacyCanonicalPages[int]
	}{
		{name: "single-inline", values: legacyCanonicalPages[int]{single: new(int), inline: new([legacyCanonicalInlineValues]int), count: 1}},
		{name: "empty-single-inline", values: legacyCanonicalPages[int]{single: new(int), inline: new([legacyCanonicalInlineValues]int)}},
		{name: "single-tree", values: legacyCanonicalPages[int]{single: new(int), root: &legacyCanonicalPageNode[int]{values: new([legacyCanonicalPageValues]int)}, count: 1, nodes: 1, pages: 1}},
		{name: "empty-single-tree", values: legacyCanonicalPages[int]{single: new(int), root: &legacyCanonicalPageNode[int]{values: new([legacyCanonicalPageValues]int)}, nodes: 1, pages: 1}},
		{name: "all-storage", values: legacyCanonicalPages[int]{single: new(int), inline: new([legacyCanonicalInlineValues]int), root: &legacyCanonicalPageNode[int]{values: new([legacyCanonicalPageValues]int)}, count: 1, nodes: 1, pages: 1}},
		{name: "single-two-values", values: legacyCanonicalPages[int]{single: new(int), count: 2}},
		{name: "single-negative-count", values: legacyCanonicalPages[int]{single: new(int), count: -1}},
		{name: "single-count-overflow", values: legacyCanonicalPages[int]{single: new(int), count: maximum}},
		{name: "single-node-metadata", values: legacyCanonicalPages[int]{single: new(int), count: 1, nodes: 1}},
		{name: "single-page-metadata", values: legacyCanonicalPages[int]{single: new(int), count: 1, pages: 1}},
		{name: "single-depth-metadata", values: legacyCanonicalPages[int]{single: new(int), count: 1, depth: 1}},
		{name: "single-node-overflow", values: legacyCanonicalPages[int]{single: new(int), nodes: maximum}},
		{name: "single-page-overflow", values: legacyCanonicalPages[int]{single: new(int), pages: maximum}},
		{name: "single-depth-overflow", values: legacyCanonicalPages[int]{single: new(int), depth: maximumDepth + 1}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			values := fixture.values
			*values.single = 41
			if values.inline != nil {
				values.inline[0] = 42
			}
			if values.root != nil {
				values.root.values[0] = 43
			}
			before := values
			if values.valid() {
				test.Fatal("overlapping or overflowing singleton metadata was accepted")
			}
			if planned, err := values.appendBytes(); planned != 0 || !errors.Is(err, ErrIntegrity) || values != before {
				test.Fatal("malformed singleton append reservation was admitted or mutated state", planned, err)
			}
			if err := values.append(99); !errors.Is(err, ErrIntegrity) || values != before || *values.single != 41 || values.inline != nil && values.inline[0] != 42 || values.root != nil && values.root.values[0] != 43 {
				test.Fatal("malformed singleton append changed original backing values", err)
			}
			var retained docRangeHeaderHeapEstimate
			values.retained(&retained)
			if owned, err := values.ownedBytes(); !errors.Is(err, ErrIntegrity) || !errors.Is(retained.err, ErrIntegrity) || owned != 0 || retained.bytes != 0 {
				test.Fatal("malformed singleton storage escaped retained accounting", owned, err, retained)
			}
			values.clear()
			if values != (legacyCanonicalPages[int]{}) || legacyCanonicalSingletonStorage(test, &values).bytes != 0 {
				test.Fatal("clear retained malformed singleton storage")
			}
		})
	}
}

func TestLegacyCanonicalSingletonPagesRejectPromotedArithmeticOverflow(test *testing.T) {
	var promoted legacyCanonicalPages[int]
	for position := 0; position < legacyCanonicalPageValues; position++ {
		if err := promoted.append(position + 1); err != nil {
			test.Fatal(err)
		}
	}
	legacyCanonicalSingletonStorage(test, &promoted)
	originalPage := *promoted.root.values
	maximum := int(^uint(0) >> 1)
	maximumDepth := (int(unsafe.Sizeof(uintptr(0)))*8 - 1) / legacyCanonicalPageShift
	for _, fixture := range []struct {
		name        string
		count       int
		nodes       int
		pages       int
		depth       int
		missingPage bool
	}{
		{name: "count-increment", count: maximum, nodes: 1, pages: 1, depth: maximumDepth},
		{name: "node-addition", count: legacyCanonicalPageValues, nodes: maximum, pages: 1},
		{name: "page-addition", count: legacyCanonicalInlineValues + 1, nodes: maximum, pages: maximum, missingPage: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			values := promoted
			values.count, values.nodes, values.pages, values.depth = fixture.count, fixture.nodes, fixture.pages, fixture.depth
			if fixture.missingPage {
				values.root = &legacyCanonicalPageNode[int]{}
			}
			if !values.valid() {
				test.Fatal("overflow fixture did not reach the append arithmetic guards")
			}
			before, rootBefore := values, *values.root
			if planned, err := values.appendBytes(); planned != 0 || !errors.Is(err, ErrIntegrity) || values != before || *values.root != rootBefore {
				test.Fatal("promoted append reservation overflowed or mutated storage", planned, err)
			}
			if err := values.append(maximum); !errors.Is(err, ErrIntegrity) || values != before || *values.root != rootBefore || *promoted.root.values != originalPage {
				test.Fatal("rejected promoted append changed its original value page", err)
			}
			values.clear()
			if values != (legacyCanonicalPages[int]{}) || legacyCanonicalSingletonStorage(test, &values).bytes != 0 {
				test.Fatal("clear retained overflowing promoted storage")
			}
		})
	}
}

func legacyCanonicalSingletonLargeValueBoundary[Value comparable](test *testing.T, value Value, wantSingle, wantPage bool) {
	test.Helper()
	var values legacyCanonicalPages[Value]
	valueBytes := int(unsafe.Sizeof(value))
	inlineBytes := int(unsafe.Sizeof([legacyCanonicalInlineValues]Value{}))
	pageBytes := int(unsafe.Sizeof([legacyCanonicalPageValues]Value{}))
	nodeBytes := int(unsafe.Sizeof(legacyCanonicalPageNode[Value]{}))
	if inlineBytes > 64<<10 || wantSingle != (inlineBytes+valueBytes <= 64<<10) || wantPage != (nodeBytes+pageBytes+inlineBytes <= 64<<10) {
		test.Fatal("large-value fixture does not straddle the actual allocation/copy boundary", valueBytes, inlineBytes, pageBytes, nodeBytes, wantSingle, wantPage)
	}
	firstBytes := inlineBytes
	if wantSingle {
		firstBytes = valueBytes
	}
	if planned, err := values.appendBytes(); err != nil || planned != firstBytes || values != (legacyCanonicalPages[Value]{}) {
		test.Fatal("large-value first allocation changed the original support boundary", planned, firstBytes, err)
	}
	for position := 0; position < legacyCanonicalInlineValues; position++ {
		if position != 0 {
			before := values
			want := 0
			if position == 1 && wantSingle {
				want = inlineBytes + valueBytes
			}
			if planned, err := values.appendBytes(); err != nil || planned != want || planned > 64<<10 || values != before {
				test.Fatal("large-value promotion omitted initialization or its one-value copy", position, planned, want, err)
			}
		}
		if err := values.append(value); err != nil || values.at(position) == nil || *values.at(position) != value || values.length() != position+1 {
			test.Fatal("formerly supported large inline value was rejected or changed", position, err)
		}
		if position == 0 && ((values.single != nil) != wantSingle || (values.inline != nil) == wantSingle || values.root != nil) {
			test.Fatal("large-value first append chose an unsupported singleton promotion path", wantSingle)
		}
		if position != 0 && (values.single != nil || values.inline == nil || values.root != nil) {
			test.Fatal("large-value append did not retain exclusive original inline storage", position)
		}
		legacyCanonicalSingletonStorage(test, &values)
	}
	before := values
	inlineSnapshot := *values.inline
	planned, err := values.appendBytes()
	if wantPage {
		want := nodeBytes + pageBytes + inlineBytes
		if err != nil || planned != want || planned > 64<<10 || values != before {
			test.Fatal("bounded large-value page promotion lost allocation or copy work", planned, want, err)
		}
		if err := values.append(value); err != nil || values.single != nil || values.inline != nil || values.root == nil || values.nodes != 1 || values.pages != 1 || values.depth != 0 || values.length() != legacyCanonicalInlineValues+1 {
			test.Fatal("bounded large-value page promotion changed the original topology", err)
		}
	} else {
		if planned != 0 || !errors.Is(err, ErrIntegrity) || values != before || *values.inline != inlineSnapshot {
			test.Fatal("oversized large-value page reservation was admitted or changed inline values", planned, err)
		}
		if err := values.append(value); !errors.Is(err, ErrIntegrity) || values != before || *values.inline != inlineSnapshot {
			test.Fatal("oversized large-value page promotion mutated supported inline storage", err)
		}
	}
	for position := 0; position < values.length(); position++ {
		if stored := values.at(position); stored == nil || *stored != value {
			test.Fatal("large-value promotion changed an original value", position)
		}
	}
	retained := legacyCanonicalSingletonStorage(test, &values)
	var empty Value
	for values.length() != 0 {
		last := values.at(values.length() - 1)
		if err := values.pop(); err != nil || *last != empty {
			test.Fatal("large-value pop did not zero retained storage", err)
		}
	}
	if actual := legacyCanonicalSingletonStorage(test, &values); actual.bytes != retained.bytes {
		test.Fatal("large-value count-zero storage lost its retained capacity", actual, retained)
	}
	if planned, err := values.appendBytes(); err != nil || planned != 0 {
		test.Fatal("large-value count-zero storage could not reuse its admitted capacity", planned, err)
	}
	if err := values.append(value); err != nil || values.at(0) == nil || *values.at(0) != value {
		test.Fatal("large-value count-zero reuse failed", err)
	}
	legacyCanonicalSingletonStorage(test, &values)
	values.clear()
	if values != (legacyCanonicalPages[Value]{}) || legacyCanonicalSingletonStorage(test, &values).bytes != 0 {
		test.Fatal("clear retained large-value backing allocations")
	}
}

func TestLegacyCanonicalSingletonPagesPreserveLargeValueSupport(test *testing.T) {
	const singletonWidth = (64 << 10) / (legacyCanonicalInlineValues + 1)
	const pageWidth = ((64 << 10) - int(unsafe.Sizeof(legacyCanonicalPageNode[byte]{}))) / (legacyCanonicalPageValues + legacyCanonicalInlineValues)
	test.Run("singleton-promotion-at-cap", func(test *testing.T) {
		legacyCanonicalSingletonLargeValueBoundary(test, [singletonWidth]byte{1}, true, false)
	})
	test.Run("one-copy-over-cap-keeps-inline", func(test *testing.T) {
		legacyCanonicalSingletonLargeValueBoundary(test, [singletonWidth + 1]byte{2}, false, false)
	})
	test.Run("original-inline-at-cap", func(test *testing.T) {
		legacyCanonicalSingletonLargeValueBoundary(test, [8192]byte{3}, false, false)
	})
	test.Run("page-allocation-and-copy-at-cap", func(test *testing.T) {
		legacyCanonicalSingletonLargeValueBoundary(test, [pageWidth]byte{4}, true, true)
	})
	test.Run("page-copy-over-cap", func(test *testing.T) {
		legacyCanonicalSingletonLargeValueBoundary(test, [pageWidth + 1]byte{5}, true, false)
	})
	test.Run("original-inline-over-cap-still-rejected", func(test *testing.T) {
		var values legacyCanonicalPages[[8193]byte]
		if unsafe.Sizeof([legacyCanonicalInlineValues][8193]byte{}) <= 64<<10 {
			test.Fatal("overlarge fixture no longer exceeds the original inline allocation cap")
		}
		if planned, err := values.appendBytes(); planned != 0 || !errors.Is(err, ErrIntegrity) || values != (legacyCanonicalPages[[8193]byte]{}) {
			test.Fatal("singleton storage expanded the original overlarge-type support boundary", planned, err)
		}
		if err := values.append([8193]byte{6}); !errors.Is(err, ErrIntegrity) || values != (legacyCanonicalPages[[8193]byte]{}) {
			test.Fatal("rejected overlarge first append allocated storage", err)
		}
		legacyCanonicalSingletonStorage(test, &values)
	})
}
