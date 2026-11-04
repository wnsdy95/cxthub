package domain

import (
	"context"
	"errors"
	"testing"
)

func TestLegacyNativeEventListCheckpointRestoreRejectsBrokenLinks(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventListRestoreFixtures()[0])
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	var control legacyNormalizationDecoderControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, root, &control); err != nil {
		test.Fatal(err)
	}
	var native legacyNativeObjectCheckpointControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, control.Event, &native); err != nil {
		test.Fatal(err)
	}
	index := legacyEventNativeFields.match("replacement")
	if index < 0 || native.Fields[index].Event == nil {
		test.Fatal("fixture has no typed replacement reference")
	}
	var original legacyNativeEventListCheckpointControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, native.Fields[index].Event.List, &original); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []string{"root-index", "parent", "missing-cursor", "binding", "missing-object", "missing-values"} {
		test.Run(scenario, func(test *testing.T) {
			state := original
			switch scenario {
			case "root-index":
				state.Index++
			case "parent":
				state.Parent = 0
			case "missing-cursor":
				state.Cursor = 9999
			case "binding":
				state.Binding = HashContent([]byte("other source"))
			case "missing-object":
				state.Object = legacyNativeCheckpointRef{}
			case "missing-values":
				state.HasValues = false
			}
			ref, err := legacyNativeCheckpointWriteJSON(context.Background(), store, state)
			if err != nil {
				test.Fatal(err)
			}
			copyNative := native
			copyNative.Fields = append(legacyNativeObjectCheckpointFields(nil), native.Fields...)
			copyNative.Fields[index].Event = &legacyNativeObjectCheckpointEventField{List: ref}
			copyControl := control
			copyControl.Event, err = legacyNativeCheckpointWriteJSON(context.Background(), store, copyNative)
			if err != nil {
				test.Fatal(err)
			}
			changed, err := legacyNativeCheckpointWriteJSON(context.Background(), store, copyControl)
			if err != nil {
				test.Fatal(err)
			}
			legacyDecoderCheckpointReject(test, changed, binding, buffer.input(), store, ErrIntegrity)
		})
	}
}

func TestLegacyNativeEventListCheckpointRestoreReleasesOwner(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventListRestoreFixtures()[2])
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	for _, ending := range []string{"cancel", "source-stop", "early-take"} {
		test.Run(ending, func(test *testing.T) {
			restore, err := newLegacyNormalizationDecoderCheckpointRestore(context.Background(), root, store, buffer.input(), binding, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			for attempt := 0; attempt < legacyDecoderCheckpointSteps; attempt++ {
				ready, err := restore.step(context.Background())
				if err != nil || ready {
					test.Fatal("restore ended before nested list owner", err)
				}
				if restore.child != nil && restore.child.eventTask != nil && restore.child.eventTask.object != nil {
					break
				}
			}
			if restore.child == nil || restore.child.eventTask == nil || restore.child.eventTask.object == nil {
				test.Fatal("restore never granted the nested child destination")
			}
			scope := restore.normalizer.mutation
			if scope.activity.Load() != -2 {
				test.Fatal("decoder restoration lost exclusive mutation ownership")
			}
			wanted := error(ErrIntegrity)
			switch ending {
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, err = restore.step(ctx)
				wanted = context.Canceled
			case "source-stop":
				scope.stop()
				_, err = restore.step(context.Background())
				wanted = ErrConflict
			case "early-take":
				_, err = restore.take(context.Background())
			}
			if !errors.Is(err, wanted) || scope.activity.Load() != 0 || scope.stepping.Load() || !restore.closed || restore.normalizer != nil {
				test.Fatal("failed nested restore leaked owner or returned a partial decoder", err, scope.activity.Load())
			}
		})
	}
}
