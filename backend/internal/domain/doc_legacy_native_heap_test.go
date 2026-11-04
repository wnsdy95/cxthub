package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

type legacyNativeHeapString string

type legacyNativeHeapStrings []legacyNativeHeapString

type legacyNativeHeapMap map[legacyNativeHeapString]any

type legacyNativeHeapInteger int64

func checkLegacyNativeHeap(test testing.TB, value reflect.Value, expected docRangeHeaderHeapEstimate) {
	test.Helper()
	actual, err := legacyNativeValueHeap(context.Background(), value)
	if err != nil || actual.err != nil || expected.err != nil || actual.bytes != expected.bytes {
		test.Fatalf("native heap mismatch: actual=%+v expected=%+v error=%v", actual, expected, err)
	}
}

func TestLegacyNativeHeapEmptyAndNilStorage(test *testing.T) {
	var emptyInterface any
	var emptyPointer *string
	var emptySlice []string
	var emptyMap map[string]any
	values := []struct {
		name  string
		value reflect.Value
	}{
		{"invalid", reflect.Value{}},
		{"interface", reflect.ValueOf(&emptyInterface).Elem()},
		{"pointer", reflect.ValueOf(emptyPointer)},
		{"slice", reflect.ValueOf(emptySlice)},
		{"map", reflect.ValueOf(emptyMap)},
		{"empty_slice", reflect.ValueOf([]string{})},
		{"empty_struct", reflect.ValueOf(struct{}{})},
		{"zero_width_slice", reflect.ValueOf(make([]struct{}, 0, 3))},
	}
	for _, fixture := range values {
		test.Run(fixture.name, func(test *testing.T) {
			var expected docRangeHeaderHeapEstimate
			if fixture.value.IsValid() && fixture.value.Type().Size() != 0 {
				expected.allocation(1, int64(fixture.value.Type().Size()))
			}
			checkLegacyNativeHeap(test, fixture.value, expected)
		})
	}
	value := reflect.ValueOf(map[string]any{})
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(value.Type().Size()))
	expected.allocation(1, 64)
	expected.allocation(8, int64(value.Type().Key().Size()+value.Type().Elem().Size())+8)
	checkLegacyNativeHeap(test, value, expected)
}

func TestLegacyNativeHeapPrimitiveKindsAndAliases(test *testing.T) {
	for _, value := range []any{
		true, int(1), int8(1), int16(1), int32(1), int64(1),
		uint(1), uint8(1), uint16(1), uint32(1), uint64(1), uintptr(1),
		float32(1.5), float64(1.5), legacyNativeHeapInteger(7),
		"text", legacyNativeHeapString("alias"), EventMessage, RoleUser,
		ProviderKind("codex"), FidelityTier("tier"), ContentHash("hash"), json.Number("123.450e-8"),
	} {
		reflected := reflect.ValueOf(value)
		test.Run(reflected.Type().String(), func(test *testing.T) {
			var expected docRangeHeaderHeapEstimate
			expected.allocation(1, int64(reflected.Type().Size()))
			if reflected.Kind() == reflect.String {
				expected.allocation(int64(reflected.Len()), 1)
			}
			checkLegacyNativeHeap(test, reflected, expected)
		})
	}
	stringsAlias := legacyNativeHeapStrings{"first", "backing-only"}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(stringsAlias)))
	expected.allocation(int64(cap(stringsAlias)), int64(unsafe.Sizeof(legacyNativeHeapString(""))))
	for _, text := range stringsAlias {
		expected.allocation(int64(len(text)), 1)
	}
	checkLegacyNativeHeap(test, reflect.ValueOf(stringsAlias[:0]), expected)
}

func TestLegacyNativeHeapPointersInterfacesAndUnexportedFields(test *testing.T) {
	text := legacyNativeHeapString("pointed string")
	fixture := struct {
		presence eventFieldPresence
		texts    [2]legacyNativeHeapString
		pointer  *legacyNativeHeapString
		dynamic  any
	}{1, [2]legacyNativeHeapString{"first", "second"}, &text, legacyNativeHeapInteger(9)}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(fixture)))
	for _, value := range fixture.texts {
		expected.allocation(int64(len(value)), 1)
	}
	expected.allocation(1, int64(unsafe.Sizeof(text)))
	expected.allocation(int64(len(text)), 1)
	expected.allocation(1, int64(unsafe.Sizeof(legacyNativeHeapInteger(0))))
	checkLegacyNativeHeap(test, reflect.ValueOf(fixture), expected)
	var dynamic any = &text
	expected = docRangeHeaderHeapEstimate{}
	expected.allocation(1, int64(unsafe.Sizeof(dynamic)))
	expected.allocation(1, int64(unsafe.Sizeof(&text)))
	expected.allocation(1, int64(unsafe.Sizeof(text)))
	expected.allocation(int64(len(text)), 1)
	checkLegacyNativeHeap(test, reflect.ValueOf(&dynamic).Elem(), expected)
	var nilPointer *legacyNativeHeapString
	dynamic = nilPointer
	expected = docRangeHeaderHeapEstimate{}
	expected.allocation(1, int64(unsafe.Sizeof(dynamic)))
	expected.allocation(1, int64(unsafe.Sizeof(nilPointer)))
	checkLegacyNativeHeap(test, reflect.ValueOf(&dynamic).Elem(), expected)
	number := json.Number("123456789.123456789")
	expected = docRangeHeaderHeapEstimate{}
	expected.allocation(1, int64(unsafe.Sizeof(&number)))
	expected.allocation(1, int64(unsafe.Sizeof(number)))
	expected.allocation(int64(len(number)), 1)
	checkLegacyNativeHeap(test, reflect.ValueOf(&number), expected)
}

func TestLegacyNativeHeapSliceCapacityIncludesNestedReferences(test *testing.T) {
	type nestedReferences struct {
		texts   [1]legacyNativeHeapString
		pointer *legacyNativeHeapString
	}
	type element struct {
		nested nestedReferences
		values legacyNativeHeapMap
	}
	pointed := legacyNativeHeapString(strings.Repeat("pointer", 64))
	number := json.Number("123456789.987654321")
	values := make([]element, 2)
	values[1].nested = nestedReferences{[1]legacyNativeHeapString{"hidden array"}, &pointed}
	values[1].values = legacyNativeHeapMap{"key": []any{number}}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(values)))
	expected.allocation(int64(cap(values)), int64(unsafe.Sizeof(element{})))
	expected.allocation(int64(len(values[1].nested.texts[0])), 1)
	expected.allocation(1, int64(unsafe.Sizeof(pointed)))
	expected.allocation(int64(len(pointed)), 1)
	expected.allocation(1, 64)
	expected.allocation(8, int64(unsafe.Sizeof(legacyNativeHeapString(""))+unsafe.Sizeof(any(nil)))+8)
	expected.allocation(int64(len("key")), 1)
	expected.allocation(1, int64(unsafe.Sizeof([]any{})))
	expected.allocation(1, int64(unsafe.Sizeof(any(nil))))
	expected.allocation(1, int64(unsafe.Sizeof(number)))
	expected.allocation(int64(len(number)), 1)
	for length := 0; length <= len(values); length++ {
		checkLegacyNativeHeap(test, reflect.ValueOf(values[:length]), expected)
	}
}

func TestLegacyNativeHeapMapBucketsKeysAndDynamicValues(test *testing.T) {
	values := make(legacyNativeHeapMap)
	for position := 0; position < 33; position++ {
		values[legacyNativeHeapString(fmt.Sprintf("key-%d", position))] = legacyNativeHeapString(fmt.Sprintf("value-%d", position))
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(values)))
	expected.allocation(1, 64)
	expected.allocation(int64(len(values)), int64(unsafe.Sizeof(legacyNativeHeapString(""))+unsafe.Sizeof(any(nil)))+8)
	for key, value := range values {
		expected.allocation(int64(len(key)), 1)
		expected.allocation(1, int64(unsafe.Sizeof(legacyNativeHeapString(""))))
		expected.allocation(int64(len(value.(legacyNativeHeapString))), 1)
	}
	checkLegacyNativeHeap(test, reflect.ValueOf(values), expected)
}

func TestLegacyNativeHeapOriginalWireRetainsDuplicateSlicesAndNonemittedInput(test *testing.T) {
	type wire CIREvent
	retained := strings.Repeat("retained", 512)
	raw := []byte(fmt.Sprintf(`{
		"kind":"message","seq":7,"role":"user",
		"blocks":[{"type":"text","text":"visible"},{"type":"text","text":%q}],
		"blocks":[null],
		"replacement":[{"kind":"message"},{"kind":"tool_call","input":{"hidden":%q}}],
		"replacement":[null],
		"input":{"retained":%q,"nested":[{"array":[true,null,"payload"]}],"nullValue":null},
		"input":{"added":false},
		"output":{"items":[null,true,2.25,"output"]},
		"is_error":false,"cross_replayable":null,
		"provider_metadata":{"turn_id":"turn","create_time":123.450e-8},
		"locked":{"provider":"codex","scheme":"encrypted_content","blob":%q},
		"locked":null,"agent_message":false
	}`, retained, retained, retained, retained))
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		test.Fatal(err)
	}
	event := CIREvent(decoded)
	if event.Kind != EventMessage || len(event.Blocks) != 1 || cap(event.Blocks) < 2 || event.Blocks[0].Text != "visible" ||
		len(event.Replacement) != 1 || cap(event.Replacement) < 2 || len(event.Input) != 4 ||
		event.Input["retained"] != retained || event.Locked != nil || event.CrossReplayable != nil ||
		event.IsError == nil || *event.IsError || event.ProviderMetadata == nil ||
		event.ProviderMetadata.CreateTime == nil || *event.ProviderMetadata.CreateTime != "123.450e-8" {
		test.Fatal("fixture lost original native decoding shape", event)
	}
	if event.Blocks[:cap(event.Blocks)][1].Text != retained ||
		event.Replacement[:cap(event.Replacement)][1].Input["hidden"] != retained {
		test.Fatal("duplicate shrink did not retain backing-only references")
	}
	whole, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(event))
	if err != nil {
		test.Fatal(err)
	}
	var original CIREvent
	if err := json.Unmarshal(raw, &original); err != nil {
		test.Fatal(err)
	}
	if original.fieldPresence == 0 {
		test.Fatal("original event did not preserve unexported field presence")
	}
	checkLegacyNativeHeap(test, reflect.ValueOf(original), whole)
	withoutInput := event
	withoutInput.Input = nil
	cleared, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(withoutInput))
	if err != nil {
		test.Fatal(err)
	}
	input, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(event.Input))
	var inputStorage docRangeHeaderHeapEstimate
	inputStorage.allocation(1, int64(unsafe.Sizeof(event.Input)))
	if err != nil || input.bytes <= int64(len(retained)) || whole.bytes-cleared.bytes != input.bytes-inputStorage.bytes {
		test.Fatal("message input retention or allocation charging changed", whole, cleared, input, err)
	}
	for _, name := range []string{"Blocks", "Replacement"} {
		field := reflect.ValueOf(event).FieldByName(name)
		short, err := legacyNativeValueHeap(context.Background(), field)
		if err != nil {
			test.Fatal(err)
		}
		checkLegacyNativeHeap(test, field.Slice(0, field.Cap()), short)
		trimmed := reflect.MakeSlice(field.Type(), field.Len(), field.Len())
		reflect.Copy(trimmed, field)
		compact, err := legacyNativeValueHeap(context.Background(), trimmed)
		if err != nil || short.bytes-compact.bytes < int64(len(retained)) {
			test.Fatal("original wire backing-only allocation was not counted", name, short, compact, err)
		}
	}
	if err := json.Unmarshal([]byte(`{"blocks":null,"replacement":null,"input":null,"output":null,"is_error":null,"provider_metadata":null}`), &decoded); err != nil {
		test.Fatal(err)
	}
	if decoded.Blocks != nil || decoded.Replacement != nil || decoded.Input != nil || decoded.Output != nil ||
		decoded.IsError != nil || decoded.ProviderMetadata != nil {
		test.Fatal("null did not release native references")
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(decoded)))
	expected.allocation(int64(len(decoded.Kind)), 1)
	expected.allocation(int64(len(decoded.Role)), 1)
	checkLegacyNativeHeap(test, reflect.ValueOf(decoded), expected)
}

func TestLegacyNativeHeapEnvelopeSourceModelsDuplicateTail(test *testing.T) {
	retained := strings.Repeat("old-model", 512)
	var envelope CIREnvelope
	raw := []byte(fmt.Sprintf(`{"source_models":["first",%q,"last"],"source_models":[null]}`, retained))
	if err := json.Unmarshal(raw, &envelope); err != nil {
		test.Fatal(err)
	}
	if len(envelope.SourceModels) != 1 || cap(envelope.SourceModels) < 3 || envelope.SourceModels[0] != "first" {
		test.Fatal("source model fixture lost duplicate-key retention")
	}
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(envelope)))
	expected.allocation(int64(cap(envelope.SourceModels)), int64(unsafe.Sizeof("")))
	for _, text := range envelope.SourceModels[:cap(envelope.SourceModels)] {
		expected.allocation(int64(len(text)), 1)
	}
	checkLegacyNativeHeap(test, reflect.ValueOf(envelope), expected)
	envelope.SourceModels = envelope.SourceModels[:0]
	checkLegacyNativeHeap(test, reflect.ValueOf(envelope), expected)
	if err := json.Unmarshal([]byte(`{"source_models":null}`), &envelope); err != nil {
		test.Fatal(err)
	}
	if envelope.SourceModels != nil {
		test.Fatal("source model null kept a backing slice")
	}
	expected = docRangeHeaderHeapEstimate{}
	expected.allocation(1, int64(unsafe.Sizeof(envelope)))
	checkLegacyNativeHeap(test, reflect.ValueOf(envelope), expected)
}

func TestLegacyNativeHeapAcceptsMaximumNativeJSONDepth(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
	}{
		{"arrays", "[", "]"},
		{"objects", `{"child":`, "}"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			const depth = 10000
			raw := []byte(strings.Repeat(fixture.prefix, depth) + "null" + strings.Repeat(fixture.suffix, depth))
			var decoded any
			if err := json.Unmarshal(raw, &decoded); err != nil {
				test.Fatal(err)
			}
			heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(decoded))
			if err != nil || heap.err != nil || heap.bytes <= depth || heap.bytes >= 4096*depth {
				test.Fatal("native maximum depth rejected or repeatedly amplified", heap, err)
			}
		})
	}
	type wire CIREvent
	raw := []byte(`{"kind":"tool_result","output":` + strings.Repeat("[", 9999) + "null" + strings.Repeat("]", 9999) + "}")
	var decoded wire
	if err := json.Unmarshal(raw, &decoded); err != nil {
		test.Fatal(err)
	}
	if heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(decoded)); err != nil || heap.err != nil || heap.bytes <= 0 {
		test.Fatal("native maximum-depth event rejected", heap, err)
	}
}

type legacyNativeHeapCancelContext struct {
	context.Context
	cancel   context.CancelFunc
	checks   int
	cancelAt int
}

func (ctx *legacyNativeHeapCancelContext) Err() error {
	ctx.checks++
	if ctx.cancel != nil && ctx.checks == ctx.cancelAt {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestLegacyNativeHeapCancellationThroughoutTraversal(test *testing.T) {
	text := "pointer"
	var dynamic any = &text
	for _, value := range []reflect.Value{
		{}, reflect.ValueOf("text"), reflect.ValueOf(&dynamic).Elem(),
		reflect.ValueOf([]any{nil, "tail", []string{"nested"}}),
		reflect.ValueOf(map[string]any{"first": &text, "second": []any{1.5, true}}),
		reflect.ValueOf(struct{ hidden [2]string }{[2]string{"first", "second"}}),
	} {
		baseline := &legacyNativeHeapCancelContext{Context: context.Background()}
		if _, err := legacyNativeValueHeap(baseline, value); err != nil {
			test.Fatal(err)
		}
		for checkpoint := 1; checkpoint <= baseline.checks; checkpoint++ {
			base, cancel := context.WithCancel(context.Background())
			ctx := &legacyNativeHeapCancelContext{Context: base, cancel: cancel, cancelAt: checkpoint}
			_, err := legacyNativeValueHeap(ctx, value)
			cancel()
			if !errors.Is(err, context.Canceled) {
				test.Fatal("native traversal missed cancellation", value.Kind(), checkpoint, baseline.checks, err)
			}
		}
	}
}

func TestLegacyNativeHeapCyclesAndUnsupportedKinds(test *testing.T) {
	type link struct{ next *link }
	pointer := &link{}
	pointer.next = pointer
	mapping := map[string]any{}
	mapping["self"] = mapping
	sequence := make([]any, 1)
	sequence[0] = sequence
	for _, value := range []any{pointer, mapping, sequence} {
		var heap docRangeHeaderHeapEstimate
		err := legacyNativeValueHeapWalk(context.Background(), reflect.ValueOf(value), legacyNativeHeapMaxDepth-4, true, &heap)
		if !errors.Is(err, ErrValidation) {
			test.Fatal("cycle depth guard did not terminate", reflect.TypeOf(value), err)
		}
		base, cancel := context.WithCancel(context.Background())
		ctx := &legacyNativeHeapCancelContext{Context: base, cancel: cancel, cancelAt: 32}
		_, err = legacyNativeValueHeap(ctx, reflect.ValueOf(value))
		cancel()
		if !errors.Is(err, context.Canceled) {
			test.Fatal("cycle traversal ignored cancellation", reflect.TypeOf(value), err)
		}
	}
	for _, value := range []any{
		complex64(1), complex128(1), make(chan int), func() {}, unsafe.Pointer(nil),
		[]any{func() {}}, map[string]any{"unsupported": make(chan int)},
		struct{ hidden chan int }{},
	} {
		if _, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(value)); !errors.Is(err, ErrValidation) {
			test.Fatal("unsupported kind did not return validation error", reflect.TypeOf(value), err)
		}
	}
}

func TestLegacyNativeHeapOverflow(test *testing.T) {
	text := "backing"
	var dynamic any = text
	for _, value := range []reflect.Value{
		reflect.ValueOf(text), reflect.ValueOf(&text), reflect.ValueOf(&dynamic).Elem(),
		reflect.ValueOf(struct{ first, second string }{text, text}),
		reflect.ValueOf([]any{text, text}), reflect.ValueOf([2]string{text, text}),
		reflect.ValueOf(map[string]any{"key": text}),
	} {
		for _, includeStorage := range []bool{false, true} {
			var baseline docRangeHeaderHeapEstimate
			if err := legacyNativeValueHeapWalk(context.Background(), value, 0, includeStorage, &baseline); err != nil || baseline.bytes <= 0 {
				test.Fatal("invalid overflow baseline", value.Type(), baseline, err)
			}
			for _, initial := range []int64{-1, math.MaxInt64 - 1, math.MaxInt64 - baseline.bytes + 1} {
				heap := docRangeHeaderHeapEstimate{bytes: initial}
				err := legacyNativeValueHeapWalk(context.Background(), value, 0, includeStorage, &heap)
				if !errors.Is(err, ErrIntegrity) || !errors.Is(heap.err, ErrIntegrity) || (initial >= 0 && heap.bytes < initial) {
					test.Fatal("native heap overflow accepted or wrapped", value.Type(), includeStorage, heap, err)
				}
			}
		}
	}
}
