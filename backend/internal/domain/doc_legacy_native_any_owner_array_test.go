package domain

import (
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeAnyOwnerArrayOracleState struct {
	shape    legacyNativeAnyArray
	decoded  []any
	previous []any
	children []legacyNativeAnyArrayChild
}

type legacyNativeAnyOwnerArrayOracleCancelContext struct {
	context.Context
	cancel context.CancelFunc
	list   *legacyNativeAnyArray
	after  int
}

func (current *legacyNativeAnyOwnerArrayOracleCancelContext) Err() error {
	if current.list.previous != nil && current.list.copied >= current.after {
		current.cancel()
	}
	return current.Context.Err()
}

func legacyNativeAnyOwnerArrayOracleNew(test *testing.T, maximum int) *legacyNativeAnyArray {
	test.Helper()
	list, err := newLegacyNativeAnyArray(maximum)
	if err != nil {
		test.Fatal(err)
	}
	return list
}

func legacyNativeAnyOwnerArrayOracleLeaf(test *testing.T, decoded any) legacyNativeAnyValue {
	test.Helper()
	heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(decoded))
	if err != nil {
		test.Fatal(err)
	}
	value := legacyNativeAnyValue{value: decoded, heap: heap}
	if text, ok := decoded.(string); ok {
		value.capacity = len(text)
	}
	return value
}

func legacyNativeAnyOwnerArrayOracleBorrow(test *testing.T, list *legacyNativeAnyArray) legacyNativeAnyValue {
	test.Helper()
	var retained docRangeHeaderHeapEstimate
	list.retained(&retained)
	value := legacyNativeAnyValue{value: list.values(), heap: list.heap(), excess: list.backingExcess(),
		owner: legacyNativeAnyValueOwner{array: list, revision: list.revision, bytes: retained.bytes}}
	if retained.err != nil {
		test.Fatal(retained.err)
	}
	if err := value.validateOwnership(); err != nil {
		test.Fatal("cannot borrow actual array storage", err)
	}
	return value
}

func legacyNativeAnyOwnerArrayOracleAppend(test *testing.T, list *legacyNativeAnyArray, value legacyNativeAnyValue) {
	test.Helper()
	for attempt := 0; attempt < 16; attempt++ {
		beforeChildren, beforeBytes, beforeLength := list.children, list.childBytes, len(list.values())
		input := &legacyNormalizationInput{ctx: context.Background(), fields: 11, values: 7, loads: 2, semanticBytes: 19}
		ready, err := list.appendValue(context.Background(), input, value)
		if err != nil || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes ||
			input.fields != 11 || input.values != 7 || input.loads != 2 || input.semanticBytes != 19 || input.unbounded ||
			input.pages[0] != nil || input.pages[1] != nil {
			test.Fatal("actual tracked append changed unrelated counters or exceeded its quantum", ready, err, input)
		}
		if !ready {
			if list.children != beforeChildren || list.childBytes != beforeBytes || len(list.values()) != beforeLength {
				test.Fatal("yield published pending child metadata")
			}
			continue
		}
		if !list.tracked || !list.children.valid() || list.children.length() != beforeLength+1 ||
			len(list.values()) != beforeLength+1 || list.childBytes != beforeBytes+value.owner.bytes {
			test.Fatal("logical append did not publish exactly one aligned child")
		}
		return
	}
	test.Fatal("actual tracked append exhausted its finite migration allowance")
}

func legacyNativeAnyOwnerArrayOracleFixtures(test *testing.T) []legacyNativeAnyValue {
	test.Helper()
	var builder strings.Builder
	builder.Grow(97)
	builder.WriteString("spare-owner-string")
	text, capacity := builder.String(), builder.Cap()
	var heap docRangeHeaderHeapEstimate
	heap.allocation(1, int64(unsafe.Sizeof(text)))
	heap.allocation(int64(capacity), 1)
	spare := legacyNativeAnyValue{value: text, heap: heap, capacity: capacity, excess: 2 * int64(capacity-len(text))}
	if capacity <= len(text) || spare.excess == 0 || spare.owner != (legacyNativeAnyValueOwner{}) {
		test.Fatal("fixture lacks actual spare string backing or a valid zero inline owner")
	}
	nested := legacyNativeAnyOwnerArrayOracleNew(test, 3)
	legacyNativeAnyOwnerArrayOracleAppend(test, nested, spare)
	legacyNativeAnyOwnerArrayOracleAppend(test, nested, legacyNativeAnyOwnerArrayOracleLeaf(test, true))
	nestedValue := legacyNativeAnyOwnerArrayOracleBorrow(test, nested)
	object := newLegacyNativeAnyMap()
	if _, err := object.putValue("nested", len("nested"), nestedValue); err != nil {
		test.Fatal(err)
	}
	var retained docRangeHeaderHeapEstimate
	object.retained(&retained)
	objectValue := legacyNativeAnyValue{value: object.values(), heap: object.heap(), excess: object.backingExcess(),
		owner: legacyNativeAnyValueOwner{object: object, revision: object.revision, bytes: retained.bytes}}
	if retained.err != nil || objectValue.validateOwnership() != nil || nestedValue.owner.bytes <= 0 || objectValue.owner.bytes <= 0 {
		test.Fatal("container fixtures require real nonnil owners and retained storage")
	}
	return []legacyNativeAnyValue{spare, nestedValue, objectValue,
		legacyNativeAnyOwnerArrayOracleLeaf(test, true), legacyNativeAnyOwnerArrayOracleLeaf(test, math.Copysign(0, -1)),
		legacyNativeAnyOwnerArrayOracleLeaf(test, nil), legacyNativeAnyOwnerArrayOracleLeaf(test, []any(nil)),
		legacyNativeAnyOwnerArrayOracleLeaf(test, map[string]any(nil))}
}

func legacyNativeAnyOwnerArrayOracleCapture(test *testing.T, list *legacyNativeAnyArray) legacyNativeAnyOwnerArrayOracleState {
	test.Helper()
	state := legacyNativeAnyOwnerArrayOracleState{shape: *list,
		decoded: append([]any(nil), list.decoded...), previous: append([]any(nil), list.previous...)}
	for position := 0; position < list.children.length(); position++ {
		child := list.children.at(position)
		if child == nil {
			test.Fatal("cannot capture missing committed child", position)
		}
		state.children = append(state.children, *child)
	}
	return state
}

func (state legacyNativeAnyOwnerArrayOracleState) unchanged(test *testing.T, list *legacyNativeAnyArray) {
	test.Helper()
	before := state.shape
	if list.maximum != before.maximum || list.copied != before.copied || list.estimate != before.estimate ||
		list.excess != before.excess || list.children != before.children || list.childBytes != before.childBytes ||
		list.revision != before.revision || list.tracked != before.tracked ||
		len(list.decoded) != len(before.decoded) || cap(list.decoded) != cap(before.decoded) ||
		len(list.previous) != len(before.previous) || cap(list.previous) != cap(before.previous) ||
		reflect.ValueOf(list.decoded).Pointer() != reflect.ValueOf(before.decoded).Pointer() ||
		reflect.ValueOf(list.previous).Pointer() != reflect.ValueOf(before.previous).Pointer() {
		test.Fatal("rejected or deferred append changed physical storage, ownership, or revision")
	}
	if len(list.decoded) != 0 && !reflect.DeepEqual(list.decoded, state.decoded) ||
		len(list.previous) != 0 && !reflect.DeepEqual(list.previous, state.previous) {
		test.Fatal("rejected or deferred append modified a physical payload slot")
	}
	for position, expected := range state.children {
		child := list.children.at(position)
		if child == nil || *child != expected {
			test.Fatal("rejected or deferred append modified committed metadata", position)
		}
	}
}

func legacyNativeAnyOwnerArrayOracleSame(test *testing.T, actual, expected legacyNativeAnyValue, position int) {
	test.Helper()
	if !reflect.DeepEqual(actual, expected) {
		test.Fatalf("child %d lost its logical payload, capacity, excess, or owner: actual=%+v expected=%+v", position, actual, expected)
	}
	if number, ok := expected.value.(float64); ok && math.Float64bits(actual.value.(float64)) != math.Float64bits(number) {
		test.Fatal("child metadata read changed floating-point bits", position)
	}
}

func legacyNativeAnyOwnerArrayOracleCheck(test *testing.T, list *legacyNativeAnyArray, expected []legacyNativeAnyValue) {
	test.Helper()
	if !list.children.valid() || list.children.length() != len(expected) || len(list.values()) != len(expected) ||
		len(list.decoded) != len(expected) || list.tracked != (len(expected) != 0) || list.children.at(len(expected)) != nil {
		test.Fatal("logical payload and child metadata are not aligned", len(list.values()), list.children.length(), len(expected))
	}
	borrowed := legacyNativeAnyOwnerArrayOracleBorrow(test, list)
	expectedHeap := int64(64) + 2*int64(unsafe.Sizeof([]any{}))
	if cap(list.decoded) != 0 {
		expectedHeap += 64 + 2*int64(cap(list.decoded))*int64(unsafe.Sizeof(any(nil)))
	}
	var expectedChildren, expectedExcess int64
	for position, value := range expected {
		child := list.children.at(position)
		if child == nil || child.heapBytes != value.heap.bytes || child.excess != value.excess ||
			child.capacity != value.capacity || child.owner != value.owner || !reflect.DeepEqual(list.values()[position], value.value) {
			test.Fatal("published child metadata does not match the committed logical value", position)
		}
		actual, err := borrowed.owner.arrayValue(context.Background(), position)
		if err != nil {
			test.Fatal("fresh borrowed snapshot must read values() during migration, including uncopied slots", position, list.copied, err)
		}
		legacyNativeAnyOwnerArrayOracleSame(test, actual, value, position)
		expectedHeap += value.heap.bytes
		expectedChildren += value.owner.bytes
		expectedExcess += value.excess
	}
	if list.childBytes != expectedChildren || list.backingExcess() != expectedExcess || list.heap().err != nil || list.heap().bytes != expectedHeap {
		test.Fatal("yield or commit double-counted or omitted payload, excess, or retained child owners", list.childBytes, expectedChildren, list.heap(), expectedHeap)
	}
	expectedRetained := int64(64) + 2*int64(unsafe.Sizeof(*list)) + expectedChildren
	if cap(list.previous) != 0 {
		expectedRetained += 64 + 2*int64(cap(list.previous))*int64(unsafe.Sizeof(any(nil)))
	}
	if list.children.single != nil {
		expectedRetained += 64 + 2*int64(unsafe.Sizeof(legacyNativeAnyArrayChild{}))
	}
	if list.children.inline != nil {
		expectedRetained += 64 + 2*int64(unsafe.Sizeof([legacyCanonicalInlineValues]legacyNativeAnyArrayChild{}))
	}
	expectedRetained += int64(list.children.nodes) * (64 + 2*int64(unsafe.Sizeof(legacyCanonicalPageNode[legacyNativeAnyArrayChild]{})))
	expectedRetained += int64(list.children.pages) * (64 + 2*int64(unsafe.Sizeof([legacyCanonicalPageValues]legacyNativeAnyArrayChild{})))
	if borrowed.owner.bytes != expectedRetained {
		test.Fatal("retained owner ledger omitted metadata pages, previous backing, or child owners", borrowed.owner.bytes, expectedRetained)
	}
}

func TestLegacyNativeAnyOwnerArrayPublicationAndBudget(test *testing.T) {
	const width = int(unsafe.Sizeof(any(nil)))
	for _, fixture := range []struct {
		name  string
		count int
	}{{name: "first-single", count: 0}, {name: "single-to-inline", count: 1},
		{name: "inline-to-page", count: legacyCanonicalInlineValues}, {name: "page-rollover", count: legacyCanonicalPageValues}} {
		test.Run(fixture.name, func(test *testing.T) {
			values := legacyNativeAnyOwnerArrayOracleFixtures(test)
			list := legacyNativeAnyOwnerArrayOracleNew(test, fixture.count+2)
			var expected []legacyNativeAnyValue
			for position := 0; position < fixture.count; position++ {
				value := values[position%len(values)]
				legacyNativeAnyOwnerArrayOracleAppend(test, list, value)
				expected = append(expected, value)
			}
			legacyNativeAnyOwnerArrayOracleCheck(test, list, expected)
			pending := values[0]
			before := legacyNativeAnyOwnerArrayOracleCapture(test, list)
			oldBorrow := legacyNativeAnyOwnerArrayOracleBorrow(test, list)
			oldValues := append([]any(nil), list.values()...)
			input := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 2*width + 1}
			ready, err := list.appendValue(context.Background(), input, pending)
			if ready || err != nil || input.workBytes != ConversationManifestChunkBytes-2*width+1 {
				test.Fatal("growth did not wait for a whole reserve quantum", ready, err, input.workBytes)
			}
			before.unchanged(test, list)
			input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 2*width}
			ready, err = list.appendValue(context.Background(), input, pending)
			if ready || err != nil || list.revision != before.shape.revision+1 || cap(list.decoded) <= cap(before.shape.decoded) ||
				list.children != before.shape.children || list.childBytes != before.shape.childBytes ||
				fixture.count > 0 && (list.copied != 1 || input.workBytes != ConversationManifestChunkBytes) ||
				fixture.count == 0 && input.workBytes != ConversationManifestChunkBytes-2*width {
				test.Fatal("physical growth published a logical value or metadata before ready", ready, err, list.copied)
			}
			if err := oldBorrow.validateOwnership(); !errors.Is(err, ErrConflict) {
				test.Fatal("physical growth failed to invalidate an old borrowed revision", err)
			}
			legacyNativeAnyOwnerArrayOracleCheck(test, list, expected)
			growthRevision := list.revision
			for attempt := 0; list.previous != nil && attempt <= fixture.count; attempt++ {
				input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 2*width}
				copied := list.copied
				ready, err = list.appendValue(context.Background(), input, pending)
				if ready || err != nil || list.copied != copied+1 || input.workBytes != ConversationManifestChunkBytes ||
					list.revision != growthRevision || list.children != before.shape.children || list.childBytes != before.shape.childBytes {
					test.Fatal("copy yield consumed a logical append or changed metadata", ready, err)
				}
				legacyNativeAnyOwnerArrayOracleCheck(test, list, expected)
			}
			if list.previous != nil {
				test.Fatal("finite copy schedule failed to finish migration")
			}
			metadataWork, future, err := list.children.appendRequirements()
			if err != nil || future.err != nil {
				test.Fatal(err, future.err)
			}
			writeCost := width + metadataWork + int(unsafe.Sizeof(legacyNativeAnyArrayChild{}))
			beforeWrite := legacyNativeAnyOwnerArrayOracleCapture(test, list)
			input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - writeCost + 1}
			ready, err = list.appendValue(context.Background(), input, pending)
			if ready || err != nil || input.workBytes != ConversationManifestChunkBytes-writeCost+1 {
				test.Fatal("metadata or payload committed one byte below the combined write budget", ready, err)
			}
			beforeWrite.unchanged(test, list)
			input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - writeCost}
			ready, err = list.appendValue(context.Background(), input, pending)
			if !ready || err != nil || input.workBytes != ConversationManifestChunkBytes || list.revision != growthRevision+1 {
				test.Fatal("exact payload plus metadata budget did not commit once", ready, err, input.workBytes)
			}
			expected = append(expected, pending)
			legacyNativeAnyOwnerArrayOracleCheck(test, list, expected)
			var metadata docRangeHeaderHeapEstimate
			list.children.retained(&metadata)
			if metadata != future || fixture.count != 0 && !reflect.DeepEqual(oldBorrow.value, oldValues) {
				test.Fatal("commit disagreed with its metadata plan or modified old borrowed backing", metadata, future)
			}
		})
	}
}

func TestLegacyNativeAnyOwnerArrayCommitMigrationCancellation(test *testing.T) {
	const width = int(unsafe.Sizeof(any(nil)))
	count := ConversationManifestChunkBytes / width
	values := legacyNativeAnyOwnerArrayOracleFixtures(test)
	list := legacyNativeAnyOwnerArrayOracleNew(test, count+1)
	expected := make([]legacyNativeAnyValue, 0, count+1)
	for position := 0; position < count; position++ {
		value := values[position%len(values)]
		legacyNativeAnyOwnerArrayOracleAppend(test, list, value)
		expected = append(expected, value)
	}
	pending := values[2]
	current := &legacyNativeAnyFrame{array: list, phase: legacyNativeAnyValueNext}
	parser := &legacyNativeAny{frame: current, rootArray: list, node: pending, pending: true, position: 27, end: 99, depth: 1}
	if err := parser.syncFrame(current); err != nil {
		test.Fatal(err)
	}
	oldBorrow := legacyNativeAnyOwnerArrayOracleBorrow(test, list)
	beforeChildren, beforeBytes := list.children, list.childBytes
	newInput := func(stored context.Context, workBytes int) *legacyNormalizationInput {
		return &legacyNormalizationInput{ctx: stored, workBytes: workBytes, fields: 11, values: 7, loads: 2, semanticBytes: 19}
	}
	checkPending := func(input *legacyNormalizationInput) {
		test.Helper()
		if !parser.pending || !reflect.DeepEqual(parser.node, pending) || parser.frame != current || current.phase != legacyNativeAnyValueNext ||
			parser.position != 27 || parser.depth != 1 || parser.keyToken || parser.finished ||
			input.values != 7 || input.fields != 11 || input.loads != 2 || input.semanticBytes != 19 || input.unbounded ||
			input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.pages[0] != nil || input.pages[1] != nil ||
			list.children != beforeChildren || list.childBytes != beforeBytes {
			test.Fatal("commit yield consumed pending node, phase, count, or unpublished child metadata")
		}
		if estimate := current.estimate(); estimate.err != nil || parser.live != estimate || current.retained != estimate.bytes {
			test.Fatal("parser failed to synchronize physical migration retention on yield", parser.live, estimate)
		}
		legacyNativeAnyOwnerArrayOracleCheck(test, list, expected)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceling := &legacyNativeAnyOwnerArrayOracleCancelContext{Context: base, cancel: cancel, list: list, after: canonicalCheckBytes / (2 * width)}
	input := newInput(context.Background(), 0)
	ready, err := parser.commit(canceling, input)
	if ready || !errors.Is(err, context.Canceled) || list.previous == nil || list.copied != canceling.after || input.workBytes != canonicalCheckBytes {
		test.Fatal("actual parser commit failed to cancel between bounded copy blocks", ready, err, list.copied, input.workBytes)
	}
	if err := oldBorrow.validateOwnership(); !errors.Is(err, ErrConflict) {
		test.Fatal("growth during canceled commit left the old borrowed snapshot valid", err)
	}
	checkPending(input)
	beforeCanceled := legacyNativeAnyOwnerArrayOracleCapture(test, list)
	input = newInput(context.Background(), 0)
	ready, err = parser.commit(base, input)
	if ready || !errors.Is(err, context.Canceled) || input.workBytes != 0 {
		test.Fatal("pre-canceled retry performed work", ready, err, input.workBytes)
	}
	beforeCanceled.unchanged(test, list)
	checkPending(input)
	copied := list.copied
	input = newInput(base, 0)
	ready, err = parser.commit(context.Background(), input)
	if ready || err != nil || list.previous == nil || list.copied != copied+ConversationManifestChunkBytes/(2*width) || input.workBytes != ConversationManifestChunkBytes {
		test.Fatal("fresh context retry retained cancellation or exceeded one migration quantum", ready, err, list.copied, input.workBytes)
	}
	checkPending(input)
	remainingCopy := (count - list.copied) * 2 * width
	if remainingCopy <= 0 || remainingCopy >= ConversationManifestChunkBytes {
		test.Fatal("fixture does not span multiple copy quanta", remainingCopy)
	}
	input = newInput(base, ConversationManifestChunkBytes-remainingCopy)
	ready, err = parser.commit(context.Background(), input)
	if ready || err != nil || list.previous != nil || list.copied != count || input.workBytes != ConversationManifestChunkBytes {
		test.Fatal("copy completion published the pending node without final write budget", ready, err, list.copied)
	}
	checkPending(input)
	metadataWork, _, err := list.children.appendRequirements()
	if err != nil {
		test.Fatal(err)
	}
	writeCost := width + metadataWork + int(unsafe.Sizeof(legacyNativeAnyArrayChild{}))
	beforeWrite := legacyNativeAnyOwnerArrayOracleCapture(test, list)
	input = newInput(base, ConversationManifestChunkBytes-writeCost+1)
	ready, err = parser.commit(context.Background(), input)
	if ready || err != nil || input.workBytes != ConversationManifestChunkBytes-writeCost+1 {
		test.Fatal("parser committed below the exact combined write boundary", ready, err)
	}
	beforeWrite.unchanged(test, list)
	checkPending(input)
	input = newInput(base, ConversationManifestChunkBytes-writeCost)
	ready, err = parser.commit(context.Background(), input)
	if !ready || err != nil || input.workBytes != ConversationManifestChunkBytes || input.values != 8 ||
		input.fields != 11 || input.loads != 2 || input.semanticBytes != 19 || parser.pending ||
		!reflect.DeepEqual(parser.node, legacyNativeAnyValue{}) || current.phase != legacyNativeAnyAfter ||
		list.childBytes != beforeBytes+pending.owner.bytes || len(list.values()) != count+1 || cap(list.values()) != count+1 {
		test.Fatal("ready parser commit did not publish and consume exactly one pending child", ready, err, input)
	}
	expected = append(expected, pending)
	legacyNativeAnyOwnerArrayOracleCheck(test, list, expected)
	if estimate := current.estimate(); parser.live != estimate || current.retained != estimate.bytes {
		test.Fatal("completed parser commit retained stale migration accounting", parser.live, estimate)
	}
	beforeDuplicate := legacyNativeAnyOwnerArrayOracleCapture(test, list)
	input = newInput(context.Background(), 0)
	if ready, err := parser.commit(context.Background(), input); ready || !errors.Is(err, ErrIntegrity) || input.values != 7 || input.workBytes != 0 {
		test.Fatal("consumed pending node was committed twice", ready, err)
	}
	beforeDuplicate.unchanged(test, list)
}

func TestLegacyNativeAnyOwnerArrayIntegrityAndPreflight(test *testing.T) {
	test.Run("compatibility-is-untracked-and-cannot-drop-tracked-metadata", func(test *testing.T) {
		list := legacyNativeAnyOwnerArrayOracleNew(test, 3)
		foreign := map[string]any{"compatibility": true}
		value := legacyNativeAnyOwnerArrayOracleLeaf(test, foreign)
		input := &legacyNormalizationInput{}
		if ready, err := list.append(context.Background(), input, foreign, value.heap, 0); !ready || err != nil ||
			list.tracked || list.children.length() != 0 || list.childBytes != 0 {
			test.Fatal("legacy direct append no longer preserves its untracked compatibility path", ready, err)
		}
		borrowed := legacyNativeAnyOwnerArrayOracleBorrow(test, list)
		if _, err := borrowed.owner.arrayValue(context.Background(), 0); !errors.Is(err, ErrIntegrity) {
			test.Fatal("untracked compatibility payload fabricated strict child metadata", err)
		}
		before := legacyNativeAnyOwnerArrayOracleCapture(test, list)
		input = &legacyNormalizationInput{}
		if ready, err := list.appendValue(context.Background(), input, legacyNativeAnyValue{}); ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 {
			test.Fatal("strict append accepted a prior payload without aligned metadata", ready, err)
		}
		before.unchanged(test, list)
		tracked := legacyNativeAnyOwnerArrayOracleNew(test, 3)
		legacyNativeAnyOwnerArrayOracleAppend(test, tracked, legacyNativeAnyValue{})
		before = legacyNativeAnyOwnerArrayOracleCapture(test, tracked)
		input = &legacyNormalizationInput{}
		if ready, err := tracked.append(context.Background(), input, nil, docRangeHeaderHeapEstimate{}, 0); ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 {
			test.Fatal("plain append silently discarded tracked child metadata", ready, err)
		}
		before.unchanged(test, tracked)
	})
	test.Run("inline-zero-owner-and-real-container-owners", func(test *testing.T) {
		values := legacyNativeAnyOwnerArrayOracleFixtures(test)
		list := legacyNativeAnyOwnerArrayOracleNew(test, len(values))
		for _, value := range values {
			legacyNativeAnyOwnerArrayOracleAppend(test, list, value)
		}
		legacyNativeAnyOwnerArrayOracleCheck(test, list, values)
		for _, fixture := range []struct {
			name  string
			value legacyNativeAnyValue
		}{
			{name: "empty-array-without-owner", value: legacyNativeAnyOwnerArrayOracleLeaf(test, []any{})},
			{name: "nonempty-array-without-owner", value: legacyNativeAnyOwnerArrayOracleLeaf(test, []any{true})},
			{name: "empty-object-without-owner", value: legacyNativeAnyOwnerArrayOracleLeaf(test, map[string]any{})},
			{name: "nonempty-object-without-owner", value: legacyNativeAnyOwnerArrayOracleLeaf(test, map[string]any{"leaf": true})},
			{name: "inline-fabricated-owner", value: legacyNativeAnyValue{value: true, owner: legacyNativeAnyValueOwner{bytes: 1}}},
		} {
			test.Run(fixture.name, func(test *testing.T) {
				target := legacyNativeAnyOwnerArrayOracleNew(test, 2)
				before := legacyNativeAnyOwnerArrayOracleCapture(test, target)
				input := &legacyNormalizationInput{}
				if ready, err := target.appendValue(context.Background(), input, fixture.value); ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 {
					test.Fatal("strict tracked append accepted ownerless containers or a fabricated inline owner", ready, err)
				}
				before.unchanged(test, target)
			})
		}
		child := values[1].owner.array
		stale := legacyNativeAnyOwnerArrayOracleBorrow(test, child)
		legacyNativeAnyOwnerArrayOracleAppend(test, child, legacyNativeAnyValue{})
		target := legacyNativeAnyOwnerArrayOracleNew(test, 2)
		before := legacyNativeAnyOwnerArrayOracleCapture(test, target)
		input := &legacyNormalizationInput{}
		if ready, err := target.appendValue(context.Background(), input, stale); ready || !errors.Is(err, ErrConflict) || input.workBytes != 0 {
			test.Fatal("tracked append accepted a stale real child owner", ready, err)
		}
		before.unchanged(test, target)
		self := legacyNativeAnyOwnerArrayOracleBorrow(test, target)
		if ready, err := target.appendValue(context.Background(), input, self); ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 {
			test.Fatal("tracked array accepted its own borrowed value", ready, err)
		}
		before.unchanged(test, target)
	})
	test.Run("metadata-retention-overflow-before-first-allocation", func(test *testing.T) {
		list := legacyNativeAnyOwnerArrayOracleNew(test, 2)
		_, metadata, err := list.children.appendRequirements()
		if err != nil || metadata.err != nil || metadata.bytes <= 0 {
			test.Fatal("fixture lacks prospective metadata retention", err, metadata)
		}
		helperBytes := int64(64) + 2*int64(unsafe.Sizeof(*list))
		list.estimate.bytes = math.MaxInt64 - helperBytes - metadata.bytes + 1
		if list.estimate.bytes > math.MaxInt64-helperBytes || list.estimate.bytes+helperBytes <= math.MaxInt64-metadata.bytes {
			test.Fatal("fixture must overflow only when prospective metadata joins payload and helper")
		}
		before := legacyNativeAnyOwnerArrayOracleCapture(test, list)
		input := &legacyNormalizationInput{}
		if ready, err := list.appendValue(context.Background(), input, legacyNativeAnyValue{}); ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 {
			test.Fatal("prospective metadata retention overflow reached allocation or logical commit", ready, err)
		}
		before.unchanged(test, list)
	})
	test.Run("combined-old-new-metadata-and-child-peak-before-growth", func(test *testing.T) {
		values := legacyNativeAnyOwnerArrayOracleFixtures(test)
		list := legacyNativeAnyOwnerArrayOracleNew(test, 4)
		legacyNativeAnyOwnerArrayOracleAppend(test, list, values[2])
		legacyNativeAnyOwnerArrayOracleAppend(test, list, legacyNativeAnyValue{})
		_, metadata, err := list.children.appendRequirements()
		if err != nil || metadata.err != nil || cap(list.decoded) != 2 || list.childBytes <= 0 {
			test.Fatal("fixture lacks real committed metadata, owners, or full old backing", err)
		}
		helperBytes := int64(64) + 2*int64(unsafe.Sizeof(*list))
		oldBacking := int64(64) + 2*int64(cap(list.decoded))*int64(unsafe.Sizeof(any(nil)))
		newBacking := int64(64) + 2*int64(list.maximum)*int64(unsafe.Sizeof(any(nil)))
		fixed := helperBytes + metadata.bytes + list.childBytes
		list.estimate.bytes = math.MaxInt64 - fixed - newBacking + 1
		finalPayload := list.estimate.bytes - oldBacking + newBacking
		if list.estimate.bytes > math.MaxInt64-fixed || finalPayload > math.MaxInt64-fixed ||
			finalPayload+fixed <= math.MaxInt64-oldBacking {
			test.Fatal("fixture must fit before and after migration but overflow the combined physical peak")
		}
		before := legacyNativeAnyOwnerArrayOracleCapture(test, list)
		input := &legacyNormalizationInput{}
		if ready, err := list.appendValue(context.Background(), input, legacyNativeAnyValue{}); ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 {
			test.Fatal("combined physical peak overflow allocated new backing or consumed child metadata", ready, err)
		}
		before.unchanged(test, list)
	})
}
