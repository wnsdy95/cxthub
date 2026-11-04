package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeAnyOwnerHandoffAllocation(count int, width uintptr) int64 {
	if count == 0 || width == 0 {
		return 0
	}
	return 64 + 2*int64(count)*int64(width)
}

func legacyNativeAnyOwnerHandoffPageBytes[Value any](test *testing.T, storage *legacyCanonicalPages[Value]) int64 {
	test.Helper()
	var retained int64
	if storage.single != nil {
		if storage.inline != nil || storage.root != nil || storage.count > 1 {
			test.Fatal("singleton metadata overlaps another backing")
		}
		retained += legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(*storage.single))
	}
	if storage.inline != nil {
		if storage.root != nil || storage.count > len(storage.inline) {
			test.Fatal("inline metadata overlaps paged backing")
		}
		retained += legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(*storage.inline))
	}
	seenNodes := make(map[*legacyCanonicalPageNode[Value]]bool)
	seenPages := make(map[*[legacyCanonicalPageValues]Value]bool)
	var visit func(*legacyCanonicalPageNode[Value])
	visit = func(node *legacyCanonicalPageNode[Value]) {
		if node == nil {
			return
		}
		if seenNodes[node] || len(seenNodes) >= 128 {
			test.Fatal("metadata node aliases or exceeds the finite fixture bound")
		}
		seenNodes[node] = true
		retained += legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(*node))
		if node.values != nil {
			if seenPages[node.values] {
				test.Fatal("metadata value pages alias")
			}
			seenPages[node.values] = true
			retained += legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(*node.values))
		}
		for _, child := range node.children {
			visit(child)
		}
	}
	visit(storage.root)
	if len(seenNodes) != storage.nodes || len(seenPages) != storage.pages {
		test.Fatal("physical metadata allocations disagree with recorded node/page counts")
	}
	return retained
}

func legacyNativeAnyOwnerHandoffMetadata(test *testing.T, owner legacyNativeAnyValueOwner, depth int) int64 {
	test.Helper()
	if depth > 32 || owner.object != nil && owner.array != nil {
		test.Fatal("owner metadata exceeds the finite fixture shape")
	}
	if owner.object == nil && owner.array == nil {
		if owner.bytes != 0 || owner.revision != 0 {
			test.Fatal("scalar retained an unbacked owner witness")
		}
		return 0
	}
	var expected, children int64
	var actual docRangeHeaderHeapEstimate
	if native := owner.object; native != nil {
		if owner.revision != native.revision {
			test.Fatal("closed map lost its borrowed revision witness")
		}
		for _, row := range native.ownership {
			children += legacyNativeAnyOwnerHandoffMetadata(test, row.owner, depth+1)
		}
		if native.childBytes != children {
			test.Fatal("map childBytes differs from independently retained child metadata", native.childBytes, children)
		}
		expected = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(*native)) +
			legacyNativeAnyOwnerHandoffAllocation(1, 64) +
			legacyNativeAnyOwnerHandoffAllocation(max(8, len(native.ownership)), unsafe.Sizeof("")+8+unsafe.Sizeof(legacyNativeAnyMapOwnership{})) +
			legacyNativeAnyOwnerHandoffPageBytes(test, &native.links) + children
		native.retained(&actual)
	} else {
		list := owner.array
		if owner.revision != list.revision {
			test.Fatal("closed array lost its borrowed revision witness")
		}
		for position := 0; position < list.children.count; position++ {
			child := list.children.at(position)
			if child == nil {
				test.Fatal("array metadata omitted a child slot", position)
			}
			children += legacyNativeAnyOwnerHandoffMetadata(test, child.owner, depth+1)
		}
		if list.childBytes != children {
			test.Fatal("array childBytes differs from independently retained child metadata", list.childBytes, children)
		}
		expected = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(*list)) +
			legacyNativeAnyOwnerHandoffAllocation(cap(list.previous), unsafe.Sizeof(any(nil))) +
			legacyNativeAnyOwnerHandoffPageBytes(test, &list.children) + children
		list.retained(&actual)
	}
	if actual.err != nil || actual.bytes != expected || owner.bytes != expected {
		test.Fatal("metadata-only retention omitted owners or recounted payload", actual, owner.bytes, expected)
	}
	return expected
}

func legacyNativeAnyOwnerHandoffValue(test *testing.T, value legacyNativeAnyValue, stringCapacity, depth int) (int64, int64) {
	test.Helper()
	if depth > 32 {
		test.Fatal("accepted-value fixture exceeded its finite nesting bound")
	}
	if err := value.validateOwnership(); err != nil {
		test.Fatal("actual parser lost closed ownership", err)
	}
	var expectedHeap, expectedExcess int64
	ctx := context.Background()
	switch decoded := value.value.(type) {
	case nil:
	case bool:
		expectedHeap = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(decoded))
	case float64:
		expectedHeap = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(decoded))
	case string:
		if decoded != "abc" || value.capacity != stringCapacity || value.capacity <= len(decoded) {
			test.Fatal("array/map handoff lost actual spare string capacity", decoded, value.capacity, stringCapacity)
		}
		expectedHeap = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(decoded)) +
			legacyNativeAnyOwnerHandoffAllocation(stringCapacity, 1)
		expectedExcess = 2 * int64(stringCapacity-len(decoded))
	case map[string]any:
		if decoded == nil || value.owner.object == nil || value.owner.array != nil ||
			reflect.ValueOf(decoded).Pointer() != reflect.ValueOf(value.owner.object.values()).Pointer() {
			test.Fatal("map checkpoint no longer refers to the actual closed parser wrapper")
		}
		expectedHeap = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(decoded)) +
			legacyNativeAnyOwnerHandoffAllocation(1, 64) +
			legacyNativeAnyOwnerHandoffAllocation(max(8, len(decoded)), unsafe.Sizeof("")+8+unsafe.Sizeof(any(nil)))
		checkpoint, err := value.owner.mapCheckpoint(ctx, true)
		if err != nil || checkpoint == nil || checkpoint.owner != value.owner.object {
			test.Fatal("closed map owner could not issue its actual checkpoint", err)
		}
		defer checkpoint.close()
		seen := make(map[string]bool)
		for range len(decoded) {
			entry, found, err := checkpoint.next(ctx)
			if err != nil || !found || seen[entry.Key] {
				test.Fatal("map owner checkpoint lost or repeated a committed child", entry.Key, found, err)
			}
			seen[entry.Key] = true
			row, exists := value.owner.object.ownership[entry.Key]
			member, present := decoded[entry.Key]
			if !exists || !present || entry.owner != row.owner || entry.KeyCapacity != row.keyCapacity ||
				entry.ValueBytes != row.valueBytes || entry.ValueExcess != row.valueExcess || !reflect.DeepEqual(entry.Value, member) {
				test.Fatal("map checkpoint entry.owner or accounting detached from its actual row", entry.Key)
			}
			child := legacyNativeAnyValue{value: entry.Value, heap: docRangeHeaderHeapEstimate{bytes: entry.ValueBytes},
				excess: entry.ValueExcess, owner: entry.owner}
			if text, ok := entry.Value.(string); ok {
				if entry.ValueExcess%2 != 0 {
					test.Fatal("map string child has fractional spare capacity")
				}
				child.capacity = len(text) + int(entry.ValueExcess/2)
			}
			childHeap, childExcess := legacyNativeAnyOwnerHandoffValue(test, child, stringCapacity, depth+1)
			expectedHeap += legacyNativeAnyOwnerHandoffAllocation(entry.KeyCapacity, 1) + childHeap
			expectedExcess += 2*int64(entry.KeyCapacity-len(entry.Key)) + childExcess
		}
		if _, found, err := checkpoint.next(ctx); err != nil || found {
			test.Fatal("map checkpoint exposed an extra child", found, err)
		}
	case []any:
		list := value.owner.array
		if decoded == nil || list == nil || value.owner.object != nil || list.previous != nil ||
			list.children.count != len(decoded) || len(decoded) != 0 && !list.tracked ||
			len(decoded) != len(list.values()) || cap(decoded) != cap(list.values()) ||
			reflect.ValueOf(decoded).Pointer() != reflect.ValueOf(list.values()).Pointer() {
			test.Fatal("array handoff lost the closed wrapper or exact child metadata slots")
		}
		expectedHeap = legacyNativeAnyOwnerHandoffAllocation(1, unsafe.Sizeof(decoded)) +
			legacyNativeAnyOwnerHandoffAllocation(cap(decoded), unsafe.Sizeof(any(nil)))
		for position := range decoded {
			child, err := value.owner.arrayValue(ctx, position)
			if err != nil {
				test.Fatal("array owner could not return its actual parser child", position, err)
			}
			row := list.children.at(position)
			if row == nil || child.heap.bytes != row.heapBytes || child.excess != row.excess ||
				child.capacity != row.capacity || child.owner != row.owner || !reflect.DeepEqual(child.value, decoded[position]) {
				test.Fatal("arrayValue changed exact child heap/excess/capacity/owner", position)
			}
			childHeap, childExcess := legacyNativeAnyOwnerHandoffValue(test, child, stringCapacity, depth+1)
			expectedHeap += childHeap
			expectedExcess += childExcess
		}
	default:
		test.Fatal("actual parser produced an unsupported fixture value")
	}
	if _, text := value.value.(string); !text && value.capacity != 0 {
		test.Fatal("non-string child acquired string capacity metadata")
	}
	if value.heap.err != nil || value.heap.bytes != expectedHeap || value.excess != expectedExcess {
		test.Fatal("closed child changed exact payload heap or excess", value.heap, expectedHeap, value.excess, expectedExcess)
	}
	return expectedHeap, expectedExcess
}

type legacyNativeAnyOwnerHandoffDocument struct {
	Value map[string]any `json:"value"`
}

func legacyNativeAnyOwnerHandoffGeneric(test *testing.T) *legacyNativeObject {
	test.Helper()
	document := &legacyNativeAnyOwnerHandoffDocument{}
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(*document))
	native, err := newLegacyNativeObject(reflect.ValueOf(document), &metadata, 1, false)
	if err != nil || len(metadata.fields) != 1 || !metadata.fields[0].genericMap {
		test.Fatal("fixture did not select the actual generic-map field path", err)
	}
	return native
}

func legacyNativeAnyOwnerHandoffDecode(test *testing.T, native *legacyNativeObject, raw string, observe func(*legacyNativeAny)) *legacyNativeAny {
	test.Helper()
	native.field, native.valueStart, native.valueEnd = 0, 0, len(raw)
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	source := buffer.input()
	ctx := context.Background()
	var frame legacyJSONValueScanner
	for stepIndex := 0; stepIndex < 512; stepIndex++ {
		input := &legacyNormalizationInput{ctx: ctx, source: source, values: legacyNativeAnyValueActions - 1}
		complete, err := native.decodeAnyContainer(ctx, input, &frame)
		if err != nil || input.failure != nil || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes ||
			input.loads > 2 || input.unbounded || input.fields > native.fieldActionLimit() || input.values > legacyNativeAnyValueActions {
			test.Fatal("actual generic-map decode escaped its finite shared quantum", stepIndex, err, input)
		}
		for _, page := range input.pages {
			clear(page)
		}
		parser := native.containers[0]
		if observe != nil {
			observe(parser)
		}
		if complete {
			if native.containerActive || !parser.finished || parser.position != len(raw) || parser.frame != nil || parser.pending || parser.depth != 0 {
				test.Fatal("generic-map decode did not close its actual root wrapper")
			}
			return parser
		}
	}
	test.Fatal("actual generic-map decode exhausted its finite commit schedule")
	return nil
}

func TestLegacyNativeAnyOwnerHandoffClosedContainers(test *testing.T) {
	text, _ := legacyNativeValueCheckpointW76Accept(test, `"abc"`)
	fixtures := []struct{ name, raw string }{
		{name: "map_array_map", raw: `{"outer":[{"text":"abc","list":["abc",{"deeper":[null,true,-0,"abc",{},[]]}]},["abc",{"leaf":"abc"}]],"empty_object":{},"empty_array":[]}`},
		{name: "array_map_array", raw: `[{"outer":{"list":["abc",{},[],false,-0]}},["abc",{"leaf":["abc"]}],{},[]]`},
	}
	for _, count := range []int{0, 1, 8, 9, 64, 65} {
		arrays, objects := make([]string, count), make([]string, count)
		for position := range count {
			arrays[position] = `"abc"`
			objects[position] = fmt.Sprintf(`"member_%d":["abc",{}]`, position)
		}
		fixtures = append(fixtures, struct{ name, raw string }{fmt.Sprintf("array_children_%d", count), `[` + strings.Join(arrays, ",") + `]`},
			struct{ name, raw string }{fmt.Sprintf("map_children_%d", count), `{` + strings.Join(objects, ",") + `}`})
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			value, buffer := legacyNativeValueCheckpointW76Accept(test, fixture.raw)
			clear(buffer.body)
			legacyNativeAnyOwnerHandoffValue(test, value, text.capacity, 0)
			legacyNativeAnyOwnerHandoffMetadata(test, value.owner, 0)
		})
	}
}

func TestLegacyNativeAnyOwnerHandoffReplacementAndReuse(test *testing.T) {
	text, _ := legacyNativeValueCheckpointW76Accept(test, `"abc"`)
	for _, replacement := range []struct{ name, raw string }{
		{name: "null", raw: `null`},
		{name: "string", raw: `"abc"`},
		{name: "empty_array", raw: `[]`},
		{name: "nested_map", raw: `{"fresh":["abc",{}]}`},
	} {
		test.Run("duplicate_"+replacement.name, func(test *testing.T) {
			old := `[{"gone":["abc",{"deeper":["abc",{},[]]}]},{"tail":{}}]`
			raw := `{"keep":[{"keep":"abc"}],"replace":` + old + `,"repl\u0061ce":` + replacement.raw + `}`
			native := legacyNativeAnyOwnerHandoffGeneric(test)
			var previous legacyNativeAnyMapOwnership
			var beforeChildren int64
			captured := false
			parser := legacyNativeAnyOwnerHandoffDecode(test, native, raw, func(parser *legacyNativeAny) {
				if !captured && parser.rootMap != nil && parser.rootMap.revision == 2 {
					var exists bool
					previous, exists = parser.rootMap.ownership["replace"]
					if exists {
						beforeChildren, captured = parser.rootMap.childBytes, true
					}
				}
			})
			root := parser.rootMap
			current := root.ownership["replace"]
			if !captured || previous.owner.array == nil || previous.owner.bytes <= 0 ||
				root.childBytes != beforeChildren-previous.owner.bytes+current.owner.bytes ||
				root.childBytes == beforeChildren+current.owner.bytes || root.revision != 3 ||
				len(root.ownership) != 2 || root.links.count != 2 || current.linkID != previous.linkID {
				test.Fatal("duplicate JSON key did not replace exactly one child owner/accounting row", beforeChildren, root.childBytes)
			}
			if err := previous.owner.validate(); err != nil {
				test.Fatal("removing a child was confused with revising its detached wrapper", err)
			}
			baseline, _ := legacyNativeValueCheckpointW76Accept(test, `{"keep":[{"keep":"abc"}],"replace":`+replacement.raw+`}`)
			if root.childBytes != baseline.owner.object.childBytes || parser.result.owner.bytes != baseline.owner.bytes {
				test.Fatal("duplicate replacement retained the old child's metadata charge")
			}
			legacyNativeAnyOwnerHandoffValue(test, parser.result, text.capacity, 0)
			legacyNativeAnyOwnerHandoffMetadata(test, parser.result.owner, 0)
		})
	}
	test.Run("borrowed_revision_is_not_persisted_reference", func(test *testing.T) {
		ctx := context.Background()
		native := legacyNativeAnyOwnerHandoffGeneric(test)
		parser := legacyNativeAnyOwnerHandoffDecode(test, native, `{"keep":{"list":["abc"]},"replace":[{"old":"abc"}]}`, nil)
		borrowed := parser.result
		root := parser.rootMap
		keep := root.ownership["keep"].owner
		cursor, err := borrowed.owner.mapCheckpoint(ctx, true)
		if err != nil {
			test.Fatal("initial borrowed owner could not open a checkpoint", err)
		}
		defer cursor.close()
		bounds := legacyNativeValueCheckpointBounds{PayloadBytes: 1024, Nodes: 128}
		snapshot, err := borrowed.checkpoint(ctx, 37, bounds)
		if err != nil {
			test.Fatal("initial container-reference record could not be captured", err)
		}
		defer snapshot.close()
		persisted := snapshot.state
		before, err := persisted.encode(ctx, bounds)
		if err != nil || persisted.Reference != 37 || persisted.Length != 2 {
			test.Fatal("fixture lost its explicit immutable record ID and old cardinality", persisted, err)
		}
		reused := legacyNativeAnyOwnerHandoffDecode(test, native, `{"replace":{"fresh":["abc"]},"added":[null,"abc"]}`, nil)
		if reused != parser || reused.rootMap != root || reused.result.owner.object != root || root.ownership["keep"].owner != keep ||
			reflect.ValueOf(borrowed.value).Pointer() != reflect.ValueOf(reused.result.value).Pointer() ||
			native.destination.Elem().Field(0).Pointer() != reflect.ValueOf(root.values()).Pointer() ||
			len(root.values()) != 3 || root.revision <= borrowed.owner.revision {
			test.Fatal("generic root-map reuse replaced the map owner or failed to advance its revision")
		}
		if _, present := borrowed.value.(map[string]any)["added"]; !present {
			test.Fatal("fixture did not expose the live alias behind the old borrowed witness")
		}
		if err := borrowed.validateOwnership(); !errors.Is(err, ErrConflict) {
			test.Fatal("old borrowed value silently accepted the new root body", err)
		}
		if err := borrowed.owner.matches(reused.result.value); !errors.Is(err, ErrConflict) {
			test.Fatal("old borrowed witness silently matched the revised map", err)
		}
		if checkpoint, err := borrowed.owner.mapCheckpoint(ctx, true); checkpoint != nil || !errors.Is(err, ErrConflict) {
			test.Fatal("stale owner issued a checkpoint over the new body", err)
		}
		if _, found, err := cursor.next(ctx); found || !errors.Is(err, ErrConflict) {
			test.Fatal("old map cursor silently traversed the new body", found, err)
		}
		if stale, err := borrowed.checkpoint(ctx, 37, bounds); stale != nil || !errors.Is(err, ErrConflict) {
			test.Fatal("stale witness captured a replacement container-reference record", err)
		}
		after, err := snapshot.state.encode(ctx, bounds)
		if err != nil || snapshot.state != persisted || !bytes.Equal(before, after) {
			test.Fatal("owner revision mutated an already captured immutable ID record", err)
		}
		record, err := decodeLegacyNativeValueCheckpoint(ctx, after, bounds)
		if err != nil || record != persisted {
			test.Fatal("immutable container-reference record changed on decode", err)
		}
		if inline, ready, err := record.inline(ctx, bounds); err != nil || ready || inline != nil {
			test.Fatal("container ID record incorrectly claimed to restore the borrowed map body", ready, err)
		}
		legacyNativeAnyOwnerHandoffValue(test, reused.result, text.capacity, 0)
		legacyNativeAnyOwnerHandoffMetadata(test, reused.result.owner, 0)
	})
}

func TestLegacyNativeAnyOwnerHandoffRejectsMetadata(test *testing.T) {
	ctx := context.Background()
	validate := func(value legacyNativeAnyValue) error { return value.validateOwnership() }
	firstChild := func(value legacyNativeAnyValue) error { _, err := value.owner.arrayValue(ctx, 0); return err }
	textChild := func(value legacyNativeAnyValue) error { _, err := value.owner.arrayValue(ctx, 1); return err }
	mapChild := func(value legacyNativeAnyValue) error {
		checkpoint, err := value.owner.mapCheckpoint(ctx, true)
		if err != nil {
			return err
		}
		defer checkpoint.close()
		_, _, err = checkpoint.next(ctx)
		return err
	}
	foreignArray, _ := legacyNativeValueCheckpointW76Accept(test, `[{"list":["abc"]},"abc"]`)
	foreignMap, _ := legacyNativeValueCheckpointW76Accept(test, `{"list":["abc"]}`)
	sentinel := errors.New("handoff heap sentinel")
	for _, fixture := range []struct {
		name    string
		raw     string
		corrupt func(*legacyNativeAnyValue)
		read    func(legacyNativeAnyValue) error
		want    error
	}{
		{name: "missing_container_owner", corrupt: func(value *legacyNativeAnyValue) { value.owner = legacyNativeAnyValueOwner{} }, read: validate, want: ErrIntegrity},
		{name: "both_owner_kinds", corrupt: func(value *legacyNativeAnyValue) { value.owner.object = foreignMap.owner.object }, read: validate, want: ErrIntegrity},
		{name: "unbacked_witness", corrupt: func(value *legacyNativeAnyValue) { value.owner = legacyNativeAnyValueOwner{bytes: 1} }, read: validate, want: ErrIntegrity},
		{name: "negative_owner_bytes", corrupt: func(value *legacyNativeAnyValue) { value.owner.bytes = -1 }, read: validate, want: ErrIntegrity},
		{name: "mismatched_owner_bytes", corrupt: func(value *legacyNativeAnyValue) { value.owner.bytes++ }, read: validate, want: ErrIntegrity},
		{name: "stale_array_witness", corrupt: func(value *legacyNativeAnyValue) { value.owner.revision++ }, read: firstChild, want: ErrConflict},
		{name: "different_array_body", corrupt: func(value *legacyNativeAnyValue) { value.value = foreignArray.value }, read: validate, want: ErrIntegrity},
		{name: "different_array_length", corrupt: func(value *legacyNativeAnyValue) { value.value = value.value.([]any)[:1] }, read: validate, want: ErrIntegrity},
		{name: "different_array_capacity", raw: `[{"list":["abc"]},"abc",null]`, corrupt: func(value *legacyNativeAnyValue) {
			decoded := value.value.([]any)
			value.value = decoded[:len(decoded):len(decoded)]
		}, read: validate, want: ErrIntegrity},
		{name: "heap_error", corrupt: func(value *legacyNativeAnyValue) { value.heap.err = sentinel }, read: validate, want: sentinel},
		{name: "negative_heap", corrupt: func(value *legacyNativeAnyValue) { value.heap.bytes = -1 }, read: validate, want: ErrIntegrity},
		{name: "excess_over_heap", corrupt: func(value *legacyNativeAnyValue) { value.excess = value.heap.bytes + 1 }, read: validate, want: ErrIntegrity},
		{name: "container_string_capacity", corrupt: func(value *legacyNativeAnyValue) { value.capacity = 1 }, read: validate, want: ErrIntegrity},
		{name: "untracked_children", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.tracked = false }, read: firstChild, want: ErrIntegrity},
		{name: "missing_child_slot", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.count-- }, read: firstChild, want: ErrIntegrity},
		{name: "invalid_metadata_pages", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.nodes = -1 }, read: firstChild, want: ErrIntegrity},
		{name: "child_bytes_mismatch", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.childBytes++ }, read: firstChild, want: ErrIntegrity},
		{name: "child_bytes_overflow", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.childBytes = math.MaxInt64 }, read: firstChild, want: ErrIntegrity},
		{name: "negative_child_heap", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.at(1).heapBytes = -1 }, read: textChild, want: ErrIntegrity},
		{name: "odd_child_string_excess", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.at(1).excess = 1 }, read: textChild, want: ErrIntegrity},
		{name: "lost_child_string_capacity", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.at(1).capacity = 0 }, read: textChild, want: ErrIntegrity},
		{name: "child_owner_bytes_mismatch", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.at(0).owner.bytes++ }, read: firstChild, want: ErrIntegrity},
		{name: "stale_child_owner", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.at(0).owner.revision++ }, read: firstChild, want: ErrConflict},
		{name: "different_child_map_body", corrupt: func(value *legacyNativeAnyValue) { value.owner.array.children.at(0).owner = foreignMap.owner }, read: firstChild, want: ErrIntegrity},
		{name: "different_map_body", raw: `{"child":["abc",{}]}`, corrupt: func(value *legacyNativeAnyValue) { value.value = foreignMap.value }, read: validate, want: ErrIntegrity},
		{name: "map_child_bytes_mismatch", raw: `{"child":["abc",{}]}`, corrupt: func(value *legacyNativeAnyValue) { value.owner.object.childBytes++ }, read: mapChild, want: ErrIntegrity},
		{name: "map_link_count_mismatch", raw: `{"child":["abc",{}]}`, corrupt: func(value *legacyNativeAnyValue) { value.owner.object.links.count-- }, read: mapChild, want: ErrIntegrity},
		{name: "map_entry_owner_mismatch", raw: `{"child":["abc",{}]}`, corrupt: func(value *legacyNativeAnyValue) {
			row := value.owner.object.ownership["child"]
			row.owner = foreignArray.owner
			value.owner.object.ownership["child"] = row
		}, read: mapChild, want: ErrIntegrity},
		{name: "map_entry_stale_witness", raw: `{"child":["abc",{}]}`, corrupt: func(value *legacyNativeAnyValue) {
			row := value.owner.object.ownership["child"]
			row.owner.revision++
			value.owner.object.ownership["child"] = row
		}, read: mapChild, want: ErrConflict},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := fixture.raw
			if raw == "" {
				raw = `[{"list":["abc"]},"abc"]`
			}
			value, _ := legacyNativeValueCheckpointW76Accept(test, raw)
			if err := value.validateOwnership(); err != nil {
				test.Fatal("corruption fixture was not initially accepted", err)
			}
			fixture.corrupt(&value)
			if err := fixture.read(value); !errors.Is(err, fixture.want) {
				test.Fatal("corrupted owner metadata was accepted or misclassified", err, fixture.want)
			}
		})
	}
	test.Run("owner_access_bounds_kind_and_cancellation", func(test *testing.T) {
		array, _ := legacyNativeValueCheckpointW76Accept(test, `["abc"]`)
		object, _ := legacyNativeValueCheckpointW76Accept(test, `{"child":["abc"]}`)
		for _, position := range []int{-1, 1} {
			if _, err := array.owner.arrayValue(ctx, position); !errors.Is(err, ErrIntegrity) {
				test.Fatal("array owner accepted an out-of-range position", position, err)
			}
		}
		if checkpoint, err := array.owner.mapCheckpoint(ctx, true); checkpoint != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatal("array witness issued a map checkpoint", err)
		}
		if _, err := object.owner.arrayValue(ctx, 0); !errors.Is(err, ErrIntegrity) {
			test.Fatal("map witness issued an array value", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		array.owner.revision++
		object.owner.revision++
		if _, err := array.owner.arrayValue(canceled, 0); !errors.Is(err, context.Canceled) {
			test.Fatal("array owner lost cancellation precedence over stale metadata", err)
		}
		if checkpoint, err := object.owner.mapCheckpoint(canceled, true); checkpoint != nil || !errors.Is(err, context.Canceled) {
			test.Fatal("map owner lost cancellation precedence over stale metadata", err)
		}
	})
}
