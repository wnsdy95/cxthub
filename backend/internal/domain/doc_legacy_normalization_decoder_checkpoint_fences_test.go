package domain

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyDecoderCheckpointReject(test *testing.T, root legacyNativeCheckpointRef, binding ContentHash, source LegacyByteSource, store legacyNativeCheckpointIO, wanted error) {
	test.Helper()
	work, failure := newLegacyNormalizationDecoderCheckpointRestore(context.Background(), root, store, source, binding, legacyDecoderCheckpointBudget)
	if work != nil {
		defer work.close()
		for attempt := 0; failure == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
			var complete bool
			complete, failure = work.step(context.Background())
			if complete {
				test.Fatal("invalid decoder checkpoint was accepted")
			}
		}
	}
	if !errors.Is(failure, wanted) {
		test.Fatal("decoder checkpoint returned wrong failure", failure, wanted)
	}
}

func TestLegacyNormalizationDecoderCheckpointValidation(test *testing.T) {
	fixture := legacyDecoderCheckpointFixtures()[1]
	fixture.raw = strings.TrimSuffix(fixture.raw, `]}`) + `,{"kind":"message","seq":7,"role":"user","blocks":[{"type":"text","text":"must-not-skip"}]}]}`
	decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
	store, objects := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	var original legacyNormalizationDecoderControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, root, &original); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []string{"kind", "version", "binding", "position", "key", "frame", "missing-event", "dangling-envelope", "advanced-event-cursor", "native-depth"} {
		test.Run(scenario, func(test *testing.T) {
			control := original
			control.Frame = bytes.Clone(control.Frame)
			switch scenario {
			case "kind":
				control.Kind = "not-a-decoder"
			case "version":
				control.Version++
			case "binding":
				control.Binding = HashContent([]byte("different verified input"))
			case "position":
				control.Position = control.SourceBytes + 1
			case "key":
				control.KeyBytes = len(control.Key) + 2
			case "frame":
				control.Frame[0]++
			case "missing-event":
				control.Event = legacyNativeCheckpointRef{}
			case "dangling-envelope":
				control.HasEnvelope = false
			case "advanced-event-cursor":
				control.Position = control.SourceBytes - 2
				if control.Position <= original.Position {
					test.Fatal("fixture has no later event to skip")
				}
			case "native-depth":
				var native legacyNativeObjectCheckpointControl
				if err := legacyNativeCheckpointReadJSON(context.Background(), store, control.Envelope, &native); err != nil {
					test.Fatal(err)
				}
				native.ParentDepth = 1
				var err error
				control.Envelope, err = legacyNativeCheckpointWriteJSON(context.Background(), store, native)
				if err != nil {
					test.Fatal(err)
				}
			}
			changed, err := legacyNativeCheckpointWriteJSON(context.Background(), store, control)
			if err != nil {
				test.Fatal(err)
			}
			legacyDecoderCheckpointReject(test, changed, binding, buffer.input(), store, ErrIntegrity)
		})
	}
	legacyDecoderCheckpointReject(test, root, HashContent([]byte("new current source binding")), buffer.input(), store, ErrIntegrity)
	broken := buffer.input()
	broken.Bytes++
	legacyDecoderCheckpointReject(test, root, binding, broken, store, ErrIntegrity)
	body := bytes.Clone(objects[root.Hash])
	objects[root.Hash][0] ^= 1
	legacyDecoderCheckpointReject(test, root, binding, buffer.input(), store, ErrIntegrity)
	objects[root.Hash] = body
	delete(objects, root.Hash)
	legacyDecoderCheckpointReject(test, root, binding, buffer.input(), store, ErrNotFound)
}

func TestLegacyNormalizationDecoderCheckpointLifecycle(test *testing.T) {
	for _, ending := range []string{"cancel", "write-failure", "take-before-ready"} {
		test.Run(ending, func(test *testing.T) {
			decoder, buffer := legacyDecoderCheckpointCut(test, legacyDecoderCheckpointFixtures()[0])
			store, _ := legacyNativeCheckpointStoreFixture(test)
			binding := HashContent(buffer.body)
			if ending == "write-failure" {
				store.Put = func(context.Context, ContentHash, []byte) error { return ErrConflict }
			}
			work, err := newLegacyNormalizationDecoderCheckpoint(context.Background(), decoder, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			switch ending {
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := work.step(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal(err)
				}
			case "write-failure":
				var failure error
				for attempt := 0; failure == nil && attempt < legacyDecoderCheckpointSteps; attempt++ {
					_, failure = work.step(context.Background())
				}
				if !errors.Is(failure, ErrConflict) {
					test.Fatal("write failure was hidden", failure)
				}
			case "take-before-ready":
				if _, err := work.take(context.Background()); !errors.Is(err, ErrIntegrity) {
					test.Fatal(err)
				}
			}
			if decoder.mutation.activity.Load() != 0 || decoder.terminal != nil {
				test.Fatal("checkpoint failure left original decoder frozen or terminal")
			}
			recoveryStore, _ := legacyNativeCheckpointStoreFixture(test)
			root := legacyDecoderCheckpointCapture(test, decoder, binding, recoveryStore)
			if !root.valid() {
				test.Fatal("original decoder could not be captured after failed attempt")
			}
		})
	}
}

func TestLegacyNormalizationDecoderCheckpointUnfinishedEncoding(test *testing.T) {
	fixture := legacyDecoderCheckpointFixture{raw: `{"envelope":{"cir_version":"2","source_provider":"codex"},"events":[{"kind":"compaction","seq":0,"replacement":[` + strings.Repeat(`{"kind":"message","seq":0,"role":"user","blocks":[{"type":"text","text":"child"}]},`, 10000) + `{"kind":"message","seq":1,"role":"user","blocks":[{"type":"text","text":"last"}]}]}]}`, cut: func(decoder *LegacyNormalizationDecoder) bool {
		return decoder.native != nil && decoder.native.object.eventActive
	}}
	decoder, buffer := legacyDecoderCheckpointCut(test, fixture)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	restored := legacyDecoderCheckpointRestore(test, root, binding, buffer.input(), store)
	if original, resumed := legacyDecoderCheckpointContinue(test, decoder), legacyDecoderCheckpointContinue(test, restored); !reflect.DeepEqual(original, resumed) {
		test.Fatal("unfinished Replacement changed after actual decoder restore", original, resumed)
	}
	buffer = &legacySourceProofBuffer{body: []byte(`{"envelope":{"cir_version":"2","source_provider":"codex"},"events":[]}`)}
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), buffer.input())
	if err != nil {
		test.Fatal(err)
	}
	for attempt := 0; decoder.phase != legacyNormalizationFinalizing && attempt < 128; attempt++ {
		if _, err := decoder.Step(context.Background()); err != nil {
			test.Fatal(err)
		}
	}
	if decoder.phase != legacyNormalizationFinalizing {
		test.Fatal("fixture did not reach actual header encoding")
	}
	binding = HashContent(buffer.body)
	root = legacyDecoderCheckpointCapture(test, decoder, binding, store)
	restored = legacyDecoderCheckpointRestore(test, root, binding, buffer.input(), store)
	if original, resumed := legacyDecoderCheckpointContinue(test, decoder), legacyDecoderCheckpointContinue(test, restored); !reflect.DeepEqual(original, resumed) {
		test.Fatal("unfinished header changed after actual decoder restore", original, resumed)
	}
	if work, err := newLegacyNormalizationDecoderCheckpoint(context.Background(), decoder, HashContent(buffer.body), store, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, ErrDocRangeIndexPending) || decoder.mutation.activity.Load() != 0 {
		test.Fatal("completed decoder was captured after releasing its source", err)
	}
}
