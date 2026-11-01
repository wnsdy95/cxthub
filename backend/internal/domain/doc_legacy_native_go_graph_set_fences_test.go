package domain

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unsafe"
)

const legacyNativeGoGraphSetW85Budget int64 = 64 << 20
const legacyNativeGoGraphSetW85Steps = 65536

type legacyNativeGoGraphSetW85Work[Value any] interface {
	step(context.Context) (bool, error)
	take(context.Context) (Value, error)
	retainedBytes() (int64, error)
	close()
}

type legacyNativeGoGraphSetW85Store struct {
	io          legacyNativeCheckpointIO
	objects     map[ContentHash][]byte
	current     context.Context
	reads       int
	writes      int
	transferred int
	failure     error
	cancelIO    context.CancelFunc
}

func legacyNativeGoGraphSetW85Storage(test *testing.T) *legacyNativeGoGraphSetW85Store {
	test.Helper()
	storage := &legacyNativeGoGraphSetW85Store{objects: make(map[ContentHash][]byte)}
	check := func(current context.Context, length int) error {
		if storage.current == nil || current != storage.current {
			test.Fatal("set CAS did not receive the current invocation context")
		}
		if length <= 0 || length > LegacyCanonicalEncodingBytes {
			test.Fatal("set CAS escaped the original 64KiB object bound", length)
		}
		if failure := current.Err(); failure != nil {
			return failure
		}
		if storage.cancelIO != nil {
			cancel := storage.cancelIO
			storage.cancelIO = nil
			cancel()
		}
		return storage.failure
	}
	storage.io.Put = func(current context.Context, hash ContentHash, body []byte) error {
		storage.writes++
		storage.transferred += len(body)
		if failure := check(current, len(body)); failure != nil {
			return failure
		}
		if HashContent(body) != hash {
			test.Fatal("set writer passed a body with the wrong content hash")
		}
		legacyNativeGoGraphSetW85Metadata(test, body)
		if previous, found := storage.objects[hash]; found && !bytes.Equal(previous, body) {
			test.Fatal("set writer changed an immutable CAS object")
		}
		storage.objects[hash] = bytes.Clone(body)
		return nil
	}
	storage.io.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
		storage.reads++
		storage.transferred += length
		if failure := check(current, length); failure != nil {
			return nil, failure
		}
		body, found := storage.objects[hash]
		if !found {
			return nil, ErrPreparedDocRangeObject
		}
		return bytes.Clone(body), nil
	}
	return storage
}

func legacyNativeGoGraphSetW85Metadata(test *testing.T, body []byte) {
	test.Helper()
	var object map[string]json.RawMessage
	if json.Unmarshal(body, &object) != nil || object == nil {
		return
	}
	for _, raw := range object {
		if len(raw) == 0 || raw[0] != '[' {
			continue
		}
		var rows []json.RawMessage
		if failure := json.Unmarshal(raw, &rows); failure != nil || len(rows) > 32 {
			test.Fatal("set metadata escaped its original 32-entry bound", len(rows), failure)
		}
	}
}

func legacyNativeGoGraphSetW85Step[Value any](test *testing.T, work legacyNativeGoGraphSetW85Work[Value], storage *legacyNativeGoGraphSetW85Store) (bool, error) {
	test.Helper()
	reads, writes, transferred := storage.reads, storage.writes, storage.transferred
	current, cancel := context.WithCancel(context.Background())
	storage.current = current
	ready, failure := work.step(current)
	cancel()
	storage.current = nil
	if storage.reads-reads+storage.writes-writes > 32 || storage.transferred-transferred > LegacyCanonicalEncodingBytes {
		test.Fatalf("set step escaped the shared 32-operation/64KiB IO quantum: reads=%d writes=%d bytes=%d", storage.reads-reads, storage.writes-writes, storage.transferred-transferred)
	}
	return ready, failure
}

func legacyNativeGoGraphSetW85Ready[Value any](test *testing.T, work legacyNativeGoGraphSetW85Work[Value], storage *legacyNativeGoGraphSetW85Store, budget int64) int64 {
	test.Helper()
	for range legacyNativeGoGraphSetW85Steps {
		ready, failure := legacyNativeGoGraphSetW85Step(test, work, storage)
		if failure != nil {
			test.Fatal("admitted set failed before ownership transfer", failure)
		}
		retained, failure := work.retainedBytes()
		if failure != nil || retained <= 0 || retained > budget {
			test.Fatal("set omitted live ownership or escaped aggregate admission", retained, budget, failure)
		}
		if ready {
			reads, writes := storage.reads, storage.writes
			if repeated, failure := legacyNativeGoGraphSetW85Step(test, work, storage); failure != nil || !repeated || storage.reads != reads || storage.writes != writes {
				test.Fatal("ready set performed IO or lost readiness", repeated, failure)
			}
			return retained
		}
	}
	test.Fatal("set exhausted the existing finite fixture schedule")
	return 0
}

func legacyNativeGoGraphSetW85Released[Value any](test *testing.T, work legacyNativeGoGraphSetW85Work[Value]) {
	test.Helper()
	if retained, failure := work.retainedBytes(); failure != nil || retained != 0 {
		test.Fatal("terminal set retained producer, graph identity, restore registry, or workspace", retained, failure)
	}
}

func legacyNativeGoGraphSetW85Failed[Value any](test *testing.T, work legacyNativeGoGraphSetW85Work[Value], storage *legacyNativeGoGraphSetW85Store, expected error) {
	test.Helper()
	legacyNativeGoGraphSetW85Released(test, work)
	reads, writes := storage.reads, storage.writes
	var empty Value
	for range 2 {
		if ready, failure := legacyNativeGoGraphSetW85Step(test, work, storage); ready || !errors.Is(failure, expected) {
			test.Fatal("failed set retried or lost its sticky error", ready, failure, expected)
		}
		if value, failure := work.take(context.Background()); !errors.Is(failure, expected) || !reflect.DeepEqual(value, empty) {
			test.Fatal("failed set transferred a partial result", failure, expected)
		}
		work.close()
		legacyNativeGoGraphSetW85Released(test, work)
	}
	if storage.reads != reads || storage.writes != writes {
		test.Fatal("terminal set touched CAS after cleanup")
	}
}

func legacyNativeGoGraphSetW85Reject[Value any](test *testing.T, work legacyNativeGoGraphSetW85Work[Value], constructorError error, storage *legacyNativeGoGraphSetW85Store, expected error, budget int64) {
	test.Helper()
	if constructorError != nil {
		if !errors.Is(constructorError, expected) {
			test.Fatal("set constructor returned the wrong rejection", constructorError, expected)
		}
		return
	}
	defer work.close()
	for range legacyNativeGoGraphSetW85Steps {
		ready, failure := legacyNativeGoGraphSetW85Step(test, work, storage)
		if failure != nil {
			if ready || !errors.Is(failure, expected) {
				test.Fatal("set returned the wrong rejection", ready, failure, expected)
			}
			legacyNativeGoGraphSetW85Failed(test, work, storage, failure)
			return
		}
		retained, failure := work.retainedBytes()
		if failure != nil || retained < 0 || retained > budget {
			test.Fatal("rejected set exceeded admission before failing", retained, budget, failure)
		}
		if ready {
			test.Fatal("malformed or unadmitted set became an empty success")
		}
	}
	test.Fatal("set rejection exhausted the existing finite fixture schedule")
}

func legacyNativeGoGraphSetW85Capture(test *testing.T, roots []any, binding ContentHash, storage *legacyNativeGoGraphSetW85Store) legacyNativeCheckpointRef {
	test.Helper()
	reads, writes := storage.reads, storage.writes
	constructor, cancel := context.WithCancel(context.Background())
	storage.current = constructor
	work, failure := newLegacyNativeCheckpointGoGraphSet(constructor, roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
	cancel()
	storage.current = nil
	if failure != nil || storage.reads != reads || storage.writes != writes {
		test.Fatal("set capture constructor failed or performed IO", failure)
	}
	defer work.close()
	legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointRef](test, work, storage, legacyNativeGoGraphSetW85Budget)
	if storage.reads != reads {
		test.Fatal("set capture read dependent CAS instead of preparing the borrowed graph")
	}
	reads, writes = storage.reads, storage.writes
	saved, failure := work.take(context.Background())
	if failure != nil || !saved.valid() || storage.reads != reads || storage.writes != writes {
		test.Fatal("set capture handoff was invalid or performed IO", failure)
	}
	legacyNativeGoGraphSetW85Released[legacyNativeCheckpointRef](test, work)
	if duplicate, failure := work.take(context.Background()); !errors.Is(failure, ErrIntegrity) || duplicate != (legacyNativeCheckpointRef{}) {
		test.Fatal("set capture transferred the same root twice", failure)
	}
	for hash, body := range storage.objects {
		if HashContent(body) != hash || len(body) == 0 || len(body) > LegacyCanonicalEncodingBytes {
			test.Fatal("set capture changed a CAS body or escaped its original size limit")
		}
	}
	return saved
}

func legacyNativeGoGraphSetW85Restore(test *testing.T, saved legacyNativeCheckpointRef, binding ContentHash, storage *legacyNativeGoGraphSetW85Store, minimum int64) []legacyNativeAnyValue {
	test.Helper()
	reads, writes := storage.reads, storage.writes
	constructor, cancel := context.WithCancel(context.Background())
	storage.current = constructor
	work, failure := newLegacyNativeCheckpointGoGraphSetRestore(constructor, saved, binding, storage.io, legacyNativeGoGraphSetW85Budget)
	cancel()
	storage.current = nil
	if failure != nil || storage.reads != reads || storage.writes != writes {
		test.Fatal("set restore constructor failed or performed IO", failure)
	}
	defer work.close()
	retained := legacyNativeGoGraphSetW85Ready[[]legacyNativeAnyValue](test, work, storage, legacyNativeGoGraphSetW85Budget)
	if retained < minimum {
		test.Fatal("ready set omitted its actual output graph from handoff admission", retained, minimum)
	}
	reads = storage.reads
	actual, failure := work.take(context.Background())
	if failure != nil || len(actual) == 0 || storage.reads != reads || storage.writes != writes {
		test.Fatal("set restore handoff returned no roots or performed IO", failure)
	}
	legacyNativeGoGraphSetW85Released[[]legacyNativeAnyValue](test, work)
	work.close()
	work.close()
	if duplicate, failure := work.take(context.Background()); !errors.Is(failure, ErrIntegrity) || duplicate != nil {
		test.Fatal("set restore transferred its owners twice", failure)
	}
	return actual
}

type legacyNativeGoGraphSetW85Identity struct {
	kind     reflect.Kind
	address  uintptr
	length   int
	capacity int
}

func legacyNativeGoGraphSetW85Values(test *testing.T, expected []any, actual []legacyNativeAnyValue) {
	test.Helper()
	if len(expected) != len(actual) {
		test.Fatal("set lost or reordered roots", len(expected), len(actual))
	}
	type pair struct {
		expected any
		actual   legacyNativeAnyValue
	}
	pending := make([]pair, 0, len(expected))
	for ordinal, value := range expected {
		pending = append(pending, pair{expected: value, actual: actual[ordinal]})
	}
	owners := make(map[legacyNativeGoGraphSetW85Identity]uintptr)
	sources := make(map[uintptr]legacyNativeGoGraphSetW85Identity)
	for visited := 0; len(pending) > 0; visited++ {
		if visited >= legacyNativeGoGraphSetW85Steps {
			test.Fatal("native owner verification exceeded its finite DAG schedule")
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if failure := current.actual.validateOwnership(); failure != nil {
			test.Fatal("restored value did not have real native ownership", failure)
		}
		var identity legacyNativeGoGraphSetW85Identity
		var owner uintptr
		switch expectedValue := current.expected.(type) {
		case nil, bool, string:
			if !reflect.DeepEqual(current.expected, current.actual.value) {
				test.Fatal("set changed an exact primitive or text value")
			}
			continue
		case float64:
			actualValue, matched := current.actual.value.(float64)
			if !matched || math.Float64bits(actualValue) != math.Float64bits(expectedValue) {
				test.Fatal("set changed exact finite float64 bits")
			}
			continue
		case map[string]any:
			actualValue, matched := current.actual.value.(map[string]any)
			if !matched || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
				test.Fatal("set changed map type, nilness, or length")
			}
			if expectedValue == nil {
				if current.actual.owner != (legacyNativeAnyValueOwner{}) {
					test.Fatal("typed nil map acquired a fabricated owner")
				}
				continue
			}
			identity = legacyNativeGoGraphSetW85Identity{kind: reflect.Map, address: reflect.ValueOf(expectedValue).Pointer()}
			owner = reflect.ValueOf(current.actual.owner.object).Pointer()
			if identity.address == reflect.ValueOf(actualValue).Pointer() {
				test.Fatal("CAS restore borrowed the original mutable map")
			}
		case []any:
			actualValue, matched := current.actual.value.([]any)
			if !matched || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) || cap(actualValue) != cap(expectedValue) {
				test.Fatal("set changed array type, nilness, length, or capacity")
			}
			if expectedValue == nil {
				if current.actual.owner != (legacyNativeAnyValueOwner{}) {
					test.Fatal("typed nil array acquired a fabricated owner")
				}
				continue
			}
			identity = legacyNativeGoGraphSetW85Identity{kind: reflect.Slice, address: reflect.ValueOf(expectedValue).Pointer(), length: len(expectedValue), capacity: cap(expectedValue)}
			owner = reflect.ValueOf(current.actual.owner.array).Pointer()
			if cap(expectedValue) > 0 && identity.address == reflect.ValueOf(actualValue).Pointer() {
				test.Fatal("CAS restore borrowed the original mutable array")
			}
		default:
			test.Fatal("fixture supplied a non-grammar value")
		}
		if owner == 0 {
			test.Fatal("non-nil container lost its actual native owner")
		}
		if previous, found := owners[identity]; found {
			if previous != owner {
				test.Fatal("shared producer, writer identity, or restore registry split a repeated container")
			}
			continue
		}
		if previous, found := sources[owner]; found && previous != identity {
			test.Fatal("set merged distinct equal-valued containers or distinct slice shapes")
		}
		owners[identity], sources[owner] = owner, identity
		if current.actual.owner.object != nil {
			expectedMap := current.expected.(map[string]any)
			cursor, failure := current.actual.owner.mapCheckpoint(context.Background(), true)
			if failure != nil {
				test.Fatal("restored map could not expose its actual native child metadata", failure)
			}
			seen := make(map[string]bool, len(expectedMap))
			for range len(expectedMap) + 1 {
				entry, found, failure := cursor.next(context.Background())
				if failure != nil {
					test.Fatal("restored map had inconsistent child metadata", failure)
				}
				if !found {
					break
				}
				expectedChild, exists := expectedMap[entry.Key]
				if !exists || seen[entry.Key] || entry.KeyCapacity < len(entry.Key) {
					test.Fatal("restored map changed keys or fabricated duplicate child metadata")
				}
				seen[entry.Key] = true
				child := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes}, excess: entry.ValueExcess, owner: entry.owner}
				if text, matched := entry.Value.(string); matched {
					if entry.ValueExcess < 0 || entry.ValueExcess%2 != 0 {
						test.Fatal("restored map child had invalid string excess")
					}
					child.capacity = len(text) + int(entry.ValueExcess/2)
				}
				pending = append(pending, pair{expected: expectedChild, actual: child})
			}
			cursor.close()
			if len(seen) != len(expectedMap) {
				test.Fatal("restored map omitted native child metadata")
			}
		} else {
			for ordinal, expectedChild := range current.expected.([]any) {
				child, failure := current.actual.owner.arrayValue(context.Background(), ordinal)
				if failure != nil {
					test.Fatal("restored array omitted tracked native child metadata", failure)
				}
				pending = append(pending, pair{expected: expectedChild, actual: child})
			}
		}
	}
}

func TestLegacyNativeGoGraphSetW85SharedOwners(test *testing.T) {
	binding := HashContent([]byte("W85 shared initial roots, not an Event completion claim"))
	for count := 2; count <= 32; count++ {
		test.Run("roots-"+strconv.Itoa(count), func(test *testing.T) {
			leaf := map[string]any{"number": math.Float64frombits(0x8000000000000000), "text": "한🙂\x00\xff"}
			array := make([]any, 2, 7)
			array[0], array[1] = leaf, leaf
			branch := map[string]any{"array": array, "leaf": leaf}
			roots := make([]any, count)
			for ordinal := range roots {
				roots[ordinal] = []any{branch, array, leaf}[ordinal%3]
			}
			storage := legacyNativeGoGraphSetW85Storage(test)
			saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
			actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 1)
			legacyNativeGoGraphSetW85Values(test, roots, actual)
			if len(leaf) != 2 || leaf["text"] != "한🙂\x00\xff" || math.Float64bits(leaf["number"].(float64)) != 0x8000000000000000 || len(array) != 2 || cap(array) != 7 || len(branch) != 2 {
				test.Fatal("set preparation mutated borrowed source values or shapes")
			}
		})
	}
	test.Run("equal-values-distinct-shapes", func(test *testing.T) {
		backing := make([]any, 3, 8)
		backing[0], backing[1], backing[2] = float64(1), float64(2), float64(3)
		firstMap, secondMap := map[string]any{"equal": true}, map[string]any{"equal": true}
		firstArray, secondArray := make([]any, 1, 4), make([]any, 1, 4)
		firstArray[0], secondArray[0] = float64(9), float64(9)
		roots := []any{firstMap, secondMap, firstMap, firstArray, secondArray, firstArray,
			backing[:1:8], backing[:2:8], backing[:1:4], backing[:1:8], backing[1:2:8],
			map[string]any(nil), map[string]any{}, []any(nil), []any{}, make([]any, 0, 5),
			nil, false, true, float64(0), math.Copysign(0, -1), math.SmallestNonzeroFloat64, math.MaxFloat64,
			"", "opaque\x00\xff한🙂", map[string]any{"nil-map": map[string]any(nil), "empty-array": make([]any, 0, 3)}}
		storage := legacyNativeGoGraphSetW85Storage(test)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		legacyNativeGoGraphSetW85Values(test, roots, legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 1))
	})
	test.Run("deep-shared-dag", func(test *testing.T) {
		leaf := map[string]any{"leaf": float64(7)}
		var current any = leaf
		for range 96 {
			current = map[string]any{"next": current, "shared": leaf}
		}
		roots := []any{current, leaf, current, []any{current, leaf}}
		storage := legacyNativeGoGraphSetW85Storage(test)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		legacyNativeGoGraphSetW85Values(test, roots, legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 1))
	})
}

func TestLegacyNativeGoGraphSetW85BoundedPayloadAdmission(test *testing.T) {
	binding := HashContent([]byte("W85 bounded root set admission"))
	text := strings.Repeat("한🙂\x00\\\"", LegacyCanonicalEncodingBytes/9+37)
	array := make([]any, 4097, 8193)
	for ordinal := range array {
		array[ordinal] = float64(ordinal)
	}
	leaf := map[string]any{"text": text, "array": array}
	roots := []any{leaf, array, leaf, text}
	test.Run("large-text-array-and-metadata", func(test *testing.T) {
		storage := legacyNativeGoGraphSetW85Storage(test)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		if storage.writes < 3 || len(storage.objects) < 3 {
			test.Fatal("large root set did not use actual bounded CAS objects")
		}
		minimum := int64(len(text)) + int64(cap(array))*int64(unsafe.Sizeof(any(nil)))
		actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, minimum)
		legacyNativeGoGraphSetW85Values(test, roots, actual)
		if len(leaf) != 2 || leaf["text"] != text || len(array) != 4097 || cap(array) != 8193 || array[4096] != float64(4096) {
			test.Fatal("bounded preparation changed its borrowed payload")
		}
		other := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, minimum)
		legacyNativeGoGraphSetW85Values(test, roots, other)
		if actual[0].owner.object == other[0].owner.object || actual[1].owner.array == other[1].owner.array {
			test.Fatal("independent restore sets reused a previously released registry")
		}
	})
	for _, fixture := range []struct {
		name   string
		roots  []any
		budget int64
	}{
		{name: "positive-but-unadmitted", roots: []any{nil, true}, budget: 1},
		{name: "live-text", roots: []any{text, text}, budget: int64(len(text) - 1)},
		{name: "hidden-array-capacity", roots: []any{make([]any, 0, 16384), nil}, budget: 16384*int64(unsafe.Sizeof(any(nil))) - 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeGoGraphSetW85Storage(test)
			writer, failure := newLegacyNativeCheckpointGoGraphSet(context.Background(), fixture.roots, binding, storage.io, fixture.budget)
			if storage.reads != 0 || storage.writes != 0 {
				test.Fatal("unadmitted constructor performed IO")
			}
			legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, writer, failure, storage, ErrDocumentResourceLimit, fixture.budget)
			saved := legacyNativeGoGraphSetW85Capture(test, fixture.roots, binding, storage)
			reads, writes := storage.reads, storage.writes
			restore, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), saved, binding, storage.io, fixture.budget)
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("unadmitted restore constructor performed IO")
			}
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, restore, failure, storage, ErrDocumentResourceLimit, fixture.budget)
		})
	}
}

func legacyNativeGoGraphSetW85Field(test *testing.T, body []byte, key string, replacement []byte) []byte {
	test.Helper()
	var object map[string]json.RawMessage
	if failure := json.Unmarshal(body, &object); failure != nil || object[key] == nil {
		test.Fatal("metadata mutation did not target an actual field", key, failure)
	}
	prefix := []byte(strconv.Quote(key) + ":")
	needle := append(bytes.Clone(prefix), object[key]...)
	changed := bytes.Replace(body, needle, append(bytes.Clone(prefix), replacement...), 1)
	if bytes.Equal(changed, body) {
		test.Fatal("metadata mutation did not change the actual bounded CAS body", key)
	}
	return changed
}

func TestLegacyNativeGoGraphSetW85FormatAndTypeFences(test *testing.T) {
	binding := HashContent([]byte("W85 closed root set grammar"))
	type namedString string
	type namedMap map[string]any
	type namedArray []any
	cyclicMap := map[string]any{}
	cyclicMap["self"] = cyclicMap
	cyclicArray := make([]any, 1)
	cyclicArray[0] = cyclicArray
	for _, fixture := range []struct {
		name  string
		roots []any
	}{
		{name: "no-roots"}, {name: "empty-roots", roots: []any{}}, {name: "roots-33", roots: make([]any, 33)},
		{name: "int", roots: []any{int(1), nil}}, {name: "float32", roots: []any{float32(1), nil}},
		{name: "json-number", roots: []any{json.Number("1"), nil}}, {name: "named-string", roots: []any{namedString("text"), nil}},
		{name: "named-map", roots: []any{namedMap{"value": true}, nil}}, {name: "named-array", roots: []any{namedArray{true}, nil}},
		{name: "wrong-map", roots: []any{map[string]string{"value": "text"}, nil}}, {name: "bytes", roots: []any{[]byte("text"), nil}},
		{name: "pointer", roots: []any{new(bool), nil}}, {name: "nan", roots: []any{math.NaN(), nil}},
		{name: "infinity", roots: []any{math.Inf(1), nil}}, {name: "nested-type", roots: []any{map[string]any{"bad": int64(1)}, nil}},
		{name: "map-cycle", roots: []any{cyclicMap, cyclicMap}}, {name: "array-cycle", roots: []any{cyclicArray, cyclicArray}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeGoGraphSetW85Storage(test)
			work, failure := newLegacyNativeCheckpointGoGraphSet(context.Background(), fixture.roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			if storage.reads != 0 || storage.writes != 0 {
				test.Fatal("grammar constructor performed IO")
			}
			legacyNativeGoGraphSetW85Reject[legacyNativeCheckpointRef](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
	storage := legacyNativeGoGraphSetW85Storage(test)
	leaf := map[string]any{"value": float64(1)}
	saved := legacyNativeGoGraphSetW85Capture(test, []any{leaf, leaf}, binding, storage)
	for _, fixture := range []struct {
		name    string
		ref     legacyNativeCheckpointRef
		binding ContentHash
		budget  int64
		io      legacyNativeCheckpointIO
	}{
		{name: "invalid-binding", ref: saved, binding: "bad", budget: legacyNativeGoGraphSetW85Budget, io: storage.io},
		{name: "zero-budget", ref: saved, binding: binding, io: storage.io},
		{name: "negative-budget", ref: saved, binding: binding, budget: -1, io: storage.io},
		{name: "missing-read", ref: saved, binding: binding, budget: legacyNativeGoGraphSetW85Budget},
		{name: "empty-ref", binding: binding, budget: legacyNativeGoGraphSetW85Budget, io: storage.io},
		{name: "ref-hash", ref: legacyNativeCheckpointRef{Hash: "bad", Bytes: saved.Bytes}, binding: binding, budget: legacyNativeGoGraphSetW85Budget, io: storage.io},
		{name: "ref-size", ref: legacyNativeCheckpointRef{Hash: saved.Hash, Bytes: LegacyCanonicalEncodingBytes + 1}, binding: binding, budget: legacyNativeGoGraphSetW85Budget, io: storage.io},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), fixture.ref, fixture.binding, fixture.io, fixture.budget)
			if failure == nil || work != nil || !errors.Is(failure, ErrIntegrity) || storage.reads != reads || storage.writes != writes {
				test.Fatal("invalid restore admission was not rejected before IO", failure)
			}
		})
	}
	for _, fixture := range []struct {
		name    string
		binding ContentHash
		budget  int64
		io      legacyNativeCheckpointIO
	}{
		{name: "capture-binding", binding: "bad", budget: legacyNativeGoGraphSetW85Budget, io: storage.io},
		{name: "capture-zero-budget", binding: binding, io: storage.io},
		{name: "capture-negative-budget", binding: binding, budget: -1, io: storage.io},
		{name: "capture-missing-put", binding: binding, budget: legacyNativeGoGraphSetW85Budget},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			work, failure := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{nil, true}, fixture.binding, fixture.io, fixture.budget)
			if failure == nil || work != nil || !errors.Is(failure, ErrIntegrity) || storage.reads != reads || storage.writes != writes {
				test.Fatal("invalid capture admission was not rejected before IO", failure)
			}
		})
	}
	var metadata map[string]json.RawMessage
	body := storage.objects[saved.Hash]
	if failure := json.Unmarshal(body, &metadata); failure != nil {
		test.Fatal("set did not publish actual root metadata", failure)
	}
	var bindingKey, rootsKey, versionKey string
	var rows []json.RawMessage
	for key, raw := range metadata {
		var text string
		if json.Unmarshal(raw, &text) == nil && text == string(binding) {
			bindingKey = key
		}
		var candidates []json.RawMessage
		if json.Unmarshal(raw, &candidates) == nil && len(candidates) == 2 {
			var descriptor legacyNativeCheckpointValue
			if json.Unmarshal(candidates[0], &descriptor) == nil && len(descriptor.Wire) == legacyNativeValueCheckpointBytes {
				rootsKey, rows = key, candidates
			}
		}
		if key == "v" || strings.EqualFold(key, "version") {
			versionKey = key
		}
	}
	if bindingKey == "" || rootsKey == "" || versionKey == "" {
		test.Fatal("root CAS metadata omitted version, binding, or actual graph descriptors")
	}
	var descriptor legacyNativeCheckpointValue
	if failure := json.Unmarshal(rows[0], &descriptor); failure != nil || !descriptor.Body.valid() {
		test.Fatal("shared map root omitted its actual graph owner body", failure)
	}
	tooMany := []byte("[" + strings.TrimSuffix(strings.Repeat(string(rows[0])+",", 33), ",") + "]")
	shortWire := legacyNativeGoGraphSetW85Field(test, rows[0], "v", []byte(strconv.Quote(base64.StdEncoding.EncodeToString([]byte{1}))))
	conflictingLength := bytes.Clone(descriptor.Wire)
	binary.LittleEndian.PutUint64(conflictingLength[24:32], 2)
	conflictingLengthRow := legacyNativeGoGraphSetW85Field(test, rows[1], "v", []byte(strconv.Quote(base64.StdEncoding.EncodeToString(conflictingLength))))
	conflictingKind := bytes.Clone(descriptor.Wire)
	conflictingKind[1] = legacyNativeCheckpointArray
	binary.LittleEndian.PutUint64(conflictingKind[32:40], 1)
	conflictingKindRow := legacyNativeGoGraphSetW85Field(test, rows[1], "v", []byte(strconv.Quote(base64.StdEncoding.EncodeToString(conflictingKind))))
	reserved := bytes.Clone(descriptor.Wire)
	reserved[3] = 1
	reservedRow := legacyNativeGoGraphSetW85Field(test, rows[0], "v", []byte(strconv.Quote(base64.StdEncoding.EncodeToString(reserved))))
	conflictingBody := []byte("{\"h\":" + strconv.Quote(string(HashContent([]byte("different shared owner body")))) + ",\"n\":" + strconv.Itoa(descriptor.Body.Bytes) + "}")
	conflictingBodyRow := legacyNativeGoGraphSetW85Field(test, rows[1], "b", conflictingBody)
	for _, fixture := range []struct {
		name string
		body []byte
	}{
		{name: "broken-json", body: []byte("{")},
		{name: "unknown-field", body: append(bytes.Clone(body[:len(body)-1]), []byte(",\"unknown_w85\":true}")...)},
		{name: "duplicate-version", body: append(bytes.Clone(body[:len(body)-1]), []byte(","+strconv.Quote(versionKey)+":"+string(metadata[versionKey])+"}")...)},
		{name: "version", body: legacyNativeGoGraphSetW85Field(test, body, versionKey, []byte("255"))},
		{name: "version-type", body: legacyNativeGoGraphSetW85Field(test, body, versionKey, []byte("\"1\""))},
		{name: "binding-type", body: legacyNativeGoGraphSetW85Field(test, body, bindingKey, []byte("null"))},
		{name: "binding-format", body: legacyNativeGoGraphSetW85Field(test, body, bindingKey, []byte("\"bad\""))},
		{name: "binding-value", body: legacyNativeGoGraphSetW85Field(test, body, bindingKey, []byte(strconv.Quote(string(HashContent([]byte("other capture"))))))},
		{name: "nil-descriptors", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("null"))},
		{name: "empty-descriptors", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("[]"))},
		{name: "descriptors-33", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, tooMany)},
		{name: "descriptor-type", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("[true,"+string(rows[1])+"]"))},
		{name: "descriptor-wire", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("["+string(shortWire)+","+string(rows[1])+"]"))},
		{name: "descriptor-reserved", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("["+string(reservedRow)+","+string(rows[1])+"]"))},
		{name: "shared-identity-conflicting-length", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("["+string(rows[0])+","+string(conflictingLengthRow)+"]"))},
		{name: "shared-identity-conflicting-kind", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("["+string(rows[0])+","+string(conflictingKindRow)+"]"))},
		{name: "shared-identity-conflicting-body", body: legacyNativeGoGraphSetW85Field(test, body, rootsKey, []byte("["+string(rows[0])+","+string(conflictingBodyRow)+"]"))},
		{name: "trailing-data", body: append(bytes.Clone(body), []byte("{}")...)},
		{name: "noncanonical-whitespace", body: append([]byte(" "), body...)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			changed := legacyNativeCheckpointRef{Hash: HashContent(fixture.body), Bytes: len(fixture.body)}
			storage.objects[changed.Hash] = bytes.Clone(fixture.body)
			reads, writes := storage.reads, storage.writes
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), changed, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("malformed root constructor performed IO")
			}
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
	test.Run("wrong-current-binding", func(test *testing.T) {
		work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), saved, HashContent([]byte("different current claim")), storage.io, legacyNativeGoGraphSetW85Budget)
		legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
	})
	for _, damage := range []string{"root-hash", "root-length", "dependent-hash", "dependent-missing"} {
		test.Run(damage, func(test *testing.T) {
			target := saved.Hash
			if strings.HasPrefix(damage, "dependent-") {
				target = descriptor.Body.Hash
			}
			original, exists := storage.objects[target]
			if !exists {
				test.Fatal("integrity fixture did not locate an actual dependent graph object")
			}
			defer func() { storage.objects[target] = original }()
			switch damage {
			case "root-length":
				storage.objects[target] = bytes.Clone(original[:len(original)-1])
			case "dependent-missing":
				delete(storage.objects, target)
			default:
				storage.objects[target] = bytes.Repeat([]byte{'x'}, len(original))
			}
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), saved, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrPreparedDocRangeObject, legacyNativeGoGraphSetW85Budget)
		})
	}
}

func legacyNativeGoGraphSetW85Lifecycle[Value any](test *testing.T, name string, prepare func(*legacyNativeGoGraphSetW85Store) func(context.Context) (legacyNativeGoGraphSetW85Work[Value], error)) {
	test.Helper()
	for _, scenario := range []string{"constructor-cancel", "step-cancel", "progress-cancel", "ready-step-cancel", "ready-take-cancel", "early-take", "close", "io-cancel", "io-failure"} {
		test.Run(name+"/"+scenario, func(test *testing.T) {
			storage := legacyNativeGoGraphSetW85Storage(test)
			construct := prepare(storage)
			reads, writes := storage.reads, storage.writes
			constructor, cancel := context.WithCancel(context.Background())
			if scenario == "constructor-cancel" {
				cancel()
			}
			storage.current = constructor
			work, failure := construct(constructor)
			cancel()
			storage.current = nil
			if storage.reads != reads || storage.writes != writes {
				test.Fatal("lifecycle constructor performed IO")
			}
			if scenario == "constructor-cancel" {
				if !errors.Is(failure, context.Canceled) {
					test.Fatal("constructor ignored its current canceled context", failure)
				}
				return
			}
			if failure != nil {
				test.Fatal("lifecycle constructor failed", failure)
			}
			defer work.close()
			expected := error(context.Canceled)
			var empty Value
			switch scenario {
			case "close":
				if _, failure := legacyNativeGoGraphSetW85Step(test, work, storage); failure != nil {
					test.Fatal("set could not prepare before explicit close", failure)
				}
				work.close()
				work.close()
				expected = ErrIntegrity
				if ready, failure := legacyNativeGoGraphSetW85Step(test, work, storage); ready || !errors.Is(failure, expected) {
					test.Fatal("closed set resumed", ready, failure)
				}
			case "early-take":
				expected = ErrIntegrity
				if value, failure := work.take(context.Background()); !errors.Is(failure, expected) || !reflect.DeepEqual(value, empty) {
					test.Fatal("unfinished set transferred partial ownership", failure)
				}
			case "io-cancel", "io-failure":
				expected = errors.New("W85 current CAS failure")
				if scenario == "io-cancel" {
					expected = context.Canceled
				} else {
					storage.failure = expected
				}
				failed := false
				for range legacyNativeGoGraphSetW85Steps {
					current, stop := context.WithCancel(context.Background())
					storage.current = current
					if scenario == "io-cancel" {
						storage.cancelIO = stop
					}
					beforeReads, beforeWrites, transferred := storage.reads, storage.writes, storage.transferred
					ready, failure := work.step(current)
					stop()
					storage.current, storage.cancelIO = nil, nil
					if storage.reads-beforeReads+storage.writes-beforeWrites > 32 || storage.transferred-transferred > LegacyCanonicalEncodingBytes {
						test.Fatal("failing IO escaped the original set quantum")
					}
					if failure != nil {
						if ready || !errors.Is(failure, expected) || storage.reads == reads && storage.writes == writes {
							test.Fatal("set failed before actual IO or lost the current IO failure", ready, failure)
						}
						failed = true
						break
					}
					if ready {
						test.Fatal("set ignored cancellation or failure returned by actual CAS")
					}
					if retained, failure := work.retainedBytes(); failure != nil || retained <= 0 || retained > legacyNativeGoGraphSetW85Budget {
						test.Fatal("set omitted live admission before its failing IO", retained, failure)
					}
				}
				if !failed {
					test.Fatal("IO failure fixture exceeded its finite schedule")
				}
			default:
				if scenario == "progress-cancel" {
					if _, failure := legacyNativeGoGraphSetW85Step(test, work, storage); failure != nil {
						test.Fatal("set could not prepare before current cancellation", failure)
					}
				}
				if strings.HasPrefix(scenario, "ready-") {
					legacyNativeGoGraphSetW85Ready(test, work, storage, legacyNativeGoGraphSetW85Budget)
				}
				current, stop := context.WithCancel(context.Background())
				stop()
				beforeReads, beforeWrites := storage.reads, storage.writes
				if scenario == "ready-take-cancel" {
					if value, failure := work.take(current); !errors.Is(failure, expected) || !reflect.DeepEqual(value, empty) {
						test.Fatal("ready set handed off through a canceled current context", failure)
					}
				} else if ready, failure := work.step(current); ready || !errors.Is(failure, expected) {
					test.Fatal("set ignored cancellation at its current step boundary", ready, failure)
				}
				if storage.reads != beforeReads || storage.writes != beforeWrites {
					test.Fatal("already canceled invocation touched CAS")
				}
			}
			legacyNativeGoGraphSetW85Failed(test, work, storage, expected)
		})
	}
}

func TestLegacyNativeGoGraphSetW85CurrentContextAndCleanup(test *testing.T) {
	binding := HashContent([]byte("W85 current context and terminal registry cleanup"))
	leaf := map[string]any{"text": strings.Repeat("한🙂", LegacyCanonicalEncodingBytes/7+13)}
	roots := []any{leaf, []any{leaf, leaf}, leaf}
	legacyNativeGoGraphSetW85Lifecycle[legacyNativeCheckpointRef](test, "capture", func(storage *legacyNativeGoGraphSetW85Store) func(context.Context) (legacyNativeGoGraphSetW85Work[legacyNativeCheckpointRef], error) {
		return func(current context.Context) (legacyNativeGoGraphSetW85Work[legacyNativeCheckpointRef], error) {
			return newLegacyNativeCheckpointGoGraphSet(current, roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
		}
	})
	legacyNativeGoGraphSetW85Lifecycle[[]legacyNativeAnyValue](test, "restore", func(storage *legacyNativeGoGraphSetW85Store) func(context.Context) (legacyNativeGoGraphSetW85Work[[]legacyNativeAnyValue], error) {
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		return func(current context.Context) (legacyNativeGoGraphSetW85Work[[]legacyNativeAnyValue], error) {
			return newLegacyNativeCheckpointGoGraphSetRestore(current, saved, binding, storage.io, legacyNativeGoGraphSetW85Budget)
		}
	})
}
