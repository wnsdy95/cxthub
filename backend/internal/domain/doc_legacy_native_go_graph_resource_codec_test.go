package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeGoGraphResourceValue(test *testing.T, owner *legacyNativeResourceOwner, offset, length, capacity int) legacyNativeAnyValue {
	test.Helper()
	array, err := newLegacyNativeResourceArray(context.Background(), owner, offset, length, capacity)
	if err != nil {
		test.Fatal(err)
	}
	for position := 0; position < capacity; position++ {
		child, err := array.resourceValue(context.Background(), position)
		if err != nil {
			test.Fatal(err)
		}
		input := &legacyNormalizationInput{}
		var appended bool
		if position < length {
			appended, err = array.appendValueGuarded(context.Background(), input, child, nil)
		} else {
			appended, err = array.appendTailValueGuarded(context.Background(), input, child, nil)
		}
		if err != nil || !appended || input.workBytes > canonicalCheckBytes {
			test.Fatal("native view metadata did not attach within the existing quantum", appended, err)
		}
	}
	array.finishResource()
	var metadata docRangeHeaderHeapEstimate
	array.retained(&metadata)
	value := legacyNativeAnyValue{value: array.values(), heap: array.heap(), excess: array.backingExcess(), owner: legacyNativeAnyValueOwner{array: array, revision: array.revision, bytes: metadata.bytes}}
	if err := value.validateOwnership(); err != nil {
		test.Fatal("resource view did not acquire valid native ownership", err)
	}
	return value
}

func legacyNativeGoGraphResourceSave(test *testing.T, values []legacyNativeAnyValue, storage *legacyNativeGoGraphSetW85Store, binding ContentHash) legacyNativeCheckpointRef {
	test.Helper()
	identity := newLegacyNativeCheckpointGraphIdentity()
	defer identity.close()
	state := legacyNativeCheckpointGoGraphSetState{Version: 1, Binding: binding, Roots: make(legacyNativeCheckpointGoGraphRoots, len(values))}
	for index, value := range values {
		writer, err := newLegacyNativeCheckpointGraphWriter(context.Background(), value, storage.io, identity, legacyNativeGoGraphSetW85Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer writer.close()
		legacyNativeGoGraphSetW85Ready[legacyNativeCheckpointValue](test, writer, storage, legacyNativeGoGraphSetW85Budget)
		state.Roots[index], err = writer.take(context.Background())
		if err != nil {
			test.Fatal(err)
		}
	}
	storage.current = context.Background()
	saved, err := legacyNativeCheckpointWriteJSON(storage.current, storage.io, state)
	storage.current = nil
	if err != nil {
		test.Fatal(err)
	}
	return saved
}

func TestLegacyNativeGoGraphResourceCodecWholeBacking(test *testing.T) {
	fixture := legacyNativeResourceW90BigFixture(test)
	child, entry := legacyNativeResourceW90Backing(test, fixture, 3)
	work := legacyNativeResourceW90Prepare(test, fixture, child, entry)
	legacyNativeResourceW90Ready(test, work, fixture)
	owner, err := work.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	defer owner.close()
	unfilled, err := newLegacyNativeResourceArray(context.Background(), owner, 1, 0, 2)
	if err != nil {
		test.Fatal(err)
	}
	childValue, err := unfilled.resourceValue(context.Background(), 0)
	if err != nil {
		test.Fatal(err)
	}
	if appended, err := unfilled.appendTailValueGuarded(context.Background(), &legacyNormalizationInput{}, childValue, nil); !appended || err != nil {
		test.Fatal("matching shared tail slot was rejected", appended, err)
	}
	if appended, err := unfilled.appendTailValueGuarded(context.Background(), &legacyNormalizationInput{}, childValue, nil); appended || !errors.Is(err, ErrIntegrity) {
		test.Fatal("shared nil slot was overwritten by different tail metadata", appended, err)
	}
	if owner.root.value.([]any)[:3][2] != nil || owner.valid() != nil {
		test.Fatal("rejected tail attachment changed the whole backing")
	}
	narrow := legacyNativeGoGraphResourceValue(test, owner, 1, 1, 1)
	wide := legacyNativeGoGraphResourceValue(test, owner, 0, 1, 3)
	storage := legacyNativeGoGraphSetW85Storage(test)
	binding := HashContent([]byte(test.Name()))
	saved := legacyNativeGoGraphResourceSave(test, []legacyNativeAnyValue{narrow, wide, narrow}, storage, binding)
	actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
	narrowValues, wideValues := actual[0].value.([]any), actual[1].value.([]any)
	if len(narrowValues) != 1 || cap(narrowValues) != 1 || len(wideValues) != 1 || cap(wideValues) != 3 || &narrowValues[0] != &wideValues[:3][1] || actual[0].owner != actual[2].owner {
		test.Fatal("codec lost physical slot sharing, exact view shape, or repeated identity")
	}
	if actual[0].owner.array.resource == nil || actual[0].owner.array.resource != actual[1].owner.array.resource {
		test.Fatal("views did not retain the same whole-backing resource owner")
	}
	payload := len(wideValues[0].(string))
	if actual[0].heap.bytes+actual[0].owner.bytes < int64(payload) {
		test.Fatal("narrow owner omitted the payload retained outside its capacity")
	}
	resaved := legacyNativeGoGraphResourceSave(test, actual[:1], storage, binding)
	restored := legacyNativeGoGraphSetW85Restore(test, resaved, binding, storage, 0)
	resource := restored[0].owner.array.resource
	if resource == nil || len(resource.root.value.([]any)[0].(string)) != payload || restored[0].heap.bytes+restored[0].owner.bytes < int64(payload) {
		test.Fatal("narrow-only recapture dropped the physical allocation's prefix payload")
	}
}

func TestLegacyNativeGoGraphResourceImportedViews(test *testing.T) {
	for _, mode := range []string{"transitive", "nil-tail", "nested-resource"} {
		test.Run(mode, func(test *testing.T) {
			shared := map[string]any{"kept": strings.Repeat("payload", 1024)}
			backing := []any{shared, true, nil, "suffix", shared, nil, false, nil}
			offsets := []int{3, 1, 0, 1, 6}
			roots := []any{backing[3:4:6], backing[1:2:4], backing[:1:2], backing[1:2:4], backing[6:7:8]}
			if mode == "nil-tail" {
				backing = make([]any, 16384)
				offsets = []int{1, 0, 1}
				roots = []any{backing[1:2:16384], backing[:0:16384], backing[1:2:16384]}
			} else if mode == "nested-resource" {
				nested := []any{shared, true, nil}
				backing[0], backing[4] = nested[:1:3], nested[1:2:3]
			}
			snapshot := legacyNativeGoGraphW85SourceSnapshot(roots)
			defer legacyNativeGoGraphW85SourceUnchanged(test, snapshot)
			storage := legacyNativeGoGraphSetW85Storage(test)
			binding := HashContent([]byte(test.Name()))
			saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
			actual := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
			legacyNativeGoGraphSetW85Values(test, roots, actual)
			for index, value := range actual {
				expected, decoded := roots[index].([]any), value.value.([]any)
				if !reflect.DeepEqual(expected[:cap(expected)], decoded[:cap(decoded)]) {
					test.Fatal("shared view omitted a physical backing value")
				}
				if value.owner.array.tail != nil {
					for position := len(decoded); position < cap(decoded); position++ {
						child, err := value.owner.array.tailValue(context.Background(), position)
						if err != nil || child.validateOwnership() != nil || !reflect.DeepEqual(child.value, decoded[:cap(decoded)][position]) {
							test.Fatal("populated hidden backing lost native child metadata", err)
						}
					}
				}
			}
			for index, value := range actual {
				if mode != "nil-tail" && index == 4 {
					if value.owner.array.resource != nil {
						test.Fatal("adjacent nonoverlapping backing was merged")
					}
					continue
				}
				resource := value.owner.array.resource
				if resource == nil || resource != actual[0].owner.array.resource || value.owner.array.offset != offsets[index] {
					test.Fatal("transitive group lost its common owner or exact offset")
				}
				decoded := value.value.([]any)
				physical := resource.root.value.([]any)
				if &decoded[:cap(decoded)][0] != &physical[:cap(physical)][offsets[index]] {
					test.Fatal("view does not address the common physical slot")
				}
				if mode == "nil-tail" && (resource.root.owner.array.tail != nil || len(physical) != 0) {
					test.Fatal("all-nil backing acquired per-hidden-slot metadata")
				}
			}
			resaved := legacyNativeGoGraphResourceSave(test, actual[:1], storage, binding)
			restored := legacyNativeGoGraphSetW85Restore(test, resaved, binding, storage, 0)
			if restored[0].owner.array.resource == nil || restored[0].owner.array.offset != offsets[0] || restored[0].heap.bytes+restored[0].owner.bytes < actual[0].owner.array.resource.bytes {
				test.Fatal("narrow-only recapture lost the original backing closure")
			}
		})
	}
}

func TestLegacyNativeGoGraphResourceCodecFences(test *testing.T) {
	storage := legacyNativeGoGraphSetW85Storage(test)
	binding := HashContent([]byte(test.Name()))
	backing := []any{true, false, nil}
	saved := legacyNativeGoGraphSetW85Capture(test, []any{backing[:0:3], backing[1:2:3]}, binding, storage)
	state := legacyNativeGoGraphW86State(test, storage, saved)
	original := legacyNativeGoGraphW86Owner(test, storage, state.Roots[0])
	if original.Allocation == nil || !original.FullBacking {
		test.Fatal("fixture lacks actual shared allocation and hidden payload")
	}
	for _, mode := range []string{"offset-overflow", "offset-outside", "missing-backing", "view-as-backing", "hidden-payload-as-implicit-nil"} {
		test.Run(mode, func(test *testing.T) {
			record := original
			allocation := *record.Allocation
			record.Allocation = &allocation
			switch mode {
			case "offset-overflow":
				allocation.Offset = ^uint64(0)
			case "offset-outside":
				allocation.Offset = 1
			case "missing-backing":
				allocation.Backing = legacyNativeCheckpointValue{}
			case "view-as-backing":
				allocation.Backing = state.Roots[1]
			case "hidden-payload-as-implicit-nil":
				record.FullBacking, record.Visible, record.Length, record.Members = false, 0, 0, legacyNativeCheckpointTree{}
			}
			body, err := json.Marshal(record)
			if err != nil {
				test.Fatal(err)
			}
			changed := legacyNativeGoGraphW86ChangedOwner(test, storage, saved, body)
			work, failure := newLegacyNativeCheckpointGoGraphSetRestore(context.Background(), changed, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			legacyNativeGoGraphSetW85Reject[[]legacyNativeAnyValue](test, work, failure, storage, ErrIntegrity, legacyNativeGoGraphSetW85Budget)
		})
	}
}

func TestLegacyNativeGoGraphResourceActualParentCapture(test *testing.T) {
	backing := []any{strings.Repeat("retained outside narrow view", 32768), true, nil}
	initial := &wire{Input: map[string]any{"narrow": backing[1:2:2]}, Output: backing[:1:3]}
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, initial, nil)
	storage := legacyNativeGoGraphSetW85Storage(test)
	parent, err := newLegacyNativeObjectCheckpoint(context.Background(), native, scanner, request.Binding, source, storage.io, legacyNativeEventW82FenceBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer parent.close()
	fixture := &legacyNativeSourceW88Fixture{native: native, scanner: scanner, source: source, request: request, storage: storage, parent: parent}
	before := legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes)
	grant := parent.capture
	legacyNativeSourceW88ParentReady(test, fixture)
	if parent.capture != grant || !grant.valid(native.mutation) {
		test.Fatal("capture-time source authority was replaced during backing preparation")
	}
	legacyNormalizationCaptureRQ87Busy(test, native.start(0, source.Bytes))
	if current := legacyNormalizationCaptureRQ87Observe(test, native, scanner, source.Bytes); !reflect.DeepEqual(current, before) {
		test.Fatal("normal source mutation changed the captured parser")
	}
	if parent.state.InitialGraphs == nil {
		test.Fatal("real parent did not store its overlapping initial graphs")
	}
	saved := *parent.state.InitialGraphs
	if _, err := parent.take(context.Background()); err != nil {
		test.Fatal(err)
	}
	legacyNormalizationCaptureRQ87Idle(test, native.mutation)
	actual := legacyNativeGoGraphSetW85Restore(test, saved, request.Binding, storage, 0)
	legacyNativeGoGraphW86Values(test, []any{initial.Input, initial.Output}, actual)
	cursor, err := actual[0].owner.mapCheckpoint(context.Background(), true)
	if err != nil {
		test.Fatal(err)
	}
	defer cursor.close()
	entry, found, err := cursor.next(context.Background())
	if err != nil || !found || entry.Key != "narrow" {
		test.Fatal("restored real input omitted native narrow-view ownership", err)
	}
	narrow, wide := entry.Value.([]any), actual[1].value.([]any)
	if &narrow[0] != &wide[:3][1] || entry.ValueBytes+entry.owner.bytes < int64(len(backing[0].(string))) {
		test.Fatal("real owner path lost shared slots or the retained prefix payload")
	}
	if _, err := actual[1].owner.array.appendValue(context.Background(), &legacyNormalizationInput{}, legacyNativeAnyValue{}); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("normal native append escaped the retained resource capture", err)
	}
}
