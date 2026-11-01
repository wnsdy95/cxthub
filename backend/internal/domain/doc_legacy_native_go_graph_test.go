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

const legacyNativeGoGraphW85Budget int64 = 64 << 20
const legacyNativeGoGraphW85StepLimit = 32768

type legacyNativeGoGraphW85NamedString string
type legacyNativeGoGraphW85NamedMap map[string]any
type legacyNativeGoGraphW85NamedArray []any
type legacyNativeGoGraphW85MapAlias = map[string]any
type legacyNativeGoGraphW85ArrayAlias = []any

type legacyNativeGoGraphW85Identity struct {
	kind     uint8
	pointer  uintptr
	length   int
	capacity int
}

func legacyNativeGoGraphW85IdentityOf(value any) legacyNativeGoGraphW85Identity {
	switch decoded := value.(type) {
	case map[string]any:
		if decoded != nil {
			return legacyNativeGoGraphW85Identity{kind: 1, pointer: reflect.ValueOf(decoded).Pointer()}
		}
	case []any:
		if decoded != nil {
			return legacyNativeGoGraphW85Identity{kind: 2, pointer: reflect.ValueOf(decoded).Pointer(), length: len(decoded), capacity: cap(decoded)}
		}
	}
	return legacyNativeGoGraphW85Identity{}
}

func legacyNativeGoGraphW85SameSource(actual, expected any) bool {
	if reflect.TypeOf(actual) != reflect.TypeOf(expected) {
		return false
	}
	switch decoded := expected.(type) {
	case nil:
		return actual == nil
	case bool:
		return actual.(bool) == decoded
	case float64:
		return math.Float64bits(actual.(float64)) == math.Float64bits(decoded)
	case string:
		return actual.(string) == decoded
	case map[string]any, []any:
		return legacyNativeGoGraphW85IdentityOf(actual) == legacyNativeGoGraphW85IdentityOf(expected)
	default:
		return reflect.DeepEqual(actual, expected)
	}
}

type legacyNativeGoGraphW85SourceRecord struct {
	source   any
	entries  map[string]any
	elements []any
}

func legacyNativeGoGraphW85SourceSnapshot(root any) []legacyNativeGoGraphW85SourceRecord {
	pending := []any{root}
	seen := make(map[legacyNativeGoGraphW85Identity]bool)
	var records []legacyNativeGoGraphW85SourceRecord
	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		identity := legacyNativeGoGraphW85IdentityOf(current)
		if identity.kind == 0 || seen[identity] {
			continue
		}
		seen[identity] = true
		record := legacyNativeGoGraphW85SourceRecord{source: current}
		switch decoded := current.(type) {
		case map[string]any:
			record.entries = make(map[string]any, len(decoded))
			for key, child := range decoded {
				record.entries[key] = child
				pending = append(pending, child)
			}
		case []any:
			backing := decoded[:cap(decoded)]
			record.elements = append([]any(nil), backing...)
			pending = append(pending, backing...)
		}
		records = append(records, record)
	}
	return records
}

func legacyNativeGoGraphW85SourceUnchanged(test *testing.T, records []legacyNativeGoGraphW85SourceRecord) {
	test.Helper()
	for _, record := range records {
		switch decoded := record.source.(type) {
		case map[string]any:
			if len(decoded) != len(record.entries) {
				test.Fatal("import changed borrowed map length")
			}
			for key, expected := range record.entries {
				actual, present := decoded[key]
				if !present || !legacyNativeGoGraphW85SameSource(actual, expected) {
					test.Fatal("import changed borrowed map value or alias", key)
				}
			}
		case []any:
			for position, expected := range record.elements {
				if !legacyNativeGoGraphW85SameSource(decoded[:cap(decoded)][position], expected) {
					test.Fatal("import changed borrowed slice backing", position)
				}
			}
		}
	}
}

type legacyNativeGoGraphW85Inspector struct {
	owners  map[legacyNativeGoGraphW85Identity]legacyNativeAnyValueOwner
	sources map[legacyNativeAnyValueOwner]legacyNativeGoGraphW85Identity
}

func (inspector *legacyNativeGoGraphW85Inspector) check(test *testing.T, source any, actual legacyNativeAnyValue) {
	test.Helper()
	if inspector.owners == nil {
		inspector.owners = make(map[legacyNativeGoGraphW85Identity]legacyNativeAnyValueOwner)
		inspector.sources = make(map[legacyNativeAnyValueOwner]legacyNativeGoGraphW85Identity)
	}
	type pair struct {
		source any
		actual legacyNativeAnyValue
	}
	pending := []pair{{source: source, actual: actual}}
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= legacyNativeGoGraphW85StepLimit {
			test.Fatal("native graph inspection exceeded finite node allowance")
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		value := current.actual
		if err := value.validateOwnership(); err != nil {
			test.Fatal("import did not construct real valid native ownership", err)
		}
		if reflect.TypeOf(value.value) != reflect.TypeOf(current.source) {
			test.Fatalf("import changed grammar type: actual=%T expected=%T", value.value, current.source)
		}
		identity := legacyNativeGoGraphW85IdentityOf(current.source)
		if identity.kind == 0 {
			if !legacyNativeGoGraphW85SameSource(value.value, current.source) || value.owner != (legacyNativeAnyValueOwner{}) {
				test.Fatal("primitive or typed nil changed value, bits, or empty owner")
			}
			heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(current.source))
			if text, ok := current.source.(string); ok {
				heap = docRangeHeaderHeapEstimate{}
				heap.allocation(1, int64(unsafe.Sizeof(text)))
				heap.allocation(int64(value.capacity), 1)
			} else if value.excess != 0 {
				test.Fatal("non-string primitive reported fabricated excess backing", value.excess)
			}
			if err != nil || value.heap != heap {
				test.Fatal("primitive heap or excess differs from original heap method", value.heap, heap, value.excess, err)
			}
			continue
		}
		if identity.kind == 1 && value.owner.object == nil || identity.kind == 2 && value.owner.array == nil {
			test.Fatal("nonnil container lacks actual native owner")
		}
		if previous, present := inspector.owners[identity]; present {
			if previous != value.owner {
				test.Fatal("one source container acquired different owners across aliases or roots")
			}
			continue
		}
		if previous, present := inspector.sources[value.owner]; present && previous != identity {
			test.Fatal("distinct source identities were merged into one native owner")
		}
		inspector.owners[identity], inspector.sources[value.owner] = value.owner, identity
		switch decoded := current.source.(type) {
		case map[string]any:
			if len(value.value.(map[string]any)) != len(decoded) || !value.owner.object.valid() {
				test.Fatal("native map does not preserve source membership")
			}
			checkpoint, err := value.owner.mapCheckpoint(context.Background(), true)
			if err != nil {
				test.Fatal("actual imported map cannot produce original checkpoint cursor", err)
			}
			seen := make(map[string]bool, len(decoded))
			for position := 0; position <= len(decoded); position++ {
				entry, present, nextErr := checkpoint.next(context.Background())
				if nextErr != nil || present != (position < len(decoded)) {
					test.Fatal("native map cursor changed count or failed", position, present, nextErr)
				}
				if !present {
					break
				}
				expected, exists := decoded[entry.Key]
				if !exists || seen[entry.Key] || entry.KeyCapacity < len(entry.Key) || entry.ValueExcess < 0 || entry.ValueExcess%2 != 0 {
					test.Fatal("native map cursor lost key or metadata", entry.Key)
				}
				seen[entry.Key] = true
				child := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
				if text, ok := entry.Value.(string); ok {
					child.capacity = len(text) + int(entry.ValueExcess/2)
				}
				pending = append(pending, pair{source: expected, actual: child})
			}
			checkpoint.close()
		case []any:
			result := value.value.([]any)
			if len(result) != len(decoded) || cap(result) != cap(decoded) {
				test.Fatal("native slice changed exact length or capacity", len(result), cap(result), len(decoded), cap(decoded))
			}
			if len(decoded) > 0 && (!value.owner.array.tracked || !value.owner.array.children.valid() || value.owner.array.children.length() != len(decoded)) {
				test.Fatal("native slice child metadata is not aligned with actual elements")
			}
			for position, expected := range decoded {
				child, err := value.owner.arrayValue(context.Background(), position)
				if err != nil {
					test.Fatal("actual native slice cannot read imported child metadata", position, err)
				}
				pending = append(pending, pair{source: expected, actual: child})
			}
		}
	}
}

func legacyNativeGoGraphW85BudgetCheck(test *testing.T, work *legacyNativeCheckpointGoGraph, registry *legacyNativeCheckpointGoGraphRegistry, budget int64) (int64, int64) {
	test.Helper()
	local, localErr := work.retainedBytes()
	shared, sharedErr := registry.retainedBytes()
	if localErr != nil || sharedErr != nil || local < 0 || shared < 0 || shared > budget || local > budget-shared {
		test.Fatal("producer local and shared retained bytes exceed combined admission", local, shared, budget, localErr, sharedErr)
	}
	return local, shared
}

func legacyNativeGoGraphW85Drain(test *testing.T, work *legacyNativeCheckpointGoGraph, registry *legacyNativeCheckpointGoGraphRegistry, budget int64) int {
	test.Helper()
	for attempt := 1; attempt <= legacyNativeGoGraphW85StepLimit; attempt++ {
		current, cancel := context.WithCancel(context.Background())
		ready, err := work.step(current)
		cancel()
		if err != nil {
			test.Fatal("import step failed", attempt, ready, err)
		}
		legacyNativeGoGraphW85BudgetCheck(test, work, registry, budget)
		if ready {
			return attempt
		}
	}
	test.Fatal("import exceeded original finite step allowance")
	return 0
}

func legacyNativeGoGraphW85WorkReleased(test *testing.T, work *legacyNativeCheckpointGoGraph) {
	test.Helper()
	work.close()
	work.close()
	if retained, err := work.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("closed producer retained local graph or metadata", retained, err)
	}
}

func legacyNativeGoGraphW85RegistryReleased(test *testing.T, registry *legacyNativeCheckpointGoGraphRegistry) {
	test.Helper()
	registry.close()
	registry.close()
	if retained, err := registry.retainedBytes(); err != nil || retained != 0 {
		test.Fatal("closed producer registry retained source, owners, or metadata", retained, err)
	}
}

func legacyNativeGoGraphW85Import(test *testing.T, source any, registry *legacyNativeCheckpointGoGraphRegistry) (legacyNativeAnyValue, int) {
	test.Helper()
	snapshot := legacyNativeGoGraphW85SourceSnapshot(source)
	constructorContext, cancel := context.WithCancel(context.Background())
	work, err := newLegacyNativeCheckpointGoGraph(constructorContext, source, registry, legacyNativeGoGraphW85Budget)
	cancel()
	if err != nil || work == nil {
		test.Fatal("valid grammar was rejected by constructor", err)
	}
	defer work.close()
	legacyNativeGoGraphW85BudgetCheck(test, work, registry, legacyNativeGoGraphW85Budget)
	steps := legacyNativeGoGraphW85Drain(test, work, registry, legacyNativeGoGraphW85Budget)
	result, err := work.take(context.Background())
	if err != nil {
		test.Fatal("ready importer could not hand off real native value", err)
	}
	if retained, heapErr := work.retainedBytes(); heapErr != nil || retained != 0 {
		test.Fatal("take retained producer-local ownership", retained, heapErr)
	}
	legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
	if duplicate, duplicateErr := work.take(context.Background()); duplicateErr == nil || !reflect.DeepEqual(duplicate, legacyNativeAnyValue{}) {
		test.Fatal("duplicate take returned success or partial ownership", duplicateErr)
	}
	legacyNativeGoGraphW85WorkReleased(test, work)
	return result, steps
}

func legacyNativeGoGraphW85Failure(test *testing.T, work *legacyNativeCheckpointGoGraph, registry *legacyNativeCheckpointGoGraphRegistry, failure error) {
	test.Helper()
	if failure == nil {
		test.Fatal("expected explicit import rejection")
	}
	if ready, err := work.step(context.Background()); ready || !errors.Is(err, failure) {
		test.Fatal("failed importer did not preserve sticky failure", ready, err, failure)
	}
	if value, err := work.take(context.Background()); !errors.Is(err, failure) || !reflect.DeepEqual(value, legacyNativeAnyValue{}) {
		test.Fatal("failed importer handed off value or lost sticky error", err, failure)
	}
	if retained, err := work.retainedBytes(); retained != 0 || err != nil {
		test.Fatal("failed importer did not release local state", retained, err)
	}
	if retained, err := registry.retainedBytes(); retained != 0 || err != nil {
		test.Fatal("failed importer did not release incomplete shared graph", retained, err)
	}
	reused, err := newLegacyNativeCheckpointGoGraph(context.Background(), nil, registry, legacyNativeGoGraphW85Budget)
	if reused != nil {
		reused.close()
	}
	if !errors.Is(err, failure) || reused != nil {
		test.Fatal("failed or incomplete producer registry was reused as successful", err, failure)
	}
	legacyNativeGoGraphW85WorkReleased(test, work)
	legacyNativeGoGraphW85RegistryReleased(test, registry)
}

func TestLegacyNativeCheckpointGoGraphW85Values(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{name: "nil", value: nil},
		{name: "false", value: false},
		{name: "true", value: true},
		{name: "positive-zero", value: float64(0)},
		{name: "negative-zero", value: math.Copysign(0, -1)},
		{name: "subnormal", value: math.SmallestNonzeroFloat64},
		{name: "negative-subnormal", value: -math.SmallestNonzeroFloat64},
		{name: "maximum-finite", value: math.MaxFloat64},
		{name: "exact-mantissa", value: math.Float64frombits(0x4340000000000001)},
		{name: "empty-text", value: ""},
		{name: "escaped-and-unicode-text", value: "\x00\"\\\n한글😀"},
		{name: "typed-nil-map", value: map[string]any(nil)},
		{name: "typed-nil-array", value: []any(nil)},
		{name: "empty-map", value: map[string]any{}},
		{name: "empty-array", value: make([]any, 0)},
		{name: "empty-spare-array", value: make([]any, 0, 17)},
		{name: "map-type-alias", value: legacyNativeGoGraphW85MapAlias{"number": float64(7)}},
		{name: "array-type-alias", value: legacyNativeGoGraphW85ArrayAlias{false, nil, "text"}},
		{name: "nested", value: map[string]any{"": []any{nil, map[string]any(nil), []any(nil), map[string]any{}, []any{}, math.Copysign(0, -1)}}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			result, _ := legacyNativeGoGraphW85Import(test, fixture.value, registry)
			inspector := &legacyNativeGoGraphW85Inspector{}
			inspector.check(test, fixture.value, result)
			legacyNativeGoGraphW85RegistryReleased(test, registry)
			(&legacyNativeGoGraphW85Inspector{}).check(test, fixture.value, result)
		})
	}
	test.Run("actual-ownership-validation-fences", func(test *testing.T) {
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		source := []any{"payload", true}
		result, _ := legacyNativeGoGraphW85Import(test, source, registry)
		malformed := []legacyNativeAnyValue{result, result, result, result, result, result}
		malformed[0].heap.bytes++
		malformed[1].excess++
		malformed[2].capacity++
		malformed[3].owner.bytes++
		malformed[4].owner.revision++
		malformed[5].value = []any{"payload", true}
		for position, value := range malformed {
			expected := ErrIntegrity
			if position == 4 {
				expected = ErrConflict
			}
			if err := value.validateOwnership(); !errors.Is(err, expected) {
				test.Fatal("real native ownership accepted changed heap, metadata, revision, or identity", position, err)
			}
		}
		(&legacyNativeGoGraphW85Inspector{}).check(test, source, result)
	})
}

func TestLegacyNativeCheckpointGoGraphW85AliasesAndScale(test *testing.T) {
	test.Run("one-producer-registry-across-roots", func(test *testing.T) {
		sharedMap := map[string]any{"bits": math.Copysign(0, -1), "text": "shared"}
		sharedArray := make([]any, 2, 7)
		sharedArray[0], sharedArray[1] = sharedMap, sharedMap
		separateMap := map[string]any{"bits": math.Copysign(0, -1), "text": "shared"}
		separateArray := make([]any, 2, 7)
		separateArray[0], separateArray[1] = separateMap, separateMap
		backing := []any{float64(1), float64(2), float64(3), float64(4)}
		roots := []any{
			map[string]any{"left": sharedMap, "right": sharedMap, "array": sharedArray, "separate": separateMap},
			[]any{sharedArray, sharedMap, separateMap, separateArray},
			sharedMap, sharedArray, separateMap, separateArray,
			[]any{backing[:2:2], backing[:2:4], backing[1:3:4], backing[:2:2]},
		}
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		inspector := &legacyNativeGoGraphW85Inspector{}
		var results []legacyNativeAnyValue
		for _, root := range roots {
			result, _ := legacyNativeGoGraphW85Import(test, root, registry)
			inspector.check(test, root, result)
			results = append(results, result)
		}
		before, err := registry.retainedBytes()
		if err != nil || before <= 0 {
			test.Fatal("successful work.take or close erased shared producer registry", before, err)
		}
		repeated, _ := legacyNativeGoGraphW85Import(test, sharedArray, registry)
		inspector.check(test, sharedArray, repeated)
		after, err := registry.retainedBytes()
		if err != nil || after != before {
			test.Fatal("same-root reuse duplicated producer registry payload or metadata", before, after, err)
		}
		legacyNativeGoGraphW85RegistryReleased(test, registry)
		inspector = &legacyNativeGoGraphW85Inspector{}
		for position, root := range roots {
			inspector.check(test, root, results[position])
		}
	})
	test.Run("large-text-array-map-and-source-backing", func(test *testing.T) {
		text := strings.Repeat("한x", ConversationManifestChunkBytes+19)
		array := make([]any, 8193, 8209)
		for position := range array {
			array[position] = float64(position)
		}
		object := make(map[string]any, 1025)
		for position := 0; position < 1025; position++ {
			object[fmt.Sprintf("key-%04d", position)] = float64(position)
		}
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		for _, source := range []any{text, array, object} {
			result, steps := legacyNativeGoGraphW85Import(test, source, registry)
			(&legacyNativeGoGraphW85Inspector{}).check(test, source, result)
			if _, container := source.([]any); container {
				if steps <= 1 {
					test.Fatal("array larger than one original quantum completed without yielding", steps)
				}
				minimum := legacyNativeAnyArrayBacking(cap(array))
				if reflect.ValueOf(result.value).Pointer() != reflect.ValueOf(array).Pointer() {
					minimum.include(legacyNativeAnyArrayBacking(cap(result.value.([]any))))
				}
				minimum.include(docRangeHeaderHeapEstimate{bytes: result.owner.bytes})
				retained, err := registry.retainedBytes()
				if minimum.err != nil || err != nil || retained < minimum.bytes {
					test.Fatal("registry omitted actual source/destination backing or native metadata", retained, minimum, err)
				}
			}
		}
	})
	test.Run("deep-shared-dag", func(test *testing.T) {
		var root any = map[string]any{"leaf": "deep"}
		for depth := 0; depth < 256; depth++ {
			if depth%2 == 0 {
				root = []any{root}
			} else {
				root = map[string]any{"child": root}
			}
		}
		root = []any{root, root}
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		result, _ := legacyNativeGoGraphW85Import(test, root, registry)
		(&legacyNativeGoGraphW85Inspector{}).check(test, root, result)
	})
}

func TestLegacyNativeCheckpointGoGraphW85Rejections(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{name: "integer", value: int(1)},
		{name: "float32", value: float32(1)},
		{name: "json-number", value: json.Number("1")},
		{name: "named-string", value: legacyNativeGoGraphW85NamedString("text")},
		{name: "named-map", value: legacyNativeGoGraphW85NamedMap{"value": nil}},
		{name: "named-array", value: legacyNativeGoGraphW85NamedArray{nil}},
		{name: "typed-nil-other-array", value: []string(nil)},
		{name: "other-map", value: map[int]any{1: nil}},
		{name: "byte-slice", value: []byte("null")},
		{name: "fixed-array", value: [1]any{nil}},
		{name: "struct", value: struct{ Value string }{Value: "text"}},
		{name: "pointer", value: new(float64)},
		{name: "nan", value: math.NaN()},
		{name: "positive-infinity", value: math.Inf(1)},
		{name: "negative-infinity", value: math.Inf(-1)},
		{name: "nested-invalid", value: map[string]any{"nested": []any{true, int(1)}}},
		{name: "nested-nonfinite", value: []any{map[string]any{"value": math.Inf(1)}}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			snapshot := legacyNativeGoGraphW85SourceSnapshot(fixture.value)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			work, failure := newLegacyNativeCheckpointGoGraph(context.Background(), fixture.value, registry, legacyNativeGoGraphW85Budget)
			if failure == nil {
				if work == nil {
					test.Fatal("constructor returned empty success")
				}
				defer work.close()
				for attempt := 0; attempt < legacyNativeGoGraphW85StepLimit; attempt++ {
					ready, err := work.step(context.Background())
					if ready {
						test.Fatal("unsupported grammar became successful native value", fixture.name, err)
					}
					if err != nil {
						failure = err
						break
					}
					legacyNativeGoGraphW85BudgetCheck(test, work, registry, legacyNativeGoGraphW85Budget)
				}
			}
			if !errors.Is(failure, ErrIntegrity) && !errors.Is(failure, ErrValidation) {
				test.Fatal("non-grammar value lacked explicit type/integrity rejection", failure)
			}
			if work != nil {
				legacyNativeGoGraphW85Failure(test, work, registry, failure)
			}
		})
	}
	for _, name := range []string{"map-cycle", "array-cycle", "mixed-cycle", "excessive-depth"} {
		test.Run(name, func(test *testing.T) {
			object := make(map[string]any)
			array := make([]any, 1)
			var source any
			switch name {
			case "map-cycle":
				object["self"], source = object, object
			case "array-cycle":
				array[0], source = array, array
			case "mixed-cycle":
				object["array"], array[0], source = array, object, object
			case "excessive-depth":
				for depth := 0; depth < 10001; depth++ {
					source = []any{source}
				}
			}
			snapshot := legacyNativeGoGraphW85SourceSnapshot(source)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			work, failure := newLegacyNativeCheckpointGoGraph(context.Background(), source, registry, legacyNativeGoGraphW85Budget)
			if failure == nil {
				if work == nil {
					test.Fatal("invalid graph constructor returned empty success")
				}
				defer work.close()
				for attempt := 0; attempt < legacyNativeGoGraphW85StepLimit; attempt++ {
					ready, err := work.step(context.Background())
					if ready {
						test.Fatal("cyclic or excessive-depth graph was accepted", name)
					}
					if err != nil {
						failure = err
						break
					}
					legacyNativeGoGraphW85BudgetCheck(test, work, registry, legacyNativeGoGraphW85Budget)
				}
			}
			if !errors.Is(failure, ErrIntegrity) && !errors.Is(failure, ErrValidation) {
				test.Fatal("cycle/depth did not receive explicit structural rejection", failure)
			}
			if work != nil {
				legacyNativeGoGraphW85Failure(test, work, registry, failure)
			}
		})
	}
	for _, name := range []string{"map-size", "map-value", "map-float-bits", "map-child-alias", "array-value", "array-child-alias"} {
		test.Run("source-change-"+name, func(test *testing.T) {
			child := map[string]any{"value": float64(1)}
			object := map[string]any{"number": math.Copysign(0, -1), "child": child}
			array := []any{math.Copysign(0, -1), child}
			var source any = object
			if strings.HasPrefix(name, "array-") {
				source = array
			}
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			work, err := newLegacyNativeCheckpointGoGraph(context.Background(), source, registry, legacyNativeGoGraphW85Budget)
			if err != nil || work == nil {
				test.Fatal(err)
			}
			defer work.close()
			legacyNativeGoGraphW85Drain(test, work, registry, legacyNativeGoGraphW85Budget)
			switch name {
			case "map-size":
				object["new"] = true
			case "map-value":
				object["number"] = float64(2)
			case "map-float-bits":
				object["number"] = float64(0)
			case "map-child-alias":
				object["child"] = map[string]any{"value": float64(1)}
			case "array-value":
				array[0] = float64(0)
			case "array-child-alias":
				array[1] = map[string]any{"value": float64(1)}
			}
			value, failure := work.take(context.Background())
			if !reflect.DeepEqual(value, legacyNativeAnyValue{}) || !errors.Is(failure, ErrConflict) && !errors.Is(failure, ErrIntegrity) {
				test.Fatal("changed current source was handed off as stale success", failure)
			}
			legacyNativeGoGraphW85Failure(test, work, registry, failure)
		})
	}
}

type legacyNativeGoGraphW85CutContext struct {
	context.Context
	cancel context.CancelFunc
	calls  int
	limit  int
}

func (current *legacyNativeGoGraphW85CutContext) Err() error {
	current.calls++
	if current.calls >= current.limit {
		current.cancel()
	}
	return current.Context.Err()
}

func TestLegacyNativeCheckpointGoGraphW85LifecycleAndAdmission(test *testing.T) {
	for _, budget := range []int64{-1, 0, 1} {
		test.Run(fmt.Sprintf("constructor-budget-%d", budget), func(test *testing.T) {
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			work, err := newLegacyNativeCheckpointGoGraph(context.Background(), nil, registry, budget)
			if work != nil {
				work.close()
			}
			expected := ErrIntegrity
			if budget > 0 {
				expected = ErrDocumentResourceLimit
			}
			if work != nil || !errors.Is(err, expected) {
				test.Fatal("constructor did not pre-admit invalid/insufficient combined budget", budget, err)
			}
			legacyNativeGoGraphW85RegistryReleased(test, registry)
		})
	}
	test.Run("nil-and-closed-registry", func(test *testing.T) {
		closed := newLegacyNativeCheckpointGoGraphRegistry()
		closed.close()
		for _, registry := range []*legacyNativeCheckpointGoGraphRegistry{nil, closed} {
			work, err := newLegacyNativeCheckpointGoGraph(context.Background(), nil, registry, legacyNativeGoGraphW85Budget)
			if work != nil {
				work.close()
			}
			if work != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid producer registry was accepted", err)
			}
		}
	})
	test.Run("constructor-does-not-walk-descendants", func(test *testing.T) {
		source := make([]any, 8193)
		source[len(source)-1] = int(7)
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		base, cancel := context.WithCancel(context.Background())
		defer cancel()
		current := &legacyNativeGoGraphW85CutContext{Context: base, cancel: cancel, limit: 33}
		work, err := newLegacyNativeCheckpointGoGraph(current, source, registry, legacyNativeGoGraphW85Budget)
		if err != nil || work == nil || current.calls >= current.limit {
			test.Fatal("constructor performed descendant validation or unbounded preparation", current.calls, err)
		}
		legacyNativeGoGraphW85WorkReleased(test, work)
	})
	test.Run("producer-local-excludes-shared-registry", func(test *testing.T) {
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		source := map[string]any{"large": strings.Repeat("x", 2*ConversationManifestChunkBytes+17)}
		result, _ := legacyNativeGoGraphW85Import(test, source, registry)
		sharedBefore, err := registry.retainedBytes()
		if err != nil || sharedBefore <= int64(ConversationManifestChunkBytes) {
			test.Fatal("registry omitted retained actual graph", sharedBefore, err)
		}
		work, err := newLegacyNativeCheckpointGoGraph(context.Background(), nil, registry, legacyNativeGoGraphW85Budget)
		if err != nil || work == nil {
			test.Fatal(err)
		}
		defer work.close()
		local, _ := legacyNativeGoGraphW85BudgetCheck(test, work, registry, legacyNativeGoGraphW85Budget)
		if local >= sharedBefore {
			test.Fatal("small primitive work counted the shared registry as local", local, sharedBefore)
		}
		legacyNativeGoGraphW85Drain(test, work, registry, legacyNativeGoGraphW85Budget)
		if _, err := work.take(context.Background()); err != nil {
			test.Fatal(err)
		}
		legacyNativeGoGraphW85WorkReleased(test, work)
		sharedAfter, err := registry.retainedBytes()
		if err != nil || sharedAfter != sharedBefore {
			test.Fatal("primitive take/close erased or duplicated another root's shared ownership", sharedBefore, sharedAfter, err)
		}
		(&legacyNativeGoGraphW85Inspector{}).check(test, source, result)
		limited, err := newLegacyNativeCheckpointGoGraph(context.Background(), nil, registry, sharedAfter-1)
		if limited != nil {
			limited.close()
		}
		if limited != nil || !errors.Is(err, ErrDocumentResourceLimit) {
			test.Fatal("constructor ignored existing registry in combined pre-admission", sharedAfter, err)
		}
	})
	test.Run("payload-budget-before-success", func(test *testing.T) {
		registry := newLegacyNativeCheckpointGoGraphRegistry()
		defer registry.close()
		const budget int64 = ConversationManifestChunkBytes
		source := map[string]any{"payload": strings.Repeat("x", 2*ConversationManifestChunkBytes)}
		snapshot := legacyNativeGoGraphW85SourceSnapshot(source)
		defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
		work, failure := newLegacyNativeCheckpointGoGraph(context.Background(), source, registry, budget)
		if failure == nil {
			if work == nil {
				test.Fatal("budget constructor returned empty success")
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeGoGraphW85StepLimit; attempt++ {
				ready, err := work.step(context.Background())
				if ready {
					test.Fatal("oversized payload became budget success")
				}
				if err != nil {
					failure = err
					break
				}
				legacyNativeGoGraphW85BudgetCheck(test, work, registry, budget)
			}
		}
		if !errors.Is(failure, ErrDocumentResourceLimit) {
			test.Fatal("payload overrun did not report original resource limit", failure)
		}
		if work != nil {
			legacyNativeGoGraphW85Failure(test, work, registry, failure)
		}
	})
	for _, phase := range []string{"constructor", "step-entry", "step-current-context", "take-current-context", "early-take", "close-incomplete", "closed-registry-during-work"} {
		test.Run(phase, func(test *testing.T) {
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			source := make([]any, 8193)
			for position := range source {
				source[position] = float64(position)
			}
			snapshot := legacyNativeGoGraphW85SourceSnapshot(source)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			constructorContext := context.Background()
			if phase == "constructor" {
				current, cancel := context.WithCancel(constructorContext)
				cancel()
				constructorContext = current
			}
			work, err := newLegacyNativeCheckpointGoGraph(constructorContext, source, registry, legacyNativeGoGraphW85Budget)
			if phase == "constructor" {
				if work != nil {
					work.close()
				}
				if work != nil || !errors.Is(err, context.Canceled) {
					test.Fatal("constructor did not observe current canceled context", err)
				}
				legacyNativeGoGraphW85RegistryReleased(test, registry)
				return
			}
			if err != nil || work == nil {
				test.Fatal(err)
			}
			defer work.close()
			var failure error
			switch phase {
			case "step-entry":
				current, cancel := context.WithCancel(context.Background())
				cancel()
				ready, stepErr := work.step(current)
				if ready || !errors.Is(stepErr, context.Canceled) {
					test.Fatal("step ignored current canceled context", ready, stepErr)
				}
				failure = stepErr
			case "step-current-context":
				base, cancel := context.WithCancel(context.Background())
				defer cancel()
				current := &legacyNativeGoGraphW85CutContext{Context: base, cancel: cancel, limit: 8}
				for attempt := 0; attempt < legacyNativeGoGraphW85StepLimit; attempt++ {
					ready, stepErr := work.step(current)
					if ready {
						test.Fatal("large producer completed without current cancellation checks")
					}
					if stepErr != nil {
						failure = stepErr
						break
					}
					legacyNativeGoGraphW85BudgetCheck(test, work, registry, legacyNativeGoGraphW85Budget)
				}
				if current.calls < current.limit || !errors.Is(failure, context.Canceled) {
					test.Fatal("in-progress work did not honor current context cancellation", current.calls, failure)
				}
			case "take-current-context":
				legacyNativeGoGraphW85Drain(test, work, registry, legacyNativeGoGraphW85Budget)
				current, cancel := context.WithCancel(context.Background())
				cancel()
				value, takeErr := work.take(current)
				if !errors.Is(takeErr, context.Canceled) || !reflect.DeepEqual(value, legacyNativeAnyValue{}) {
					test.Fatal("take ignored current context or exposed canceled ownership", takeErr)
				}
				failure = takeErr
			case "early-take":
				value, takeErr := work.take(context.Background())
				if !errors.Is(takeErr, ErrIntegrity) || !reflect.DeepEqual(value, legacyNativeAnyValue{}) {
					test.Fatal("early take returned partial success", takeErr)
				}
				failure = takeErr
			case "close-incomplete":
				legacyNativeGoGraphW85WorkReleased(test, work)
				ready, stepErr := work.step(context.Background())
				if ready || !errors.Is(stepErr, ErrIntegrity) {
					test.Fatal("closed incomplete importer resumed as successful", ready, stepErr)
				}
				failure = stepErr
			case "closed-registry-during-work":
				registry.close()
				ready, stepErr := work.step(context.Background())
				if ready || !errors.Is(stepErr, ErrIntegrity) {
					test.Fatal("work resumed after shared producer registry release", ready, stepErr)
				}
				failure = stepErr
			}
			legacyNativeGoGraphW85Failure(test, work, registry, failure)
		})
	}
}
