package domain

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"unsafe"
)

func TestLegacyNativeObjectCheckpointDecoderEnvelopeOwner(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, original, scanner, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	store := storage.io()
	read := store.Read
	store.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
		if _, err := decoder.Step(ctx); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("decoder mutated while checkpoint IO was restoring its owner", err)
		}
		capture, err := decoder.mutation.beginCapture()
		if capture != nil {
			capture.release()
		}
		if !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("capture observed partially restored owner during checkpoint IO", err)
		}
		return read(ctx, hash, length)
	}
	owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
	work, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, store, decoder.source, request.Binding, legacyNativeEnvelopeW80Budget, owner)
	if err != nil || !reflect.ValueOf(decoder.envelope).IsZero() {
		test.Fatal("owner changed before root validation", err)
	}
	defer work.close()
	legacyNativeAnyW78Drain(test, work, storage, legacyNativeEnvelopeW80Budget)
	restored, err := work.take(context.Background())
	if err != nil || restored.Object.destination.Interface() != &decoder.envelope || restored.Object.mutation != decoder.mutation {
		test.Fatal("envelope restore detached from decoder ownership", err)
	}
	decoder.envelopeNative = restored.Object
	decoder.frame, decoder.source = restored.Scanner, restored.Source
	if !reflect.DeepEqual(legacyNativeEnvelopeW80StateOf(test, original, &scanner), legacyNativeEnvelopeW80StateOf(test, decoder.envelopeNative, &decoder.frame)) {
		test.Fatal("owner binding changed checkpoint semantics")
	}
	capture, err := decoder.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	if _, err := decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("decoder escaped its checkpoint barrier", err)
	}
	capture.release()
	if _, err := legacyNativeEnvelopeW80Continue(test, original, &scanner, restored.Source); err != nil {
		test.Fatal(err)
	}
	if _, err := legacyNativeEnvelopeW80Continue(test, decoder.envelopeNative, &decoder.frame, decoder.source); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(original.destination.Interface().(*CIREnvelope), &decoder.envelope) {
		test.Fatal("continued parse updated a detached envelope instead of decoder state")
	}
}

func TestLegacyNativeObjectCheckpointDecoderEventOwner(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, original := legacyNativeEventW82Capture(test, legacyNativeEventW82Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	event := &legacyNormalizationNativeEvent{}
	owner := legacyNativeObjectCheckpointOwner{decoder: decoder, event: event}
	work, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEventModel, storage.io(), decoder.source, request.Binding, legacyNativeEventW82Budget, owner)
	if err != nil || !reflect.ValueOf(event.decoded).IsZero() {
		test.Fatal("event changed before root validation", err)
	}
	defer work.close()
	legacyNativeAnyW78Drain(test, work, storage, legacyNativeEventW82Budget)
	restored, err := work.take(context.Background())
	if err != nil || event == nil || restored.Object.destination.Interface() != &event.decoded || restored.Object.mutation != decoder.mutation {
		test.Fatal("event restore detached from decoder ownership", err)
	}
	event.object = restored.Object
	decoder.native, decoder.nativeCache = event, event
	decoder.frame, decoder.source = restored.Scanner, restored.Source
	list := legacyNativeEventW82List(event.object)
	if list == nil || list.object == nil || list.mutation != decoder.mutation || list.object.mutation != decoder.mutation {
		test.Fatal("cached child escaped the decoder mutation scope")
	}
	if !reflect.DeepEqual(legacyNativeEventW82StateOf(test, original), legacyNativeEventW82StateOf(test, &restored)) {
		test.Fatal("owner binding changed event checkpoint semantics")
	}
	capture, err := decoder.mutation.beginCapture()
	if err != nil {
		test.Fatal(err)
	}
	if err := event.releaseValues(); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("event escaped decoder checkpoint barrier", err)
	}
	if err := list.object.start(0, 0); !errors.Is(err, ErrLegacyNormalizationBusy) {
		test.Fatal("nested parser escaped decoder checkpoint barrier", err)
	}
	capture.release()
	if _, err := legacyNativeEventW82Continue(test, original); err != nil {
		test.Fatal(err)
	}
	if _, err := legacyNativeEventW82Continue(test, &restored); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(original.Object.destination.Interface().(*wire), &event.decoded) {
		test.Fatal("continued parse updated detached event state")
	}
	second, err := newLegacyNativeObjectCheckpoint(context.Background(), event.object, &restored.Scanner, request.Binding, restored.Source, storage.io(), legacyNativeEventW82Budget)
	if err != nil {
		test.Fatal("continued owner cannot produce its next checkpoint", err)
	}
	defer second.close()
	legacyNativeAnyW78Drain(test, second, storage, legacyNativeEventW82Budget)
	if root, err := second.take(context.Background()); err != nil || !root.valid() {
		test.Fatal("second owner checkpoint is incomplete", root, err)
	}
}

func TestLegacyNativeObjectCheckpointOwnerAdmission(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	for _, scenario := range []string{"binding", "budget", "source", "cas", "cancel", "missing-decoder", "missing-scope", "closed", "busy"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			decoder, err := NewLegacyNormalizationDecoder(ctx, source)
			if err != nil {
				test.Fatal(err)
			}
			scope := decoder.mutation
			owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
			binding, budget, current, store := request.Binding, legacyNativeEnvelopeW80Budget, decoder.source, storage.io()
			wanted := ErrIntegrity
			switch scenario {
			case "binding":
				binding = HashContent([]byte("different owner source"))
			case "budget":
				budget, wanted = 1, ErrDocumentResourceLimit
			case "source":
				current.Bytes++
			case "cas":
				store.Read = func(context.Context, ContentHash, int) ([]byte, error) { return nil, ErrIntegrity }
			case "cancel":
				cancel()
				wanted = context.Canceled
			case "missing-decoder":
				owner.decoder, owner.event = nil, &legacyNormalizationNativeEvent{}
			case "missing-scope":
				decoder.mutation = nil
			case "closed":
				scope.stop()
				wanted = ErrConflict
			case "busy":
				capture, err := scope.beginCapture()
				if err != nil {
					test.Fatal(err)
				}
				defer capture.release()
				wanted = ErrLegacyNormalizationBusy
			}
			work, failure := newLegacyNativeObjectCheckpointRestoreForOwner(ctx, request.Root, legacyNativeCheckpointEnvelopeModel, store, current, binding, budget, owner)
			if work != nil {
				defer work.close()
				for attempt := 0; failure == nil && attempt < legacyNativeEnvelopeW80Steps; attempt++ {
					var done bool
					done, failure = work.step(ctx)
					if done {
						test.Fatal("invalid restore completed")
					}
				}
			}
			if !errors.Is(failure, wanted) || !reflect.ValueOf(decoder.envelope).IsZero() {
				test.Fatal("owner changed before failed validation/admission", failure, wanted)
			}
		})
	}
}

func TestLegacyNativeObjectCheckpointOwnerRejectsLiveDestination(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	decoder.envelope.Cwd = "existing live state"
	owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
	work, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, legacyNativeEnvelopeW80Budget, owner)
	if err != nil {
		test.Fatal(err)
	}
	_, err = work.step(context.Background())
	work.close()
	if !errors.Is(err, ErrIntegrity) || decoder.envelope.Cwd != "existing live state" || decoder.mutation.activity.Load() != 0 {
		test.Fatal("restore accepted or modified a non-fresh destination, or retained its grant", err, decoder.envelope.Cwd)
	}
}

func TestLegacyNativeObjectCheckpointOwnerGrantLifecycle(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	for _, ending := range []string{"close", "cancel", "stop", "take-before-ready"} {
		test.Run(ending, func(test *testing.T) {
			decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
			if err != nil {
				test.Fatal(err)
			}
			owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
			work, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, legacyNativeEnvelopeW80Budget, owner)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if _, err := decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Fatal("decoder progressed between restore steps", err)
			}
			duplicate, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, legacyNativeEnvelopeW80Budget, owner)
			if duplicate != nil || !errors.Is(err, ErrLegacyNormalizationBusy) {
				test.Fatal("two restores acquired the same decoder owner", err)
			}
			switch ending {
			case "close":
				work.close()
			case "cancel":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := work.step(ctx); !errors.Is(err, context.Canceled) {
					test.Fatal(err)
				}
			case "stop":
				decoder.mutation.stop()
				if _, err := work.step(context.Background()); !errors.Is(err, ErrConflict) {
					test.Fatal(err)
				}
			case "take-before-ready":
				if _, err := work.take(context.Background()); !errors.Is(err, ErrIntegrity) {
					test.Fatal(err)
				}
			}
			if decoder.mutation.activity.Load() != 0 {
				test.Fatal("restore retained its owner grant after termination")
			}
			work.close()
			if decoder.mutation.activity.Load() != 0 {
				test.Fatal("duplicate close released another owner's activity")
			}
		})
	}
}

func TestLegacyNativeObjectCheckpointOwnerBudget(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	var minimum docRangeHeaderHeapEstimate
	minimum.allocation(1, int64(unsafe.Sizeof(legacyNativeObjectCheckpointRestore{})))
	minimum.include(legacyNativeAnyCheckpointSourceHeap(decoder.source))
	minimum.allocation(legacyNativeAnyCheckpointScratch, 1)
	if minimum.err != nil {
		test.Fatal(minimum.err)
	}
	standalone, err := newLegacyNativeObjectCheckpointRestore(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, minimum.bytes)
	if err != nil {
		test.Fatal("standalone admission changed", err)
	}
	standalone.close()
	owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
	work, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, minimum.bytes, owner)
	if work != nil || !errors.Is(err, ErrDocumentResourceLimit) || decoder.mutation.activity.Load() != 0 {
		test.Fatal("decoder ownership was omitted from admission or retained a grant", err)
	}
	envelopeBytes, err := owner.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	owner.event = &legacyNormalizationNativeEvent{}
	eventBytes, err := owner.retainedBytes()
	var eventHeap docRangeHeaderHeapEstimate
	eventHeap.allocation(1, int64(unsafe.Sizeof(*owner.event)))
	if err != nil || eventHeap.err != nil || eventBytes-envelopeBytes != eventHeap.bytes {
		test.Fatal("native event owner body was omitted from admission", envelopeBytes, eventBytes, err)
	}
}

func TestLegacyNativeObjectCheckpointOwnerEmbeddedModelBudget(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
	work, err := newLegacyNativeObjectCheckpointRestoreForOwner(context.Background(), request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, legacyNativeEnvelopeW80Budget, owner)
	if err != nil {
		test.Fatal(err)
	}
	defer work.close()
	if _, err := work.step(context.Background()); err != nil || work.native == nil || work.native.destination.Interface() != &decoder.envelope {
		test.Fatal("fixture did not bind its embedded model", err)
	}
	owned, err := work.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	ownerBytes, err := owner.retainedBytes()
	if err != nil {
		test.Fatal(err)
	}
	standalone := *work
	standalone.owner = legacyNativeObjectCheckpointOwner{}
	independent, err := standalone.retainedBytes()
	var model docRangeHeaderHeapEstimate
	model.allocation(1, int64(work.native.metadata.model.Size()))
	if err != nil || model.err != nil || owned-ownerBytes != independent-model.bytes {
		test.Fatal("embedded destination was counted as a second allocation", owned, ownerBytes, independent, model.bytes, err)
	}
}

type legacyCheckpointOwnerConstructionContext struct {
	context.Context
	inspect func()
}

func (current *legacyCheckpointOwnerConstructionContext) Err() error {
	current.inspect()
	return current.Context.Err()
}

func TestLegacyNativeObjectCheckpointOwnerConstructionGrant(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, _, _, _ := legacyNativeEnvelopeW80Capture(test, legacyNativeEnvelopeW80Fixtures()[1], storage)
	source := legacyNativeAnyW78Source(test, request, &legacyNativeAnyW78Reads{})
	decoder, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil {
		test.Fatal(err)
	}
	inspections := 0
	ctx := &legacyCheckpointOwnerConstructionContext{Context: context.Background(), inspect: func() {
		inspections++
		if inspections == 1 {
			return
		}
		if _, err := decoder.Step(context.Background()); !errors.Is(err, ErrLegacyNormalizationBusy) {
			test.Fatal("constructor read mutable owner state before acquiring its grant", err)
		}
	}}
	owner := legacyNativeObjectCheckpointOwner{decoder: decoder}
	work, err := newLegacyNativeObjectCheckpointRestoreForOwner(ctx, request.Root, legacyNativeCheckpointEnvelopeModel, storage.io(), decoder.source, request.Binding, legacyNativeEnvelopeW80Budget, owner)
	if err != nil {
		test.Fatal(err)
	}
	work.close()
	if inspections < 2 || decoder.mutation.activity.Load() != 0 || !reflect.ValueOf(decoder.envelope).IsZero() {
		test.Fatal("constructor did not exercise and release the owner guard", inspections)
	}
}
