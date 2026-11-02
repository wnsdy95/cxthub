package domain

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"unsafe"
)

func legacyNativeAnyArrayTestNew(test *testing.T, maximum int) *legacyNativeAnyArray {
	test.Helper()
	list, err := newLegacyNativeAnyArray(maximum)
	if err != nil {
		test.Fatal(err)
	}
	return list
}

func legacyNativeAnyArrayTestAppend(test *testing.T, list *legacyNativeAnyArray, value any, heap docRangeHeaderHeapEstimate, excess int64) {
	test.Helper()
	for stepIndex := 0; stepIndex < 64; stepIndex++ {
		input := &legacyNormalizationInput{ctx: context.Background()}
		complete, err := list.append(input.ctx, input, value, heap, excess)
		if err != nil || input.workBytes > ConversationManifestChunkBytes || input.fields != 0 || input.loads != 0 || input.semanticBytes != 0 || input.unbounded {
			test.Fatal("array storage exceeded its shared copy budget or performed parser work", err, input)
		}
		if complete {
			return
		}
	}
	test.Fatal("array storage exhausted its copy progress allowance")
}

func legacyNativeAnyArrayTestHeap(test *testing.T, list *legacyNativeAnyArray, payload docRangeHeaderHeapEstimate) {
	test.Helper()
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof([]any{})))
	expected.allocation(int64(cap(list.decoded)), int64(unsafe.Sizeof(any(nil))))
	expected.include(payload)
	if actual := list.heap(); actual.err != nil || expected.err != nil || actual.bytes != expected.bytes {
		test.Fatal("array heap omitted current backing or counted child payload twice", actual, expected)
	}
	retained := list.heap()
	list.retained(&retained)
	expected.allocation(1, int64(unsafe.Sizeof(*list)))
	expected.allocation(int64(cap(list.previous)), int64(unsafe.Sizeof(any(nil))))
	if retained.err != nil || expected.err != nil || retained.bytes != expected.bytes {
		test.Fatal("array scratch retention omitted its helper or previous backing", retained, expected)
	}
	var scratch docRangeHeaderHeapEstimate
	list.retained(&scratch)
	var expectedScratch docRangeHeaderHeapEstimate
	expectedScratch.allocation(1, int64(unsafe.Sizeof(*list)))
	expectedScratch.allocation(int64(cap(list.previous)), int64(unsafe.Sizeof(any(nil))))
	if scratch.err != nil || scratch.bytes != expectedScratch.bytes {
		test.Fatal("array scratch retention recounted current backing or committed children", scratch, expectedScratch)
	}
}

func TestLegacyNativeAnyArrayEmptyAndFreshOwnership(test *testing.T) {
	empty := legacyNativeAnyArrayTestNew(test, 0)
	if empty.values() == nil || len(empty.values()) != 0 || cap(empty.values()) != 0 || empty.backingExcess() != 0 {
		test.Fatal("empty generic array became nil or retained foreign capacity")
	}
	legacyNativeAnyArrayTestHeap(test, empty, docRangeHeaderHeapEstimate{})
	if complete, err := empty.append(context.Background(), &legacyNormalizationInput{}, nil, docRangeHeaderHeapEstimate{}, 0); complete || !errors.Is(err, ErrIntegrity) {
		test.Fatal("zero source-derived maximum accepted an element", complete, err)
	}
	first := legacyNativeAnyArrayTestNew(test, 3)
	second := legacyNativeAnyArrayTestNew(test, 3)
	legacyNativeAnyArrayTestAppend(test, first, "prior", docRangeHeaderHeapEstimate{}, 0)
	legacyNativeAnyArrayTestAppend(test, second, nil, docRangeHeaderHeapEstimate{}, 0)
	if &first.values()[0] == &second.values()[0] || second.values()[0] != nil {
		test.Fatal("fresh generic arrays shared backing or a prior hidden value")
	}
	foreign := []any{"foreign-head", "foreign-tail"}[:1]
	legacyNativeAnyArrayTestAppend(test, second, foreign, docRangeHeaderHeapEstimate{}, 0)
	child := second.values()[1].([]any)
	if &second.values()[0] == &foreign[0] || &child[0] != &foreign[0] || foreign[:cap(foreign)][1] != "foreign-tail" {
		test.Fatal("array storage adopted foreign backing or cloned a committed child")
	}
	legacyNativeAnyArrayTestAppend(test, second, nil, docRangeHeaderHeapEstimate{}, 0)
	if second.values()[2] != nil || first.values()[0] != "prior" {
		test.Fatal("fresh null element reused hidden foreign or sibling storage")
	}
}

func TestLegacyNativeAnyArrayCapacityWorkAndPayloadAccounting(test *testing.T) {
	const width = int(unsafe.Sizeof(any(nil)))
	list := legacyNativeAnyArrayTestNew(test, 5)
	values := []any{nil, math.Copysign(0, -1), true, "text", []any{float64(7)}}
	capacities := []int{1, 2, 4, 4, 5}
	var payload docRangeHeaderHeapEstimate
	var totalExcess int64
	for index, value := range values {
		oldLength, oldCapacity := len(list.values()), cap(list.values())
		childHeap := docRangeHeaderHeapEstimate{bytes: int64(100 + index)}
		excess := int64(index + 1)
		input := &legacyNormalizationInput{ctx: context.Background(), workBytes: 17, fields: LegacyNormalizationStepEvents, loads: 2, semanticBytes: 19}
		complete, err := list.append(input.ctx, input, value, childHeap, excess)
		expectedWork := 17 + width
		if oldLength == oldCapacity {
			expectedWork += 2 * width * oldLength
		}
		if err != nil || !complete || input.workBytes != expectedWork || input.fields != LegacyNormalizationStepEvents || input.loads != 2 || input.semanticBytes != 19 || input.unbounded || input.pages[0] != nil || input.pages[1] != nil {
			test.Fatal("array storage changed parser counters or charged the wrong interface copy width", complete, err, input, expectedWork)
		}
		if len(list.values()) != index+1 || cap(list.values()) != capacities[index] || cap(list.values()) > list.maximum || list.previous != nil {
			test.Fatal("array storage exceeded its source maximum or retained completed migration", len(list.values()), cap(list.values()))
		}
		payload.include(childHeap)
		totalExcess += excess
		legacyNativeAnyArrayTestHeap(test, list, payload)
		if list.backingExcess() != totalExcess {
			test.Fatal("committed child backing excess was not aggregated", list.backingExcess(), totalExcess)
		}
	}
	if !reflect.DeepEqual(list.values(), values) || math.Float64bits(list.values()[1].(float64)) != math.Float64bits(values[1].(float64)) {
		test.Fatal("array storage changed a committed value or numeric bit pattern", list.values())
	}
	before := list.heap()
	input := &legacyNormalizationInput{ctx: context.Background()}
	if complete, err := list.append(input.ctx, input, "overflow", docRangeHeaderHeapEstimate{bytes: 50}, 9); complete || !errors.Is(err, ErrIntegrity) || list.heap() != before || input.workBytes != 0 || len(list.values()) != 5 {
		test.Fatal("array storage appended beyond the source-derived maximum", complete, err)
	}
}

func TestLegacyNativeAnyArrayMigrationWaitsForExactSharedBudget(test *testing.T) {
	const width = int(unsafe.Sizeof(any(nil)))
	list := legacyNativeAnyArrayTestNew(test, 8)
	input := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 2*width + 1}
	before := list.heap()
	if complete, err := list.append(context.Background(), input, nil, docRangeHeaderHeapEstimate{}, 0); complete || err != nil || list.heap() != before || cap(list.values()) != 0 || input.workBytes != ConversationManifestChunkBytes-2*width+1 {
		test.Fatal("array allocated or appended before its reserve budget was ready", complete, err)
	}
	var payload docRangeHeaderHeapEstimate
	for index := 0; index < 4; index++ {
		childHeap := docRangeHeaderHeapEstimate{bytes: int64(100 + index)}
		legacyNativeAnyArrayTestAppend(test, list, float64(index), childHeap, int64(index))
		payload.include(childHeap)
	}
	prior := list.values()
	want := []any{float64(0), float64(1), float64(2), float64(3)}
	childHeap := docRangeHeaderHeapEstimate{bytes: 900}
	for copied := 1; copied <= len(prior); copied++ {
		input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 2*width, fields: 77, loads: 2}
		complete, err := list.append(context.Background(), input, "pending", childHeap, 33)
		if err != nil || complete || input.workBytes != ConversationManifestChunkBytes || input.fields != 77 || input.loads != 2 || list.copied != copied || len(list.values()) != len(prior) || !reflect.DeepEqual(list.values(), want) || list.backingExcess() != 6 {
			test.Fatal("migration exposed an uncommitted element, payload, or partial prefix", complete, err, list.copied, copied)
		}
		if cap(list.decoded) != 8 || copied < len(prior) && (list.previous == nil || &list.values()[0] != &prior[0]) || copied == len(prior) && list.previous != nil {
			test.Fatal("migration changed capacity or released old backing before its copy completed")
		}
		legacyNativeAnyArrayTestHeap(test, list, payload)
		if copied < len(prior) {
			deferred := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 2*width + 1}
			before = list.heap()
			if complete, err := list.append(context.Background(), deferred, "pending", childHeap, 33); complete || err != nil || list.copied != copied || list.heap() != before || deferred.workBytes != ConversationManifestChunkBytes-2*width+1 {
				test.Fatal("migration copied a fractional interface or committed before it was ready", complete, err)
			}
		}
	}
	input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - width + 1}
	if complete, err := list.append(context.Background(), input, "pending", childHeap, 33); complete || err != nil || len(list.values()) != 4 || input.workBytes != ConversationManifestChunkBytes-width+1 {
		test.Fatal("ready backing appended without a full interface write budget", complete, err)
	}
	input = &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - width}
	if complete, err := list.append(context.Background(), input, "pending", childHeap, 33); !complete || err != nil || input.workBytes != ConversationManifestChunkBytes || len(list.values()) != 5 || list.values()[4] != "pending" || list.backingExcess() != 39 {
		test.Fatal("ready backing did not commit exactly one pending child", complete, err)
	}
	payload.include(childHeap)
	legacyNativeAnyArrayTestHeap(test, list, payload)
	if !reflect.DeepEqual(prior, want) || list.decoded[5:cap(list.decoded)][0] != nil {
		test.Fatal("migration modified old backing or populated an uncommitted tail slot")
	}
}

func TestLegacyNativeAnyArrayCancellationAndMultiQuantumMigration(test *testing.T) {
	const width = int(unsafe.Sizeof(any(nil)))
	count := ConversationManifestChunkBytes / width
	list := legacyNativeAnyArrayTestNew(test, count+1)
	var payload docRangeHeaderHeapEstimate
	for index := 0; index < count; index++ {
		childHeap := docRangeHeaderHeapEstimate{bytes: 3}
		legacyNativeAnyArrayTestAppend(test, list, float64(index), childHeap, 1)
		payload.include(childHeap)
	}
	base, cancel := context.WithCancel(context.Background())
	ctx := &legacyNativeHeapCancelContext{Context: base, cancel: cancel, cancelAt: 4}
	input := &legacyNormalizationInput{ctx: context.Background()}
	complete, err := list.append(ctx, input, "last", docRangeHeaderHeapEstimate{bytes: 101}, 7)
	cancel()
	if complete || !errors.Is(err, context.Canceled) || list.previous == nil || list.copied != canonicalCheckBytes/(2*width) || input.workBytes != canonicalCheckBytes || len(list.values()) != count || list.backingExcess() != int64(count) {
		test.Fatal("migration failed to check cancellation between bounded copy blocks", complete, err, list.copied, input.workBytes)
	}
	legacyNativeAnyArrayTestHeap(test, list, payload)
	canceled, stop := context.WithCancel(context.Background())
	stop()
	before, copied, previous := list.heap(), list.copied, list.previous
	var retainedBefore docRangeHeaderHeapEstimate
	list.retained(&retainedBefore)
	input = &legacyNormalizationInput{ctx: context.Background()}
	complete, err = list.append(canceled, input, "last", docRangeHeaderHeapEstimate{bytes: 101}, 7)
	var retainedAfter docRangeHeaderHeapEstimate
	list.retained(&retainedAfter)
	if complete || !errors.Is(err, context.Canceled) || list.heap() != before || retainedBefore != retainedAfter || list.copied != copied || &list.previous[0] != &previous[0] || input.workBytes != 0 {
		test.Fatal("pre-canceled migration changed committed or retained storage", complete, err)
	}
	input = &legacyNormalizationInput{ctx: canceled, fields: LegacyNormalizationStepEvents, loads: 2}
	complete, err = list.append(context.Background(), input, "last", docRangeHeaderHeapEstimate{bytes: 101}, 7)
	if complete || err != nil || list.previous == nil || input.workBytes != ConversationManifestChunkBytes || list.copied != copied+ConversationManifestChunkBytes/(2*width) || input.fields != LegacyNormalizationStepEvents || input.loads != 2 {
		test.Fatal("migration ignored the shared quantum or retained an earlier call context", complete, err, input.workBytes)
	}
	legacyNativeAnyArrayTestHeap(test, list, payload)
	legacyNativeAnyArrayTestAppend(test, list, "last", docRangeHeaderHeapEstimate{bytes: 101}, 7)
	payload.include(docRangeHeaderHeapEstimate{bytes: 101})
	legacyNativeAnyArrayTestHeap(test, list, payload)
	if list.previous != nil || len(list.values()) != count+1 || cap(list.values()) != count+1 || list.values()[count] != "last" || list.backingExcess() != int64(count)+7 {
		test.Fatal("resumed migration did not release old backing or commit the pending child exactly once")
	}
	for index, value := range list.values()[:count] {
		if value != float64(index) {
			test.Fatal("resumed migration lost a committed interface", index, value)
		}
	}
}

func TestLegacyNativeAnyArrayRejectsInvalidInputWithoutMutation(test *testing.T) {
	const width = int(unsafe.Sizeof(any(nil)))
	maximumInt := int(^uint(0) >> 1)
	for _, maximum := range []int{-1, maximumInt, maximumInt/width + 1} {
		if list, err := newLegacyNativeAnyArray(maximum); list != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("array accepted an overflowing or negative source maximum", maximum, err)
		}
	}
	lazy := legacyNativeAnyArrayTestNew(test, maximumInt/width)
	if cap(lazy.values()) != 0 {
		test.Fatal("array eagerly allocated its source-derived maximum")
	}
	for _, capacity := range []int{-1, maximumInt, maximumInt/width + 1} {
		if backing := legacyNativeAnyArrayBacking(capacity); !errors.Is(backing.err, ErrIntegrity) {
			test.Fatal("array backing estimate accepted byte-size or accounting overflow", capacity, backing)
		}
	}
	if int64(maximumInt) == math.MaxInt64 {
		if backing := legacyNativeAnyArrayBacking(maximumInt / width); !errors.Is(backing.err, ErrIntegrity) {
			test.Fatal("array backing estimate accepted conservative accounting overflow", backing)
		}
	}
	var header docRangeHeaderHeapEstimate
	header.allocation(1, int64(unsafe.Sizeof([]any{})))
	failure := errors.New("child heap failure")
	for _, fixture := range []struct {
		name   string
		input  *legacyNormalizationInput
		heap   docRangeHeaderHeapEstimate
		excess int64
		want   error
	}{
		{name: "nil-input", want: ErrIntegrity},
		{name: "negative-work", input: &legacyNormalizationInput{workBytes: -1}, want: ErrIntegrity},
		{name: "over-budget-work", input: &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes + 1}, want: ErrIntegrity},
		{name: "unbounded-work", input: &legacyNormalizationInput{unbounded: true}, want: ErrIntegrity},
		{name: "negative-child-heap", input: &legacyNormalizationInput{}, heap: docRangeHeaderHeapEstimate{bytes: -1}, want: ErrIntegrity},
		{name: "child-heap-error", input: &legacyNormalizationInput{}, heap: docRangeHeaderHeapEstimate{err: failure}, want: failure},
		{name: "negative-excess", input: &legacyNormalizationInput{}, excess: -1, want: ErrIntegrity},
		{name: "child-heap-overflow", input: &legacyNormalizationInput{}, heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}, want: ErrIntegrity},
		{name: "backing-heap-overflow", input: &legacyNormalizationInput{}, heap: docRangeHeaderHeapEstimate{bytes: math.MaxInt64 - header.bytes}, want: ErrIntegrity},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			list := legacyNativeAnyArrayTestNew(test, 2)
			before := list.heap()
			var priorInput legacyNormalizationInput
			if fixture.input != nil {
				priorInput = *fixture.input
			}
			complete, err := list.append(context.Background(), fixture.input, "rejected", fixture.heap, fixture.excess)
			if complete || !errors.Is(err, fixture.want) || list.heap() != before || len(list.values()) != 0 || cap(list.values()) != 0 || list.previous != nil || list.backingExcess() != 0 || fixture.input != nil && !reflect.DeepEqual(*fixture.input, priorInput) {
				test.Fatal("invalid append mutated storage, counters, or error identity", complete, err)
			}
		})
	}
	var absent *legacyNativeAnyArray
	if complete, err := absent.append(context.Background(), &legacyNormalizationInput{}, nil, docRangeHeaderHeapEstimate{}, 0); complete || !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil array helper accepted an append", complete, err)
	}
	var estimate docRangeHeaderHeapEstimate
	absent.retained(&estimate)
	if estimate != (docRangeHeaderHeapEstimate{}) {
		test.Fatal("nil array helper retained storage", estimate)
	}
	list := legacyNativeAnyArrayTestNew(test, 2)
	legacyNativeAnyArrayTestAppend(test, list, nil, docRangeHeaderHeapEstimate{}, math.MaxInt64)
	before := list.heap()
	if complete, err := list.append(context.Background(), &legacyNormalizationInput{}, nil, docRangeHeaderHeapEstimate{}, 1); complete || !errors.Is(err, ErrIntegrity) || list.heap() != before || len(list.values()) != 1 || list.backingExcess() != math.MaxInt64 {
		test.Fatal("child excess aggregation wrapped or committed an invalid value", complete, err)
	}
}

func TestLegacyNativeAnyArrayAccountingDoesNotWalkChildren(test *testing.T) {
	child := map[string]any{}
	child["cycle"] = child
	list := legacyNativeAnyArrayTestNew(test, 3)
	childHeap := docRangeHeaderHeapEstimate{bytes: 1234}
	legacyNativeAnyArrayTestAppend(test, list, child, childHeap, 23)
	legacyNativeAnyArrayTestAppend(test, list, nil, docRangeHeaderHeapEstimate{}, 0)
	legacyNativeAnyArrayTestAppend(test, list, nil, docRangeHeaderHeapEstimate{}, 0)
	legacyNativeAnyArrayTestHeap(test, list, childHeap)
	if list.backingExcess() != 23 || len(list.values()[0].(map[string]any)) != 1 {
		test.Fatal("array accounting traversed or changed committed dynamic children")
	}
}
