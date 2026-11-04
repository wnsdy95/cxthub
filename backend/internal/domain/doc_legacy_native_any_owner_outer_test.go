package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"unsafe"
)

type legacyNativeAnyOwnerOuterDestination struct {
	Input  map[string]any `json:"input"`
	Output any            `json:"output"`
	Tail   string         `json:"tail"`
}

type legacyNativeAnyOwnerOuterFixture struct {
	destination *legacyNativeAnyOwnerOuterDestination
	native      *legacyNativeObject
	field       int
	frame       legacyJSONValueScanner
	source      LegacyByteSource
	raw         []byte
}

type legacyNativeAnyOwnerOuterStopContext struct {
	context.Context
	cancel  context.CancelFunc
	fixture *legacyNativeAnyOwnerOuterFixture
	ready   func(*legacyNativeAny) bool
	stopped *legacyNativeAny
}

func (current *legacyNativeAnyOwnerOuterStopContext) Err() error {
	if current.Context.Err() == nil && current.fixture.native.containerActive && current.fixture.native.field == current.fixture.field {
		parser := current.fixture.native.containers[current.fixture.field]
		if parser != nil && current.ready(parser) {
			current.stopped = parser
			current.cancel()
		}
	}
	return current.Context.Err()
}

func legacyNativeAnyOwnerOuterAllocation(count int, width uintptr) int64 {
	if count == 0 {
		return 0
	}
	return 64 + 2*int64(count)*int64(width)
}

func legacyNativeAnyOwnerOuterPages[Value any](test *testing.T, pages *legacyCanonicalPages[Value]) int64 {
	test.Helper()
	if !pages.valid() {
		test.Fatal("actual parser retained invalid metadata pages")
	}
	var retained int64
	if pages.single != nil {
		retained += legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*pages.single))
	}
	if pages.inline != nil {
		retained += legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*pages.inline))
	}
	var visit func(*legacyCanonicalPageNode[Value])
	visit = func(node *legacyCanonicalPageNode[Value]) {
		if node == nil {
			return
		}
		retained += legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*node))
		if node.values != nil {
			retained += legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*node.values))
		}
		for _, child := range node.children {
			visit(child)
		}
	}
	visit(pages.root)
	return retained
}

func legacyNativeAnyOwnerOuterMapMetadata(test *testing.T, object *legacyNativeAnyMap) int64 {
	test.Helper()
	if object == nil || !object.valid() {
		test.Fatal("metadata oracle requires an actual valid parser map")
	}
	retained := legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*object))
	retained += legacyNativeAnyOwnerOuterAllocation(1, 64)
	retained += legacyNativeAnyOwnerOuterAllocation(max(8, len(object.ownership)), unsafe.Sizeof("")+8+unsafe.Sizeof(legacyNativeAnyMapOwnership{}))
	retained += legacyNativeAnyOwnerOuterPages(test, &object.links)
	var children int64
	for _, row := range object.ownership {
		children += legacyNativeAnyOwnerOuterMetadata(test, row.owner)
	}
	if children != object.childBytes {
		test.Fatal("map child metadata differs from actual recursively owned containers", children, object.childBytes)
	}
	return retained + children
}

func legacyNativeAnyOwnerOuterArrayMetadata(test *testing.T, array *legacyNativeAnyArray) int64 {
	test.Helper()
	if array == nil || !array.tracked || array.children.length() != len(array.decoded) {
		test.Fatal("metadata oracle requires an actual tracked parser array")
	}
	retained := legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*array))
	retained += legacyNativeAnyOwnerOuterAllocation(cap(array.previous), unsafe.Sizeof(any(nil)))
	retained += legacyNativeAnyOwnerOuterPages(test, &array.children)
	var children int64
	for position := range array.decoded {
		child := array.children.at(position)
		if child == nil {
			test.Fatal("actual parser array lost a child ownership row", position)
		}
		children += legacyNativeAnyOwnerOuterMetadata(test, child.owner)
	}
	if children != array.childBytes {
		test.Fatal("array child metadata differs from actual recursively owned containers", children, array.childBytes)
	}
	return retained + children
}

func legacyNativeAnyOwnerOuterMetadata(test *testing.T, owner legacyNativeAnyValueOwner) int64 {
	test.Helper()
	var retained int64
	if owner.object != nil {
		if owner.array != nil || owner.revision != owner.object.revision {
			test.Fatal("actual parser map owner lost its unique root or revision")
		}
		retained = legacyNativeAnyOwnerOuterMapMetadata(test, owner.object)
	} else if owner.array != nil {
		if owner.revision != owner.array.revision {
			test.Fatal("actual parser array owner lost its revision")
		}
		retained = legacyNativeAnyOwnerOuterArrayMetadata(test, owner.array)
	} else if owner.revision != 0 {
		test.Fatal("empty actual parser owner retained a revision")
	}
	if retained != owner.bytes {
		test.Fatal("actual parser owner metadata differs from independent physical accounting", retained, owner.bytes)
	}
	return retained
}

func newLegacyNativeAnyOwnerOuterFixture(test *testing.T, raw, name string) *legacyNativeAnyOwnerOuterFixture {
	test.Helper()
	destination := &legacyNativeAnyOwnerOuterDestination{}
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(*destination))
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 3, false)
	if err != nil {
		test.Fatal("outer owner constructor rejected valid fixture", err)
	}
	native.fieldLimit = 1
	native.start(0, len(raw))
	buffer := &legacySourceProofBuffer{body: []byte(raw)}
	return &legacyNativeAnyOwnerOuterFixture{destination: destination, native: native, field: metadata.match(name), source: buffer.input(), raw: buffer.body}
}

func (fixture *legacyNativeAnyOwnerOuterFixture) step(test *testing.T, ctx context.Context) (bool, *legacyNormalizationInput, error) {
	test.Helper()
	input := &legacyNormalizationInput{ctx: ctx, source: fixture.source}
	complete, err := fixture.native.step(ctx, input, &fixture.frame)
	if input.failure != nil || input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 ||
		input.fields < 0 || input.fields > 1 || input.values < 0 || input.values > legacyNativeAnyValueActions {
		test.Fatal("outer owner fixture escaped its actual shared input", input.failure, input.workBytes, input.loads, input.fields, input.values)
	}
	return complete, input, err
}

func (fixture *legacyNativeAnyOwnerOuterFixture) first(test *testing.T) *legacyNativeAny {
	test.Helper()
	for range 32 {
		complete, input, err := fixture.step(test, context.Background())
		if err != nil || complete {
			test.Fatal("first actual container did not expose its field handoff", err, complete)
		}
		if len(fixture.native.containers) > fixture.field {
			parser := fixture.native.containers[fixture.field]
			if parser != nil && parser.finished && !fixture.native.containerActive {
				if input.fields != 1 || parser.visible != (legacyNativeAnyValueOwner{}) {
					test.Fatal("first actual field handoff retained an extra visible owner", input.fields, parser.visible)
				}
				return parser
			}
		}
	}
	test.Fatal("first actual field exceeded its finite step allowance")
	return nil
}

func (fixture *legacyNativeAnyOwnerOuterFixture) stop(test *testing.T, finished bool) *legacyNativeAny {
	test.Helper()
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	current := &legacyNativeAnyOwnerOuterStopContext{Context: parent, cancel: cancel, fixture: fixture}
	current.ready = func(parser *legacyNativeAny) bool {
		if finished {
			return parser.finished
		}
		return !parser.finished && parser.frame != nil && parser.frame.parent == nil && parser.frame.phase == legacyNativeAnyAfter &&
			!parser.pending && !parser.applying && !parser.scanning && parser.scalar == nil && parser.number == nil &&
			(parser.rootMap != nil && parser.rootMap.childBytes > 0 || parser.rootArray != nil && parser.rootArray.childBytes > 0)
	}
	for range 32 {
		complete, input, err := fixture.step(test, current)
		if current.stopped != nil {
			if complete || !errors.Is(err, context.Canceled) || input.fields != 0 || fixture.native.phase != legacyNativeObjectApply ||
				!fixture.native.containerActive || current.stopped.finished != finished || fixture.destination.Tail != "" {
				test.Fatal("conditional cancellation did not stop the actual outer field before handoff", err, complete, input.fields, finished)
			}
			return current.stopped
		}
		if complete || err != nil || input.fields != 0 {
			test.Fatal("actual replacement passed the requested ownership boundary", err, complete, input.fields, finished)
		}
	}
	test.Fatal("conditional outer field cancellation exceeded its finite step allowance", finished)
	return nil
}

func (fixture *legacyNativeAnyOwnerOuterFixture) accounting(test *testing.T, parser *legacyNativeAny, oldMetadata, oldPayload int64) int64 {
	test.Helper()
	if parser.scalar != nil || parser.number != nil || parser.pending || parser.semanticError != nil || parser.live.err != nil {
		test.Fatal("outer accounting observation is not at a quiescent actual container boundary")
	}
	parserBytes := legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*parser)) + legacyNativeAnyOwnerOuterAllocation(cap(parser.raw), 1) + oldMetadata
	var live int64
	for active := parser.frame; active != nil; active = active.parent {
		live += legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(*active)) + legacyNativeAnyOwnerOuterAllocation(active.keyCapacity, 1)
		if active.object != nil {
			heap := active.object.heap()
			if heap.err != nil {
				test.Fatal("actual live map payload accounting failed", heap.err)
			}
			live += heap.bytes + legacyNativeAnyOwnerOuterMapMetadata(test, active.object)
		} else if active.array != nil {
			heap := active.array.heap()
			if heap.err != nil {
				test.Fatal("actual live array payload accounting failed", heap.err)
			}
			live += heap.bytes + legacyNativeAnyOwnerOuterArrayMetadata(test, active.array)
		} else {
			test.Fatal("actual live frame lost its container")
		}
	}
	if live != parser.live.bytes {
		test.Fatal("actual live frame metadata or payload differs from physical accounting", live, parser.live.bytes)
	}
	parserBytes += live
	if parser.rootReserved {
		if parser.rootMap != nil {
			heap := parser.rootMap.heap()
			if heap.err != nil {
				test.Fatal("actual reserved map payload accounting failed", heap.err)
			}
			parserBytes += heap.bytes + legacyNativeAnyOwnerOuterMapMetadata(test, parser.rootMap)
		} else if parser.rootArray != nil {
			heap := parser.rootArray.heap()
			if heap.err != nil {
				test.Fatal("actual reserved array payload accounting failed", heap.err)
			}
			parserBytes += heap.bytes + legacyNativeAnyOwnerOuterArrayMetadata(test, parser.rootArray)
		} else {
			test.Fatal("actual reserved parser lost its root")
		}
	}
	var reported docRangeHeaderHeapEstimate
	parser.retained(&reported)
	if reported.err != nil || reported.bytes != parserBytes {
		test.Fatal("parser retained old or current metadata other than exactly once", reported.bytes, parserBytes, oldMetadata, reported.err)
	}
	base := *fixture.native
	base.containers = make([]*legacyNativeAny, len(fixture.native.containers), cap(fixture.native.containers))
	copy(base.containers, fixture.native.containers)
	base.containers[fixture.field] = nil
	base.heaps = make([]docRangeHeaderHeapEstimate, len(fixture.native.heaps), cap(fixture.native.heaps))
	copy(base.heaps, fixture.native.heaps)
	storage := legacyNativeAnyOwnerOuterAllocation(1, fixture.native.destination.Elem().Field(fixture.native.metadata.fields[fixture.field].index).Type().Size())
	base.heaps[fixture.field] = docRangeHeaderHeapEstimate{bytes: storage}
	baseBytes, baseErr := base.retained()
	retained, retainedErr := fixture.native.retained()
	if baseErr != nil || retainedErr != nil || retained != baseBytes+parserBytes+oldPayload {
		test.Fatal("outer owner metadata and old field payload were not retained exactly once", retained, baseBytes, parserBytes, oldPayload, baseErr, retainedErr)
	}
	return retained
}

func (fixture *legacyNativeAnyOwnerOuterFixture) finish(test *testing.T) {
	test.Helper()
	var expected legacyNativeAnyOwnerOuterDestination
	if err := json.Unmarshal(fixture.raw, &expected); err != nil {
		test.Fatal("encoding/json rejected valid outer ownership fixture", err)
	}
	for range 32 {
		complete, _, err := fixture.step(test, context.Background())
		if err != nil {
			test.Fatal("fresh context could not finish the actual native object", err)
		}
		if complete {
			if fixture.destination.Tail != "done" || !reflect.DeepEqual(*fixture.destination, expected) {
				test.Fatal("outer field retry changed replacement or merge semantics", fixture.destination, expected)
			}
			return
		}
	}
	test.Fatal("actual native object retry exceeded its finite step allowance")
}

func TestLegacyNativeAnyOwnerOuterInterfaceReplacement(test *testing.T) {
	for _, oldRoot := range []struct{ name, raw string }{
		{name: "object", raw: `{"keep":{"branch":[{"text":"old-object"}]},"other":[null,{"flag":true}]}`},
		{name: "array", raw: `[{"keep":[{"text":"old-array"}]},[false,{"count":7}]]`},
	} {
		for _, newRoot := range []struct{ name, raw string }{
			{name: "object", raw: `{"fresh":[{"text":"new-object"}],"tail":{"leaf":[null,3]}}`},
			{name: "array", raw: `[{"fresh":[{"text":"new-array"}]},[true,{"count":9}]]`},
		} {
			test.Run(oldRoot.name+"-to-"+newRoot.name, func(test *testing.T) {
				fixture := newLegacyNativeAnyOwnerOuterFixture(test, `{"output":`+oldRoot.raw+`,"OUTPUT":`+newRoot.raw+`,"tail":"done"}`, "output")
				previous := fixture.first(test)
				oldOwner, oldHeap, oldExcess := previous.result.owner, fixture.native.heaps[fixture.field], fixture.native.backingExcess[fixture.field]
				oldValue := fixture.destination.Output
				oldMetadata := legacyNativeAnyOwnerOuterMetadata(test, oldOwner)
				storage := legacyNativeAnyOwnerOuterAllocation(1, unsafe.Sizeof(any(nil)))
				oldPayload := oldHeap.bytes - storage
				if oldHeap.err != nil || oldPayload <= 0 || oldPayload != previous.result.heap.bytes || oldMetadata <= 0 ||
					oldOwner.object != nil && oldOwner.object.childBytes <= 0 || oldOwner.array != nil && oldOwner.array.childBytes <= 0 {
					test.Fatal("first real outer field did not retain nested metadata and payload", oldHeap, oldMetadata, oldPayload)
				}
				fixture.accounting(test, previous, 0, 0)
				parser := fixture.stop(test, false)
				if parser == previous || parser.visible != oldOwner || fixture.native.heaps[fixture.field] != oldHeap ||
					fixture.native.backingExcess[fixture.field] != oldExcess || !reflect.DeepEqual(fixture.destination.Output, oldValue) ||
					reflect.ValueOf(fixture.destination.Output).Pointer() != reflect.ValueOf(oldValue).Pointer() {
					test.Fatal("actual interface replacement did not carry previous.result.owner while preserving the old field")
				}
				fixture.accounting(test, parser, oldMetadata, oldPayload)
				finished := fixture.stop(test, true)
				if finished != parser || parser.frame != nil || !parser.rootReserved || parser.visible != oldOwner ||
					fixture.native.heaps[fixture.field] != oldHeap || fixture.native.backingExcess[fixture.field] != oldExcess ||
					!reflect.DeepEqual(fixture.destination.Output, oldValue) || reflect.ValueOf(fixture.destination.Output).Pointer() != reflect.ValueOf(oldValue).Pointer() {
					test.Fatal("finished-but-active actual replacement released its old metadata or field before handoff")
				}
				legacyNativeAnyOwnerOuterMetadata(test, parser.result.owner)
				before := fixture.accounting(test, parser, oldMetadata, oldPayload)
				complete, input, err := fixture.step(test, context.Background())
				if err != nil || complete || input.fields != 1 || fixture.native.containerActive || parser.visible != (legacyNativeAnyValueOwner{}) ||
					fixture.native.containers[fixture.field] != parser || !reflect.DeepEqual(fixture.destination.Output, parser.result.value) ||
					reflect.ValueOf(fixture.destination.Output).Pointer() != reflect.ValueOf(parser.result.value).Pointer() ||
					fixture.native.heaps[fixture.field].bytes != parser.result.heap.bytes+storage || fixture.native.backingExcess[fixture.field] != parser.result.excess {
					test.Fatal("fresh context did not perform the actual interface field handoff", err, complete, input.fields)
				}
				after := fixture.accounting(test, parser, 0, 0)
				if before-after != oldMetadata+oldPayload {
					test.Fatal("actual field handoff did not release exactly the old metadata and old payload", before-after, oldMetadata+oldPayload)
				}
				fixture.finish(test)
			})
		}
	}
}

func TestLegacyNativeAnyOwnerOuterGenericMapReuse(test *testing.T) {
	fixture := newLegacyNativeAnyOwnerOuterFixture(test,
		`{"input":{"keep":{"branch":[{"old":"payload"}]},"replace":{"old":[true,"before"]}},"INPUT":{"replace":[{"new":"after"}],"added":{"leaf":[null,7]}},"tail":"done"}`, "input")
	previous := fixture.first(test)
	root, oldHeap := previous.rootMap, fixture.native.heaps[fixture.field]
	if root == nil || root.childBytes <= 0 || previous.result.owner.object != root {
		test.Fatal("first actual generic map lost its nested root ownership")
	}
	legacyNativeAnyOwnerOuterMetadata(test, previous.result.owner)
	keptValue, keptOwner := fixture.destination.Input["keep"], root.ownership["keep"].owner
	replacedOwner := root.ownership["replace"].owner
	if keptOwner.object == nil || replacedOwner.object == nil {
		test.Fatal("generic map fixture lacks real nested absent-key and overwritten owners")
	}
	fixture.accounting(test, previous, 0, 0)
	parser := fixture.stop(test, false)
	if parser != previous || parser.rootMap != root || parser.visible != (legacyNativeAnyValueOwner{}) || fixture.native.heaps[fixture.field] != oldHeap ||
		reflect.ValueOf(fixture.destination.Input).Pointer() != reflect.ValueOf(root.values()).Pointer() ||
		root.ownership["keep"].owner != keptOwner || root.ownership["replace"].owner.object == replacedOwner.object ||
		reflect.ValueOf(fixture.destination.Input["keep"]).Pointer() != reflect.ValueOf(keptValue).Pointer() {
		test.Fatal("actual genericMap restart did not reuse its root and retain absent keys without a visible duplicate")
	}
	if _, found := fixture.destination.Input["added"]; found {
		test.Fatal("incomplete generic map observation occurred after its following member")
	}
	fixture.accounting(test, parser, 0, 0)
	finished := fixture.stop(test, true)
	if finished != parser || parser.rootMap != root || parser.frame != nil || parser.visible != (legacyNativeAnyValueOwner{}) ||
		parser.result.owner.object != root || fixture.native.heaps[fixture.field] != oldHeap || len(fixture.destination.Input) != 3 ||
		root.ownership["keep"].owner != keptOwner || reflect.ValueOf(fixture.destination.Input["keep"]).Pointer() != reflect.ValueOf(keptValue).Pointer() {
		test.Fatal("finished-but-active genericMap lost its merged root or duplicated old ownership")
	}
	legacyNativeAnyOwnerOuterMetadata(test, parser.result.owner)
	before := fixture.accounting(test, parser, 0, 0)
	complete, input, err := fixture.step(test, context.Background())
	if err != nil || complete || input.fields != 1 || fixture.native.containerActive || fixture.native.containers[fixture.field] != previous ||
		parser.visible != (legacyNativeAnyValueOwner{}) || fixture.native.heaps[fixture.field] != parser.result.heap ||
		fixture.native.backingExcess[fixture.field] != parser.result.excess || root.ownership["keep"].owner != keptOwner ||
		reflect.ValueOf(fixture.destination.Input).Pointer() != reflect.ValueOf(root.values()).Pointer() {
		test.Fatal("fresh context did not hand off the actual reused genericMap root", err, complete, input.fields)
	}
	after := fixture.accounting(test, parser, 0, 0)
	if before != after {
		test.Fatal("genericMap handoff changed retention despite already owning exactly one merged root", before, after)
	}
	fixture.finish(test)
}
