package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyCanonicalSourceCheckpointStage(test *testing.T, kind byte) (*LegacyNormalizationPreparation, *legacySourceProofBuffer) {
	test.Helper()
	var event strings.Builder
	switch kind {
	case 'a':
		event.WriteString(`{"kind":"tool_call","seq":1,"input":{"items":[`)
		event.WriteString(strings.Repeat("null,", 5000))
		event.WriteString(`null]}}`)
	case 'o':
		event.WriteString(`{"kind":"tool_result","seq":1,"output":{`)
		for index := 0; index < 5000; index++ {
			if index != 0 {
				event.WriteByte(',')
			}
			fmt.Fprintf(&event, `"key-%06d":{"items":[true,null,%d]}`, index, index)
		}
		event.WriteString(`}}`)
	case 'r':
		event.WriteString(`{"kind":"compaction","seq":1,"replacement_complete":true,"replacement":[`)
		for index := 0; index < 1200; index++ {
			if index != 0 {
				event.WriteByte(',')
			}
			fmt.Fprintf(&event, `{"kind":"turn","seq":%d}`, 1200-index)
		}
		event.WriteString(`]}`)
	default:
		test.Fatal("unknown actual preparation fixture", kind)
	}
	stage, source := legacyCanonicalInitialCheckpointStage(test, event.String()+`,{"kind":"turn","seq":9000}`)
	for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
		complete, err := stage.Step(context.Background())
		if err != nil || complete || stage.phase != legacyNormalizationStage || stage.next != 0 {
			test.Fatal("actual worker passed the requested preparation boundary", complete, err)
		}
		encoding := stage.pending[0].Encoding
		if encoding.preparation == nil {
			test.Fatal("actual event finished preparation before the requested boundary")
		}
		for index := 0; index < encoding.preparation.frames.length(); index++ {
			frame := encoding.preparation.frames.at(index)
			if frame.kind == kind && (kind == 'r' && frame.phase == 1 || kind != 'r' && frame.position > 0 && frame.position < frame.count) {
				return stage, source
			}
		}
	}
	test.Fatal("actual worker never reached the requested preparation boundary")
	return nil, nil
}

func TestLegacyCanonicalSourceCheckpointAdvancedPreparation(test *testing.T) {
	for _, kind := range []byte{'a', 'o', 'r'} {
		test.Run(string(kind), func(test *testing.T) {
			stage, source := legacyCanonicalSourceCheckpointStage(test, kind)
			encoding := stage.pending[0].Encoding
			owner, preparation := encoding.nativeOwner, encoding.preparation
			before := *encoding
			frames := make([]legacyCanonicalPreparationFrame, preparation.frames.length())
			for index := range frames {
				frames[index] = *preparation.frames.at(index)
				if frames[index].iterator != nil {
					iterator := *frames[index].iterator
					frames[index].iterator = &iterator
				}
			}
			retained, err := encoding.RetainedBytes()
			if err != nil {
				test.Fatal(err)
			}
			stageRetained, err := stage.RetainedBytes()
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
			var captureHeap docRangeHeaderHeapEstimate
			captureHeap.allocation(1, int64(unsafe.Sizeof(*grant)))
			additionalBudget := legacyDecoderCheckpointBudget - stageRetained - captureHeap.bytes
			if captureHeap.err != nil || additionalBudget <= 0 {
				test.Fatal("source capture has no room after retaining the actual worker", stageRetained, captureHeap)
			}
			store, _ := legacyNativeCheckpointStoreFixture(test)
			written := 0
			put := store.Put
			store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				written += len(body)
				return put(ctx, hash, body)
			}
			binding := HashContent(source.body)
			reads := len(source.reads)
			if task, err := newLegacyCanonicalInitialCheckpoint(context.Background(), encoding, stage.mutation, binding, len(source.body), store, legacyDecoderCheckpointBudget); task != nil || !errors.Is(err, ErrIntegrity) {
				test.Fatal("progressed preparation was incorrectly accepted as an unvisited event", err)
			}
			checkpoint, err := newLegacyCanonicalSourceCheckpoint(context.Background(), encoding, stage.mutation, binding, len(source.body), store, additionalBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer checkpoint.close()
			defer func() {
				if !test.Failed() || checkpoint.child == nil {
					return
				}
				child := checkpoint.child
				if child.current != nil {
					test.Logf("source writer frame: phase=%d index=%d frames=%d", child.current.phase, child.current.index, child.frames)
				}
				if child.fields != nil && child.fields.graph != nil {
					graph := child.fields.graph
					test.Logf("source graph: position=%d phase=%d registry=%d", graph.position, graph.phase, graph.registry.entries)
					if graph.prepare != nil {
						test.Logf("source graph preparation: %+v", *graph.prepare)
					}
					if graph.write != nil && graph.write.frame != nil {
						frame := graph.write.frame
						test.Logf("source graph writer: phase=%d position=%d", frame.phase, frame.position)
					}
				}
			}()
			if _, err := stage.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Fatal("source capture did not freeze the actual worker", err)
			}
			ready, steps, maximumWrite := false, 0, 0
			var maximumHeap int64
			var references []legacyCanonicalSourceReference
			for ; steps < legacyDecoderCheckpointSteps && !ready; steps++ {
				beforeWrite := written
				var parts [9]int64
				if checkpoint.child.fields != nil && checkpoint.child.fields.graph != nil {
					graph := checkpoint.child.fields.graph
					parts[0], _ = graph.registry.retainedBytes()
					parts[1], _ = graph.identity.retainedBytes()
					parts[2] = graph.budget
					parts[5], parts[6], parts[7], parts[8] = graph.registry.nodes, graph.registry.entries, graph.registry.owned, graph.sourceHeap.bytes
					if graph.write != nil {
						parts[3], _ = graph.write.retainedBytes()
						parts[4] = graph.write.budget
					}
				}
				ready, err = checkpoint.step(context.Background())
				maximumWrite = max(maximumWrite, written-beforeWrite)
				heap, heapErr := checkpoint.retainedBytes()
				currentStage, stageErr := stage.RetainedBytes()
				maximumHeap = max(maximumHeap, heap)
				if err != nil || heapErr != nil || stageErr != nil || currentStage != stageRetained || heap < 0 || heap+currentStage+captureHeap.bytes > legacyDecoderCheckpointBudget || maximumWrite > 512<<10 {
					test.Fatal("source capture escaped its aggregate heap, error, or I/O contract", steps, err, heapErr, stageErr, heap, currentStage, captureHeap, maximumWrite, parts)
				}
				if checkpoint.sourceGraph() != nil && kind != 'r' {
					var rawValue any = owner.event.Output
					if kind == 'a' {
						rawValue = owner.event.Input["items"]
					}
					if _, found, err := checkpoint.sourceReference(context.Background(), encoding.root); err != nil || found {
						test.Fatal("canonical projection was treated as raw source", found, err)
					}
					for _, frame := range frames {
						if frame.kind != kind || frame.position <= 0 || frame.position >= frame.count || !legacyNativeCheckpointGoGraphSetSame(frame.value, rawValue) {
							continue
						}
						reference, found, err := checkpoint.sourceReference(context.Background(), frame.value)
						if err != nil || !found {
							test.Fatal("actual progressed frame has no stored source identity", kind, found, err)
						}
						references = append(references, reference)
					}
				}
			}
			test.Logf("source capture steps=%d written=%d maximum-step-write=%d maximum-additional-heap=%d stage-heap=%d grant-heap=%d maximum-aggregate-heap=%d", steps, written, maximumWrite, maximumHeap, stageRetained, captureHeap.bytes, maximumHeap+stageRetained+captureHeap.bytes)
			if !ready {
				test.Fatal("source capture exceeded the existing finite step schedule")
			}
			ref, err := checkpoint.take(context.Background())
			if err != nil || !ref.valid() || grant.references.Load() != 1 {
				test.Fatal("source capture did not release borrowed grants", ref, err, grant.references.Load())
			}
			if !reflect.DeepEqual(before, *encoding) || encoding.nativeOwner != owner || encoding.preparation != preparation {
				test.Fatal("source capture changed canonical processing state")
			}
			for index := range frames {
				if !reflect.DeepEqual(frames[index], *preparation.frames.at(index)) {
					test.Fatal("source capture advanced the active cursor", index)
				}
			}
			after, err := encoding.RetainedBytes()
			if err != nil || after != retained || len(source.reads) != reads {
				test.Fatal("source capture reread the input or added normal-path metadata", retained, after, err)
			}
			restore, err := newLegacyCanonicalInitialRestore(context.Background(), ref, binding, len(source.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			resolved := legacyCanonicalSourceReferencesRestore(test, restore, references)
			restoredEncoding, err := restore.take(context.Background())
			if err != nil || !legacyCanonicalInitialState(restoredEncoding) {
				test.Fatal("source-only checkpoint must not fabricate a restored preparation cursor", err)
			}
			defer restoredEncoding.release()
			record := restoredEncoding.nativeOwner
			if kind != 'r' {
				if len(resolved) != 1 {
					test.Fatal("progressed frame binding was missing or repeated", len(resolved))
				}
				var expected any = record.event.Output
				if kind == 'a' {
					expected = record.event.Input["items"]
				}
				if !legacyNativeCheckpointGoGraphSetSame(resolved[0], expected) {
					test.Fatal("progressed frame was bound to a copy instead of the restored raw source")
				}
			}
			if !reflect.DeepEqual(record.event, owner.event) || record.heap.err != nil || record.heap.bytes < 0 {
				test.Fatal("source capture changed raw event values or lost payload accounting", record.heap, owner.heap)
			}
			if kind == 'a' {
				original := owner.event.Input["items"].([]any)
				restored := record.event.Input["items"].([]any)
				if len(original) != len(restored) || cap(original) != cap(restored) || !reflect.DeepEqual(original[:cap(original)], restored[:cap(restored)]) {
					test.Fatal("source restoration changed the array's full backing")
				}
			}
			legacyEventValuesCheckpointCompare(test, owner.replacement, record.replacement)
			if !legacyCanonicalInitialState(stage.pending[1].Encoding) || len(source.reads) != reads {
				test.Fatal("source restoration consumed a future event or the original input")
			}
			grant.release()
			grant = nil
			if complete, err := stage.Step(context.Background()); err != nil || complete {
				test.Fatal("original worker could not continue after source capture", complete, err)
			}
		})
	}
}

func TestLegacyCanonicalSourceCheckpointFences(test *testing.T) {
	for _, scenario := range []string{"source-stop", "stop-on-write", "owner-replaced", "preparation-replaced", "cancel-child", "write-failure", "budget", "early-take"} {
		test.Run(scenario, func(test *testing.T) {
			stage, source := legacyCanonicalSourceCheckpointStage(test, 'a')
			encoding := stage.pending[0].Encoding
			owner, preparation := encoding.nativeOwner, encoding.preparation
			grant, err := stage.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer grant.release()
			store, _ := legacyNativeCheckpointStoreFixture(test)
			if scenario == "write-failure" {
				store.Put = func(context.Context, ContentHash, []byte) error { return ErrConflict }
			}
			if scenario == "stop-on-write" {
				put := store.Put
				store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
					err := put(ctx, hash, body)
					stage.mutation.stop()
					return err
				}
			}
			work, err := newLegacyCanonicalSourceCheckpoint(context.Background(), encoding, stage.mutation, HashContent(source.body), len(source.body), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := error(ErrConflict)
			switch scenario {
			case "source-stop":
				stage.mutation.stop()
			case "owner-replaced":
				replacement := *owner
				encoding.nativeOwner = &replacement
			case "preparation-replaced":
				replacement := *preparation
				encoding.preparation = &replacement
			case "cancel-child":
				expected = context.Canceled
			case "budget":
				work.budget, expected = 1, ErrDocumentResourceLimit
			case "early-take":
				expected = ErrIntegrity
			}
			var failure error
			if scenario == "early-take" {
				_, failure = work.take(ctx)
			} else {
				for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
					if scenario == "cancel-child" && work.child != nil && work.child.fields != nil {
						cancel()
					}
					ready, err := work.step(ctx)
					if ready {
						test.Fatal("invalid source capture published a root")
					}
					if err != nil {
						failure = err
						break
					}
				}
			}
			encoding.nativeOwner, encoding.preparation = owner, preparation
			if !errors.Is(failure, expected) || grant.references.Load() != 1 {
				test.Fatal("source capture changed its error or leaked a capture reference", failure, expected, grant.references.Load())
			}
			if _, err := work.take(context.Background()); !errors.Is(err, failure) {
				test.Fatal("source capture lost its terminal error", err)
			}
			legacyNativeAnyW78Released(test, work)
		})
	}
}

func TestLegacyCanonicalSourceFrozenGraphSamples(test *testing.T) {
	for _, stop := range []bool{false, true} {
		test.Run(fmt.Sprint(stop), func(test *testing.T) {
			shared := map[string]any{"items": []any{true, nil, float64(3)}, "nested": map[string]any{"value": "retained"}}
			heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(shared))
			if err != nil {
				test.Fatal(err)
			}
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{Input: shared, Output: shared}, heap: heap})
			store, _ := legacyNativeCheckpointStoreFixture(test)
			binding := HashContent([]byte("frozen-source-samples"))
			work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			ready, reclaimed := false, false
			for attempt := 0; attempt < legacyDecoderCheckpointSteps && !ready; attempt++ {
				ready, err = work.step(context.Background())
				if err != nil {
					test.Fatal(err)
				}
				if work.graph == nil || work.graph.registry.first == nil || !work.graph.registry.first.verified {
					continue
				}
				registry := work.graph.registry
				if registry.capture != work.capture || !registry.capture.valid(list.mutation) || registry.first.samples.length() != 0 {
					test.Fatal("verified samples were not retired under the existing source grant")
				}
				var expected docRangeHeaderHeapEstimate
				expected.allocation(1, int64(unsafe.Sizeof(*registry)))
				for count := int64(0); count < registry.nodes; count++ {
					expected.allocation(1, int64(unsafe.Sizeof(legacyNativeCheckpointGoGraphLookup{})))
				}
				var sourceBytes int64
				for entry := registry.first; entry != nil; entry = entry.next {
					expected.allocation(1, int64(unsafe.Sizeof(*entry)))
					expected.include(docRangeHeaderHeapEstimate{bytes: entry.owned - entry.sourceBytes})
					sourceBytes += entry.sourceBytes
				}
				retained, retainedErr := registry.retainedBytes()
				remaining, remainingErr := work.graph.remaining()
				parentRemaining, parentErr := work.remaining()
				if expected.err != nil || retainedErr != nil || retained != expected.bytes || sourceBytes <= 0 || sourceBytes != registry.sourceBytes ||
					sourceBytes > work.record.heap.bytes || work.graph.sourceHeap != (docRangeHeaderHeapEstimate{}) || work.graph.record != work ||
					remainingErr != nil || parentErr != nil || remaining != parentRemaining {
					test.Fatal("frozen source reservation duplicated payload or omitted additional owners", retained, expected, sourceBytes, retainedErr, remainingErr, parentErr)
				}
				reclaimed = true
				if stop {
					list.mutation.stop()
					if ready, err := work.step(context.Background()); ready || !errors.Is(err, ErrConflict) {
						test.Fatal("retired samples allowed reuse after source revocation", ready, err)
					}
					if list.mutation.capture.Load() != nil || list.mutation.activity.Load() != 0 {
						test.Fatal("revoked source retained the capture grant")
					}
					return
				}
			}
			if !ready || !reclaimed {
				test.Fatal("actual field writer did not reclaim verified samples and finish")
			}
			root, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyNativeEventRecordFieldsRestore(context.Background(), root, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
			result, err := restore.take(context.Background())
			if err != nil || !reflect.DeepEqual(result.Event.Input, shared) || reflect.ValueOf(result.Event.Input).Pointer() != reflect.ValueOf(result.Event.Output).Pointer() {
				test.Fatal("sample retirement changed source values or cross-root alias identity", err)
			}
		})
	}
	test.Run("unfrozen", func(test *testing.T) {
		store, _ := legacyNativeCheckpointStoreFixture(test)
		work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), []any{map[string]any{"value": "checked"}}, HashContent([]byte("unfrozen-source")), store, legacyDecoderCheckpointBudget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		legacyDecoderCheckpointDrain(test, work, legacyDecoderCheckpointBudget)
		entry := work.registry.first
		if entry == nil || entry.verified || entry.samples.length() != 1 || work.registry.capture != nil {
			test.Fatal("unfrozen graph discarded source verification state")
		}
	})
}

func TestLegacyCanonicalSourceReservationFences(test *testing.T) {
	for _, scenario := range []string{"detached-parent", "replaced-parent", "changed-root", "lost-grant", "budget", "unaccounted-source"} {
		test.Run(scenario, func(test *testing.T) {
			shared := map[string]any{"items": []any{true, nil, float64(3)}}
			heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(shared))
			if err != nil {
				test.Fatal(err)
			}
			list := legacyNativeEventRecordFieldsTestList(test, legacyNativeEventValue{event: CIREvent{Input: shared}, heap: heap})
			store, _ := legacyNativeCheckpointStoreFixture(test)
			writes, put := 0, store.Put
			store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
				writes++
				return put(ctx, hash, body)
			}
			work, err := newLegacyNativeEventRecordFieldsCheckpoint(context.Background(), list, 0, HashContent([]byte("source-reservation-fence")), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			graph := work.graph
			defer graph.close()
			expected := error(ErrConflict)
			switch scenario {
			case "detached-parent":
				work.graph = nil
			case "replaced-parent":
				replacement := *work
				graph.record = &replacement
			case "changed-root":
				work.roots[16] = map[string]any{"items": shared["items"]}
			case "lost-grant":
				graph.registry.capture = nil
				expected = ErrIntegrity
			case "budget":
				work.budget, err = work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
				expected = ErrDocumentResourceLimit
			case "unaccounted-source":
				graph.registry.sourceBytes = work.record.heap.bytes + 1
				expected = ErrIntegrity
			}
			if ready, err := graph.step(context.Background()); ready || !errors.Is(err, expected) {
				test.Fatal("owned graph accepted an invalid source reservation", ready, err, expected)
			}
			if writes != 0 || !graph.closed || graph.failure == nil || graph.record != nil {
				test.Fatal("rejected source retained borrowed state or performed IO", writes)
			}
			work.close()
			if list.mutation.capture.Load() != nil || list.mutation.activity.Load() != 0 {
				test.Fatal("rejected source did not release its owner grant")
			}
		})
	}
}
