package domain

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestLegacyNormalizationHeaderCheckpointRejectsBrokenState(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyHeaderCheckpointFixtures()[0])
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	var control legacyNormalizationDecoderControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, root, &control); err != nil || control.Header == nil {
		test.Fatal("actual header root is missing", err)
	}
	var original legacyNormalizationHeaderCheckpointControl
	if err := legacyNativeCheckpointReadJSON(context.Background(), store, *control.Header, &original); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []string{"binding", "schema", "position", "copying", "missing-envelope", "scalar-cursor", "pending-hash"} {
		test.Run(scenario, func(test *testing.T) {
			state := original
			encoding := *state.Encoding
			state.Encoding = &encoding
			switch scenario {
			case "binding":
				state.Binding = HashContent([]byte("other source"))
			case "schema":
				state.Schema = HashContent([]byte("old envelope schema"))
			case "position":
				state.Position++
			case "copying":
				state.Copying = true
			case "missing-envelope":
				state.Envelope = legacyNativeCheckpointRef{}
			case "scalar-cursor":
				scalar := *state.Encoding.Scalar
				scalar.Position = decoder.source.Bytes + 1
				state.Encoding.Scalar = &scalar
			case "pending-hash":
				state.Encoding.Pending.Hash = HashContent([]byte("not the pending bytes"))
			}
			changedHeader, err := legacyNativeCheckpointWriteJSON(context.Background(), store, state)
			if err != nil {
				test.Fatal(err)
			}
			changedControl := control
			changedControl.Header = &changedHeader
			changed, err := legacyNativeCheckpointWriteJSON(context.Background(), store, changedControl)
			if err != nil {
				test.Fatal(err)
			}
			wanted := error(ErrIntegrity)
			if scenario == "pending-hash" {
				wanted = ErrNotFound
			}
			legacyDecoderCheckpointReject(test, changed, binding, buffer.input(), store, wanted)
		})
	}
}

func TestLegacyNormalizationHeaderCheckpointRestoreOwnsMutation(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyHeaderCheckpointFixtures()[0])
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	for _, ending := range []string{"complete", "cancel", "source-stop", "early-take"} {
		test.Run(ending, func(test *testing.T) {
			restore, err := newLegacyNormalizationDecoderCheckpointRestore(context.Background(), root, store, buffer.input(), binding, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			for attempt := 0; attempt < legacyDecoderCheckpointSteps && (restore.header == nil || restore.header.phase < 3); attempt++ {
				ready, err := restore.step(context.Background())
				if ready || err != nil {
					test.Fatal("header restore ended before its exclusive output restore", err)
				}
			}
			if restore.header == nil || restore.header.phase < 3 {
				test.Fatal("header restore never reached its chunk consumer")
			}
			scope := restore.normalizer.mutation
			read := restore.store.Read
			restore.header.store.Read = func(ctx context.Context, hash ContentHash, size int) ([]byte, error) {
				if _, err := restore.normalizer.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
					test.Fatal("partial header was visible to normal decoder execution", err)
				}
				capture, err := scope.beginCapture()
				if capture != nil {
					capture.release()
				}
				if !errors.Is(err, ErrLegacyNormalizationBusy) {
					test.Fatal("partial header was visible to another capture", err)
				}
				return read(ctx, hash, size)
			}
			var failure error
			wanted := error(ErrIntegrity)
			switch ending {
			case "complete":
				legacyDecoderCheckpointDrain(test, restore, legacyDecoderCheckpointBudget)
				if _, err := restore.take(context.Background()); err != nil || scope.activity.Load() != 0 {
					test.Fatal("complete header retained its exclusive owner", err)
				}
				return
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				_, failure = restore.step(ctx)
				wanted = context.Canceled
			case "source-stop":
				scope.stop()
				_, failure = restore.step(context.Background())
				wanted = ErrConflict
			case "early-take":
				_, failure = restore.take(context.Background())
			}
			if !errors.Is(failure, wanted) || scope.activity.Load() != 0 || !restore.closed || restore.normalizer != nil {
				test.Fatal("header restore failure leaked its owner or a partial decoder", failure, scope.activity.Load())
			}
		})
	}
}

func TestLegacyNormalizationHeaderCheckpointBufferAdmission(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyHeaderCheckpointFixtures()[2])
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent(buffer.body)
	root := legacyDecoderCheckpointCapture(test, decoder, binding, store)
	restore, err := newLegacyNormalizationDecoderCheckpointRestore(context.Background(), root, store, buffer.input(), binding, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	for attempt := 0; attempt < legacyDecoderCheckpointSteps && (restore.header == nil || restore.header.phase != 4); attempt++ {
		ready, err := restore.step(context.Background())
		if ready || err != nil {
			test.Fatal("restore ended before buffer admission", err)
		}
	}
	if restore.header == nil || restore.header.phase != 4 || restore.header.header.header != nil {
		test.Fatal("fixture did not reach an unallocated header buffer")
	}
	work := restore.header
	retained, err := work.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	shared, err := restore.normalizer.RetainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	work.budget = retained + shared + int64(work.state.Header.Bytes)*3/2
	if ready, err := work.restoreBuffer(context.Background(), work.state.Header, false); ready || !errors.Is(err, ErrDocumentResourceLimit) || work.header.header != nil || work.cursor != nil {
		test.Fatal("buffer was allocated before full retained admission", ready, err)
	}
}

func TestLegacyNormalizationDecoderCheckpointV1HasNoHeaderExtension(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyDecoderCheckpointFixtures()[0])
	store, objects := legacyNativeCheckpointStoreFixture(test)
	root := legacyDecoderCheckpointCapture(test, decoder, HashContent(buffer.body), store)
	if bytes.Contains(objects[root.Hash], []byte(`"header"`)) {
		test.Fatal("v1 decoder control acquired a v2 header field")
	}
}
