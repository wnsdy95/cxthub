package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyCanonicalPreparationCompletedMapSourceBinding(test *testing.T) {
	ctx := context.Background()
	event := `{"kind":"tool_result","seq":1,"call_id":"completed-map","output":[[null,[{}],null,[null,{}]],[` + strings.Repeat("null,", 5000) + `null]]}`
	stage, source := legacyCanonicalInitialCheckpointStage(test, event+`,{"kind":"turn","seq":99}`)
	storage := stage.files[0].storage.(*legacyNormalizationPreparationStorage)
	canonical, original, err := legacyNormalizationPreparationOracle(source.body)
	if err != nil {
		test.Fatal(err)
	}
	encoding := stage.pending[0].Encoding
	output := encoding.nativeOwner.event.Output.([]any)
	prefix := output[0].([]any)
	want := []any{prefix[1].([]any)[0], prefix[3].([]any)[1]}
	legacyNormalizationPreparationAdvance(test, stage, func() bool {
		if stage.phase != legacyNormalizationStage || encoding.preparation == nil || encoding.mapKeys.length() != 3 {
			return false
		}
		for index := 0; index < encoding.preparation.frames.length(); index++ {
			frame := encoding.preparation.frames.at(index)
			if frame.kind == 'a' && frame.length == 5001 && frame.position > 0 && frame.position < frame.count {
				return true
			}
		}
		return false
	})
	if !legacyNativeCheckpointGoGraphSetSame(encoding.mapKeys.at(0).source, encoding.root) {
		test.Fatal("canonical root lost its owner binding")
	}
	for index, value := range want {
		keys := encoding.mapKeys.at(index + 1)
		if keys == nil || keys.fields.length() != 0 || keys.endKeys != index+2 || !legacyNativeCheckpointGoGraphSetSame(keys.source, value) {
			test.Fatal("completed empty map lost its exact source owner", index)
		}
		for frameIndex := 0; frameIndex < encoding.preparation.frames.length(); frameIndex++ {
			if legacyNativeCheckpointGoGraphSetSame(encoding.preparation.frames.at(frameIndex).value, value) {
				test.Fatal("fixture did not remove completed map frames", index)
			}
		}
	}
	before := *encoding
	frames := make([]legacyCanonicalPreparationFrame, encoding.preparation.frames.length())
	for index := range frames {
		frames[index] = *encoding.preparation.frames.at(index)
		if frames[index].iterator != nil {
			iterator := *frames[index].iterator
			frames[index].iterator = &iterator
		}
	}
	keyStates := make([]legacyCanonicalEncodingKeys, encoding.mapKeys.length())
	fields := make([][]legacyCanonicalKeyField, encoding.mapKeys.length())
	for index := range keyStates {
		keyStates[index] = *encoding.mapKeys.at(index)
		fields[index] = make([]legacyCanonicalKeyField, keyStates[index].fields.length())
		for position := range fields[index] {
			fields[index][position] = *keyStates[index].fields.at(position)
		}
	}
	unchanged := func() bool {
		if !reflect.DeepEqual(before, *encoding) || encoding.preparation.frames.length() != len(frames) || encoding.mapKeys.length() != len(keyStates) {
			return false
		}
		for index, frame := range frames {
			if !reflect.DeepEqual(frame, *encoding.preparation.frames.at(index)) {
				return false
			}
		}
		for index, state := range keyStates {
			current := encoding.mapKeys.at(index)
			if !legacyNativeCheckpointGoGraphSetSame(current.source, state.source) || current.heap != state.heap || current.endKeys != state.endKeys ||
				current.sorted != state.sorted || current.fields.length() != len(fields[index]) {
				return false
			}
			for position, field := range fields[index] {
				actual := current.fields.at(position)
				if actual == nil || actual.key != field.key || actual.childKeys != field.childKeys || !reflect.DeepEqual(actual.value, field.value) {
					return false
				}
			}
		}
		return true
	}
	encoding.mapKeys.at(1).source = map[string]any{}
	if unchanged() {
		test.Fatal("state comparison aliases changed owner metadata")
	}
	encoding.mapKeys.at(1).source = keyStates[1].source
	encoding.preparation.frames.at(0).position++
	if unchanged() {
		test.Fatal("state comparison aliases the current preparation cursor")
	}
	encoding.preparation.frames.at(0).position--
	retained, err := stage.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	grant, err := stage.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	defer func() {
		if grant != nil {
			grant.release()
		}
	}()
	var grantHeap docRangeHeaderHeapEstimate
	grantHeap.allocation(1, int64(unsafe.Sizeof(*grant)))
	store, _ := legacyNativeCheckpointStoreFixture(test)
	checkpoint, err := newLegacyCanonicalSourceCheckpoint(ctx, encoding, stage.mutation, HashContent(source.body), len(source.body), store,
		legacyDecoderCheckpointBudget-retained-grantHeap.bytes)
	if err != nil {
		test.Fatal(err)
	}
	defer checkpoint.close()
	reads, steps, ready := len(source.reads), 0, false
	var references []legacyCanonicalSourceReference
	for ; steps < legacyDecoderCheckpointSteps && !ready; steps++ {
		ready, err = checkpoint.step(ctx)
		heap, heapErr := checkpoint.retainedBytes()
		if err != nil || heapErr != nil || retained+grantHeap.bytes+heap > legacyDecoderCheckpointBudget {
			test.Fatal("completed-owner source capture exceeded the actual aggregate budget", err, heapErr, heap)
		}
		if checkpoint.sourceGraph() == nil || references != nil {
			continue
		}
		if _, found, err := checkpoint.sourceReference(ctx, encoding.mapKeys.at(0).source); err != nil || found {
			test.Fatal("canonical projection was mistaken for raw source", found, err)
		}
		for position := 1; position < encoding.mapKeys.length(); position++ {
			reference, found, err := checkpoint.sourceReference(ctx, encoding.mapKeys.at(position).source)
			if err != nil || !found {
				test.Fatal("completed map has no durable original-source binding", position, found, err)
			}
			references = append(references, reference)
		}
	}
	if !ready || len(references) != 2 || references[0] == references[1] || !unchanged() || len(source.reads) != reads {
		test.Fatal("source binding conflated equal maps, advanced preparation, or reread input")
	}
	root, err := checkpoint.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	wire, err := json.Marshal(references)
	if err != nil {
		test.Fatal(err)
	}
	references = nil
	if err := json.Unmarshal(wire, &references); err != nil {
		test.Fatal(err)
	}
	restore, err := newLegacyCanonicalInitialRestore(ctx, root, HashContent(source.body), len(source.body), store,
		legacyDecoderCheckpointBudget-retained-grantHeap.bytes)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	values := legacyCanonicalSourceReferencesRestore(test, restore, references)
	restored, err := restore.take(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer restored.release()
	restoredPrefix := restored.nativeOwner.event.Output.([]any)[0].([]any)
	restoredWant := []any{restoredPrefix[1].([]any)[0], restoredPrefix[3].([]any)[1]}
	for index, value := range values {
		if !legacyNativeCheckpointGoGraphSetSame(value, restoredWant[index]) || legacyNativeCheckpointGoGraphSetSame(value, want[index]) ||
			index > 0 && legacyNativeCheckpointGoGraphSetSame(value, values[index-1]) {
			test.Fatal("durable binding did not return the distinct restored source occurrence", index)
		}
	}
	if _, err := stage.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("source binding released the caller's capture", err)
	}
	if !unchanged() || len(source.reads) != reads || !legacyCanonicalInitialState(stage.pending[1].Encoding) {
		test.Fatal("owner capture consumed preparation or a future event")
	}
	grant.release()
	grant = nil
	complete := false
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && !complete; attempt++ {
		complete, err = stage.Step(ctx)
		if err != nil {
			test.Fatal("owner metadata changed actual canonical preparation", err)
		}
	}
	if !complete || encoding.mapKeys.length() != 0 || encoding.nativeOwner != nil {
		test.Fatal("completed encoding kept its borrowed owners")
	}
	legacyNormalizationPreparationCheck(test, stage, storage, canonical, original)
	test.Logf("completed maps=%d; source save steps=%d; durable references=%d bytes", len(want), steps, len(wire))
}
