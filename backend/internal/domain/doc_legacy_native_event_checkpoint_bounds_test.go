package domain

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func legacyNativeEventW82ActiveBlockBounds(test *testing.T) (legacyNativeAnyW78Request, legacyNativeObjectRestored, *legacyNativeAnyW78CAS) {
	test.Helper()
	raw := `{"kind":"message","seq":0,"blocks":[{"type":"text","text":"` + strings.Repeat("a", 2*LegacyNormalizationPartBytes) + `"}],"id":"later"}`
	request, native, scanner, source := legacyNativeEventW82FencePrepare(test, raw, nil, func(native *legacyNativeObject) bool {
		if !native.blockActive || len(native.blocks) <= native.field {
			return false
		}
		list := native.blocks[native.field]
		return list != nil && list.object != nil && list.object.scalar != nil && list.object.scalar.text != nil && list.object.scalar.text.Len() > 0 && list.object.scalar.position < list.object.scalar.end
	})
	storage := legacyNativeAnyW78Store(test)
	storage.preload(test, request)
	request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
	return request, legacyNativeEventW82FenceRestore(test, request, storage), storage
}

func TestLegacyNativeEventCheckpointW82SharedFrameAndBounds(test *testing.T) {
	test.Run("active-range-corruption", func(test *testing.T) {
		request, restored, storage := legacyNativeEventW82ActiveBlockBounds(test)
		var state legacyNativeObjectCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil {
			test.Fatal(err)
		}
		list := legacyNativeEventW82List(restored.Object)
		for _, from := range []int{state.End, list.position + 1, state.ValueStart + 3} {
			changed := state
			changed.Position, changed.ValueStart = from, from
			ref, err := legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), changed)
			if err != nil {
				test.Fatal(err)
			}
			if err := legacyNativeEventW82FenceReject(test, ref, restored.Source, request.Binding, legacyNativeCheckpointEventModel, storage.io(), legacyNativeEventW82FenceBudget); !errors.Is(err, ErrIntegrity) {
				test.Fatal("active cached parser disagreed with its root range", from, err)
			}
		}
	})
	test.Run("nil-header-without-parser", func(test *testing.T) {
		request, source := legacyNativeAnyW78FullSource(test, `{}`)
		storage := legacyNativeAnyW78Store(test)
		storage.preload(test, request)
		for _, paged := range []bool{false, true} {
			for _, initialized := range []bool{false, true} {
				state := legacyNativeObjectCheckpointBlockField{Destination: legacyNativeCheckpointBlockArray{Nil: true}, Paged: paged, Initialized: initialized}
				work, err := newLegacyNativeObjectBlockCheckpointRestore(context.Background(), state, request.Binding, source, storage.io(), legacyNativeEventW82FenceBudget)
				if err != nil {
					test.Fatal("valid nil destination rejected", paged, initialized, err)
				}
				defer work.close()
				legacyNativeAnyW78Drain(test, work, storage, legacyNativeEventW82FenceBudget)
				result, err := work.take(context.Background())
				var expected docRangeHeaderHeapEstimate
				if initialized {
					expected.allocation(1, int64(unsafe.Sizeof([]ContentBlock(nil))))
				}
				if err != nil || result.Values != nil || result.Parser != nil || result.Excess != 0 || result.Heap != expected {
					test.Fatal("nil field acquired a parser or lost exact header ownership", paged, initialized, result, err)
				}
				legacyNativeAnyW78Released(test, work)
			}
		}
	})
	test.Run("binding-pays-quantum-first", func(test *testing.T) {
		_, restored, _ := legacyNativeEventW82ActiveBlockBounds(test)
		native := restored.Object
		if native.checkpointScanner == nil {
			test.Fatal("restored root lost its owned scanner")
		}
		frame := restored.Scanner
		frame.stack[len(frame.stack)-1] ^= 1
		width := int(unsafe.Sizeof(frame)) + len(native.blocks)*int(unsafe.Sizeof((*legacyNativeBlockList)(nil)))
		input := &legacyNormalizationInput{workBytes: ConversationManifestChunkBytes - width + 1}
		if ready, err := native.bindCheckpointScanner(context.Background(), input, &frame); ready || err != nil || native.checkpointScanner == nil {
			test.Fatal("scanner comparison ran before quantum admission", ready, err)
		}
		input.workBytes = 0
		if ready, err := native.bindCheckpointScanner(context.Background(), input, &frame); ready || !errors.Is(err, ErrIntegrity) || native.checkpointScanner == nil {
			test.Fatal("different frame bound on fresh allowance", ready, err)
		}
		frame = restored.Scanner
		if ready, err := native.bindCheckpointScanner(context.Background(), input, &frame); !ready || err != nil || native.checkpointScanner != nil || input.workBytes != width {
			test.Fatal("valid frame failed charged binding", ready, err, input.workBytes)
		}
	})
	test.Run("boxed-string-heap", func(test *testing.T) {
		request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"output":"kept","is_error":false}`, nil, nil)
		storage := legacyNativeAnyW78Store(test)
		storage.preload(test, request)
		request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
		restored := legacyNativeEventW82FenceRestore(test, request, storage)
		index := restored.Object.metadata.match("output")
		value := restored.Object.destination.Elem().Field(restored.Object.metadata.fields[index].index)
		if value.Kind() != reflect.Interface || value.IsNil() {
			test.Fatal("Output string missing")
		}
		heap, err := legacyNativeValueHeap(context.Background(), value)
		if err != nil {
			test.Fatal(err)
		}
		heap.include(docRangeHeaderHeapEstimate{bytes: restored.Object.backingExcess[index]})
		if restored.Object.heaps[index] != heap {
			test.Fatal("boxed string ownership omitted", restored.Object.heaps[index], heap)
		}
	})
	test.Run("inactive-cache-after-restart", func(test *testing.T) {
		request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{"blocks":[{"text":"kept"}],"id":"done"}`, nil, nil)
		storage := legacyNativeAnyW78Store(test)
		storage.preload(test, request)
		request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
		restored := legacyNativeEventW82FenceRestore(test, request, storage)
		restored.Object.start(0, restored.Source.Bytes)
		restored.Scanner = legacyJSONValueScanner{}
		request = legacyNativeEventW82FenceCapture(test, restored.Object, &restored.Scanner, request, restored.Source, storage)
		recaptured := legacyNativeEventW82FenceRestore(test, request, storage)
		if list := legacyNativeEventW82List(recaptured.Object); list == nil || list.phase != legacyNativeBlocksDone || recaptured.Object.phase != legacyNativeObjectStart {
			test.Fatal("restart lost the inactive reusable cache")
		}
		if legacyNativeEventW82List(recaptured.Object).frame != nil || recaptured.Object.checkpointScanner != nil {
			test.Fatal("inactive nil frame was replaced by fabricated shared state")
		}
		if _, err := legacyNativeEventW82Continue(test, &recaptured); err != nil {
			test.Fatal("restarted Event failed", err)
		}
		if value := recaptured.Object.destination.Interface().(*wire); len(value.Blocks) != 1 || value.Blocks[0].Text != "kept" {
			test.Fatal("restarted cached Event did not finish", value)
		}
	})
	test.Run("uninitialized-independent-destination", func(test *testing.T) {
		request, native, scanner, source := legacyNativeEventW82FencePrepare(test, `{}`, &wire{Blocks: []ContentBlock{{Text: "owned"}}}, nil)
		storage := legacyNativeAnyW78Store(test)
		storage.preload(test, request)
		request = legacyNativeEventW82FenceCapture(test, native, scanner, request, source, storage)
		var state legacyNativeObjectCheckpointControl
		if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &state); err != nil {
			test.Fatal(err)
		}
		row := *state.Fields[legacyEventNativeFields.match("blocks")].Block
		row.Initialized = false
		reads := 0
		current := storage.io()
		current.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
			reads++
			return storage.io().Read(ctx, hash, length)
		}
		work, err := newLegacyNativeObjectBlockCheckpointRestore(context.Background(), row, request.Binding, source, current, legacyNativeEventW82FenceBudget)
		if work != nil {
			work.close()
		}
		if !errors.Is(err, ErrIntegrity) || reads != 0 {
			test.Fatal("uninitialized independent storage passed pre-read validation", err, reads)
		}
	})
	test.Run("block-model-binding", func(test *testing.T) {
		schema, err := legacyNativeCheckpointBlockSchema()
		if err != nil || !legacyNativeCheckpointBlockListHashValid(schema) {
			test.Fatal("compiled block schema unavailable", schema, err)
		}
		if allocations := testing.AllocsPerRun(20, func() {
			if current, failure := legacyNativeCheckpointBlockSchema(); failure != nil || current != schema {
				test.Fatal("stable block binding changed", current, failure)
			}
		}); allocations != 0 {
			test.Fatal("block binding allocates on every call", allocations)
		}
		original := legacyContentBlockNativeFields.fields[0]
		defer func() { legacyContentBlockNativeFields.fields[0] = original }()
		legacyContentBlockNativeFields.fields[0].name = "changed"
		if _, err := legacyNativeCheckpointBlockSchema(); !errors.Is(err, ErrIntegrity) {
			test.Fatal("changed block metadata accepted", err)
		}
	})
	test.Run("all-model-bindings-without-rehash", func(test *testing.T) {
		for model := uint8(1); model <= legacyNativeCheckpointProviderModel; model++ {
			metadata := legacyNativeCheckpointModelMetadata(model)
			expected, err := legacyNativeCheckpointModelSchemaValue(metadata, model)
			if err != nil {
				test.Fatal("reference model schema unavailable", model, err)
			}
			if allocations := testing.AllocsPerRun(20, func() {
				if actual, failure := legacyNativeCheckpointModelSchema(metadata); failure != nil || actual != expected {
					test.Fatal("compiled model schema differs from original format", model, actual, expected, failure)
				}
			}); allocations != 0 {
				test.Fatal("model schema remarshal/rehash remained in checkpoint loop", model, allocations)
			}
		}
	})
	test.Run("block-schema-before-body", func(test *testing.T) {
		reads := 0
		storage := legacyNativeCheckpointIO{Read: func(context.Context, ContentHash, int) ([]byte, error) {
			reads++
			return nil, errors.New("unexpected dependent read")
		}}
		work, err := newLegacyNativeCheckpointBlockRecordRestore(context.Background(), legacyNativeCheckpointBlockRecord{Schema: HashContent([]byte("foreign compiled block model"))}, storage, 1, legacyNativeEventW82FenceBudget)
		if work != nil {
			work.close()
		}
		if !errors.Is(err, ErrIntegrity) || reads != 0 {
			test.Fatal("foreign block schema reached payloads", err, reads)
		}
	})
}
