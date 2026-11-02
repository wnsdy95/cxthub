package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeEventListCheckpointControlCursor(test *testing.T, list *legacyNativeEventList) int {
	test.Helper()
	if list.cursor == nil {
		return -1
	}
	index := 0
	for current := list.cursor; current != list; current = current.parent {
		if current == nil || index >= 10000 {
			test.Fatal("actual decoder lost its event-list cursor chain")
		}
		index++
	}
	return index
}

func legacyNativeEventListCheckpointControlReady(state legacyNativeEventListCheckpointControl) legacyNativeEventListCheckpointControl {
	ref := legacyNativeCheckpointRef{Hash: HashContent([]byte("event-list-control-reference")), Bytes: 1}
	state.Data = ref
	if state.HasFrame {
		state.Frame = ref
	}
	if state.Child == 2 {
		state.Object = ref
	}
	return state
}

func TestLegacyNativeEventListCheckpointControlActualDecoderStates(test *testing.T) {
	fixtures := legacyEventValuesCheckpointFixtures()[:2]
	fixtures = append(fixtures, legacyDecoderCheckpointFixture{
		name: "inactive-nullable-cache",
		raw:  string(legacyNativeEventDocument("2", `{"kind":"compaction","replacement":null,"id":"`+strings.Repeat("n", 2*ConversationManifestChunkBytes)+`"}`)),
		cut: func(decoder *LegacyNormalizationDecoder) bool {
			if decoder.native == nil || decoder.native.object.eventActive || decoder.native.object.scalar == nil {
				return false
			}
			for _, list := range decoder.native.object.events {
				if list != nil && list.phase == legacyNativeEventsDone && list.values == nil {
					return true
				}
			}
			return false
		},
	})
	for ordinal, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			list, _ := legacyEventValuesCheckpointList(test, decoder)
			binding := HashContent(buffer.body)
			cursor := legacyNativeEventListCheckpointControlCursor(test, list)
			state, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, cursor)
			if err != nil {
				test.Fatal("actual decoder control was rejected", err)
			}
			children := []uint8{2, 1, 0}
			if state.Child != children[ordinal] || state.HasValues != (list.values != nil) || state.HasFrame != (list.frame != nil) ||
				state.DecodedNil != (list.decoded == nil) || state.Count != list.count || state.Borrowed != list.borrowed.bytes ||
				state.Parent != -1 || state.Cursor != cursor || state.Data != (legacyNativeCheckpointRef{}) || state.Frame != (legacyNativeCheckpointRef{}) || state.Object != (legacyNativeCheckpointRef{}) {
				test.Fatal("control lost the actual paged list shape", state)
			}
			if err := legacyNativeEventListCheckpointControlReady(state).validate(binding, decoder.source.Bytes, true); err != nil {
				test.Fatal("ready references were rejected", err)
			}
			if list.cursor != nil && list.cursor != list {
				child := list.cursor
				childCursor := -1
				if child.cursor != nil {
					childCursor = cursor + legacyNativeEventListCheckpointControlCursor(test, child)
				}
				if _, err := legacyNativeEventListCheckpointControlFor(child, binding, decoder.source.Bytes, cursor, cursor-1, childCursor); err != nil {
					test.Fatal("actual nested cursor control was rejected", err)
				}
			}
			if state.Literal != len("null") {
				prior := list.decoded
				list.decoded = []CIREvent{}
				empty, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, cursor)
				list.decoded = prior
				if err != nil || empty.DecodedNil {
					test.Fatal("non-nil capacity-zero decoded slice lost its identity", err)
				}
			}
		})
	}
}

func TestLegacyNativeEventListCheckpointControlNestedIndices(test *testing.T) {
	fixture := legacyEventValuesCheckpointFixtures()[1]
	fixture.name = "nested-cursor-control"
	fixture.cut = func(decoder *LegacyNormalizationDecoder) bool {
		if decoder.native != nil {
			for _, list := range decoder.native.object.events {
				if list != nil && list.cursor != nil && list.cursor != list {
					return true
				}
			}
		}
		return false
	}
	decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
	root, _ := legacyEventValuesCheckpointList(test, decoder)
	index := legacyNativeEventListCheckpointControlCursor(test, root)
	child := root.cursor
	cursor := -1
	if child.cursor != nil {
		cursor = index + legacyNativeEventListCheckpointControlCursor(test, child)
	}
	binding := HashContent(buffer.body)
	state, err := legacyNativeEventListCheckpointControlFor(child, binding, decoder.source.Bytes, index, index-1, cursor)
	if err != nil || state.Index != index || state.Parent != index-1 || state.Cursor != cursor {
		test.Fatal("nested source lost its parent/cursor indices", state, err)
	}
	if _, err := legacyNativeEventListCheckpointControlFor(child, binding, decoder.source.Bytes, index, index-2, cursor); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nested source accepted the wrong parent index", err)
	}
	if _, err := legacyNativeEventListCheckpointControlFor(root, binding, decoder.source.Bytes, 0, -1, 0); !errors.Is(err, ErrIntegrity) {
		test.Fatal("descendant cursor was relabeled as its root", err)
	}
}

func TestLegacyNativeEventListCheckpointControlSourceFences(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[0])
	list, _ := legacyEventValuesCheckpointList(test, decoder)
	binding := HashContent(buffer.body)
	cursor := legacyNativeEventListCheckpointControlCursor(test, list)
	for _, scenario := range []struct {
		name   string
		change func(*legacyNativeEventList)
	}{
		{name: "decoded-backing", change: func(list *legacyNativeEventList) { list.decoded = make([]CIREvent, 0, 1) }},
		{name: "previous-empty-non-nil", change: func(list *legacyNativeEventList) { list.previous = []CIREvent{} }},
		{name: "owned", change: func(list *legacyNativeEventList) { list.owned = true }},
		{name: "previous-owned", change: func(list *legacyNativeEventList) { list.previousOwned = true }},
		{name: "copied", change: func(list *legacyNativeEventList) { list.copied = 1 }},
		{name: "records", change: func(list *legacyNativeEventList) { list.records.single = new(legacyNativeEventRecord) }},
		{name: "payload", change: func(list *legacyNativeEventList) { list.payload.bytes = 1 }},
		{name: "added-error", change: func(list *legacyNativeEventList) { list.added.err = ErrIntegrity }},
		{name: "borrowed-error", change: func(list *legacyNativeEventList) { list.borrowed.err = ErrIntegrity }},
		{name: "values-count", change: func(list *legacyNativeEventList) { list.values.count++ }},
		{name: "values-heap", change: func(list *legacyNativeEventList) { list.values.payload.bytes = -1 }},
		{name: "values-pages", change: func(list *legacyNativeEventList) { list.values.records.count = -1 }},
		{name: "estimate", change: func(list *legacyNativeEventList) { list.estimate.bytes++ }},
		{name: "foreign-destination", change: func(list *legacyNativeEventList) { list.object.destination = reflect.ValueOf(new(wire)) }},
		{name: "foreign-metadata", change: func(list *legacyNativeEventList) { list.object.metadata = &legacyContentBlockNativeFields }},
		{name: "foreign-mutation", change: func(list *legacyNativeEventList) { list.object.mutation = &legacyNormalizationMutation{} }},
		{name: "child-depth", change: func(list *legacyNativeEventList) { list.object.parentDepth++ }},
		{name: "child-presence", change: func(list *legacyNativeEventList) { list.object.trackPresence = false }},
		{name: "child-paging", change: func(list *legacyNativeEventList) { list.object.pagedEvents = false }},
		{name: "child-end", change: func(list *legacyNativeEventList) { list.object.end-- }},
		{name: "self-parent", change: func(list *legacyNativeEventList) { list.parent = list }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			prior, object, values := *list, *list.object, *list.values
			defer func() {
				*list, *prior.object, *prior.values = prior, object, values
			}()
			scenario.change(list)
			if state, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, cursor); !errors.Is(err, ErrIntegrity) || state != (legacyNativeEventListCheckpointControl{}) {
				test.Fatal("invalid source control escaped", state, err)
			}
		})
	}
	for _, indices := range [][3]int{{-1, -1, cursor}, {0, 0, cursor}, {0, -1, -2}, {0, -1, -1}} {
		if _, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, indices[0], indices[1], indices[2]); !errors.Is(err, ErrIntegrity) {
			test.Fatal("source pointers disagreed with caller indices", indices, err)
		}
	}
	wrongCursor := 0
	if list.cursor == list {
		wrongCursor = 1
	}
	if _, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, wrongCursor); !errors.Is(err, ErrIntegrity) {
		test.Fatal("self cursor disagreed with its index", err)
	}
	if _, err := legacyNativeEventListCheckpointControlFor(nil, binding, decoder.source.Bytes, 0, -1, -1); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil source was admitted", err)
	}
	list.paged = false
	defer func() { list.paged = true }()
	if _, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, cursor); !errors.Is(err, ErrDocRangeIndexPending) {
		test.Fatal("non-paged source did not remain pending", err)
	}
}

func TestLegacyNativeEventListCheckpointControlResetChildFences(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[1])
	list, _ := legacyEventValuesCheckpointList(test, decoder)
	binding := HashContent(buffer.body)
	for _, scenario := range []struct {
		name   string
		change func(*legacyNativeEventList)
	}{
		{name: "wire-value", change: func(list *legacyNativeEventList) { list.child.ID = "retained" }},
		{name: "nested-cache", change: func(list *legacyNativeEventList) { list.object.events = []*legacyNativeEventList{} }},
		{name: "scalar-cache", change: func(list *legacyNativeEventList) { list.object.scalar = &legacyNativeScalar{} }},
		{name: "key", change: func(list *legacyNativeEventList) { list.object.key = append(list.object.key, 'x') }},
		{name: "raw", change: func(list *legacyNativeEventList) { list.object.raw = []byte("x") }},
		{name: "position", change: func(list *legacyNativeEventList) { list.object.position = 1 }},
		{name: "field", change: func(list *legacyNativeEventList) { list.object.field = 0 }},
		{name: "heap", change: func(list *legacyNativeEventList) {
			list.object.heaps = append([]docRangeHeaderHeapEstimate(nil), list.object.heaps...)
			list.object.heaps[0].bytes = 1
		}},
		{name: "backing-excess", change: func(list *legacyNativeEventList) {
			list.object.backingExcess = append([]int64(nil), list.object.backingExcess...)
			list.object.backingExcess[0] = 1
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			prior, object := *list, *list.object
			defer func() { *list, *prior.object = prior, object }()
			scenario.change(list)
			if _, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, -1); !errors.Is(err, ErrIntegrity) {
				test.Fatal("unsafe reset cache was admitted", err)
			}
		})
	}
}

func TestLegacyNativeEventListCheckpointControlGeometryAndReferences(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[0])
	list, _ := legacyEventValuesCheckpointList(test, decoder)
	binding := HashContent(buffer.body)
	state, err := legacyNativeEventListCheckpointControlFor(list, binding, decoder.source.Bytes, 0, -1, legacyNativeEventListCheckpointControlCursor(test, list))
	if err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name   string
		change func(*legacyNativeEventListCheckpointControl)
	}{
		{name: "version", change: func(state *legacyNativeEventListCheckpointControl) { state.Version++ }},
		{name: "binding", change: func(state *legacyNativeEventListCheckpointControl) { state.Binding = HashContent([]byte("foreign")) }},
		{name: "source-bytes", change: func(state *legacyNativeEventListCheckpointControl) { state.SourceBytes++ }},
		{name: "index", change: func(state *legacyNativeEventListCheckpointControl) { state.Index = -1 }},
		{name: "parent", change: func(state *legacyNativeEventListCheckpointControl) { state.Index, state.Parent = 2, 0 }},
		{name: "cursor", change: func(state *legacyNativeEventListCheckpointControl) { state.Index, state.Cursor = 2, 1 }},
		{name: "position", change: func(state *legacyNativeEventListCheckpointControl) { state.Position = state.End + 1 }},
		{name: "end", change: func(state *legacyNativeEventListCheckpointControl) { state.End = state.SourceBytes + 1 }},
		{name: "maximum", change: func(state *legacyNativeEventListCheckpointControl) { state.Maximum = state.End/2 + 1 }},
		{name: "count", change: func(state *legacyNativeEventListCheckpointControl) { state.Count = state.Maximum + 1 }},
		{name: "child-start", change: func(state *legacyNativeEventListCheckpointControl) { state.ChildStart = state.Position + 1 }},
		{name: "literal", change: func(state *legacyNativeEventListCheckpointControl) { state.Literal = 5 }},
		{name: "depth", change: func(state *legacyNativeEventListCheckpointControl) { state.ParentDepth = 10001 }},
		{name: "borrowed", change: func(state *legacyNativeEventListCheckpointControl) { state.Borrowed = -1 }},
		{name: "missing-values", change: func(state *legacyNativeEventListCheckpointControl) { state.HasValues = false }},
		{name: "missing-apply-child", change: func(state *legacyNativeEventListCheckpointControl) { state.Child = 1 }},
		{name: "child-outside-apply", change: func(state *legacyNativeEventListCheckpointControl) { state.Phase = legacyNativeEventsScan }},
		{name: "early-reference", change: func(state *legacyNativeEventListCheckpointControl) {
			state.Data = legacyNativeEventListCheckpointControlReady(*state).Data
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			invalid := state
			scenario.change(&invalid)
			if err := invalid.validate(binding, decoder.source.Bytes, false); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid control geometry was admitted", err)
			}
		})
	}
	ready := legacyNativeEventListCheckpointControlReady(state)
	for _, ref := range []*legacyNativeCheckpointRef{&ready.Data, &ready.Frame, &ready.Object} {
		prior := *ref
		*ref = legacyNativeCheckpointRef{}
		if err := ready.validate(binding, decoder.source.Bytes, true); !errors.Is(err, ErrIntegrity) {
			test.Fatal("required ready reference was omitted", err)
		}
		*ref = prior
	}
	noFrame := state
	noFrame.Phase, noFrame.Child, noFrame.First, noFrame.ChildStart = legacyNativeEventsStart, 0, 0, 0
	noFrame.Position, noFrame.Maximum, noFrame.Count = 0, noFrame.End/2, 0
	noFrame.HasValues, noFrame.HasFrame, noFrame.Cursor = false, false, -1
	noFrame = legacyNativeEventListCheckpointControlReady(noFrame)
	if err := noFrame.validate(binding, decoder.source.Bytes, true); err != nil {
		test.Fatal("nil data still needs its own valid data reference", err)
	}
	missingData := noFrame
	missingData.Data = legacyNativeCheckpointRef{}
	if err := missingData.validate(binding, decoder.source.Bytes, true); !errors.Is(err, ErrIntegrity) {
		test.Fatal("nil values omitted their separate ready data reference", err)
	}
	for _, ref := range []*legacyNativeCheckpointRef{&noFrame.Frame, &noFrame.Object} {
		*ref = noFrame.Data
		if err := noFrame.validate(binding, decoder.source.Bytes, true); !errors.Is(err, ErrIntegrity) {
			test.Fatal("unexpected ready reference was admitted", err)
		}
		*ref = legacyNativeCheckpointRef{}
	}
}

func legacyNativeEventListCheckpointControlCut(test *testing.T, raw string, cut func(*legacyNativeEventList) bool) (*legacyNativeCanonicalOwnerFixture, *legacyNativeEventList) {
	test.Helper()
	fixture := legacyNativeCanonicalOwnerStart(test, raw)
	fixture.native.object.pagedEvents = true
	fixture.source = legacyNormalizationPreparationInput(test, fixture.buffer, true)
	for attempt := 0; attempt < 512; attempt++ {
		remaining := 1
		for _, list := range fixture.native.object.events {
			if list == nil {
				continue
			}
			if list.phase == legacyNativeEventsStart && list.values == nil && raw[list.position] == '[' {
				remaining += int(unsafe.Sizeof(legacyNativeEventValues{}))
			}
			if (list.phase == legacyNativeEventsFirst || list.phase == legacyNativeEventsValue) && !legacyNormalizationSpace(raw[list.position]) && raw[list.position] != ']' && list.count == list.values.records.length() {
				storage, err := list.values.records.appendBytes()
				if err != nil {
					test.Fatal(err)
				}
				remaining += storage + int(unsafe.Sizeof(legacyNativeEventValue{}))
			}
		}
		_, complete, _, err := legacyNativeEventsL02Step(test, fixture, context.Background(), remaining, 0, 0)
		if err != nil || complete {
			test.Fatal("real native decoder stopped before its bounded control cut", err)
		}
		for _, list := range fixture.native.object.events {
			if list != nil && cut(list) {
				return fixture, list
			}
		}
	}
	test.Fatal("real native decoder did not reach its bounded control cut")
	return nil, nil
}

func TestLegacyNativeEventListCheckpointControlDeferredInputStates(test *testing.T) {
	for _, scenario := range []struct {
		name string
		raw  string
		cut  func(*legacyNativeEventList) bool
	}{
		{name: "scan-invalid-start-before-rejection", raw: `{"replacement":[!]}`, cut: func(list *legacyNativeEventList) bool {
			return list.phase == legacyNativeEventsScan && list.first == '!'
		}},
		{name: "partial-null-before-rejection", raw: `{"replacement":nulX}`, cut: func(list *legacyNativeEventList) bool {
			return list.phase == legacyNativeEventsNull && list.literal == 3
		}},
		{name: "apply-before-object-rejection", raw: `{"replacement":[{]}`, cut: func(list *legacyNativeEventList) bool {
			return list.phase == legacyNativeEventsApply && list.object.phase == legacyNativeObjectStart
		}},
		{name: "null-child-done-before-commit", raw: `{"replacement":[null]}`, cut: func(list *legacyNativeEventList) bool {
			return list.phase == legacyNativeEventsApply && list.object.phase == legacyNativeObjectDone
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			fixture, list := legacyNativeEventListCheckpointControlCut(test, scenario.raw, scenario.cut)
			binding := HashContent(fixture.buffer.body)
			state, err := legacyNativeEventListCheckpointControlFor(list, binding, fixture.source.Bytes, 0, -1, 0)
			if err != nil {
				test.Fatal("actual intermediate input state was rejected early", err)
			}
			if state.Phase == legacyNativeEventsApply && state.Child != 2 || state.Phase == legacyNativeEventsScan && state.First != '!' {
				test.Fatal("actual intermediate state was rewritten", state)
			}
			if err := legacyNativeEventListCheckpointControlReady(state).validate(binding, fixture.source.Bytes, true); err != nil {
				test.Fatal("intermediate control was not ready with its references", err)
			}
		})
	}
}
