package domain

import (
	"bytes"
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

func legacyCanonicalIndependentPageHeap[Value any](test testing.TB, values *legacyCanonicalPages[Value]) docRangeHeaderHeapEstimate {
	test.Helper()
	var heap docRangeHeaderHeapEstimate
	if values.count < 0 || values.depth < 0 || values.depth > (int(unsafe.Sizeof(uintptr(0)))*8-1)/legacyCanonicalPageShift {
		test.Fatal("paged metadata has an invalid count or depth", values.count, values.depth)
	}
	var empty Value
	if values.single != nil {
		if values.inline != nil || values.root != nil || values.depth != 0 || values.nodes != 0 || values.pages != 0 || values.count > 1 {
			test.Fatal("single metadata overlaps other backing or exceeds one value")
		}
		if width := unsafe.Sizeof(*values.single); width != 0 {
			heap.allocation(1, int64(width))
		}
		if values.count == 0 && !reflect.DeepEqual(*values.single, empty) {
			test.Fatal("unused single slot retains a released value")
		}
	}
	if values.inline != nil {
		if values.root != nil || values.depth != 0 || values.count > len(values.inline) {
			test.Fatal("inline metadata overlaps paged storage or exceeds its capacity")
		}
		if width := unsafe.Sizeof(*values.inline); width != 0 {
			heap.allocation(1, int64(width))
		}
		for position := values.count; position < len(values.inline); position++ {
			if !reflect.DeepEqual(values.inline[position], empty) {
				test.Fatal("unused inline slot retains a released value", position)
			}
		}
	}
	nodes, pages, visible := 0, 0, 0
	seenNodes := make(map[*legacyCanonicalPageNode[Value]]bool)
	seenPages := make(map[*[legacyCanonicalPageValues]Value]bool)
	var walk func(*legacyCanonicalPageNode[Value], int, int)
	walk = func(node *legacyCanonicalPageNode[Value], level, start int) {
		if seenNodes[node] || level < 0 {
			test.Fatal("paged metadata aliases a node or exceeds its recorded depth")
		}
		seenNodes[node] = true
		nodes++
		heap.allocation(1, int64(unsafe.Sizeof(*node)))
		if node.values != nil {
			if level != 0 || unsafe.Sizeof(*node.values) != 0 && seenPages[node.values] {
				test.Fatal("paged metadata aliases a value page or stores it above leaf depth")
			}
			seenPages[node.values] = true
			pages++
			if width := unsafe.Sizeof(*node.values); width != 0 {
				heap.allocation(1, int64(width))
			}
			for position := range node.values {
				if start+position < values.count {
					visible++
				} else if !reflect.DeepEqual(node.values[position], empty) {
					test.Fatal("unused page slot retains a released value", start+position)
				}
			}
		} else if level == 0 {
			test.Fatal("leaf node has no allocated value page")
		}
		for position, child := range node.children {
			if child != nil {
				if level == 0 {
					test.Fatal("leaf node retains an unexpected child")
				}
				walk(child, level-1, start+(position<<(legacyCanonicalPageShift*level)))
			}
		}
	}
	if values.root != nil {
		walk(values.root, values.depth, 0)
		if visible != values.count {
			test.Fatal("paged metadata omits visible slots", visible, values.count)
		}
	} else if values.depth != 0 || values.inline == nil && values.single == nil && values.count != 0 {
		test.Fatal("paged metadata has live values or depth without backing storage")
	}
	if nodes != values.nodes || pages != values.pages || heap.err != nil {
		test.Fatal("independent page allocations differ from recorded metadata", nodes, values.nodes, pages, values.pages, heap)
	}
	return heap
}

func collectLegacyCanonicalEncoding(test testing.TB, encoding *LegacyCanonicalEncoding) ([]byte, int) {
	test.Helper()
	var result []byte
	var preparedKeyWork uint64
	for steps := 1; steps <= 1<<16; steps++ {
		before := encoding.keyWork
		wasPreparing := encoding.preparation != nil
		beforeInspectWork := encoding.inspectWork
		beforeNext, beforePosition := encoding.numbers.next, encoding.numbers.position
		validating := beforeNext < encoding.numbers.pending.length()
		if !wasPreparing && validating {
			assertLegacyCanonicalNumberPendingConsumerState(test, encoding, preparedKeyWork)
		}
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
			test.Fatalf("oversized chunk: len=%d cap=%d", len(chunk), cap(chunk))
		}
		if wasPreparing {
			if encoding.preparation == nil {
				preparedKeyWork = encoding.keyWork
			}
			if encoding.preparation != nil && (len(chunk) != 0 || complete) {
				test.Fatal("native preparation exposed canonical output", len(chunk), complete)
			}
			if encoding.inspectWork <= beforeInspectWork {
				test.Fatal("native preparation made no inspection progress")
			}
			if encoding.inspectWork-beforeInspectWork > legacyCanonicalEncodingActions {
				test.Fatal("native preparation exceeded its action quantum")
			}
		} else if validating {
			if len(chunk) != 0 || complete {
				test.Fatal("numeric preparation exposed canonical output", len(chunk), complete)
			}
			assertLegacyCanonicalNumberPendingConsumerState(test, encoding, preparedKeyWork)
			afterNext, afterPosition := encoding.numbers.next, encoding.numbers.position
			if afterNext < beforeNext || afterNext-beforeNext > 1024 || afterNext == beforeNext && afterPosition <= beforePosition {
				test.Fatal("number preparation stalled or exceeded its item quantum", beforeNext, beforePosition, afterNext, afterPosition)
			}
			consumed := afterPosition - beforePosition
			for position := beforeNext; position < afterNext; position++ {
				consumed += len(*encoding.numbers.pending.at(position))
			}
			if consumed < 0 || consumed > 64<<10 {
				test.Fatal("number preparation reset or exceeded its shared character quantum", consumed)
			}
		} else if len(chunk) == 0 && !complete && encoding.keyWork <= before {
			test.Fatal("incomplete encoding made no output or ordering progress")
		}
		result = append(result, chunk...)
		if complete {
			assertLegacyCanonicalNumberPendingReleased(test, encoding, nil)
			return result, steps
		}
	}
	test.Fatal("encoding did not terminate")
	return nil, 0
}

func assertLegacyCanonicalEncodingValue(test testing.TB, value any) {
	test.Helper()
	want, err := json.Marshal(value)
	if err != nil {
		test.Fatal(err)
	}
	encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
	if err != nil {
		test.Fatal(err)
	}
	got, _ := collectLegacyCanonicalEncoding(test, encoding)
	if !bytes.Equal(got, want) {
		test.Fatalf("canonical bytes differ: got=%q want=%q", got, want)
	}
	chunk, complete, err := encoding.Next(context.Background())
	if err != nil || !complete || len(chunk) != 0 {
		test.Fatal("completed encoding changed on retry", complete, err)
	}
}

func TestLegacyCanonicalEncodingMarshalParity(test *testing.T) {
	number := json.Number("1787683260.123456789")
	values := []any{
		nil, false, true, int(9007199254740993), math.Copysign(0, -1),
		1e-7, 1e21, math.SmallestNonzeroFloat64, math.MaxFloat64,
		"", "\"\\/\b\f\n\r\t\x00\x01\x1f<&>\u2028\u2029한국🙂\ufffd\xff\x80",
		EventMessage, RoleAssistant, ProviderKind("codex"), FidelityTier("exact"),
		map[string]any(nil), map[string]any{}, []any(nil), []any{},
		[]map[string]any(nil), []map[string]any{}, (*json.Number)(nil), &number,
		map[string]any{"z": []map[string]any{nil, {}, {"z": number, "a": []any{nil, false}}}, "a": map[string]any{"z": 1, "a": 2}},
	}
	for _, lexeme := range []string{"", "0", "-0", "1.0", "1e0", "1E+3", "1e1000", "9007199254740993"} {
		value := json.Number(lexeme)
		values = append(values, value, &value)
	}
	for index, value := range values {
		test.Run(fmt.Sprint(index), func(test *testing.T) { assertLegacyCanonicalEncodingValue(test, value) })
	}
}

func TestLegacyCanonicalEncodingOriginalDecodedEvents(test *testing.T) {
	accepted := 0
	for _, candidate := range canonicalInputEventCases(test) {
		test.Run(candidate.name, func(test *testing.T) {
			var event CIREvent
			if json.Unmarshal(candidate.raw, &event) != nil {
				return
			}
			value, err := canonicalEventJSONObject(event)
			if err != nil {
				return
			}
			before, err := json.Marshal(event)
			if err != nil {
				test.Fatal(err)
			}
			want, err := canonicalInputEventJSON(event)
			if err != nil {
				test.Fatal(err)
			}
			encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
			if err != nil {
				test.Fatal(err)
			}
			got, _ := collectLegacyCanonicalEncoding(test, encoding)
			plain, err := json.Marshal(value)
			if err != nil || !bytes.Equal(got, plain) || !bytes.Equal(got, want) {
				test.Fatal("original typed-decoded event bytes differ", err)
			}
			after, err := json.Marshal(event)
			if err != nil || !bytes.Equal(before, after) {
				test.Fatal("encoding mutated the original event", err)
			}
			accepted++
		})
	}
	if accepted == 0 {
		test.Fatal("no original decoded events exercised")
	}
}

func TestLegacyCanonicalEncodingDeepOriginalDecodedEvents(test *testing.T) {
	for _, depth := range []int{64, 65, 1024, 9997} {
		test.Run(fmt.Sprint(depth), func(test *testing.T) {
			raw := `{"events":[{"kind":"tool_result","seq":9007199254740993,"output":` + strings.Repeat("[", depth) + `null` + strings.Repeat("]", depth) + `}]}`
			var document CIRDocument
			if err := json.Unmarshal([]byte(raw), &document); err != nil {
				test.Fatal(err)
			}
			event := document.Events[0]
			value, err := canonicalEventJSONObject(event)
			if err != nil {
				test.Fatal(err)
			}
			want, err := canonicalInputEventJSON(event)
			if err != nil {
				test.Fatal(err)
			}
			encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
			if err != nil {
				test.Fatal("valid source depth was rejected", err)
			}
			got, _ := collectLegacyCanonicalEncoding(test, encoding)
			plain, err := json.Marshal(value)
			if err != nil || !bytes.Equal(got, plain) || !bytes.Equal(got, want) {
				test.Fatal("deep original event canonical bytes differ", err)
			}
		})
	}
	var value any = nil
	for depth := 0; depth < 9998; depth++ {
		value = []any{value}
	}
	assertLegacyCanonicalEncodingValue(test, value)
	if encoding, err := newLegacyCanonicalEncoding(context.Background(), []any{value}); err == nil || encoding != nil {
		test.Fatal("accepted a container beyond the source's 10000-level budget")
	}
}

func TestLegacyCanonicalEncodingChunksAndOwnership(test *testing.T) {
	for _, text := range []string{
		strings.Repeat("a", 4*LegacyCanonicalEncodingBytes+17),
		strings.Repeat("🙂<&>\u2028\u2029\"\\\x00\xff", LegacyCanonicalEncodingBytes),
		strings.Repeat("a", LegacyCanonicalEncodingBytes-3) + "🙂" + strings.Repeat("b", 3*LegacyCanonicalEncodingBytes),
	} {
		encoding, err := newLegacyCanonicalEncoding(context.Background(), text)
		if err != nil {
			test.Fatal(err)
		}
		var chunks, snapshots [][]byte
		var result []byte
		previous := 0
		for {
			chunk, complete, err := encoding.Next(context.Background())
			if err != nil || len(chunk) > LegacyCanonicalEncodingBytes || cap(chunk) > LegacyCanonicalEncodingBytes {
				test.Fatal("chunk limit or encoding failure", len(chunk), cap(chunk), err)
			}
			position := len(text)
			if encoding.frames.length() != 0 {
				position = encoding.frames.at(0).position
			}
			if position-previous > LegacyCanonicalEncodingBytes {
				test.Fatal("string source budget exceeded", position-previous)
			}
			previous = position
			chunks = append(chunks, chunk)
			snapshots = append(snapshots, bytes.Clone(chunk))
			result = append(result, chunk...)
			if complete {
				break
			}
		}
		want, err := json.Marshal(text)
		if err != nil || len(chunks) <= 3 || !bytes.Equal(result, want) {
			test.Fatal("long string bytes or chunk count differ", len(chunks), err)
		}
		for index := range chunks {
			if !bytes.Equal(chunks[index], snapshots[index]) {
				test.Fatal("later Next overwrote an owned output chunk")
			}
		}
		chunks[0][0] ^= 0xff
		for index := 1; index < len(chunks); index++ {
			if !bytes.Equal(chunks[index], snapshots[index]) {
				test.Fatal("output chunks share mutable backing storage")
			}
		}
	}
}

func TestLegacyCanonicalEncodingAdaptiveOutputAndActions(test *testing.T) {
	encoding, err := newLegacyCanonicalEncoding(context.Background(), map[string]any{"kind": EventTurn, "role": RoleUser, "seq": 0})
	if err != nil {
		test.Fatal(err)
	}
	chunk, complete, err := encoding.Next(context.Background())
	if err != nil || !complete || cap(chunk) > 1024 {
		test.Fatal("tiny event allocated a full output chunk", cap(chunk), complete, err)
	}
	values := make([]any, 4096)
	encoding, err = newLegacyCanonicalEncoding(context.Background(), values)
	if err != nil {
		test.Fatal(err)
	}
	first, complete, err := encoding.Next(context.Background())
	if err != nil || complete || len(first) > legacyCanonicalEncodingActions*5 || encoding.frames.at(0).position > legacyCanonicalEncodingActions {
		test.Fatal("structural work did not yield", len(first), complete, err)
	}
	rest, _ := collectLegacyCanonicalEncoding(test, encoding)
	want, _ := json.Marshal(values)
	if !bytes.Equal(append(first, rest...), want) {
		test.Fatal("structural yields changed bytes")
	}
}

func TestLegacyCanonicalEncodingASCIIRunBoundaries(test *testing.T) {
	for _, offset := range []int{-4, -1, 0, 1, 5} {
		for _, separator := range []string{"<>&", "\"\\", "\x00\n\t", "한🙂\u2028\u2029", "\xff"} {
			text := strings.Repeat("a", LegacyCanonicalEncodingBytes+offset) + separator + strings.Repeat("z", LegacyCanonicalEncodingBytes+3)
			assertLegacyCanonicalEncodingValue(test, map[string]any{"body": text})
		}
	}
}

func TestLegacyCanonicalEncodingNumberLiteralStreaming(test *testing.T) {
	number := json.Number("1" + strings.Repeat("0", 4*LegacyCanonicalEncodingBytes+7))
	encoding, err := newLegacyCanonicalEncoding(context.Background(), number)
	if err != nil {
		test.Fatal(err)
	}
	got, steps := collectLegacyCanonicalEncoding(test, encoding)
	if steps <= 3 || !bytes.Equal(got, []byte(number)) {
		test.Fatal("large validated number was changed or not streamed", steps)
	}
	invalid := json.Number(string(number) + "x")
	if encoding, err := newLegacyCanonicalEncoding(context.Background(), map[string]any{"a": true, "z": invalid}); err == nil || encoding != nil {
		test.Fatal("numeric failure was deferred until Next")
	}
}

type legacyCanonicalFinalizationContext struct {
	context.Context
	encoding *LegacyCanonicalEncoding
}

func (ctx legacyCanonicalFinalizationContext) Err() error {
	if ctx.encoding.frames.length() == 0 {
		return context.Canceled
	}
	return ctx.Context.Err()
}

func TestLegacyCanonicalEncodingCancellation(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if encoding, err := newLegacyCanonicalEncoding(canceled, true); encoding != nil || !errors.Is(err, context.Canceled) {
		test.Fatal("constructor ignored cancellation", err)
	}
	for _, finalization := range []bool{false, true} {
		value := any(strings.Repeat("x", 4*LegacyCanonicalEncodingBytes))
		if finalization {
			value = nil
		}
		encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
		if err != nil {
			test.Fatal(err)
		}
		ctx := context.Context(canceled)
		if finalization {
			ctx = legacyCanonicalFinalizationContext{Context: context.Background(), encoding: encoding}
		} else {
			if _, complete, err := encoding.Next(context.Background()); err != nil || complete {
				test.Fatal("fixture did not yield before cancellation", err)
			}
		}
		chunk, complete, err := encoding.Next(ctx)
		if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) {
			test.Fatal("cancellation returned partial or finalized output", len(chunk), complete, err)
		}
		chunk, complete, err = encoding.Next(context.Background())
		if len(chunk) != 0 || complete || !errors.Is(err, context.Canceled) || encoding.root != nil ||
			encoding.frames != (legacyCanonicalPages[legacyCanonicalEncodingFrame]{}) || encoding.mapKeys != (legacyCanonicalPages[legacyCanonicalEncodingKeys]{}) {
			test.Fatal("cancellation was not terminal or retained the root", err)
		}
		if retained, err := encoding.RetainedBytes(); retained != 0 || !errors.Is(err, context.Canceled) {
			test.Fatal("failed encoding has a valid resource estimate", retained, err)
		}
	}
	var absent *LegacyCanonicalEncoding
	if _, _, err := absent.Next(canceled); !errors.Is(err, context.Canceled) {
		test.Fatal("nil receiver masked cancellation", err)
	}
}

func TestLegacyCanonicalEncodingRejectsMalformedGoValues(test *testing.T) {
	cycleMap := map[string]any{}
	cycleMap["self"] = cycleMap
	cycleSlice := make([]any, 1)
	cycleSlice[0] = cycleSlice
	type namedMap map[string]any
	badNumber := json.Number("01")
	for _, value := range []any{
		struct{ Text string }{}, (*int)(nil), []byte("raw"), json.RawMessage(`null`),
		[]string{}, namedMap{}, map[int]any{}, func() {}, make(chan int),
		int64(1), math.NaN(), math.Inf(1), json.Number("+1"), &badNumber,
		map[string]any{"a": true, "z": []any{json.Number("NaN")}}, cycleMap, cycleSlice,
	} {
		if encoding, err := newLegacyCanonicalEncoding(context.Background(), value); err == nil || encoding != nil {
			test.Fatalf("unsupported value %T was admitted", value)
		}
	}
}

func TestLegacyCanonicalEncodingRetainedBytes(test *testing.T) {
	text := strings.Repeat("r", 4*LegacyCanonicalEncodingBytes)
	var value any = text
	var expected docRangeHeaderHeapEstimate
	expected.allocation(1, int64(unsafe.Sizeof(text)))
	expected.allocation(int64(len(text)), 1)
	for depth := 0; depth < 20; depth++ {
		value = []any{value}
		expected.allocation(1, int64(unsafe.Sizeof([]any{})))
		expected.allocation(1, int64(unsafe.Sizeof(any(nil))))
	}
	encoding, err := newLegacyCanonicalEncoding(context.Background(), value)
	if err != nil || encoding.rootHeap.bytes != expected.bytes {
		test.Fatal("child heap was omitted or counted more than once", err)
	}
	before, err := encoding.RetainedBytes()
	if err != nil || before < expected.bytes {
		test.Fatal("root heap uncharged", before, expected.bytes, err)
	}
	if _, complete, err := encoding.Next(context.Background()); err != nil || complete {
		test.Fatal("retention fixture did not yield", err)
	}
	during, err := encoding.RetainedBytes()
	if err != nil || during < before || during-before > 32<<10 {
		test.Fatal("active frames counted the retained root again", before, during, err)
	}
	collectLegacyCanonicalEncoding(test, encoding)
	after, err := encoding.RetainedBytes()
	var empty docRangeHeaderHeapEstimate
	empty.allocation(1, int64(unsafe.Sizeof(*encoding)))
	if err != nil || after != empty.bytes {
		test.Fatal("completed encoding retains traversal storage", after, empty.bytes, err)
	}
	tail := make([]any, 1, 4)
	tail[:cap(tail)][3] = map[string]any{"tail": strings.Repeat("t", 8192)}
	value = []any{tail, map[string]any{"z": true, "a": false}}
	encoding, err = newLegacyCanonicalEncoding(context.Background(), value)
	if err != nil {
		test.Fatal(err)
	}
	if retained, err := encoding.RetainedBytes(); err != nil || retained < 8192+int64(cap(tail))*int64(unsafe.Sizeof(any(nil))) {
		test.Fatal("backing capacity or tail references uncharged", retained, err)
	}
	got, _ := collectLegacyCanonicalEncoding(test, encoding)
	want, _ := json.Marshal(value)
	if !bytes.Equal(got, want) {
		test.Fatal("retained slice tail changed serialized traversal")
	}
	encoding, err = newLegacyCanonicalEncoding(context.Background(), map[string]any{})
	if err != nil || encoding.rootHeap.bytes < 8*int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil))) {
		test.Fatal("empty non-nil map's minimum bucket storage uncharged", err)
	}
	encoding, err = newLegacyCanonicalEncoding(context.Background(), map[string]any{text: nil})
	if err != nil {
		test.Fatal(err)
	}
	before, _ = encoding.RetainedBytes()
	if _, complete, err := encoding.Next(context.Background()); err != nil || complete {
		test.Fatal("key retention fixture did not yield", err)
	}
	during, err = encoding.RetainedBytes()
	if err != nil || during < before || during-before > 4096 {
		test.Fatal("active key slice counted aliased root key bytes again", before, during, err)
	}
}

func FuzzLegacyCanonicalEncoding(fuzzer *testing.F) {
	prefix := strings.Repeat("p", 16000)
	for _, value := range []any{
		legacyKeyOrderNestedFixture(17),
		map[string]any{prefix + "z": true, prefix + "a": false, prefix + "m": nil},
	} {
		raw, err := json.Marshal(value)
		if err != nil {
			fuzzer.Fatal(err)
		}
		fuzzer.Add(raw)
	}
	for _, raw := range []string{`null`, `{"z":[null,false,{"z":1,"a":2}],"a":"<&>\u2028🙂"}`, `9007199254740993`, `"\ud800"`} {
		fuzzer.Add([]byte(raw))
	}
	for _, candidate := range canonicalInputEventCases(fuzzer) {
		if len(candidate.raw) <= 64<<10 {
			fuzzer.Add(candidate.raw)
		}
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 64<<10 || !json.Valid(raw) {
			return
		}
		wrapped := append([]byte("[["), raw...)
		wrapped = append(wrapped, ']', ']')
		var parents []any
		if json.Unmarshal(wrapped, &parents) != nil {
			return
		}
		value := parents[0].([]any)[0]
		assertLegacyCanonicalEncodingValue(test, value)
		var event CIREvent
		if json.Unmarshal(raw, &event) != nil {
			return
		}
		object, err := canonicalEventJSONObject(event)
		if err != nil {
			return
		}
		encoding, err := newLegacyCanonicalEncoding(context.Background(), object)
		if err != nil {
			test.Fatal(err)
		}
		got, _ := collectLegacyCanonicalEncoding(test, encoding)
		want, err := canonicalInputEventJSON(event)
		if err != nil || !bytes.Equal(got, want) {
			test.Fatal("original decoded CIR fuzz parity failed", err)
		}
	})
}
