package domain

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func legacyEventListRestoreFixtures() []legacyDecoderCheckpointFixture {
	fixtures := legacyEventValuesCheckpointFixtures()[:2]
	large := strings.Repeat("중첩🙂", ConversationManifestChunkBytes/3)
	fixtures = append(fixtures, legacyDecoderCheckpointFixture{name: "suspended-nested-child-cursor",
		raw: string(legacyNativeEventDocument("2", `{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":[{"kind":"compaction","seq":1,"replacement_complete":true,"replacement":[{"kind":"message","seq":2,"role":"user","blocks":[{"type":"text","text":"`+large+`"}]}]}]}`)),
		cut: func(decoder *LegacyNormalizationDecoder) bool {
			if decoder.native == nil {
				return false
			}
			for _, list := range decoder.native.object.events {
				if list != nil && list.cursor != nil && list.cursor != list {
					blocks := legacyNativeEventW82List(list.cursor.object)
					return blocks != nil && blocks.object != nil && blocks.object.scalar != nil && blocks.object.scalar.text != nil && blocks.object.scalar.text.Len() > 0
				}
			}
			return false
		}})
	return append(fixtures, legacyEventListRestoreNullFixture(), legacyEventListRestoreKeyFixture())
}

func legacyEventListRestoreNullFixture() legacyDecoderCheckpointFixture {
	return legacyDecoderCheckpointFixture{name: "inactive-null-cache-before-replacement-reuse",
		raw: string(legacyNativeEventDocument("2", `{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":null,"id":"`+
			strings.Repeat("null-cache", ConversationManifestChunkBytes/4)+`","replacement":[{"kind":"message","seq":1,"role":"user","blocks":[]}]}`)),
		cut: func(decoder *LegacyNormalizationDecoder) bool {
			if decoder.native != nil && !decoder.native.object.eventActive && decoder.native.object.scalar != nil {
				for _, list := range decoder.native.object.events {
					if list != nil && list.phase == legacyNativeEventsDone && list.values == nil && list.literal == len("null") {
						return true
					}
				}
			}
			return false
		}}
}

func TestLegacyNativeEventListCheckpointPreservesDeferredRejection(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[2])
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	restored := legacyDecoderCheckpointRestore(test, root, binding, buffer.input(), store)
	if original, resumed := legacyEventValuesCheckpointRejectedContinuation(test, decoder), legacyEventValuesCheckpointRejectedContinuation(test, restored); !reflect.DeepEqual(original, resumed) {
		test.Fatal("restoring an inactive list changed later null-child rejection", original, resumed)
	}
}

func TestLegacyNativeEventListCheckpointActualDecoderRestore(test *testing.T) {
	for _, fixture := range legacyEventListRestoreFixtures() {
		test.Run(fixture.name, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
			store, _ := legacyNativeCheckpointStoreFixture(test)
			binding := HashContent(buffer.body)
			reads := len(buffer.reads)
			root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
			restored := legacyDecoderCheckpointRestore(test, root, binding, buffer.input(), store)
			if len(buffer.reads) != reads || restored.native == nil || restored.native.object.destination.Interface() != &restored.native.decoded {
				test.Fatal("restore replayed source or detached the actual native destination")
			}
			left, _ := legacyEventValuesCheckpointList(test, decoder)
			right, _ := legacyEventValuesCheckpointList(test, restored)
			if left.phase != right.phase || left.count != right.count || left.position != right.position || left.end != right.end ||
				(left.cursor == nil) != (right.cursor == nil) || right.frame != &restored.frame {
				test.Fatal("restored list lost the real parser cursor or shared scanner")
			}
			legacyEventValuesCheckpointCompare(test, left.values, right.values)
			for list := right; list != nil; {
				if list.object != nil && (list.object.destination.Interface() != &list.child || list.object.mutation != restored.mutation) {
					test.Fatal("child was restored into a temporary value or foreign mutation scope")
				}
				next, err := legacyNativeEventListCheckpointChild(list)
				if err != nil {
					test.Fatal(err)
				}
				list = next
			}
			second := legacyDecoderCheckpointCapture(test, restored, binding, store)
			again := legacyDecoderCheckpointRestore(test, second, binding, buffer.input(), store)
			if len(buffer.reads) != reads {
				test.Fatal("recapture re-read original source")
			}
			original := legacyDecoderCheckpointContinue(test, decoder)
			if got := legacyDecoderCheckpointContinue(test, restored); !reflect.DeepEqual(got, original) {
				test.Fatal("restored replacement changed actual decoder output", got, original)
			}
			if got := legacyDecoderCheckpointContinue(test, again); !reflect.DeepEqual(got, original) {
				test.Fatal("recaptured replacement changed actual decoder output", got, original)
			}
		})
	}
}

func TestLegacyNativeEventListCheckpointNewProcess(test *testing.T) {
	legacyDecoderCheckpointNewProcesses(test, legacyEventListRestoreFixtures())
}

func TestLegacyNativeEventListCheckpointStandaloneScanner(test *testing.T) {
	fixture := legacyEventListRestoreFixtures()[2]
	decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	capture, err := newLegacyNativeObjectCheckpoint(context.Background(), decoder.native.object, &decoder.frame, binding, decoder.source, store, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.close()
	legacyDecoderCheckpointDrain(test, capture, legacyDecoderCheckpointBudget)
	root, err := capture.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	restore, err := newLegacyNativeObjectCheckpointRestore(context.Background(), root, legacyNativeCheckpointEventModel, store, decoder.source, binding, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
	result, err := restore.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	wrongFrame := result.Scanner
	wrongFrame.complete = !wrongFrame.complete
	blocked := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - 1}
	if ready, err := result.Object.bindCheckpointScanner(context.Background(), blocked, &wrongFrame); ready || err != nil || result.Object.checkpointBinding {
		test.Fatal("root scanner compared before admitting its quantum", ready, err)
	}
	if ready, err := result.Object.bindCheckpointScanner(context.Background(), &legacyNormalizationInput{}, &result.Scanner); ready || err != nil || !result.Object.checkpointBinding {
		test.Fatal("nested scanner cursor was not staged for bounded rebinding", ready, err)
	}
	cursor := result.Object.checkpointCursor
	if ready, err := result.Object.bindCheckpointScanner(context.Background(), blocked, &wrongFrame); ready || err != nil || result.Object.checkpointCursor != cursor {
		test.Fatal("nested scanner compared or advanced before admitting its quantum", ready, err)
	}
	original := legacyNativeObjectRestored{Object: decoder.native.object, Scanner: decoder.frame, Source: decoder.source}
	for _, owner := range []struct {
		native *legacyNativeObject
		source LegacyByteSource
		frame  *legacyJSONValueScanner
	}{{original.Object, original.Source, &decoder.frame}, {result.Object, result.Source, &result.Scanner}} {
		complete := false
		for attempt := 0; !complete && attempt < legacyDecoderCheckpointSteps; attempt++ {
			input := &legacyNormalizationInput{ctx: context.Background(), source: owner.source, numberDiagnostics: legacyNativeNumberDiagnosticsFull}
			complete, err = owner.native.step(context.Background(), input, owner.frame)
			if err != nil {
				test.Fatal("standalone scanner rebind failed", err)
			}
			if input.unbounded || input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads > 2 {
				test.Fatal("standalone rebind or continuation escaped its shared quantum", input.workBytes, input.loads)
			}
		}
		if !complete || owner.native.checkpointScanner != nil || owner.native.checkpointCursor != nil || owner.native.checkpointBinding {
			test.Fatal("standalone list never completed its bounded scanner handoff")
		}
	}
	left, err := original.Object.pagedReplacement()
	if err != nil {
		test.Fatal(err)
	}
	right, err := result.Object.pagedReplacement()
	if err != nil {
		test.Fatal(err)
	}
	legacyEventValuesCheckpointCompare(test, left, right)
}

func legacyEventListRestoreKeyFixture() legacyDecoderCheckpointFixture {
	return legacyDecoderCheckpointFixture{name: "shared-scanner-in-long-unknown-child-key",
		raw: string(legacyNativeEventDocument("2", `{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":[{"kind":"message","seq":1,"role":"user","ignored":{"`+
			strings.Repeat("unknown-key", ConversationManifestChunkBytes/4)+`":true},"blocks":[{"type":"text","text":"after-ignored-key"}]}]}`)),
		cut: func(decoder *LegacyNormalizationDecoder) bool {
			return decoder.native != nil && decoder.native.object.eventActive && decoder.frame.key
		}}
}

func TestLegacyNativeEventListCheckpointScannerKeyInChild(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventListRestoreKeyFixture())
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	restored := legacyDecoderCheckpointRestore(test, root, binding, buffer.input(), store)
	if original, resumed := legacyDecoderCheckpointContinue(test, decoder), legacyDecoderCheckpointContinue(test, restored); !reflect.DeepEqual(original, resumed) {
		test.Fatal("child-key scanner changed actual decoder continuation", original, resumed)
	}
}
