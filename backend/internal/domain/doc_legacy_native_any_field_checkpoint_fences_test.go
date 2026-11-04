package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const legacyNativeAnyFieldW84FenceBudget int64 = 32 << 20
const legacyNativeAnyFieldW84FenceSteps = 32768

type legacyNativeAnyFieldW84ContextKey struct{}

func legacyNativeAnyFieldW84IO(test *testing.T, storage *legacyNativeAnyW78CAS) legacyNativeCheckpointIO {
	test.Helper()
	check := func(current context.Context, body []byte) {
		if current.Err() != nil || current.Value(legacyNativeAnyFieldW84ContextKey{}) == nil || len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
			test.Fatal("field checkpoint used stale context or escaped the original 64KiB IO bound", current.Err(), len(body))
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(body, &object) != nil {
			return
		}
		entries := bytes.TrimSpace(object["e"])
		if len(entries) == 0 || entries[0] != '[' {
			return
		}
		var rows []json.RawMessage
		if failure := json.Unmarshal(entries, &rows); failure != nil || len(rows) > legacyNativeCheckpointPageItems || len(rows) > 32 {
			test.Fatal("field checkpoint escaped the closed metadata page bound", len(rows), failure)
		}
	}
	storageIO := storage.io()
	put, read := storageIO.Put, storageIO.Read
	storageIO.Put = func(current context.Context, hash ContentHash, body []byte) error {
		check(current, body)
		return put(current, hash, body)
	}
	storageIO.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		body, failure := read(current, hash, length)
		if failure == nil {
			check(current, body)
		}
		return body, failure
	}
	return storageIO
}

func legacyNativeAnyFieldW84Ready(test *testing.T, work legacyNativeAnyW78Work, storage *legacyNativeAnyW78CAS) int64 {
	test.Helper()
	for attempt := 0; attempt < legacyNativeAnyFieldW84FenceSteps; attempt++ {
		storage.quantum = 0
		current, cancel := context.WithCancel(context.WithValue(context.Background(), legacyNativeAnyFieldW84ContextKey{}, attempt))
		ready, failure := work.step(current)
		cancel()
		retained, heapFailure := work.retainedBytes()
		if failure != nil || heapFailure != nil || retained <= 0 || retained > legacyNativeAnyFieldW84FenceBudget || storage.quantum > LegacyCanonicalEncodingBytes {
			test.Fatal("field checkpoint escaped finite admission or IO", fmt.Sprintf("%T", work), attempt, ready, failure, retained, heapFailure, storage.quantum)
		}
		if ready {
			reads, writes := storage.reads, storage.writes
			if repeated, failure := work.step(context.Background()); !repeated || failure != nil || storage.reads != reads || storage.writes != writes {
				test.Fatal("ready field checkpoint lost readiness or performed IO", repeated, failure)
			}
			return retained
		}
	}
	test.Fatal("field checkpoint exceeded the original finite schedule")
	return 0
}

func legacyNativeAnyFieldW84Reject[Value any](test *testing.T, factory func() (legacyNativeAnyCheckpointTask[Value], error), expected error) {
	test.Helper()
	work, failure := factory()
	if failure != nil {
		if work != nil {
			work.close()
			test.Fatal("rejected field constructor returned owned work", failure)
		}
		if !errors.Is(failure, expected) {
			test.Fatal("field constructor lost the explicit rejection", failure, expected)
		}
		return
	}
	if work == nil {
		test.Fatal("field constructor returned empty success")
	}
	defer work.close()
	for attempt := 0; attempt < legacyNativeAnyFieldW84FenceSteps; attempt++ {
		ready, failure := work.step(context.Background())
		if failure != nil {
			if !errors.Is(failure, expected) {
				test.Fatal("field checkpoint lost the explicit rejection", failure, expected)
			}
			legacyNativeObjectW79Failed[Value](test, work, failure)
			return
		}
		if ready {
			test.Fatal("invalid field checkpoint fabricated ready state")
		}
	}
	test.Fatal("invalid field checkpoint exceeded finite rejection schedule")
}

func legacyNativeAnyFieldW84Source(test *testing.T, restored, current LegacyByteSource) {
	test.Helper()
	if restored.authority == 0 || restored.authority != current.authority || restored.Read == nil || restored.Bytes != current.Bytes || restored.partBytes() != current.partBytes() || !reflect.DeepEqual(restored.Parts, current.Parts) {
		test.Fatal("field restore lost the current source geometry, manifest, or grant")
	}
}

func legacyNativeAnyFieldW84RoundTrip(test *testing.T, request legacyNativeAnyW78Request, native *legacyNativeObject, field int, scanner *legacyJSONValueScanner, source LegacyByteSource, storage *legacyNativeAnyW78CAS) (legacyNativeObjectCheckpointAnyField, legacyNativeObjectAnyRestored) {
	test.Helper()
	metadata := native.metadata.fields[field]
	value := native.destination.Elem().Field(metadata.index).Interface()
	beforeValue, beforeScanner := legacyNativeAnyW78Native(test, value), *scanner
	heap, excess := native.heaps[field], native.backingExcess[field]
	var parser *legacyNativeAny
	if len(native.containers) != 0 {
		parser = native.containers[field]
	}
	frame := legacyJSONValueScanner{}
	if native.containerActive && native.field == field {
		frame = *scanner
	}
	var beforeParser legacyNativeAnyW78StateRecord
	if parser != nil {
		beforeParser = legacyNativeAnyW78State(test, parser, &frame)
	}
	reads, writes, rawReads := storage.reads, storage.writes, 0
	borrowed := source
	borrowed.Read = func(current context.Context, offset, length int) ([]byte, error) {
		rawReads++
		return nil, ErrConflict
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, failure := newLegacyNativeObjectAnyCheckpoint(constructor, native, field, scanner, request.Binding, borrowed, legacyNativeAnyFieldW84IO(test, storage), legacyNativeAnyFieldW84FenceBudget)
	cancel()
	if failure != nil || work == nil || storage.reads != reads || storage.writes != writes || rawReads != 0 {
		test.Fatal("field capture constructor replayed source or did IO", failure)
	}
	defer work.close()
	legacyNativeAnyFieldW84Ready(test, work, storage)
	saved, failure := work.take(context.Background())
	if failure != nil || saved.validate(metadata.genericMap, heap.bytes != 0) != nil {
		test.Fatal("field capture produced an invalid descriptor", failure, saved.Binding)
	}
	legacyNativeAnyW78Released(test, work)
	if duplicate, failure := work.take(context.Background()); !errors.Is(failure, ErrIntegrity) || !reflect.DeepEqual(duplicate, legacyNativeObjectCheckpointAnyField{}) {
		test.Fatal("field capture transferred its descriptor twice", failure)
	}
	if rawReads != 0 || beforeValue != legacyNativeAnyW78Native(test, native.destination.Elem().Field(metadata.index).Interface()) || *scanner != beforeScanner || native.heaps[field] != heap || native.backingExcess[field] != excess {
		test.Fatal("field capture mutated its borrowed destination, scanner, or accounting")
	}
	if parser != nil && !reflect.DeepEqual(beforeParser, legacyNativeAnyW78State(test, parser, &frame)) {
		test.Fatal("field capture changed the real cached parser")
	}
	current := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	readCurrent := current.Read
	current.Read = func(step context.Context, offset, length int) ([]byte, error) {
		rawReads++
		return readCurrent(step, offset, length)
	}
	reads, writes = storage.reads, storage.writes
	constructor, cancel = context.WithCancel(context.Background())
	restore, failure := newLegacyNativeObjectAnyCheckpointRestore(constructor, saved, metadata.genericMap, heap.bytes != 0, request.Binding, current, legacyNativeAnyFieldW84IO(test, storage), legacyNativeAnyFieldW84FenceBudget)
	cancel()
	if failure != nil || restore == nil || storage.reads != reads || storage.writes != writes {
		test.Fatal("field restore constructor retained its context or performed IO", failure)
	}
	defer restore.close()
	retained := legacyNativeAnyFieldW84Ready(test, restore, storage)
	restored, failure := restore.take(context.Background())
	if failure != nil || beforeValue != legacyNativeAnyW78Native(test, restored.Value) || restored.Heap.err != nil || restored.Heap.bytes < 0 || (restored.Heap.bytes != 0) != (heap.bytes != 0) || restored.Excess < 0 || restored.Excess > restored.Heap.bytes || retained < restored.Heap.bytes || rawReads != 0 || storage.writes != writes {
		test.Fatal("field restore lost its value, exact bits, initialization, or owned heap", failure, restored.Heap, restored.Excess, retained, rawReads)
	}
	legacyNativeAnyFieldW84Source(test, restored.Source, current)
	if (parser == nil) != (restored.Parser == nil) || restored.Parser != nil && restored.Parser == parser {
		test.Fatal("field restore borrowed or discarded the real parser")
	}
	if restored.Parser != nil {
		if !reflect.DeepEqual(beforeParser, legacyNativeAnyW78State(test, restored.Parser, &restored.Scanner)) {
			test.Fatal("field restore changed cached control, owners, scanner, or semantics")
		}
		var parserHeap docRangeHeaderHeapEstimate
		restored.Parser.retained(&parserHeap)
		if parserHeap.err != nil || parserHeap.bytes <= 0 || retained < parserHeap.bytes {
			test.Fatal("field restore omitted a cached parser from aggregate ownership", retained, parserHeap)
		}
	}
	legacyNativeAnyW78Released(test, restore)
	if duplicate, failure := restore.take(context.Background()); !errors.Is(failure, ErrIntegrity) || !reflect.DeepEqual(duplicate, legacyNativeObjectAnyRestored{}) {
		test.Fatal("field restore transferred native ownership twice", failure)
	}
	if storage.inputWrites != 0 {
		test.Fatal("field capture rewrote immutable input dependencies")
	}
	return saved, restored
}

func TestLegacyNativeAnyFieldCheckpointW84IndependentValues(test *testing.T) {
	fixtures := []struct {
		name  string
		value any
	}{
		{name: "nil"},
		{name: "false", value: false},
		{name: "true", value: true},
		{name: "positive-zero", value: float64(0)},
		{name: "negative-zero", value: math.Copysign(0, -1)},
		{name: "subnormal", value: math.SmallestNonzeroFloat64},
		{name: "maximum-finite", value: math.MaxFloat64},
		{name: "fraction", value: float64(0.1)},
		{name: "empty-string", value: ""},
		{name: "opaque-string", value: "\ud55c🙂\x00\xff\"\\\n"},
		{name: "paged-string", value: strings.Repeat("\ud55c🙂", LegacyCanonicalEncodingBytes/7+19)},
		{name: "typed-nil-map", value: map[string]any(nil)},
		{name: "typed-nil-array", value: []any(nil)},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Output: fixture.value}, nil)
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			saved, restored := legacyNativeAnyFieldW84RoundTrip(test, request, native, native.metadata.match("output"), scanner, source, storage)
			if saved.Binding != 0 || saved.Destination == nil || saved.Parser != (legacyNativeCheckpointRef{}) || restored.Parser != nil || reflect.TypeOf(fixture.value) != reflect.TypeOf(restored.Value) {
				test.Fatal("independent value acquired a fake parser or lost its dynamic nil type")
			}
		})
	}
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{name: "nan", value: math.NaN()},
		{name: "positive-infinity", value: math.Inf(1)},
		{name: "negative-infinity", value: math.Inf(-1)},
		{name: "untracked-initial-map", value: map[string]any{"nested": []any{true, float64(7)}}},
		{name: "untracked-initial-array", value: []any{map[string]any{"nested": false}}},
	} {
		test.Run("reject-"+fixture.name, func(test *testing.T) {
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Output: fixture.value}, nil)
			storage := legacyNativeAnyW78Store(test)
			legacyNativeAnyFieldW84Reject[legacyNativeObjectCheckpointAnyField](test, func() (legacyNativeAnyCheckpointTask[legacyNativeObjectCheckpointAnyField], error) {
				work, failure := newLegacyNativeObjectAnyCheckpoint(context.Background(), native, native.metadata.match("output"), scanner, request.Binding, source, storage.io(), legacyNativeAnyFieldW84FenceBudget)
				if failure != nil {
					return nil, failure
				}
				return work, nil
			}, ErrIntegrity)
		})
	}
	test.Log("initial nonempty untracked graphs and cross-field deep aliases remain mandatory follow-up work, not successful empty checkpoints")
}

func TestLegacyNativeAnyFieldCheckpointW84ActualCacheBindings(test *testing.T) {
	fixtures := []struct {
		name    string
		raw     string
		field   string
		prior   any
		active  bool
		second  bool
		other   bool
		binding uint8
	}{
		{name: "input-partial-uninitialized", raw: `{"input":{"kept":-0,"` + strings.Repeat("k", 2*LegacyNormalizationPartBytes) + `":[true,null,{"deep":"value"}]}}`, field: "input", active: true, binding: 1},
		{name: "input-inactive-result", raw: `{"input":{"kept":[-0,true,null]}}`, field: "input", binding: 1},
		{name: "input-duplicate-root-reuse", raw: `{"input":{"kept":{"nested":[-0,true] }},"input":{"added":[false]}}`, field: "input", active: true, second: true, binding: 1},
		{name: "input-cache-other-active-field", raw: `{"input":{"kept":true},"output":{"next":[false,null]}}`, field: "input", active: true, other: true, binding: 1},
		{name: "output-inactive-array-result", raw: `{"output":[-0,true,null,{"nested":"kept"}]}`, field: "output", binding: 2},
		{name: "output-active-map-visible-owner", raw: `{"output":{"prior":[-0,"visible"]},"output":{"next":[true,null]}}`, field: "output", active: true, second: true, binding: 3},
		{name: "output-active-array-visible-owner", raw: `{"output":[-0,{"prior":"visible"}],"output":[true,null]}`, field: "output", active: true, second: true, binding: 3},
		{name: "output-active-independent-string", raw: `{"output":{"next":[true,null]}}`, field: "output", prior: "prior", active: true, binding: 0},
		{name: "output-active-independent-bool", raw: `{"output":[true,null]}`, field: "output", prior: false, active: true, binding: 0},
		{name: "output-active-independent-nil", raw: `{"output":{"next":true}}`, field: "output", active: true, binding: 0},
		{name: "input-null-retires-cache", raw: `{"input":{"kept":true},"input":null}`, field: "input", binding: 0},
		{name: "output-null-retires-cache", raw: `{"output":[true],"output":null}`, field: "output", binding: 0},
		{name: "output-scalar-retires-cache", raw: `{"output":{"kept":true},"output":false}`, field: "output", binding: 0},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			var stop func(*legacyNativeObject) bool
			if fixture.active {
				stop = func(native *legacyNativeObject) bool {
					if !native.containerActive || len(native.containers) == 0 || native.containers[native.field] == nil {
						return false
					}
					current := native.containers[native.field]
					if fixture.other {
						return native.field == native.metadata.match("output") && current.frame != nil
					}
					if native.field != native.metadata.match(fixture.field) || current.frame == nil {
						return false
					}
					if fixture.second && native.valueStart <= strings.Index(fixture.raw, `,"`+fixture.field+`":`) {
						return false
					}
					if fixture.field == "input" {
						return len(native.destination.Interface().(*wire).Input) > 0
					}
					return true
				}
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, fixture.raw, &wire{Output: fixture.prior}, stop)
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			field := native.metadata.match(fixture.field)
			before := legacyNativeAnyW78Native(test, native.destination.Elem().Field(native.metadata.fields[field].index).Interface())
			saved, restored := legacyNativeAnyFieldW84RoundTrip(test, request, native, field, scanner, source, storage)
			if saved.Binding != fixture.binding && !(fixture.binding == 1 && restored.Parser != nil && restored.Parser.finished && saved.Binding == 2) || (saved.Binding != 0 && saved.Destination != nil) || (saved.Binding != 0 && !saved.Parser.valid()) {
				test.Fatal("actual field selected the wrong destination/cache binding", saved.Binding, fixture.binding)
			}
			if fixture.name == "input-partial-uninitialized" && (restored.Parser == nil || restored.Heap.bytes != 0 || native.heaps[field].bytes != 0) {
				test.Fatal("partial Input fabricated initialized-field heap or discarded live parser ownership")
			}
			switch saved.Binding {
			case 1:
				if restored.Parser.rootMap == nil || reflect.ValueOf(restored.Value).Pointer() != reflect.ValueOf(restored.Parser.rootMap.values()).Pointer() {
					test.Fatal("Input destination lost exact restored rootMap alias")
				}
			case 2:
				if failure := restored.Parser.result.owner.matches(restored.Value); failure != nil {
					test.Fatal("inactive destination lost exact result ownership", failure)
				}
			case 3:
				if failure := restored.Parser.visible.matches(restored.Value); failure != nil || restored.Parser.visible == (legacyNativeAnyValueOwner{}) {
					test.Fatal("active Output lost its distinct visible owner", failure)
				}
			}
			if restored.Parser == nil || restored.Parser.finished {
				if restored.Scanner != (legacyJSONValueScanner{}) {
					test.Fatal("inactive field cache captured another field's active scanner")
				}
				return
			}
			complete := false
			for attempt := 0; attempt < legacyNativeEventW82SourceSteps; attempt++ {
				current := context.Background()
				input := &legacyNormalizationInput{ctx: current, source: restored.Source, numberDiagnostics: legacyNativeNumberDiagnosticsFull}
				ready, failure := restored.Parser.step(current, input, &restored.Scanner)
				for _, page := range input.pages {
					clear(page)
				}
				if failure != nil || input.unbounded || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 {
					test.Fatal("actual field container escaped finite continuation", failure)
				}
				if ready {
					complete = restored.Parser.finished && restored.Parser.position == len(fixture.raw)-1
					break
				}
			}
			if !complete {
				test.Fatal("actual field container did not finish at its enclosing Event boundary")
			}
			var expected wire
			expected.Output = fixture.prior
			if failure := json.Unmarshal([]byte(fixture.raw), &expected); failure != nil {
				test.Fatal("actual cache fixture is not independently accepted", failure)
			}
			var want any = expected.Output
			if fixture.field == "input" {
				want = expected.Input
			}
			if legacyNativeAnyW78Native(test, restored.Parser.result.value) != legacyNativeAnyW78Native(test, want) || fixture.field == "output" && legacyNativeAnyW78Native(test, restored.Value) != before {
				test.Fatal("restored actual parser lost duplicate-map reuse or replaced prior Output before parent publication")
			}
		})
	}
}

func TestLegacyNativeAnyFieldCheckpointW84IntegrityFences(test *testing.T) {
	stop := func(native *legacyNativeObject) bool {
		return native.containerActive && len(native.containers) != 0 && native.containers[native.field] != nil && native.containers[native.field].frame != nil
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"output":[true,null,{"next":-0}]}`, &wire{Output: "prior"}, stop)
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	field := native.metadata.match("output")
	saved, _ := legacyNativeAnyFieldW84RoundTrip(test, request, native, field, scanner, source, storage)
	for _, name := range []string{"unknown-binding", "bound-with-destination", "bound-without-parser", "independent-without-destination", "parser-ref-hash", "parser-ref-size", "value-wire", "wrong-field-type", "binding", "invalid-binding", "no-authority", "no-reader", "source-size", "part-width", "part-count", "part-hash", "part-size", "parser-body-hash", "parser-body-short"} {
		test.Run(name, func(test *testing.T) {
			changed, current, binding, genericMap := saved, source, request.Binding, false
			storageIO := storage.io()
			switch name {
			case "unknown-binding":
				changed.Binding = 255
			case "bound-with-destination":
				changed.Binding = 1
			case "bound-without-parser":
				changed.Binding, changed.Destination, changed.Parser = 3, nil, legacyNativeCheckpointRef{}
			case "independent-without-destination":
				changed.Destination = nil
			case "parser-ref-hash":
				changed.Parser.Hash = "bad"
			case "parser-ref-size":
				changed.Parser.Bytes = 0
			case "value-wire":
				value := *changed.Destination
				value.Wire = bytes.Clone(value.Wire)
				if len(value.Wire) == 0 {
					test.Fatal("independent prior string lacks its actual value wire")
				}
				value.Wire[0] ^= 1
				changed.Destination = &value
			case "wrong-field-type":
				genericMap = true
			case "binding":
				binding = HashContent([]byte("different actual Any field job"))
			case "invalid-binding":
				binding = "bad"
			case "no-authority":
				current.authority = 0
			case "no-reader":
				current.Read = nil
			case "source-size":
				current.Bytes--
			case "part-width":
				current.PartBytes = 1234
			case "part-count":
				current.Parts = nil
			case "part-hash", "part-size":
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				if name == "part-hash" {
					current.Parts[0].Hash = "bad"
				} else {
					current.Parts[0].Bytes--
				}
			case "parser-body-hash", "parser-body-short":
				read := storageIO.Read
				storageIO.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
					body, failure := read(current, hash, length)
					if failure == nil && hash == saved.Parser.Hash {
						body = bytes.Clone(body)
						if name == "parser-body-hash" {
							body[0] ^= 1
						} else {
							body = body[:len(body)-1]
						}
					}
					return body, failure
				}
			}
			switch name {
			case "unknown-binding", "bound-with-destination", "bound-without-parser", "independent-without-destination", "parser-ref-hash", "parser-ref-size":
				if failure := changed.validate(false, true); !errors.Is(failure, ErrIntegrity) {
					test.Fatal("field descriptor admitted an impossible binding shape", name, failure)
				}
			}
			legacyNativeAnyFieldW84Reject[legacyNativeObjectAnyRestored](test, func() (legacyNativeAnyCheckpointTask[legacyNativeObjectAnyRestored], error) {
				work, failure := newLegacyNativeObjectAnyCheckpointRestore(context.Background(), changed, genericMap, true, binding, current, storageIO, legacyNativeAnyFieldW84FenceBudget)
				if failure != nil {
					return nil, failure
				}
				return work, nil
			}, ErrIntegrity)
		})
	}
	for _, fieldIndex := range []int{-1, len(native.metadata.fields), native.metadata.match("seq")} {
		test.Run(fmt.Sprintf("capture-field-model-%d", fieldIndex), func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			legacyNativeAnyFieldW84Reject[legacyNativeObjectCheckpointAnyField](test, func() (legacyNativeAnyCheckpointTask[legacyNativeObjectCheckpointAnyField], error) {
				work, failure := newLegacyNativeObjectAnyCheckpoint(context.Background(), native, fieldIndex, scanner, request.Binding, source, storage.io(), legacyNativeAnyFieldW84FenceBudget)
				if failure != nil {
					return nil, failure
				}
				return work, nil
			}, ErrIntegrity)
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("wrong field model reached dependent IO")
			}
		})
	}
	for _, name := range []string{"parser-control", "scanner"} {
		test.Run("borrowed-"+name, func(test *testing.T) {
			work, failure := newLegacyNativeObjectAnyCheckpoint(context.Background(), native, field, scanner, request.Binding, source, storage.io(), legacyNativeAnyFieldW84FenceBudget)
			if failure != nil {
				test.Fatal(failure)
			}
			defer work.close()
			parser := native.containers[field]
			position, frame := parser.position, *scanner
			defer func() { parser.position, *scanner = position, frame }()
			if name == "parser-control" {
				parser.position++
			} else {
				scanner.invalid = !scanner.invalid
			}
			writes := storage.writes
			for attempt := 0; attempt < legacyNativeAnyFieldW84FenceSteps; attempt++ {
				ready, failure := work.step(context.Background())
				if failure != nil {
					if !errors.Is(failure, ErrConflict) || storage.writes != writes {
						test.Fatal("changed borrowed parser/scanner reached IO or lost conflict priority", failure)
					}
					legacyNativeObjectW79Failed[legacyNativeObjectCheckpointAnyField](test, work, failure)
					return
				}
				if ready {
					test.Fatal("changed borrowed field fabricated ready state")
				}
			}
			test.Fatal("changed borrowed field did not fail finitely")
		})
	}
}

func legacyNativeAnyFieldW84FloatRoundTrip(test *testing.T, request legacyNativeAnyW78Request, native *legacyNativeObject, source LegacyByteSource, storage *legacyNativeAnyW78CAS) (legacyNativeCheckpointRef, legacyNativeObjectFloatRestored) {
	test.Helper()
	if native.number == nil || native.number.parser == nil {
		test.Fatal("float fence lacks the real active outer number parser")
	}
	before, failure := legacyNativeCheckpointNumberCapture(native.number, source.Bytes)
	if failure != nil {
		test.Fatal(failure)
	}
	rawReads, reads, writes := 0, storage.reads, storage.writes
	borrowed := source
	borrowed.Read = func(current context.Context, offset, length int) ([]byte, error) {
		rawReads++
		return nil, ErrConflict
	}
	constructor, cancel := context.WithCancel(context.Background())
	work, failure := newLegacyNativeObjectFloatCheckpoint(constructor, native, request.Binding, borrowed, legacyNativeAnyFieldW84IO(test, storage), legacyNativeAnyFieldW84FenceBudget)
	cancel()
	if failure != nil || work == nil || storage.reads != reads || storage.writes != writes || rawReads != 0 {
		test.Fatal("outer float capture constructor did IO or replayed RAW", failure)
	}
	defer work.close()
	legacyNativeAnyFieldW84Ready(test, work, storage)
	ref, failure := work.take(context.Background())
	if failure != nil || !ref.valid() {
		test.Fatal("outer float capture failed its owned handoff", failure)
	}
	legacyNativeAnyW78Released(test, work)
	after, failure := legacyNativeCheckpointNumberCapture(native.number, source.Bytes)
	if failure != nil || !reflect.DeepEqual(before, after) || rawReads != 0 || storage.inputWrites != 0 {
		test.Fatal("outer float capture mutated actual conversion state or rewrote source", failure)
	}
	position := native.number.parser.position
	if native.number.errorText != nil {
		position = min(position, native.number.errorText.position)
	}
	suffix := request
	suffix.SuffixOffset = position / request.PartBytes * request.PartBytes
	suffix.Suffix = request.Suffix[suffix.SuffixOffset:]
	sourceReads := &legacyNativeAnyW78Reads{}
	current := legacyNativeAnyW78Source(test, suffix, sourceReads)
	reads, writes = storage.reads, storage.writes
	constructor, cancel = context.WithCancel(context.Background())
	restore, failure := newLegacyNativeObjectFloatCheckpointRestore(constructor, ref, request.Binding, current, legacyNativeAnyFieldW84IO(test, storage), legacyNativeAnyFieldW84FenceBudget)
	cancel()
	if failure != nil || restore == nil || storage.reads != reads || storage.writes != writes || len(sourceReads.Ranges) != 0 {
		test.Fatal("outer float restore constructor did IO or retained its retired context", failure)
	}
	defer restore.close()
	retained := legacyNativeAnyFieldW84Ready(test, restore, storage)
	restored, failure := restore.take(context.Background())
	if failure != nil || restored.Number == nil || restored.Number == native.number || restored.Number.parser == native.number.parser || len(sourceReads.Ranges) != 0 || storage.writes != writes {
		test.Fatal("outer float restore borrowed live work, read RAW, or discarded the number", failure)
	}
	legacyNativeAnyFieldW84Source(test, restored.Source, current)
	after, failure = legacyNativeCheckpointNumberCapture(restored.Number, current.Bytes)
	var numberHeap docRangeHeaderHeapEstimate
	restored.Number.retained(&numberHeap)
	if failure != nil || !reflect.DeepEqual(before, after) || numberHeap.err != nil || numberHeap.bytes <= 0 || retained < numberHeap.bytes || restored.Number.parser.authority != current.authority {
		test.Fatal("outer float restore lost exact conversion, diagnostics, grant, or aggregate ownership", failure, retained, numberHeap)
	}
	legacyNativeAnyW78Released(test, restore)
	if duplicate, failure := restore.take(context.Background()); !errors.Is(failure, ErrIntegrity) || !reflect.DeepEqual(duplicate, legacyNativeObjectFloatRestored{}) {
		test.Fatal("outer float transferred its parser twice", failure)
	}
	currentContext := context.WithValue(context.Background(), legacyNativeAnyFieldW84ContextKey{}, "after-take")
	var declared legacyNativeCheckpointNumberState
	if failure := legacyNativeCheckpointReadJSON(currentContext, storage.io(), ref, &declared); failure != nil {
		test.Fatal("outer float dependency manifest", failure)
	}
	first, limit := declared.sourceRange()
	for ordinal := first; ordinal < limit && ordinal*request.PartBytes < suffix.SuffixOffset; ordinal++ {
		part := request.Parts[ordinal]
		body, failure := restored.Source.Read(currentContext, ordinal*request.PartBytes, int(part.Bytes))
		if failure != nil || HashContent(body) != part.Hash || sourceReads.Forbidden != 0 {
			test.Fatal("outer float lost a declared immutable consumed-prefix source dependency after close/take", ordinal, failure)
		}
	}
	return ref, restored
}

func legacyNativeAnyFieldW84Lifecycle[Value any](test *testing.T, storage *legacyNativeAnyW78CAS, factory func(context.Context, legacyNativeCheckpointIO, int64) (legacyNativeAnyCheckpointTask[Value], error)) {
	test.Helper()
	for _, phase := range []string{"constructor", "step", "io", "take", "early-take", "close-ready"} {
		test.Run(phase, func(test *testing.T) {
			current, cancel := context.WithCancel(context.Background())
			defer cancel()
			storageIO := storage.io()
			if phase == "io" {
				put, read := storageIO.Put, storageIO.Read
				storageIO.Put = func(step context.Context, hash ContentHash, body []byte) error {
					failure := put(step, hash, body)
					cancel()
					return failure
				}
				storageIO.Read = func(step context.Context, hash ContentHash, length int) ([]byte, error) {
					body, failure := read(step, hash, length)
					cancel()
					return body, failure
				}
			}
			if phase == "constructor" {
				cancel()
				work, failure := factory(current, storageIO, legacyNativeAnyFieldW84FenceBudget)
				if work != nil || !errors.Is(failure, context.Canceled) {
					if work != nil {
						work.close()
					}
					test.Fatal("pre-canceled field constructor published owned state", failure)
				}
				return
			}
			constructor, retire := context.WithCancel(context.Background())
			work, failure := factory(constructor, storageIO, legacyNativeAnyFieldW84FenceBudget)
			retire()
			if failure != nil || work == nil {
				test.Fatal("field lifecycle constructor", failure)
			}
			defer work.close()
			if phase == "take" || phase == "close-ready" {
				legacyNativeAnyFieldW84Ready(test, work, storage)
			}
			if phase == "take" || phase == "early-take" {
				if phase == "take" {
					cancel()
				}
				var zero Value
				value, failure := work.take(current)
				expected := error(ErrIntegrity)
				if phase == "take" {
					expected = context.Canceled
				}
				if !reflect.DeepEqual(value, zero) || !errors.Is(failure, expected) {
					test.Fatal("invalid field take published partial state", failure)
				}
				legacyNativeObjectW79Failed[Value](test, work, failure)
				return
			}
			if phase == "close-ready" {
				legacyNativeAnyW78Released(test, work)
				if ready, failure := work.step(context.Background()); ready || !errors.Is(failure, ErrIntegrity) {
					test.Fatal("closed ready field resumed native work", ready, failure)
				}
				return
			}
			if phase == "step" {
				cancel()
			}
			for attempt := 0; attempt < legacyNativeAnyFieldW84FenceSteps; attempt++ {
				ready, failure := work.step(current)
				if failure != nil {
					if !errors.Is(failure, context.Canceled) {
						test.Fatal("field cancellation lost priority", failure)
					}
					legacyNativeObjectW79Failed[Value](test, work, failure)
					return
				}
				if ready {
					test.Fatal("canceled field work fabricated ready state")
				}
			}
			test.Fatal("field cancellation exceeded finite schedule")
		})
	}
	for _, budget := range []int64{1, LegacyCanonicalEncodingBytes, legacyNativeAnyCheckpointScratch} {
		test.Run(fmt.Sprintf("budget-%d", budget), func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			legacyNativeAnyFieldW84Reject[Value](test, func() (legacyNativeAnyCheckpointTask[Value], error) {
				return factory(context.Background(), storage.io(), budget)
			}, ErrDocumentResourceLimit)
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("field work performed IO before aggregate workspace admission")
			}
		})
	}
	test.Run("sticky-storage-failure", func(test *testing.T) {
		injected := errors.New("W84 field immutable storage unavailable")
		storage.failPut, storage.failRead = injected, injected
		defer func() { storage.failPut, storage.failRead = nil, nil }()
		legacyNativeAnyFieldW84Reject[Value](test, func() (legacyNativeAnyCheckpointTask[Value], error) {
			return factory(context.Background(), storage.io(), legacyNativeAnyFieldW84FenceBudget)
		}, injected)
	})
}

func legacyNativeAnyFieldW84RetiredSource(test *testing.T, number bool) (LegacyByteSource, <-chan struct{}) {
	test.Helper()
	released := make(chan struct{})
	storage := legacyNativeAnyW78Store(test)
	if number {
		raw := `{"output":-0.` + strings.Repeat("0", 2*LegacyNormalizationPartBytes) + `}`
		stop := func(native *legacyNativeObject) bool {
			return native.number != nil && native.number.parser.position >= LegacyNormalizationPartBytes && native.number.parser.position < native.number.parser.end
		}
		request, native, _, source := legacyNativeEventW82FencePrepare(test, raw, nil, stop)
		storage.preload(test, request)
		_, restored := legacyNativeAnyFieldW84FloatRoundTrip(test, request, native, source, storage)
		runtime.SetFinalizer(restored.Number, func(*legacyNativeNumberValue) { close(released) })
		return restored.Source, released
	}
	stop := func(native *legacyNativeObject) bool {
		return native.containerActive && len(native.containers) != 0 && native.containers[native.field] != nil && native.containers[native.field].frame != nil
	}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"input":{"kept":true,"next":[false]}}`, nil, stop)
	storage.preload(test, request)
	_, restored := legacyNativeAnyFieldW84RoundTrip(test, request, native, native.metadata.match("input"), scanner, source, storage)
	runtime.SetFinalizer(restored.Parser, func(*legacyNativeAny) { close(released) })
	return restored.Source, released
}

func TestLegacyNativeAnyFieldCheckpointW84FloatAndLifecycle(test *testing.T) {
	fixtures := []struct {
		name       string
		token      string
		diagnostic bool
	}{
		{name: "negative-zero", token: "-0." + strings.Repeat("0", 2*LegacyNormalizationPartBytes)},
		{name: "finite-rounded", token: "0." + strings.Repeat("1234567890", LegacyNormalizationPartBytes/5+2)},
		{name: "full-overflow-error", token: "1" + strings.Repeat("0", 2*LegacyNormalizationPartBytes)},
		{name: "partial-full-diagnostic", token: "1" + strings.Repeat("0", 2*LegacyNormalizationPartBytes), diagnostic: true},
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			raw := `{"output":` + fixture.token + `}`
			stop := func(native *legacyNativeObject) bool {
				if native.number == nil || native.number.parser == nil {
					return false
				}
				if fixture.diagnostic {
					text := native.number.errorText
					return text != nil && text.position >= LegacyNormalizationPartBytes && text.position < text.end
				}
				return native.number.parser.position >= LegacyNormalizationPartBytes && native.number.parser.position < native.number.parser.end
			}
			request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, &wire{Output: "prior"}, stop)
			if len(request.Parts) > 4 {
				test.Fatal("outer float fixture exceeded the original four-part source limit")
			}
			storage := legacyNativeAnyW78Store(test)
			storage.preload(test, request)
			ref, restored := legacyNativeAnyFieldW84FloatRoundTrip(test, request, native, source, storage)
			want, expectedFailure := strconv.ParseFloat(fixture.token, 64)
			finished := false
			for attempt := 0; attempt < legacyNativeEventW82SourceSteps; attempt++ {
				current, cancel := context.WithCancel(context.WithValue(context.Background(), legacyNativeAnyFieldW84ContextKey{}, attempt))
				input := &legacyNormalizationInput{ctx: current, source: restored.Source, numberDiagnostics: legacyNativeNumberDiagnosticsFull}
				ready, failure := restored.Number.step(current, input)
				cancel()
				for _, page := range input.pages {
					clear(page)
				}
				if input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.failure != nil {
					test.Fatal("restored outer float escaped its original finite source quantum", input.failure)
				}
				if failure != nil || ready {
					if !ready || !restored.Number.complete || expectedFailure == nil && (failure != nil || math.Float64bits(want) != math.Float64bits(restored.Number.value)) {
						test.Fatal("restored outer float changed completion or exact finite/-0 bits", ready, failure)
					}
					if expectedFailure != nil {
						var semantic *json.UnmarshalTypeError
						if !errors.As(failure, &semantic) || semantic != restored.Number.semantic || semantic.Type != reflect.TypeFor[float64]() || semantic.Value != "number "+fixture.token || semantic.Offset != int64(len(fixture.token)+1) {
							test.Fatal("outer float truncated or replaced its original full semantic diagnostic", failure)
						}
					}
					finished = true
					break
				}
			}
			if !finished {
				test.Fatal("restored outer float exceeded the original finite source schedule")
			}
			if fixture.name != "negative-zero" {
				return
			}
			for _, name := range []string{"binding", "ref-hash", "ref-size", "no-authority", "source-size", "part-width", "part-hash", "consumed-part-hash", "part-size"} {
				test.Run("restore-"+name, func(test *testing.T) {
					changed, current, binding := ref, source, request.Binding
					switch name {
					case "binding":
						binding = HashContent([]byte("different outer float binding"))
					case "ref-hash":
						changed.Hash = "bad"
					case "ref-size":
						changed.Bytes = 0
					case "no-authority":
						current.authority = 0
					case "source-size":
						current.Bytes--
					case "part-width":
						current.PartBytes = 1234
					case "part-hash", "consumed-part-hash", "part-size":
						current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
						if name == "part-size" {
							current.Parts[0].Bytes--
						} else if name == "consumed-part-hash" {
							current.Parts[0].Hash = HashContent([]byte("different immutable consumed numeric prefix"))
						} else {
							current.Parts[0].Hash = "bad"
						}
					}
					legacyNativeAnyFieldW84Reject[legacyNativeObjectFloatRestored](test, func() (legacyNativeAnyCheckpointTask[legacyNativeObjectFloatRestored], error) {
						work, failure := newLegacyNativeObjectFloatCheckpointRestore(context.Background(), changed, binding, current, storage.io(), legacyNativeAnyFieldW84FenceBudget)
						if failure != nil {
							return nil, failure
						}
						return work, nil
					}, ErrIntegrity)
				})
			}
			test.Run("capture-current-grant", func(test *testing.T) {
				current := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
				legacyNativeAnyFieldW84Reject[legacyNativeCheckpointRef](test, func() (legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef], error) {
					work, failure := newLegacyNativeObjectFloatCheckpoint(context.Background(), native, request.Binding, current, storage.io(), legacyNativeAnyFieldW84FenceBudget)
					if failure != nil {
						return nil, failure
					}
					return work, nil
				}, ErrIntegrity)
			})
			test.Run("float-capture-lifecycle", func(test *testing.T) {
				legacyNativeAnyFieldW84Lifecycle[legacyNativeCheckpointRef](test, storage, func(current context.Context, storageIO legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef], error) {
					work, failure := newLegacyNativeObjectFloatCheckpoint(current, native, request.Binding, source, storageIO, budget)
					if failure != nil {
						return nil, failure
					}
					return work, nil
				})
			})
			test.Run("float-restore-lifecycle", func(test *testing.T) {
				legacyNativeAnyFieldW84Lifecycle[legacyNativeObjectFloatRestored](test, storage, func(current context.Context, storageIO legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeObjectFloatRestored], error) {
					work, failure := newLegacyNativeObjectFloatCheckpointRestore(current, ref, request.Binding, source, storageIO, budget)
					if failure != nil {
						return nil, failure
					}
					return work, nil
				})
			})
			saved, _ := legacyNativeAnyFieldW84RoundTrip(test, request, native, native.metadata.match("output"), scanner, source, storage)
			if saved.Destination == nil || string(saved.Destination.Inline) != "prior" || saved.Destination.Text != (legacyNativeCheckpointText{}) {
				test.Fatal("original independent value did not retain its raw inline checkpoint")
			}
			ioRequest, ioNative, ioScanner, ioSource := legacyNativeEventW82FencePrepare(test, raw, &wire{Output: strings.Repeat("prior", 13)}, stop)
			ioSaved, _ := legacyNativeAnyFieldW84RoundTrip(test, ioRequest, ioNative, ioNative.metadata.match("output"), ioScanner, ioSource, storage)
			if ioSaved.Destination == nil || ioSaved.Destination.Inline != nil || ioSaved.Destination.Text.Bytes != 65 {
				test.Fatal("IO lifecycle fixture did not retain its legacy text dependency")
			}
			test.Run("any-capture-lifecycle", func(test *testing.T) {
				legacyNativeAnyFieldW84Lifecycle[legacyNativeObjectCheckpointAnyField](test, storage, func(current context.Context, storageIO legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeObjectCheckpointAnyField], error) {
					work, failure := newLegacyNativeObjectAnyCheckpoint(current, ioNative, ioNative.metadata.match("output"), ioScanner, ioRequest.Binding, ioSource, storageIO, budget)
					if failure != nil {
						return nil, failure
					}
					return work, nil
				})
			})
			test.Run("any-restore-lifecycle", func(test *testing.T) {
				legacyNativeAnyFieldW84Lifecycle[legacyNativeObjectAnyRestored](test, storage, func(current context.Context, storageIO legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeObjectAnyRestored], error) {
					work, failure := newLegacyNativeObjectAnyCheckpointRestore(current, ioSaved, false, true, ioRequest.Binding, ioSource, storageIO, budget)
					if failure != nil {
						return nil, failure
					}
					return work, nil
				})
			})
		})
	}
	for _, number := range []bool{false, true} {
		test.Run(fmt.Sprintf("source-does-not-retain-retired-parser-%t", number), func(test *testing.T) {
			source, released := legacyNativeAnyFieldW84RetiredSource(test, number)
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				runtime.GC()
				select {
				case <-released:
					current := context.WithValue(context.Background(), legacyNativeAnyFieldW84ContextKey{}, "retired-parser")
					body, failure := source.Read(current, 0, int(source.Parts[0].Bytes))
					if failure != nil || HashContent(body) != source.Parts[0].Hash {
						test.Fatal("live restored Source lost dependencies after retiring its parser", failure)
					}
					runtime.KeepAlive(source)
					return
				default:
				}
				time.Sleep(5 * time.Millisecond)
			}
			runtime.KeepAlive(source)
			test.Fatal("live Source.Read retained retired numeric/Any parser work")
		})
	}
}
