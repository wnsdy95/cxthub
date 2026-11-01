package domain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyCanonicalPreparingFrameRef(name string) legacyNativeCheckpointRef {
	return legacyNativeCheckpointRef{Hash: HashContent([]byte(name)), Bytes: len(name)}
}

func legacyCanonicalPreparingFrameSuffix(test *testing.T, frame *legacyCanonicalPreparationFrame) *legacyCanonicalMapContinuation {
	test.Helper()
	if frame.mapResume != nil {
		return frame.mapResume
	}
	if frame.iterator == nil {
		return nil
	}
	iterator := *frame.iterator
	continuation := &legacyCanonicalMapContinuation{}
	for iterator.Next() {
		if err := continuation.fields.append(legacyCanonicalKeyField{key: iterator.Key().String(), value: iterator.Value().Interface()}); err != nil {
			test.Fatal(err)
		}
	}
	return continuation
}

func legacyCanonicalPreparingFrameRoundTrip(test *testing.T, frame *legacyCanonicalPreparationFrame) legacyCanonicalPreparingFrameState {
	test.Helper()
	before := *frame
	var iteratorBefore reflect.MapIter
	if frame.iterator != nil {
		iteratorBefore = *frame.iterator
	}
	continuation := legacyCanonicalPreparingFrameSuffix(test, frame)
	var continuationRef legacyNativeCheckpointRef
	if continuation != nil {
		continuationRef = legacyCanonicalPreparingFrameRef("continuation")
	}
	state, err := captureLegacyCanonicalPreparingFrame(frame, legacyCanonicalPreparingFrameRef("value"), continuationRef)
	if err != nil {
		test.Fatal("producer frame was rejected", frame.kind, frame.phase, err)
	}
	wire, err := json.Marshal(state)
	if err != nil {
		test.Fatal(err)
	}
	var decoded legacyCanonicalPreparingFrameState
	if err := json.Unmarshal(wire, &decoded); err != nil || decoded != state {
		test.Fatal("finite metadata did not round trip", err)
	}
	restored, err := restoreLegacyCanonicalPreparingFrame(decoded, frame.value, continuation)
	if err != nil {
		test.Fatal(err)
	}
	want := before
	want.iterator, want.mapResume = nil, continuation
	if !reflect.DeepEqual(restored, want) || restored.mapResume != continuation || restored.iterator != nil {
		test.Fatal("restoration changed a scalar or continuation", restored, want)
	}
	if frame.value != nil {
		originalValue, restoredValue := reflect.ValueOf(frame.value), reflect.ValueOf(restored.value)
		switch originalValue.Kind() {
		case reflect.Map, reflect.Slice, reflect.Pointer:
			if originalValue.Pointer() != restoredValue.Pointer() {
				test.Fatal("restoration reconstructed borrowed value storage")
			}
		}
	}
	recaptured, err := captureLegacyCanonicalPreparingFrame(&restored, state.Value, state.Continuation)
	if err != nil || recaptured != state || !reflect.DeepEqual(*frame, before) ||
		frame.iterator != nil && !reflect.DeepEqual(*frame.iterator, iteratorBefore) {
		test.Fatal("capture changed its input or lost a scalar", err)
	}
	return state
}

func TestLegacyCanonicalPreparingFrameUnenteredProducerValues(test *testing.T) {
	number := json.Number("1e-7")
	for index, value := range []any{
		nil, true, 7, 1.5, "text", EventMessage, RoleUser, ProviderKind("codex"), FidelityTier("full"), number, &number, (*json.Number)(nil),
		ContentBlock{Type: "text", Text: "leaf"}, map[string]any(nil), map[string]any{},
		[]any(nil), []map[string]any(nil), []ContentBlock(nil), (*legacyNativeBlockValues)(nil), (*legacyCanonicalReplacement)(nil),
		&legacyCanonicalReplacement{events: []CIREvent{}},
	} {
		test.Run(fmt.Sprint(index), func(test *testing.T) {
			for _, prepare := range []bool{false, true} {
				preparation := newLegacyCanonicalPreparation(value, 10000, prepare)
				state := legacyCanonicalPreparingFrameRoundTrip(test, preparation.frames.at(0))
				if state.Kind != 0 || state.Depth != 10000 || state.Prepare != prepare {
					test.Fatal("unentered frame metadata changed")
				}
			}
		})
	}
}

func TestLegacyCanonicalPreparingFrameProducerShapes(test *testing.T) {
	blocks := &legacyNativeBlockValues{count: 1}
	events := &legacyNativeEventValues{count: 2}
	for index := 0; index < 2200; index++ {
		if err := blocks.records.append(legacyNativeBlockValue{block: ContentBlock{Type: "text", Text: "backing"}}); err != nil {
			test.Fatal(err)
		}
	}
	for index := 0; index < 3; index++ {
		if err := events.records.append(legacyNativeEventValue{event: CIREvent{Kind: EventTurn, Seq: 3 - index}}); err != nil {
			test.Fatal(err)
		}
	}
	fixtures := []struct {
		name  string
		value any
		kind  byte
	}{
		{name: "nil-map", value: map[string]any(nil)},
		{name: "empty-map", value: map[string]any{}, kind: 'o'},
		{name: "map", value: map[string]any{"": nil, "value": true}, kind: 'o'},
		{name: "long-map-key", value: map[string]any{strings.Repeat("key", 400): nil}, kind: 'o'},
		{name: "nil-array", value: []any(nil), kind: 'a'},
		{name: "empty-array", value: []any{}, kind: 'a'},
		{name: "array-capacity-tail", value: make([]any, 1, 2200), kind: 'a'},
		{name: "zero-length-array-tail", value: make([]any, 0, 2200), kind: 'a'},
		{name: "nil-map-array", value: []map[string]any(nil), kind: 'm'},
		{name: "empty-map-array", value: []map[string]any{}, kind: 'm'},
		{name: "map-array-capacity-tail", value: make([]map[string]any, 1, 2200), kind: 'm'},
		{name: "nil-block-array", value: []ContentBlock(nil), kind: 'c'},
		{name: "empty-block-array", value: []ContentBlock{}, kind: 'c'},
		{name: "block-array-capacity-tail", value: make([]ContentBlock, 1, 2200), kind: 'c'},
		{name: "nil-native-blocks", value: (*legacyNativeBlockValues)(nil)},
		{name: "empty-native-blocks", value: &legacyNativeBlockValues{}, kind: 'd'},
		{name: "native-blocks-capacity-tail", value: blocks, kind: 'd'},
		{name: "empty-event-replacement", value: &legacyCanonicalReplacement{events: []CIREvent{}}, kind: 'r'},
		{name: "event-replacement", value: &legacyCanonicalReplacement{events: []CIREvent{{Kind: EventTurn, Seq: 2}, {Kind: EventTurn, Seq: 1}}}, kind: 'r'},
		{name: "empty-native-replacement", value: &legacyCanonicalReplacement{native: &legacyNativeEventValues{}}, kind: 'r'},
		{name: "native-replacement", value: &legacyCanonicalReplacement{native: events}, kind: 'r'},
	}
	for _, fixture := range fixtures {
		for _, prepare := range []bool{false, true} {
			if fixture.kind == 'r' && !prepare {
				continue
			}
			test.Run(fmt.Sprintf("%s/prepare-%t", fixture.name, prepare), func(test *testing.T) {
				preparation := newLegacyCanonicalPreparation(fixture.value, 3, prepare)
				encoding := &LegacyCanonicalEncoding{nativeOwner: &legacyNativeCanonicalOwner{}}
				seen := make(map[uint8]bool)
				seenTail := false
				for attempt := 0; attempt < 10000 && !preparation.complete; attempt++ {
					for index := 0; index < preparation.frames.length(); index++ {
						frame := preparation.frames.at(index)
						state := legacyCanonicalPreparingFrameRoundTrip(test, frame)
						if index == 0 && frame.kind == fixture.kind {
							seen[frame.phase] = true
							seenTail = seenTail || frame.position > frame.length
							if fixture.name == "long-map-key" && prepare && frame.position > 0 && frame.phase == 0 && state.KeyBytes != 1025 {
								test.Fatal("producer key-byte saturation was not preserved")
							}
						}
					}
					preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes = 0, 0, 0
					frame := preparation.frames.at(preparation.frames.length() - 1)
					var err error
					switch frame.kind {
					case 0:
						_, err = preparation.enter(context.Background(), encoding)
					case 'o':
						_, err = preparation.stepMap(context.Background(), encoding)
					case 'r':
						_, err = preparation.stepReplacement(context.Background(), encoding)
					default:
						_, err = preparation.step(context.Background(), encoding)
					}
					if err != nil {
						test.Fatal("producer could not advance", err)
					}
				}
				if !preparation.complete || !seen[0] {
					test.Fatal("producer fixture did not reach its entered state and finish")
				}
				if (fixture.kind == 'o' || fixture.kind == 'r') && prepare && (!seen[1] || !seen[2]) {
					test.Fatal("producer fixture missed a preparation phase", seen)
				}
				if strings.Contains(fixture.name, "tail") && !seenTail {
					test.Fatal("producer fixture did not traverse hidden capacity")
				}
			})
		}
	}
}

func TestLegacyCanonicalPreparingFrameExhaustedStepBeforePop(test *testing.T) {
	blocks := &legacyNativeBlockValues{}
	for range legacyCanonicalEncodingActions - 1 {
		if err := blocks.records.append(legacyNativeBlockValue{}); err != nil {
			test.Fatal(err)
		}
	}
	for _, value := range []any{make([]ContentBlock, 0, legacyCanonicalEncodingActions-1), blocks} {
		for _, prepare := range []bool{false, true} {
			test.Run(fmt.Sprintf("%T/prepare-%t", value, prepare), func(test *testing.T) {
				preparation := newLegacyCanonicalPreparation(value, 3, prepare)
				encoding := &LegacyCanonicalEncoding{nativeOwner: &legacyNativeCanonicalOwner{}}
				complete, err := preparation.step(context.Background(), encoding)
				if err != nil || complete || preparation.frames.length() != 1 || preparation.lastActions != legacyCanonicalEncodingActions {
					test.Fatal("actual step did not stop before popping the exhausted frame", complete, err)
				}
				frame := preparation.frames.at(0)
				if frame.count != legacyCanonicalEncodingActions-1 || frame.position != frame.count || frame.length != 0 {
					test.Fatal("actual step lost the exhausted capacity tail", frame)
				}
				state := legacyCanonicalPreparingFrameRoundTrip(test, frame)
				restoredFrame, err := restoreLegacyCanonicalPreparingFrame(state, value, nil)
				if err != nil {
					test.Fatal(err)
				}
				restored := *preparation
				restored.frames = legacyCanonicalPages[legacyCanonicalPreparationFrame]{}
				if err := restored.frames.append(restoredFrame); err != nil {
					test.Fatal(err)
				}
				restoredEncoding := *encoding
				before := encoding.inspectWork
				complete, err = preparation.step(context.Background(), encoding)
				restoredComplete, restoredErr := restored.step(context.Background(), &restoredEncoding)
				if err != nil || restoredErr != nil || !complete || !restoredComplete ||
					preparation.heap != restored.heap || preparation.frames.length() != 0 || restored.frames.length() != 0 ||
					encoding.inspectWork != before+1 || restoredEncoding.inspectWork != encoding.inspectWork {
					test.Fatal("resuming the exhausted frame replayed elements or lost final heap", complete, restoredComplete, err, restoredErr)
				}
			})
		}
	}
}

func TestLegacyCanonicalPreparingFrameNilSlicesAtDepthLimit(test *testing.T) {
	for _, value := range []any{[]any(nil), []map[string]any(nil), []ContentBlock(nil)} {
		preparation := newLegacyCanonicalPreparation(value, 10000, true)
		if entered, err := preparation.enter(context.Background(), &LegacyCanonicalEncoding{}); err != nil || !entered {
			test.Fatal("producer rejected a nil slice at the depth limit", err)
		}
		legacyCanonicalPreparingFrameRoundTrip(test, preparation.frames.at(0))
	}
}

func TestLegacyCanonicalPreparingFrameActualStageMapCodec(test *testing.T) {
	stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"tool_result","seq":1,"call_id":"frames","output":{"nested":[]}}`)
	encoding := stage.pending[0].Encoding
	preparation := encoding.preparation
	legacyCanonicalPreparingFrameRoundTrip(test, preparation.frames.at(0))
	if entered, err := preparation.enter(context.Background(), encoding); err != nil || !entered {
		test.Fatal(err)
	}
	if advanced, err := preparation.stepMap(context.Background(), encoding); err != nil || !advanced {
		test.Fatal(err)
	}
	frame := preparation.frames.at(0)
	before := *frame
	suffix := legacyCanonicalPreparingFrameSuffix(test, frame)
	var fields []legacyCanonicalKeyField
	for index := 0; index < suffix.fields.length(); index++ {
		fields = append(fields, *suffix.fields.at(index))
	}
	codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
	retained, err := stage.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	codec.ownerBytes += retained
	store, _ := legacyNativeCheckpointStoreFixture(test)
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer grant.release()
	binding := HashContent(source.body)
	writer, err := newLegacyCanonicalMapContinuationCheckpoint(context.Background(), stage, 0, grant, codec, binding, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer writer.close()
	ref := legacyCanonicalMapContinuationCheckpointDrain(test, writer)
	state, err := captureLegacyCanonicalPreparingFrame(frame, legacyCanonicalPreparingFrameRef("actual-root"), ref)
	if err != nil {
		test.Fatal(err)
	}
	reader, err := newLegacyCanonicalMapContinuationRestore(context.Background(), ref, binding, frame.count, frame.position, grant, codec, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer reader.close()
	result := legacyCanonicalMapContinuationRestoreDrain(test, reader)
	restored, err := restoreLegacyCanonicalPreparingFrame(state, frame.value, result.continuation)
	if err != nil || restored.iterator != nil || restored.mapResume != result.continuation || !reflect.DeepEqual(*frame, before) {
		test.Fatal("actual map codec did not restore the frame without advancing its source", err)
	}
	legacyCanonicalPreparingFrameRoundTrip(test, &restored)
	if _, err := stage.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("metadata handling bypassed the outer capture freeze", err)
	}
}

func TestLegacyCanonicalPreparingFrameMalformedMetadata(test *testing.T) {
	valueRef := legacyCanonicalPreparingFrameRef("value")
	continuationRef := legacyCanonicalPreparingFrameRef("continuation")
	initial := legacyCanonicalPreparingFrameState{Value: valueRef, Depth: 3, Prepare: true}
	object := legacyCanonicalPreparingFrameState{Value: valueRef, Continuation: continuationRef, Heap: 1024, Depth: 3, Count: 2, Kind: 'o', Prepare: true}
	array := legacyCanonicalPreparingFrameState{Value: valueRef, Heap: 1024, Depth: 3, Length: 1, Count: 2, Kind: 'a', Prepare: true}
	replacement := legacyCanonicalPreparingFrameState{Value: valueRef, Heap: 1024, Depth: 3, Count: 2, Kind: 'r', Prepare: true}
	for _, fixture := range []struct {
		name   string
		state  legacyCanonicalPreparingFrameState
		mutate func(*legacyCanonicalPreparingFrameState)
	}{
		{name: "missing-value-ref", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Value = legacyNativeCheckpointRef{} }},
		{name: "partial-value-ref", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Value.Bytes = 0 }},
		{name: "negative-heap", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Heap = -1 }},
		{name: "missing-active-heap", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Heap = 0 }},
		{name: "negative-depth", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Depth = -1 }},
		{name: "excess-depth", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Depth = 10001 }},
		{name: "negative-position", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Position = -1 }},
		{name: "negative-length", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Length = -1 }},
		{name: "negative-count", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Count = -1 }},
		{name: "negative-map-position", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.MapPosition = -1 }},
		{name: "negative-key-bytes", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.KeyBytes = -1 }},
		{name: "excess-key-bytes", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.KeyBytes = 1026 }},
		{name: "unknown-kind", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Kind = 'x' }},
		{name: "initial-heap", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Heap = 1 }},
		{name: "initial-phase", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Phase = 1 }},
		{name: "initial-position", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Position, state.Count = 1, 1 }},
		{name: "initial-length", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Length = 1 }},
		{name: "initial-count", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Count = 1 }},
		{name: "initial-map-position", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.MapPosition = 1 }},
		{name: "initial-key-bytes", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.KeyBytes = 1 }},
		{name: "initial-continuation", state: initial, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Continuation = continuationRef }},
		{name: "map-missing-continuation", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Continuation = legacyNativeCheckpointRef{} }},
		{name: "map-partial-continuation", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Continuation.Hash = "" }},
		{name: "map-length", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Length = 1 }},
		{name: "map-depth", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Depth = 10000 }},
		{name: "map-phase", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) {
			state.Phase, state.Continuation = 3, legacyNativeCheckpointRef{}
		}},
		{name: "map-phase-one-position", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) {
			state.Phase, state.Position, state.Continuation = 1, 1, legacyNativeCheckpointRef{}
		}},
		{name: "map-phase-two-continuation", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Phase = 2 }},
		{name: "map-unconsumed-key-bytes", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.KeyBytes = 1 }},
		{name: "empty-map-key-bytes", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) {
			state.Count, state.KeyBytes, state.Phase, state.Continuation = 0, 1, 2, legacyNativeCheckpointRef{}
		}},
		{name: "heap-only-map-keys", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Prepare, state.MapPosition = false, 1 }},
		{name: "heap-only-map-key-bytes", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) {
			state.Prepare, state.Position, state.KeyBytes = false, 1, 1
		}},
		{name: "heap-only-map-phase", state: object, mutate: func(state *legacyCanonicalPreparingFrameState) {
			state.Prepare, state.Phase, state.Continuation = false, 1, legacyNativeCheckpointRef{}
		}},
		{name: "array-position", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Position = 3 }},
		{name: "array-length", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Length = 3 }},
		{name: "array-phase", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Phase = 1 }},
		{name: "array-map-position", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.MapPosition = 1 }},
		{name: "array-key-bytes", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.KeyBytes = 1 }},
		{name: "array-continuation", state: array, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Continuation = continuationRef }},
		{name: "replacement-heap-only", state: replacement, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Prepare = false }},
		{name: "replacement-length", state: replacement, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Length = 1 }},
		{name: "replacement-key-bytes", state: replacement, mutate: func(state *legacyCanonicalPreparingFrameState) { state.KeyBytes = 1 }},
		{name: "replacement-phase", state: replacement, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Phase = 3 }},
		{name: "replacement-sort-position", state: replacement, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Phase, state.Position = 1, 1 }},
		{name: "replacement-continuation", state: replacement, mutate: func(state *legacyCanonicalPreparingFrameState) { state.Continuation = continuationRef }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			fixture.mutate(&fixture.state)
			if err := fixture.state.validate(); !errors.Is(err, ErrIntegrity) {
				test.Fatal("malformed metadata was accepted", err)
			}
			frame, err := restoreLegacyCanonicalPreparingFrame(fixture.state, nil, nil)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(frame, legacyCanonicalPreparationFrame{}) {
				test.Fatal("malformed metadata produced a partial frame", err)
			}
		})
	}
}

func TestLegacyCanonicalPreparingFrameMalformedValues(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		value any
		kind  byte
		depth int
	}{
		{name: "nil-map", value: map[string]any(nil), kind: 'o'},
		{name: "nonempty-map", value: map[string]any{"extra": nil}, kind: 'o'},
		{name: "wrong-map-type", value: []any{}, kind: 'o'},
		{name: "wrong-array-type", value: []map[string]any{}, kind: 'a'},
		{name: "array-capacity", value: make([]any, 0, 1), kind: 'a'},
		{name: "array-length", value: []any{nil}, kind: 'a'},
		{name: "map-array-capacity", value: make([]map[string]any, 0, 1), kind: 'm'},
		{name: "wrong-map-array-type", value: []any{}, kind: 'm'},
		{name: "block-array-capacity", value: make([]ContentBlock, 0, 1), kind: 'c'},
		{name: "wrong-block-array-type", value: []any{}, kind: 'c'},
		{name: "empty-array-depth", value: []any{}, kind: 'a', depth: 10000},
		{name: "empty-map-array-depth", value: []map[string]any{}, kind: 'm', depth: 10000},
		{name: "empty-block-array-depth", value: []ContentBlock{}, kind: 'c', depth: 10000},
		{name: "nil-native-blocks", value: (*legacyNativeBlockValues)(nil), kind: 'd'},
		{name: "wrong-native-blocks-type", value: []ContentBlock{}, kind: 'd'},
		{name: "native-blocks-length", value: &legacyNativeBlockValues{count: 1}, kind: 'd'},
		{name: "native-blocks-pages", value: &legacyNativeBlockValues{records: legacyCanonicalPages[legacyNativeBlockValue]{nodes: 1}}, kind: 'd'},
		{name: "native-blocks-depth", value: &legacyNativeBlockValues{}, kind: 'd', depth: 10000},
		{name: "nil-replacement", value: (*legacyCanonicalReplacement)(nil), kind: 'r'},
		{name: "wrong-replacement-type", value: []CIREvent{}, kind: 'r'},
		{name: "nil-replacement-events", value: &legacyCanonicalReplacement{}, kind: 'r'},
		{name: "dual-replacement-source", value: &legacyCanonicalReplacement{events: []CIREvent{}, native: &legacyNativeEventValues{}}, kind: 'r'},
		{name: "replacement-count", value: &legacyCanonicalReplacement{events: []CIREvent{{Kind: EventTurn}}}, kind: 'r'},
		{name: "replacement-negative-count", value: &legacyCanonicalReplacement{native: &legacyNativeEventValues{count: -1}}, kind: 'r'},
		{name: "replacement-pages", value: &legacyCanonicalReplacement{native: &legacyNativeEventValues{records: legacyCanonicalPages[legacyNativeEventValue]{nodes: 1}}}, kind: 'r'},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			state := legacyCanonicalPreparingFrameState{Value: legacyCanonicalPreparingFrameRef("value"), Heap: 1024, Kind: fixture.kind, Prepare: true, Depth: fixture.depth}
			var continuation *legacyCanonicalMapContinuation
			if fixture.kind == 'o' {
				state.Continuation, continuation = legacyCanonicalPreparingFrameRef("continuation"), &legacyCanonicalMapContinuation{}
			}
			if err := state.validate(); err != nil {
				test.Fatal("fixture metadata is not valid", err)
			}
			frame, err := restoreLegacyCanonicalPreparingFrame(state, fixture.value, continuation)
			if !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(frame, legacyCanonicalPreparationFrame{}) {
				test.Fatal("contradictory value shape was accepted", err)
			}
		})
	}
}

func TestLegacyCanonicalPreparingFrameContinuationContradictions(test *testing.T) {
	preparation := newLegacyCanonicalPreparation(map[string]any{}, 2, true)
	if _, err := preparation.enter(context.Background(), &LegacyCanonicalEncoding{}); err != nil {
		test.Fatal(err)
	}
	frame := *preparation.frames.at(0)
	valueRef, continuationRef := legacyCanonicalPreparingFrameRef("value"), legacyCanonicalPreparingFrameRef("continuation")
	state := legacyCanonicalPreparingFrameRoundTrip(test, &frame)
	for _, fixture := range []struct {
		name   string
		mutate func(*legacyCanonicalPreparationFrame, *legacyNativeCheckpointRef)
	}{
		{name: "missing-cursor", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) { frame.iterator = nil }},
		{name: "dual-cursor", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) {
			frame.mapResume = &legacyCanonicalMapContinuation{}
		}},
		{name: "missing-ref", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) {
			*ref = legacyNativeCheckpointRef{}
		}},
		{name: "wrong-phase-iterator", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) {
			frame.phase, *ref = 1, legacyNativeCheckpointRef{}
		}},
		{name: "wrong-phase-resume", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) {
			frame.phase, frame.iterator, frame.mapResume, *ref = 2, nil, &legacyCanonicalMapContinuation{}, legacyNativeCheckpointRef{}
		}},
		{name: "resume-position", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) {
			frame.iterator, frame.mapResume = nil, &legacyCanonicalMapContinuation{position: 1}
		}},
		{name: "resume-pages", mutate: func(frame *legacyCanonicalPreparationFrame, ref *legacyNativeCheckpointRef) {
			frame.iterator, frame.mapResume = nil, &legacyCanonicalMapContinuation{fields: legacyCanonicalPages[legacyCanonicalKeyField]{nodes: 1}}
		}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			changed, ref := frame, continuationRef
			fixture.mutate(&changed, &ref)
			if captured, err := captureLegacyCanonicalPreparingFrame(&changed, valueRef, ref); !errors.Is(err, ErrIntegrity) || captured != (legacyCanonicalPreparingFrameState{}) {
				test.Fatal("contradictory capture was accepted", err)
			}
		})
	}
	for _, continuation := range []*legacyCanonicalMapContinuation{nil, {position: -1}, {position: 1}, {fields: legacyCanonicalPages[legacyCanonicalKeyField]{count: 1}}} {
		if restored, err := restoreLegacyCanonicalPreparingFrame(state, frame.value, continuation); !errors.Is(err, ErrIntegrity) || !reflect.DeepEqual(restored, legacyCanonicalPreparationFrame{}) {
			test.Fatal("contradictory continuation was restored", err)
		}
	}
	state.Phase, state.Continuation = 1, legacyNativeCheckpointRef{}
	if _, err := restoreLegacyCanonicalPreparingFrame(state, frame.value, &legacyCanonicalMapContinuation{}); !errors.Is(err, ErrIntegrity) {
		test.Fatal("non-iterating map accepted a continuation", err)
	}
	if captured, err := captureLegacyCanonicalPreparingFrame(nil, valueRef, continuationRef); !errors.Is(err, ErrIntegrity) || captured != (legacyCanonicalPreparingFrameState{}) {
		test.Fatal("nil frame was captured", err)
	}
	frame.heap.err = ErrDocumentResourceLimit
	if captured, err := captureLegacyCanonicalPreparingFrame(&frame, valueRef, continuationRef); !errors.Is(err, ErrDocumentResourceLimit) || captured != (legacyCanonicalPreparingFrameState{}) {
		test.Fatal("capture lost the frame heap error", err)
	}
}

func TestLegacyCanonicalPreparingFrameConsumedContinuationPrefix(test *testing.T) {
	continuation := &legacyCanonicalMapContinuation{position: 1}
	for _, field := range []legacyCanonicalKeyField{{key: "consumed", childKeys: -1}, {key: "remaining", value: true}} {
		if err := continuation.fields.append(field); err != nil {
			test.Fatal(err)
		}
	}
	frame := legacyCanonicalPreparationFrame{value: map[string]any{"consumed": nil, "remaining": true}, heap: docRangeHeaderHeapEstimate{bytes: 2048},
		mapResume: continuation, depth: 4, position: 1, count: 2, kind: 'o'}
	legacyCanonicalPreparingFrameRoundTrip(test, &frame)
	frame.position, continuation.position = 2, 2
	legacyCanonicalPreparingFrameRoundTrip(test, &frame)
	continuation.position = 1
	if _, err := captureLegacyCanonicalPreparingFrame(&frame, legacyCanonicalPreparingFrameRef("value"), legacyCanonicalPreparingFrameRef("continuation")); !errors.Is(err, ErrIntegrity) {
		test.Fatal("remaining continuation count was not checked", err)
	}
}

func TestLegacyCanonicalPreparingFrameLeavesPayloadAndAuthorityToOwner(test *testing.T) {
	number := json.Number("not-yet-validated")
	values := make([]any, 1, 2)
	values[0] = &number
	values[:cap(values)][1] = values
	preparation := newLegacyCanonicalPreparation(values, 2, true)
	if _, err := preparation.enter(context.Background(), &LegacyCanonicalEncoding{}); err != nil {
		test.Fatal(err)
	}
	frame := preparation.frames.at(0)
	state := legacyCanonicalPreparingFrameRoundTrip(test, frame)
	other := make([]any, len(values), cap(values))
	restored, err := restoreLegacyCanonicalPreparingFrame(state, other, nil)
	if err != nil || reflect.ValueOf(restored.value).Pointer() != reflect.ValueOf(other).Pointer() {
		test.Fatal("local metadata attempted source binding or payload reconstruction", err)
	}
	if values[0] != &number || reflect.ValueOf(values[:cap(values)][1]).Pointer() != reflect.ValueOf(values).Pointer() {
		test.Fatal("metadata handling inspected or rewrote the unprepared backing")
	}
}
