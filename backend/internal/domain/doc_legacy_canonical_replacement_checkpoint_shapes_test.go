package domain

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

func TestLegacyCanonicalReplacementCheckpointProducerShapes(test *testing.T) {
	for _, count := range []int{0, 1, 3, 33} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			ctx := context.Background()
			stage, source := legacyCanonicalInitialCheckpointStage(test, `{"kind":"compaction","seq":1,"replacement":[],"replacement_complete":true}`)
			encoding := stage.pending[0].Encoding
			root := encoding.root.(map[string]any)
			replacement := root["replacement"].(*legacyCanonicalReplacement)
			replacement.native.records.clear()
			replacement.native.count = count
			shared := map[string]any{"shared": []any{true, nil, "same"}}
			for ordinal := 0; ordinal < count; ordinal++ {
				event := CIREvent{Kind: EventToolCall, Seq: -ordinal / 2, CallID: "call", ToolName: "test", Input: shared}
				if ordinal == 0 {
					event.Seq = int(^uint(0) >> 1)
				}
				if ordinal == count-1 {
					event.Seq = -int(^uint(0)>>1) - 1
				}
				var child *legacyNativeEventValues
				var blocks *legacyNativeBlockValues
				switch ordinal % 4 {
				case 2:
					event.Kind, event.Role = EventMessage, RoleUser
					if ordinal%8 == 2 {
						blocks = &legacyNativeBlockValues{}
					}
				case 3:
					event.Kind, event.ReplacementComplete = EventCompaction, true
					if ordinal%8 == 3 {
						child = &legacyNativeEventValues{}
					} else {
						event.Locked = &LockedBlob{Provider: "codex", Scheme: "opaque", Blob: "locked"}
					}
				}
				if err := replacement.native.records.append(legacyNativeEventValue{event: event, replacement: child, blocks: blocks}); err != nil {
					test.Fatal(err)
				}
			}
			preparation := encoding.preparation
			preparation.frames.clear()
			if err := preparation.frames.append(legacyCanonicalPreparationFrame{value: replacement, depth: 3, prepare: true}); err != nil {
				test.Fatal(err)
			}
			frame := preparation.frames.at(0)
			if entered, err := preparation.enterReplacement(encoding, frame, replacement); err != nil || !entered {
				test.Fatal("producer could not enter native replacement", entered, err)
			}
			for frame.phase == 0 {
				preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes = 0, 0, 0
				if progress, err := preparation.stepReplacement(ctx, encoding); err != nil || !progress {
					test.Fatal("producer could not reach sorting", progress, err)
				}
			}
			keys := encoding.mapKeys.at(frame.mapPosition)
			if count > 1 {
				for replacement.order.phase != legacyCanonicalKeyOrderCompareChildren && replacement.order.phase != legacyCanonicalKeyOrderCompareParent {
					before := replacement.order.work
					if _, complete, _, err := replacement.order.advance(&keys.fields, 2); err != nil || complete {
						test.Fatal("producer did not yield during key comparison", complete, err)
					}
					encoding.keyWork += replacement.order.work - before
				}
				before := replacement.order.work
				if _, complete, _, err := replacement.order.advance(&keys.fields, 2); err != nil || complete {
					test.Fatal("partial comparison did not yield", complete, err)
				}
				encoding.keyWork += replacement.order.work - before
			}
			grant, err := stage.mutation.beginCapture()
			if err != nil {
				test.Fatal(err)
			}
			defer grant.release()
			store, _ := legacyNativeCheckpointStoreFixture(test)
			binding := HashContent(source.body)
			capture, err := newLegacyCanonicalReplacementCheckpoint(ctx, stage, 0, grant, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer capture.close()
			legacyDecoderCheckpointDrain(test, capture, legacyDecoderCheckpointBudget)
			ref, err := capture.take(ctx)
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyCanonicalReplacementRestore(ctx, replacement, ref, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
			result, err := restore.take(ctx)
			if err != nil || result.order != replacement.order || result.fields.length() != count {
				test.Fatal("producer state did not round trip", err)
			}
			for ordinal := 0; ordinal < count; ordinal++ {
				before, after := keys.fields.at(ordinal), result.fields.at(ordinal)
				if before.key != after.key || !reflect.DeepEqual(before.value, after.value) {
					test.Fatal("restoration changed order, projection, or nil/empty shape", ordinal)
				}
				beforeView, afterView := before.value.(map[string]any), after.value.(map[string]any)
				for _, name := range []string{"input", "blocks"} {
					if beforeView[name] != nil && reflect.ValueOf(beforeView[name]).Pointer() != reflect.ValueOf(afterView[name]).Pointer() {
						test.Fatal("restoration copied native source ownership", ordinal, name)
					}
				}
				if child, ok := beforeView["replacement"].(*legacyCanonicalReplacement); ok && afterView["replacement"].(*legacyCanonicalReplacement).native != child.native {
					test.Fatal("restoration copied the replacement source", ordinal)
				}
			}
			order := result.order
			for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
				_, complete, _, err := order.advance(&result.fields, LegacyCanonicalEncodingBytes)
				if err != nil {
					test.Fatal(err)
				}
				if complete {
					for position := 1; position < count; position++ {
						if result.fields.at(position-1).key >= result.fields.at(position).key {
							test.Fatal("restored tie ordering is not stable", position)
						}
					}
					return
				}
			}
			test.Fatal("restored sort did not finish")
		})
	}
}
