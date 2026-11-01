package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyCanonicalPagedPreparationShape struct {
	single bool
	inline bool
	nodes  int
	pages  int
}

func legacyCanonicalPagedPreparationStorage[Value any](test *testing.T, values *legacyCanonicalPages[Value]) legacyCanonicalPagedPreparationShape {
	test.Helper()
	shape := legacyCanonicalPagedPreparationShape{single: values.single != nil, inline: values.inline != nil}
	if shape.single {
		if shape.inline || values.root != nil || values.length() > 1 || values.length() == 1 && values.at(0) != values.single {
			test.Fatal("single storage overlaps other backing or lost its actual value address")
		}
		var empty Value
		if values.length() == 0 && !reflect.DeepEqual(*values.single, empty) {
			test.Fatal("popped single storage retained a released value")
		}
	}
	seenNodes := make(map[*legacyCanonicalPageNode[Value]]bool)
	seenPages := make(map[*[legacyCanonicalPageValues]Value]bool)
	var visit func(*legacyCanonicalPageNode[Value], int)
	visit = func(node *legacyCanonicalPageNode[Value], level int) {
		if node == nil || seenNodes[node] || level < 0 {
			test.Fatal("actual page storage contains a missing, aliased, or misplaced node", level)
		}
		seenNodes[node] = true
		shape.nodes++
		if node.values != nil {
			if level != 0 || unsafe.Sizeof(*node.values) != 0 && seenPages[node.values] {
				test.Fatal("actual page storage contains a misplaced or aliased value page", level)
			}
			seenPages[node.values] = true
			shape.pages++
		} else if level == 0 {
			test.Fatal("actual page storage has a leaf without fixed value storage")
		}
		for _, child := range node.children {
			if child != nil {
				visit(child, level-1)
			}
		}
	}
	if values.root != nil {
		if shape.single || shape.inline {
			test.Fatal("promoted page storage retained obsolete single or inline backing")
		}
		visit(values.root, values.depth)
	}
	if shape.nodes != values.nodes || shape.pages != values.pages {
		test.Fatal("page counters differ from actual reachable allocations", shape, values.nodes, values.pages)
	}
	want := int64(0)
	if shape.single && unsafe.Sizeof(*values.single) != 0 {
		want += 64 + 2*int64(unsafe.Sizeof(*values.single))
	}
	if shape.inline && unsafe.Sizeof([legacyCanonicalInlineValues]Value{}) != 0 {
		want += 64 + 2*int64(unsafe.Sizeof([legacyCanonicalInlineValues]Value{}))
	}
	if shape.nodes != 0 {
		want += int64(shape.nodes) * (64 + 2*int64(unsafe.Sizeof(legacyCanonicalPageNode[Value]{})))
	}
	if shape.pages != 0 && unsafe.Sizeof([legacyCanonicalPageValues]Value{}) != 0 {
		want += int64(shape.pages) * (64 + 2*int64(unsafe.Sizeof([legacyCanonicalPageValues]Value{})))
	}
	owned, err := values.ownedBytes()
	var retained docRangeHeaderHeapEstimate
	values.retained(&retained)
	if err != nil || retained.err != nil || owned != want || retained.bytes != want {
		test.Fatal("actual fixed arrays or nodes escaped the charged storage ledger", shape, owned, retained.bytes, want, err, retained.err)
	}
	if values.at(-1) != nil || values.at(values.length()) != nil {
		test.Fatal("page access admitted an out-of-range position", values.length())
	}
	return shape
}

func legacyCanonicalPagedPreparationAllocated[Value any](test *testing.T, before, after legacyCanonicalPagedPreparationShape) int {
	test.Helper()
	if after.nodes < before.nodes || after.pages < before.pages {
		test.Fatal("pending preparation unexpectedly discarded fixed page storage", before, after)
	}
	allocation := (after.nodes-before.nodes)*int(unsafe.Sizeof(legacyCanonicalPageNode[Value]{})) +
		(after.pages-before.pages)*int(unsafe.Sizeof([legacyCanonicalPageValues]Value{}))
	valueBytes := int(unsafe.Sizeof(*new(Value)))
	inlineBytes := int(unsafe.Sizeof([legacyCanonicalInlineValues]Value{}))
	if before.nodes == 0 {
		if !before.single && !before.inline && (after.single || after.inline || after.nodes != 0) &&
			inlineBytes <= LegacyCanonicalEncodingBytes-valueBytes {
			allocation += valueBytes
		}
		if !before.inline && (after.inline || after.nodes != 0) {
			allocation += inlineBytes
			if inlineBytes <= LegacyCanonicalEncodingBytes-valueBytes {
				allocation += valueBytes
			}
		}
		if after.nodes != 0 {
			allocation += inlineBytes
		}
	}
	return allocation
}

func TestLegacyCanonicalPagedPreparationFixedGrowthPlanning(test *testing.T) {
	var values legacyCanonicalPages[legacyCanonicalKeyField]
	nodeBytes := int(unsafe.Sizeof(legacyCanonicalPageNode[legacyCanonicalKeyField]{}))
	pageBytes := int(unsafe.Sizeof([legacyCanonicalPageValues]legacyCanonicalKeyField{}))
	valueBytes := int(unsafe.Sizeof(legacyCanonicalKeyField{}))
	inlineBytes := int(unsafe.Sizeof([legacyCanonicalInlineValues]legacyCanonicalKeyField{}))
	checkpoints := map[int]struct {
		single bool
		inline bool
		nodes  int
		pages  int
		depth  int
	}{
		1:    {single: true},
		2:    {inline: true},
		8:    {inline: true, nodes: 0, pages: 0, depth: 0},
		9:    {nodes: 1, pages: 1, depth: 0},
		64:   {nodes: 1, pages: 1, depth: 0},
		65:   {nodes: 3, pages: 2, depth: 1},
		4096: {nodes: 65, pages: 64, depth: 1},
		4097: {nodes: 68, pages: 65, depth: 2},
	}
	var stable *legacyCanonicalKeyField
	for position := 0; position < 4097; position++ {
		before := values
		planned, err := values.appendBytes()
		want := 0
		switch {
		case position == 0:
			want = valueBytes
		case position == 1:
			want = inlineBytes + valueBytes
		case position == 8:
			want = nodeBytes + pageBytes + inlineBytes
		case position == 64:
			want = 2*nodeBytes + pageBytes
		case position == 4096:
			want = 3*nodeBytes + pageBytes
		case position > 8 && position%64 == 0:
			want = nodeBytes + pageBytes
		}
		if err != nil || values != before || planned != want || planned < 0 || planned > 64<<10 {
			test.Fatal("append planning mutated storage or lost fixed allocation/copy work", position, planned, want, err)
		}
		if err := values.append(legacyCanonicalKeyField{key: fmt.Sprint(position), value: position, childKeys: position + 1}); err != nil {
			test.Fatal("bounded fixed-page append failed", position, err)
		}
		if values.length() != position+1 || values.at(position) == nil || values.at(position).value != position {
			test.Fatal("fixed-page append lost a value", position, values.length())
		}
		if position == 8 {
			stable = values.at(0)
		} else if position > 8 && values.at(0) != stable {
			test.Fatal("page growth moved an already promoted value", position)
		}
		if checkpoint, ok := checkpoints[values.length()]; ok {
			shape := legacyCanonicalPagedPreparationStorage(test, &values)
			if shape.single != checkpoint.single || shape.inline != checkpoint.inline || shape.nodes != checkpoint.nodes ||
				shape.pages != checkpoint.pages || values.depth != checkpoint.depth {
				test.Fatal("actual growth boundary changed the fixed allocation topology", values.length(), shape, values.depth, checkpoint)
			}
		}
	}
	for _, position := range []int{4096, 0, 64, 8, 4095, 63, 7, 2048} {
		field := values.at(position)
		if field == nil || field.key != fmt.Sprint(position) || field.value != position || field.childKeys != position+1 {
			test.Fatal("random page access lost a stored field", position, field)
		}
	}
	first, last := values.at(7), values.at(4096)
	*first, *last = *last, *first
	if values.at(7).value != 4096 || values.at(4096).value != 7 {
		test.Fatal("cross-page field swap failed")
	}
	shape := legacyCanonicalPagedPreparationStorage(test, &values)
	for values.length() != 0 {
		last := values.at(values.length() - 1)
		if err := values.pop(); err != nil || *last != (legacyCanonicalKeyField{}) {
			test.Fatal("pop retained a key, value, or child directory reference", values.length(), err)
		}
	}
	if err := values.pop(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("empty pop was admitted", err)
	}
	if actual := legacyCanonicalPagedPreparationStorage(test, &values); actual != shape {
		test.Fatal("popped but retained page capacity escaped accounting", actual, shape)
	}
	for position := 0; position < 4097; position++ {
		planned, err := values.appendBytes()
		if err != nil || planned != 0 {
			test.Fatal("reappend allocated instead of reusing charged fixed pages", position, planned, err)
		}
		if err := values.append(legacyCanonicalKeyField{key: "reused", value: -position}); err != nil {
			test.Fatal(err)
		}
	}
	if values.at(0) != stable || values.at(4096).key != "reused" || values.at(4096).value != -4096 {
		test.Fatal("reappend changed stable page addresses or exposed a stale field")
	}
	values.clear()
	if values != (legacyCanonicalPages[legacyCanonicalKeyField]{}) {
		test.Fatal("clear retained fixed-page storage")
	}
	legacyCanonicalPagedPreparationStorage(test, &values)
	planned, err := values.appendBytes()
	if err != nil || planned != valueBytes {
		test.Fatal("cleared storage did not restart at the single allocation", planned, valueBytes, err)
	}
}

func TestLegacyCanonicalPagedPreparationRejectsInvalidAndOverflowStorage(test *testing.T) {
	maximum := int(^uint(0) >> 1)
	maximumDepth := (int(unsafe.Sizeof(uintptr(0)))*8 - 1) / legacyCanonicalPageShift
	for _, fixture := range []struct {
		name   string
		values legacyCanonicalPages[legacyCanonicalKeyField]
	}{
		{name: "negative-count", values: legacyCanonicalPages[legacyCanonicalKeyField]{count: -1}},
		{name: "missing-storage", values: legacyCanonicalPages[legacyCanonicalKeyField]{count: 1}},
		{name: "negative-nodes", values: legacyCanonicalPages[legacyCanonicalKeyField]{nodes: -1}},
		{name: "negative-pages", values: legacyCanonicalPages[legacyCanonicalKeyField]{pages: -1}},
		{name: "invalid-depth", values: legacyCanonicalPages[legacyCanonicalKeyField]{depth: maximumDepth + 1}},
		{name: "inline-overflow", values: legacyCanonicalPages[legacyCanonicalKeyField]{inline: new([legacyCanonicalInlineValues]legacyCanonicalKeyField), count: 9}},
		{name: "count-overflow", values: legacyCanonicalPages[legacyCanonicalKeyField]{root: &legacyCanonicalPageNode[legacyCanonicalKeyField]{}, count: maximum, nodes: 1, pages: 1, depth: maximumDepth}},
		{name: "node-overflow", values: legacyCanonicalPages[legacyCanonicalKeyField]{root: &legacyCanonicalPageNode[legacyCanonicalKeyField]{}, count: 64, nodes: maximum, pages: 1}},
		{name: "page-overflow", values: legacyCanonicalPages[legacyCanonicalKeyField]{root: &legacyCanonicalPageNode[legacyCanonicalKeyField]{}, count: 9, nodes: maximum, pages: maximum}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			values := fixture.values
			before := values
			planned, err := values.appendBytes()
			if planned != 0 || !errors.Is(err, ErrIntegrity) || values != before {
				test.Fatal("invalid or overflowing append planning was admitted or mutated state", planned, err)
			}
			if err := values.append(legacyCanonicalKeyField{key: "rejected"}); !errors.Is(err, ErrIntegrity) || values != before {
				test.Fatal("rejected append changed its original storage", err)
			}
			values.clear()
			if values != (legacyCanonicalPages[legacyCanonicalKeyField]{}) {
				test.Fatal("clear did not discard invalid storage")
			}
			legacyCanonicalPagedPreparationStorage(test, &values)
		})
	}
	var overflow legacyCanonicalPages[legacyCanonicalKeyField]
	overflow.root, overflow.nodes, overflow.pages = &legacyCanonicalPageNode[legacyCanonicalKeyField]{}, maximum, maximum
	if _, err := overflow.ownedBytes(); !errors.Is(err, ErrIntegrity) {
		test.Fatal("retained storage arithmetic overflow was admitted", err)
	}
}

func TestLegacyCanonicalPagedPreparationRejectsOversizedFixedAllocations(test *testing.T) {
	var oversizedInline legacyCanonicalPages[[8193]byte]
	if planned, err := oversizedInline.appendBytes(); planned != 0 || !errors.Is(err, ErrIntegrity) {
		test.Fatal("oversized inline allocation was admitted", planned, err)
	}
	if err := oversizedInline.append([8193]byte{}); !errors.Is(err, ErrIntegrity) || oversizedInline != (legacyCanonicalPages[[8193]byte]{}) {
		test.Fatal("rejected oversized inline append allocated storage", err)
	}
	var values legacyCanonicalPages[[1024]byte]
	for position := 0; position < 8; position++ {
		if err := values.append([1024]byte{byte(position + 1)}); err != nil {
			test.Fatal("bounded inline allocation failed", position, err)
		}
	}
	before := values
	if planned, err := values.appendBytes(); planned != 0 || !errors.Is(err, ErrIntegrity) || values != before {
		test.Fatal("64KiB page plus node/copy work escaped the shared append cap", planned, err)
	}
	if err := values.append([1024]byte{}); !errors.Is(err, ErrIntegrity) || values != before || values.at(7)[0] != 8 {
		test.Fatal("rejected oversized promotion changed inline values", err)
	}
	legacyCanonicalPagedPreparationStorage(test, &values)
	values.clear()
	if values != (legacyCanonicalPages[[1024]byte]{}) {
		test.Fatal("clear retained oversized-promotion storage")
	}
}

func TestLegacyCanonicalPagedPreparationActualNativeStorageQuanta(test *testing.T) {
	var dense strings.Builder
	dense.WriteString(`{"kind":"tool_result","seq":7,"output":{`)
	for position := 0; position < 4097; position++ {
		if position != 0 {
			dense.WriteByte(',')
		}
		fmt.Fprintf(&dense, `"key-%05d":{"nested":true}`, 4097-position)
	}
	dense.WriteString(`}}`)
	for _, fixture := range []struct {
		name          string
		raw           string
		minimumMaps   int
		minimumFields int
		minimumFrames int
		inspectWork   uint64
	}{
		{name: "dense-native-maps", raw: dense.String(), minimumMaps: 4099, minimumFields: 4097, minimumFrames: 4, inspectWork: 10*4097 + 36},
		{name: "full-depth-native-arrays", raw: `{"kind":"tool_result","seq":7,"output":` + strings.Repeat("[", 9997) + `null` + strings.Repeat("]", 9997) + `}`, minimumMaps: 1, minimumFields: 3, minimumFrames: 9998, inspectWork: 3*9997 + 33},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			native, event, whole := legacyCanonicalNativePreparationDecode(test, fixture.raw)
			want := legacyCanonicalNativePreparationOracle(test, event)
			encoding := legacyCanonicalNativePreparationNew(test, native)
			assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
			owner := encoding.nativeOwner
			progress := func(preparation *legacyCanonicalPreparation) [10]int {
				state := [10]int{preparation.frames.length(), encoding.mapKeys.length(), encoding.frames.length()}
				if (preparation.frames.length() == 0) != preparation.complete {
					test.Fatal("actual preparation root-pop state is inconsistent", preparation.frames.length(), preparation.complete)
				}
				if preparation.complete {
					state[9] = 1
				}
				for position := 0; position < encoding.mapKeys.length(); position++ {
					state[3] += encoding.mapKeys.at(position).fields.length()
				}
				if frame := preparation.frames.at(preparation.frames.length() - 1); frame != nil {
					state[4], state[5], state[6], state[7], state[8] = frame.position, int(frame.phase), int(frame.kind), frame.depth, frame.mapPosition
				}
				return state
			}
			maximumMaps, maximumFields, maximumFrameSlots, chargedSteps, steps := 0, 0, 0, 0, 0
			for encoding.preparation != nil && steps < 1<<16 {
				preparation := encoding.preparation
				beforeWork := encoding.inspectWork
				beforeProgress := progress(preparation)
				beforeFrames := legacyCanonicalPagedPreparationStorage(test, &preparation.frames)
				beforeMaps := legacyCanonicalPagedPreparationStorage(test, &encoding.mapKeys)
				beforeOutput := legacyCanonicalPagedPreparationStorage(test, &encoding.frames)
				beforeFields := make([]legacyCanonicalPagedPreparationShape, encoding.mapKeys.length())
				for position := range beforeFields {
					beforeFields[position] = legacyCanonicalPagedPreparationStorage(test, &encoding.mapKeys.at(position).fields)
				}
				ctx, cancel := context.WithCancel(context.Background())
				chunk, complete, err := encoding.Next(ctx)
				cancel()
				steps++
				if err != nil || complete || len(chunk) != 0 || cap(chunk) > 64<<10 || encoding.inspectWork <= beforeWork ||
					encoding.inspectWork-beforeWork > 1024 || preparation.lastActions <= 0 || preparation.lastActions > 1024 ||
					encoding.inspectWork-beforeWork != uint64(preparation.lastActions) || preparation.lastBytes < 0 || preparation.lastBytes > 64<<10 ||
					preparation.lastStorageBytes < 0 || preparation.lastStorageBytes > preparation.lastBytes || encoding.lastStorageBytes != preparation.lastStorageBytes {
					test.Fatal("actual native page preparation escaped bounded empty-output progress", steps, len(chunk), complete, err, preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes)
				}
				if beforeProgress == progress(preparation) {
					test.Fatal("preparation changed work counters without an actual fixture state transition", steps, beforeProgress)
				}
				if beforeProgress[0] == 0 && (beforeProgress[9] != 1 || encoding.preparation != nil || encoding.frames.length() != 1 ||
					encoding.inspectWork-beforeWork != 1 || preparation.lastActions != 1) {
					test.Fatal("deferred root initialization was not exactly one actual frame transition", steps, encoding.frames.length(), encoding.inspectWork-beforeWork, preparation.lastActions)
				}
				maximumMaps = max(maximumMaps, encoding.mapKeys.length())
				afterMaps := legacyCanonicalPagedPreparationStorage(test, &encoding.mapKeys)
				afterFrames := legacyCanonicalPagedPreparationStorage(test, &preparation.frames)
				afterOutput := legacyCanonicalPagedPreparationStorage(test, &encoding.frames)
				frameSlots := afterFrames.pages * legacyCanonicalPageValues
				if afterFrames.single {
					frameSlots = 1
				} else if afterFrames.inline {
					frameSlots = legacyCanonicalInlineValues
				}
				maximumFrameSlots = max(maximumFrameSlots, frameSlots)
				allocation := legacyCanonicalPagedPreparationAllocated[legacyCanonicalEncodingKeys](test, beforeMaps, afterMaps)
				allocation += legacyCanonicalPagedPreparationAllocated[legacyCanonicalPreparationFrame](test, beforeFrames, afterFrames)
				allocation += legacyCanonicalPagedPreparationAllocated[legacyCanonicalEncodingFrame](test, beforeOutput, afterOutput)
				for position := 0; position < encoding.mapKeys.length(); position++ {
					fields := &encoding.mapKeys.at(position).fields
					maximumFields = max(maximumFields, fields.length())
					var before legacyCanonicalPagedPreparationShape
					if position < len(beforeFields) {
						before = beforeFields[position]
					}
					after := legacyCanonicalPagedPreparationStorage(test, fields)
					allocation += legacyCanonicalPagedPreparationAllocated[legacyCanonicalKeyField](test, before, after)
				}
				if allocation > 64<<10 || preparation.lastStorageBytes != allocation {
					test.Fatal("actual page allocation/copy work exceeded the charged shared quantum", steps, allocation, preparation.lastStorageBytes)
				}
				if allocation != 0 {
					chargedSteps++
				}
				retained, err := encoding.RetainedBytes()
				if err != nil || encoding.nativeOwner != owner || encoding.rootHeap.bytes < whole || retained < encoding.rootHeap.bytes {
					test.Fatal("paged preparation dropped or undercharged the actual native owner", steps, whole, encoding.rootHeap.bytes, retained, err)
				}
			}
			if encoding.preparation != nil || steps < 2 || chargedSteps < 2 || maximumMaps < fixture.minimumMaps ||
				maximumFields < fixture.minimumFields || maximumFrameSlots < fixture.minimumFrames {
				test.Fatal("actual native fixture did not exercise paged directory, field, or stack growth", steps, chargedSteps, maximumMaps, maximumFields, maximumFrameSlots)
			}
			if encoding.inspectWork != fixture.inspectWork {
				test.Fatal("bounded preparation counted a failed reservation instead of only real fixture transitions", encoding.inspectWork, fixture.inspectWork)
			}
			projected, err := legacyCanonicalEventJSONObject(event)
			if err != nil {
				test.Fatal(err)
			}
			synchronous, err := newLegacyCanonicalEncodingPending(context.Background(), projected)
			if err != nil || synchronous == nil {
				test.Fatal("synchronous preparation work oracle rejected the actual native fixture", err)
			}
			if encoding.inspectWork != synchronous.inspectWork || synchronous.frames.length() != 1 {
				test.Fatal("native or synchronous preparation omitted or invented inspect transitions", encoding.inspectWork, synchronous.inspectWork, synchronous.frames.length())
			}
			synchronous.release()
			actual, remainingPreparation := legacyCanonicalNativePreparationDrain(test, encoding, whole)
			if remainingPreparation != 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
				test.Fatal("actual paged native preparation changed full-depth canonical bytes or hash", remainingPreparation, len(actual), len(want))
			}
		})
	}
}

func TestLegacyCanonicalPagedPreparationDefersOnlyActualRootInitialization(test *testing.T) {
	const rootFields = 6
	const booleanCount = 256
	const rootMapTransitions = 4 + 2*rootFields + rootFields*rootFields
	const rootScalarTransitions = rootFields - 1
	const booleanTransitions = 2
	const arrayTransitions = 3
	remainingTransitions := legacyCanonicalEncodingActions - booleanTransitions*booleanCount - rootMapTransitions - rootScalarTransitions + 1
	if remainingTransitions <= 0 || remainingTransitions%arrayTransitions != 0 {
		test.Fatal("root-initialization fixture cannot reach the exact action boundary", remainingTransitions)
	}
	nestedArrays := remainingTransitions / arrayTransitions
	raw := `{"kind":"tool_result","seq":7,"id":"boundary","is_error":true,"output":` + strings.Repeat("[", nestedArrays) +
		strings.Repeat("true,", booleanCount-1) + `true` + strings.Repeat("]", nestedArrays) + `}`
	native, event, whole := legacyCanonicalNativePreparationDecode(test, raw)
	value := event.Output
	for depth := 0; depth < nestedArrays; depth++ {
		array, valid := value.([]any)
		length := 1
		if depth == nestedArrays-1 {
			length = booleanCount
		}
		if !valid || len(array) != length || cap(array) != length {
			test.Fatal("exact boundary fixture introduced uncounted hidden native capacity", depth, length, len(array), cap(array))
		}
		value = array[0]
	}
	want := legacyCanonicalNativePreparationOracle(test, event)
	encoding := legacyCanonicalNativePreparationNew(test, native)
	assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
	preparation := encoding.preparation
	ctx, cancel := context.WithCancel(context.Background())
	chunk, complete, err := encoding.Next(ctx)
	cancel()
	if err != nil || complete || len(chunk) != 0 || encoding.inspectWork != 1024 || preparation.lastActions != 1024 ||
		encoding.preparation != preparation || !preparation.complete || preparation.frames.length() != 0 || encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) ||
		preparation.lastStorageBytes < 0 || preparation.lastStorageBytes > preparation.lastBytes || preparation.lastBytes > 64<<10 ||
		encoding.lastStorageBytes != preparation.lastStorageBytes {
		test.Fatal("exact preparation action boundary failed to defer the real root frame", len(chunk), complete, err, encoding.inspectWork, preparation.lastActions, preparation.frames.length(), encoding.frames.length())
	}
	legacyCanonicalPagedPreparationStorage(test, &preparation.frames)
	beforeWork := encoding.inspectWork
	ctx, cancel = context.WithCancel(context.Background())
	chunk, complete, err = encoding.Next(ctx)
	cancel()
	storage := int(unsafe.Sizeof(legacyCanonicalEncodingFrame{}))
	if err != nil || complete || len(chunk) != 0 || encoding.preparation != nil || !preparation.complete || preparation.frames.length() != 0 || encoding.frames.length() != 1 ||
		encoding.inspectWork != beforeWork+1 || preparation.lastActions != 1 || preparation.lastStorageBytes != storage ||
		encoding.lastStorageBytes != storage || preparation.lastBytes != storage+int(unsafe.Sizeof(legacyCanonicalEncodingFrame{})) || preparation.lastBytes > 64<<10 {
		test.Fatal("empty preparation counted a no-op instead of one budgeted root-frame initialization", len(chunk), complete, err, encoding.inspectWork, preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes)
	}
	legacyCanonicalPagedPreparationStorage(test, &encoding.frames)
	projected, err := legacyCanonicalEventJSONObject(event)
	if err != nil {
		test.Fatal(err)
	}
	synchronous, err := newLegacyCanonicalEncodingPending(context.Background(), projected)
	if err != nil || synchronous == nil {
		test.Fatal("synchronous root-frame work oracle rejected the native fixture", err)
	}
	if synchronous.inspectWork != encoding.inspectWork {
		test.Fatal("native root-frame deferral invented or lost an inspection transition", encoding.inspectWork, synchronous.inspectWork)
	}
	synchronous.release()
	actual, remainingPreparation := legacyCanonicalNativePreparationDrain(test, encoding, whole)
	if remainingPreparation != 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) {
		test.Fatal("deferred root initialization changed canonical bytes/hash", remainingPreparation, len(actual), len(want))
	}
}

func TestLegacyCanonicalPagedPreparationOverDepthNativeSourceCannotTransferOwner(test *testing.T) {
	raw := `{"kind":"tool_result","seq":7,"output":` + strings.Repeat("[", 9998) + `null` + strings.Repeat("]", 9998) + `}`
	var original CIRDocument
	if err := json.Unmarshal(legacyNativeEventDocument("2", raw), &original); err == nil {
		test.Fatal("over-depth document was accepted by the original JSON decoder")
	}
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal(err)
	}
	native.reset(0, len(raw))
	native.object.prefix = true
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	var scanner legacyJSONValueScanner
	for stepIndex := 0; stepIndex < 64+32*len(source.Parts); stepIndex++ {
		ctx, cancel := context.WithCancel(context.Background())
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		event, complete, err := native.step(ctx, input, &scanner)
		cancel()
		if complete || native.canonicalReady || input.failure != nil || input.loads > 2 || input.workBytes > ConversationManifestChunkBytes ||
			event.Kind != "" || event.Output != nil || event.Input != nil {
			test.Fatal("over-depth source completed or escaped native input bounds", stepIndex, complete, err, input.failure)
		}
		if err != nil {
			if !errors.Is(err, ErrIntegrity) {
				test.Fatal("over-depth native source changed its existing rejection", stepIndex, err)
			}
			owner, transferErr := native.takeCanonicalOwner(context.Background())
			if owner != nil || !errors.Is(transferErr, ErrIntegrity) {
				test.Fatal("rejected source acquired a canonical owner", owner, transferErr)
			}
			native.releaseValues()
			if native.canonicalReady || native.decoded.Output != nil || native.decoded.Input != nil {
				test.Fatal("over-depth release retained decoded payload or transfer readiness")
			}
			return
		}
	}
	test.Fatal("over-depth native source exhausted its finite rejection allowance")
}
