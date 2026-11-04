package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

const legacyCanonicalPreparedValueTestBudget int64 = 64 << 20

type legacyCanonicalPreparedValueTestStore struct {
	io      legacyNativeCheckpointIO
	objects map[ContentHash][]byte
	bytes   int
	reads   int
	writes  int
}

func legacyCanonicalPreparedValueTestStorage(test *testing.T) *legacyCanonicalPreparedValueTestStore {
	test.Helper()
	store, objects := legacyNativeCheckpointStoreFixture(test)
	storage := &legacyCanonicalPreparedValueTestStore{objects: objects}
	storage.io = legacyNativeCheckpointIO{
		Put: func(ctx context.Context, hash ContentHash, body []byte) error {
			storage.bytes += len(body)
			storage.writes++
			return store.Put(ctx, hash, body)
		},
		Read: func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
			storage.bytes += length
			storage.reads++
			return store.Read(ctx, hash, length)
		},
	}
	return storage
}

func legacyCanonicalPreparedValueTestWrite(test *testing.T, value any, numbers *legacyCanonicalNumberValidation, binding ContentHash, storage *legacyCanonicalPreparedValueTestStore, budget int64) legacyNativeCheckpointRef {
	test.Helper()
	ctx := context.Background()
	work, err := newLegacyCanonicalPreparedValueWrite(ctx, value, numbers, binding, storage.io, budget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 0; steps < 1000000; steps++ {
		storage.bytes = 0
		ready, err := work.step(ctx)
		if err != nil || storage.bytes > LegacyCanonicalEncodingBytes {
			test.Fatal("write quantum", steps, storage.bytes, err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained <= 0 || retained > budget {
			test.Fatal("writer ownership", retained, err)
		}
		if ready {
			before := storage.reads + storage.writes
			ref, err := work.take(ctx)
			if err != nil || !ref.valid() || storage.reads+storage.writes != before {
				test.Fatal("write handoff", ref, err)
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("writer did not release", retained, err)
			}
			return ref
		}
	}
	test.Fatal("writer did not complete")
	return legacyNativeCheckpointRef{}
}

func legacyCanonicalPreparedValueTestRestore(test *testing.T, ref legacyNativeCheckpointRef, numbers *legacyCanonicalNumberValidation, binding ContentHash, storage *legacyCanonicalPreparedValueTestStore) (any, docRangeHeaderHeapEstimate) {
	test.Helper()
	ctx := context.Background()
	work, err := newLegacyCanonicalPreparedValueRestore(ctx, ref, numbers, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	for steps := 0; steps < 1000000; steps++ {
		storage.bytes = 0
		ready, err := work.step(ctx)
		if err != nil || storage.bytes > LegacyCanonicalEncodingBytes {
			test.Fatal("restore quantum", steps, storage.bytes, err)
		}
		retained, err := work.retainedBytes()
		if err != nil || retained <= 0 || retained > legacyCanonicalPreparedValueTestBudget {
			test.Fatal("restore ownership", retained, err)
		}
		if ready {
			before := storage.reads + storage.writes
			value, heap, err := work.take(ctx)
			if err != nil || heap.err != nil || storage.reads+storage.writes != before {
				test.Fatal("restore handoff", heap, err)
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("restore did not release", retained, err)
			}
			return value, heap
		}
	}
	test.Fatal("restore did not complete")
	return nil, docRangeHeaderHeapEstimate{}
}

func legacyCanonicalPreparedValueTestHeap(test *testing.T, value any) docRangeHeaderHeapEstimate {
	test.Helper()
	var heap docRangeHeaderHeapEstimate
	switch value := value.(type) {
	case nil:
	case bool:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case int:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case float64:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case string:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
		heap.allocation(int64(len(value)), 1)
	case json.Number:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case *json.Number:
		heap.allocation(1, int64(unsafe.Sizeof(*value)))
	case ContentBlock:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
		heap.allocation(int64(len(value.Text)), 1)
		heap.allocation(int64(len(value.Type)), 1)
	case *legacyCanonicalPreparedObject:
		heap.allocation(1, int64(unsafe.Sizeof(*value)))
	case *legacyCanonicalPreparedReplacement:
		heap.allocation(1, int64(unsafe.Sizeof(*value)))
	case *legacyCanonicalPreparedArray:
		heap.allocation(1, int64(unsafe.Sizeof(*value)))
		heap.include(legacyCanonicalIndependentPageHeap(test, &value.values))
		for position := 0; position < value.values.length(); position++ {
			heap.include(legacyCanonicalPreparedValueTestHeap(test, *value.values.at(position)))
		}
	default:
		test.Fatalf("unexpected prepared type %T", value)
	}
	return heap
}

func TestLegacyCanonicalPreparedValueScalars(test *testing.T) {
	var nilNumber *json.Number
	var nilReplacement *legacyCanonicalReplacement
	var nilBlocks *legacyNativeBlockValues
	var nilPointer *int
	fixtures := []struct {
		name  string
		value any
		want  any
	}{
		{"nil", nil, nil}, {"false", false, false}, {"true", true, true},
		{"negative-int", -99, -99}, {"maximum-int", int(^uint(0) >> 1), int(^uint(0) >> 1)},
		{"negative-zero", math.Copysign(0, -1), math.Copysign(0, -1)},
		{"smallest-float", math.SmallestNonzeroFloat64, math.SmallestNonzeroFloat64},
		{"largest-float", math.MaxFloat64, math.MaxFloat64},
		{"empty", "", ""}, {"bytes", "\x00\xff\ud55c\uad6d\uc5b4", "\x00\xff\ud55c\uad6d\uc5b4"},
		{"kind", EventKind("event"), "event"}, {"role", Role("role"), "role"},
		{"provider", ProviderKind("provider"), "provider"}, {"tier", FidelityTier("tier"), "tier"},
		{"block", ContentBlock{Type: "text", Text: "\ubcf8\ubb38"}, ContentBlock{Type: "text", Text: "\ubcf8\ubb38"}},
		{"nil-number", nilNumber, nil}, {"nil-replacement", nilReplacement, nil},
		{"nil-blocks", nilBlocks, nil}, {"nil-pointer", nilPointer, nil},
		{"nil-array", []any(nil), nil}, {"nil-maps", []map[string]any(nil), nil},
		{"nil-content", []ContentBlock(nil), nil}, {"nil-map", map[string]any(nil), nil},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyCanonicalPreparedValueTestStorage(test)
			binding := HashContent([]byte("prepared-value-scalars"))
			ref := legacyCanonicalPreparedValueTestWrite(test, fixture.value, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
			value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
			if !reflect.DeepEqual(value, fixture.want) {
				test.Fatalf("got %#v, want %#v", value, fixture.want)
			}
			if wanted, ok := fixture.want.(float64); ok && math.Float64bits(value.(float64)) != math.Float64bits(wanted) {
				test.Fatal("float bits changed")
			}
			if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
				test.Fatal("payload ownership", heap, expected)
			}
		})
	}
}

func TestLegacyCanonicalPreparedValueOpaqueObjectsAndReplacement(test *testing.T) {
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("prepared-opaque"))
	object := map[string]any{"unsupported": make(chan int)}
	object["cycle"] = object
	replacement := &legacyCanonicalReplacement{events: []CIREvent{{}}}
	ref := legacyCanonicalPreparedValueTestWrite(test, []any{object, replacement, map[string]any{}, &legacyCanonicalReplacement{events: []CIREvent{}}}, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
	array := value.(*legacyCanonicalPreparedArray)
	if array.values.length() != 4 || (*array.values.at(0)).(*legacyCanonicalPreparedObject).count != 2 ||
		(*array.values.at(1)).(*legacyCanonicalPreparedReplacement).count != 1 ||
		(*array.values.at(2)).(*legacyCanonicalPreparedObject).count != 0 ||
		(*array.values.at(3)).(*legacyCanonicalPreparedReplacement).count != 0 {
		test.Fatal("object or replacement was reprojected")
	}
	if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
		test.Fatal("opaque payload ownership", heap, expected)
	}
	if recaptured := legacyCanonicalPreparedValueTestWrite(test, value, nil, binding, storage, legacyCanonicalPreparedValueTestBudget); recaptured != ref {
		test.Fatal("recapture changed the logical value")
	}
}

func TestLegacyCanonicalPreparedValueNumbersKeepOrdinals(test *testing.T) {
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("prepared-numbers"))
	texts := []string{strings.Clone("1e2"), strings.Clone("1e2"), ""}
	var original, restored legacyCanonicalNumberValidation
	for _, text := range texts {
		if err := original.prepare(text); err != nil {
			test.Fatal(err)
		}
		if err := restored.prepare(strings.Clone(text)); err != nil {
			test.Fatal(err)
		}
	}
	pointed := json.Number(texts[1])
	ref := legacyCanonicalPreparedValueTestWrite(test, []any{json.Number(texts[0]), &pointed, json.Number(texts[2])}, &original, binding, storage, legacyCanonicalPreparedValueTestBudget)
	value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, &restored, binding, storage)
	array := value.(*legacyCanonicalPreparedArray)
	for position := 0; position < array.values.length(); position++ {
		var text string
		switch number := (*array.values.at(position)).(type) {
		case json.Number:
			text = string(number)
		case *json.Number:
			text = string(*number)
		default:
			test.Fatal("number type was lost")
		}
		ordinal, err := restored.index(text)
		if err != nil || ordinal != position || legacyCanonicalNumberKey(text) != legacyCanonicalNumberKey(*restored.pending.at(position)) {
			test.Fatal("number detached from original pending ordinal", position, ordinal, err)
		}
	}
	if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
		test.Fatal("borrowed pending text was counted twice", heap, expected)
	}
}

func TestLegacyCanonicalPreparedValueVisibleArrays(test *testing.T) {
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("prepared-visible-arrays"))
	backing := []any{"visible", make(chan int), math.Inf(1)}
	native := &legacyNativeBlockValues{count: 1}
	if err := native.records.append(legacyNativeBlockValue{block: ContentBlock{Type: "text", Text: "native"}}); err != nil {
		test.Fatal(err)
	}
	if err := native.records.append(legacyNativeBlockValue{block: ContentBlock{Text: "hidden"}}); err != nil {
		test.Fatal(err)
	}
	input := []any{backing[:1], []map[string]any{{"not-walked": make(chan int)}, nil}, []ContentBlock{{Type: "text", Text: "slice"}}, native, []any{}}
	ref := legacyCanonicalPreparedValueTestWrite(test, input, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
	array := value.(*legacyCanonicalPreparedArray)
	for position, count := range []int{1, 2, 1, 1, 0} {
		child := (*array.values.at(position)).(*legacyCanonicalPreparedArray)
		if child.values.length() != count {
			test.Fatal("hidden backing became visible", position, child.values.length())
		}
	}
	if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
		test.Fatal("visible payload ownership", heap, expected)
	}
}

func TestLegacyCanonicalPreparedValueArrayContinuation(test *testing.T) {
	prepared := &legacyCanonicalPreparedArray{}
	for _, value := range []any{make(chan int), "future"} {
		if err := prepared.values.append(value); err != nil {
			test.Fatal(err)
		}
	}
	fixtures := []any{
		[]any{make(chan int), "future"},
		[]map[string]any{{"past": make(chan int)}, {"future": true}},
		[]ContentBlock{{Text: strings.Repeat("past", 100000)}, {Type: "text", Text: "future"}},
		prepared,
		&legacyNativeBlockValues{count: 1},
	}
	for _, fixture := range fixtures {
		storage := legacyCanonicalPreparedValueTestStorage(test)
		binding := HashContent([]byte("prepared-continuation"))
		length, err := legacyCanonicalPreparedArrayLength(fixture)
		if err != nil {
			test.Fatal(err)
		}
		ref := legacyCanonicalPreparedValueTestWrite(test, legacyCanonicalPreparedArraySlice{value: fixture, position: 1}, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
		value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
		array := value.(*legacyCanonicalPreparedArray)
		if array.values.length() != length || *array.values.at(0) != nil {
			test.Fatal("consumed prefix was revisited")
		}
		if length > 1 && *array.values.at(1) == nil {
			test.Fatal("future value was discarded")
		}
		if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
			test.Fatal("continuation payload ownership", heap, expected)
		}
	}
	for _, fixture := range []legacyCanonicalPreparedArraySlice{
		{value: []any{}, position: -1}, {value: []any{}, position: 1}, {value: map[string]any{}, position: 0},
	} {
		storage := legacyCanonicalPreparedValueTestStorage(test)
		work, err := newLegacyCanonicalPreparedValueWrite(context.Background(), fixture, nil, HashContent([]byte("bounds")), storage.io, legacyCanonicalPreparedValueTestBudget)
		if work != nil || !errors.Is(err, ErrIntegrity) || storage.writes != 0 {
			test.Fatal("invalid continuation admitted", err)
		}
	}
}

func TestLegacyCanonicalPreparedValuePagedAndDeep(test *testing.T) {
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("prepared-paged"))
	wide := make([]any, 1103)
	for position := range wide {
		wide[position] = position
	}
	ref := legacyCanonicalPreparedValueTestWrite(test, wide, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
	array := value.(*legacyCanonicalPreparedArray)
	for position := range wide {
		if *array.values.at(position) != position {
			test.Fatal("paged order changed", position)
		}
	}
	if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
		test.Fatal("paged payload ownership", heap, expected)
	}
	var deep any = true
	const depth = 2048
	for position := 0; position < depth; position++ {
		deep = []any{deep}
	}
	ref = legacyCanonicalPreparedValueTestWrite(test, deep, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	value, _ = legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, storage)
	for position := 0; position < depth; position++ {
		value = *value.(*legacyCanonicalPreparedArray).values.at(0)
	}
	if value != true {
		test.Fatal("deep cursor lost its leaf")
	}
}

func TestLegacyCanonicalPreparedValuePortableTextOwnership(test *testing.T) {
	storage := legacyCanonicalPreparedValueTestStorage(test)
	binding := HashContent([]byte("prepared-portable-text"))
	text := strings.Repeat("\ud55c\uae00\x00\xff", 500000)
	ref := legacyCanonicalPreparedValueTestWrite(test, text, nil, binding, storage, 2<<20)
	other := legacyCanonicalPreparedValueTestStorage(test)
	for hash, body := range storage.objects {
		other.objects[hash] = bytes.Clone(body)
	}
	value, heap := legacyCanonicalPreparedValueTestRestore(test, ref, nil, binding, other)
	if value != text || unsafe.StringData(value.(string)) == unsafe.StringData(text) {
		test.Fatal("text was not restored from portable storage")
	}
	if expected := legacyCanonicalPreparedValueTestHeap(test, value); heap != expected {
		test.Fatal("text temporary workspace escaped handoff", heap, expected)
	}
	limited := legacyCanonicalPreparedValueTestStorage(test)
	for hash, body := range storage.objects {
		limited.objects[hash] = body
	}
	work, err := newLegacyCanonicalPreparedValueRestore(context.Background(), ref, nil, binding, limited.io, 2<<20)
	if err != nil {
		test.Fatal(err)
	}
	for steps := 0; steps < 10; steps++ {
		_, err = work.step(context.Background())
		if err != nil {
			break
		}
	}
	if !errors.Is(err, ErrDocumentResourceLimit) || limited.reads != 1 {
		test.Fatal("text payload was read before heap admission", limited.reads, err)
	}
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("failed restore retained payload", retained, err)
	}
}

func TestLegacyCanonicalPreparedValueFencesAndCleanup(test *testing.T) {
	ctx := context.Background()
	binding := HashContent([]byte("prepared-fences"))
	storage := legacyCanonicalPreparedValueTestStorage(test)
	for _, input := range []any{math.Inf(1), math.Inf(-1), math.NaN(), int64(1), make(chan int), json.Number("unregistered")} {
		work, err := newLegacyCanonicalPreparedValueWrite(ctx, input, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
		if work != nil || err == nil {
			test.Fatal("unsupported value admitted", input)
		}
	}
	if work, err := newLegacyCanonicalPreparedValueWrite(ctx, true, nil, binding, storage.io, 1); work != nil || !errors.Is(err, ErrDocumentResourceLimit) {
		test.Fatal("writer metadata was not admitted", err)
	}
	ref := legacyCanonicalPreparedValueTestWrite(test, "text", nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	var saved legacyCanonicalPreparedValueRecord
	if err := legacyNativeCheckpointReadJSON(ctx, storage.io, ref, &saved); err != nil {
		test.Fatal(err)
	}
	mutations := []func(*legacyCanonicalPreparedValueRecord){
		func(record *legacyCanonicalPreparedValueRecord) { record.Kind = "other" },
		func(record *legacyCanonicalPreparedValueRecord) { record.Version++ },
		func(record *legacyCanonicalPreparedValueRecord) { record.Binding = HashContent([]byte("other")) },
		func(record *legacyCanonicalPreparedValueRecord) { record.Count = 1 },
		func(record *legacyCanonicalPreparedValueRecord) { record.Number = 1 },
		func(record *legacyCanonicalPreparedValueRecord) { record.Texts[1] = record.Texts[0] },
		func(record *legacyCanonicalPreparedValueRecord) {
			record.Tag = legacyCanonicalPreparedBool
			record.Bits = 2
		},
	}
	for _, mutate := range mutations {
		record := saved
		mutate(&record)
		bad, err := legacyNativeCheckpointWriteJSON(ctx, storage.io, record)
		if err != nil {
			test.Fatal(err)
		}
		work, err := newLegacyCanonicalPreparedValueRestore(ctx, bad, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
		if err != nil {
			test.Fatal(err)
		}
		if ready, err := work.step(ctx); ready || !errors.Is(err, ErrIntegrity) {
			test.Fatal("record fence bypass", ready, err)
		}
		if _, _, err := work.take(ctx); !errors.Is(err, ErrIntegrity) {
			test.Fatal("restore failure was not latched", err)
		}
		if retained, err := work.retainedBytes(); retained != 0 || err != nil {
			test.Fatal("record failure retained workspace", retained, err)
		}
	}
	storage.objects[ref.Hash][0] ^= 1
	corrupt, err := newLegacyCanonicalPreparedValueRestore(ctx, ref, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := corrupt.step(ctx); !errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("hash fence bypass", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if work, err := newLegacyCanonicalPreparedValueWrite(canceled, nil, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget); work != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("canceled writer admitted", err)
	}
	work, err := newLegacyCanonicalPreparedValueWrite(ctx, []any{true}, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	before := storage.reads + storage.writes
	if _, err := work.step(canceled); !errors.Is(err, context.Canceled) || storage.reads+storage.writes != before {
		test.Fatal("canceled writer performed IO", err)
	}
	if _, err := work.take(ctx); !errors.Is(err, context.Canceled) {
		test.Fatal("writer cancellation was not latched", err)
	}
	work.close()
	work.close()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("canceled writer retained workspace", retained, err)
	}
}

func TestLegacyCanonicalPreparedValuePerStepAdmissionAndRestoreCancellation(test *testing.T) {
	ctx := context.Background()
	binding := HashContent([]byte("prepared-admission"))
	storage := legacyCanonicalPreparedValueTestStorage(test)
	ref := legacyCanonicalPreparedValueTestWrite(test, []any{"payload"}, nil, binding, storage, legacyCanonicalPreparedValueTestBudget)
	writer, err := newLegacyCanonicalPreparedValueWrite(ctx, []any{"payload"}, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	retained, err := writer.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	scratch := legacyCanonicalPreparedValueWorkspace()
	writer.budget = retained + scratch.bytes - 1
	before := storage.reads + storage.writes
	if _, err := writer.step(ctx); !errors.Is(err, ErrDocumentResourceLimit) || storage.reads+storage.writes != before {
		test.Fatal("writer performed work before admission", err)
	}
	restore, err := newLegacyCanonicalPreparedValueRestore(ctx, ref, nil, binding, storage.io, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	retained, err = restore.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	restore.budget = retained + scratch.bytes - 1
	if _, err := restore.step(ctx); !errors.Is(err, ErrDocumentResourceLimit) || storage.reads+storage.writes != before {
		test.Fatal("restore performed IO before admission", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	store := storage.io
	store.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		body, err := storage.io.Read(current, hash, length)
		cancel()
		return body, err
	}
	restore, err = newLegacyCanonicalPreparedValueRestore(canceled, ref, nil, binding, store, legacyCanonicalPreparedValueTestBudget)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := restore.step(canceled); !errors.Is(err, context.Canceled) {
		test.Fatal("restore ignored cancellation during IO", err)
	}
	if _, _, err := restore.take(ctx); !errors.Is(err, context.Canceled) {
		test.Fatal("restore cancellation was not latched", err)
	}
	restore.close()
	restore.close()
	if retained, err := restore.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("canceled restore retained workspace", retained, err)
	}
}
