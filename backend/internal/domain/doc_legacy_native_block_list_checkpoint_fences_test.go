package domain

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestLegacyNativeBlockListCheckpointW81Fences(test *testing.T) {
	storage := legacyNativeAnyW78Store(test)
	request, list, source := legacyNativeBlockListW81Capture(test, legacyNativeBlockListW81Fixtures()[0], storage)
	test.Run("capture-lifecycle", func(test *testing.T) {
		legacyNativeEnvelopeW80Lifecycle[legacyNativeCheckpointRef](test, storage, func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeCheckpointRef], error) {
			work, err := newLegacyNativeBlockListCheckpoint(ctx, list, request.Binding, source, current, budget)
			if err != nil {
				return nil, err
			}
			return work, nil
		})
	})
	test.Run("restore-lifecycle", func(test *testing.T) {
		legacyNativeEnvelopeW80Lifecycle[legacyNativeBlockListRestored](test, storage, func(ctx context.Context, current legacyNativeCheckpointIO, budget int64) (legacyNativeAnyCheckpointTask[legacyNativeBlockListRestored], error) {
			work, err := newLegacyNativeBlockListCheckpointRestore(ctx, request.Root, current, source, request.Binding, budget)
			if err != nil {
				return nil, err
			}
			return work, nil
		})
	})
	var original legacyNativeCheckpointBlockListState
	if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), request.Root, &original); err != nil {
		test.Fatal(err)
	}
	for _, name := range []string{"binding", "authority", "source-size", "part-size", "root-hash", "root-length", "cursor", "depth", "phase", "capacity", "rows", "alias-mask", "unknown-key", "root-body-hash"} {
		test.Run(name, func(test *testing.T) {
			ref, binding, current, io := request.Root, request.Binding, source, storage.io()
			state := original
			changed := false
			switch name {
			case "binding":
				binding = HashContent([]byte("different actual block-list binding"))
			case "authority":
				current.authority = 0
			case "source-size":
				current.Bytes--
			case "part-size":
				current.Parts = append([]ConversationManifestChunk(nil), source.Parts...)
				current.Parts[0].Bytes--
			case "root-hash":
				ref.Hash = "bad"
			case "root-length":
				ref.Bytes = 16385
			case "cursor":
				state.Position = state.End + 1
				changed = true
			case "depth":
				state.ParentDepth = 10001
				changed = true
			case "phase":
				state.Phase = 255
				changed = true
			case "capacity":
				state.Decoded.Capacity = MaxFinalizedDocBytes + 1
				changed = true
			case "rows":
				state.Decoded.Records.Count++
				changed = true
			case "alias-mask":
				state.BlockAliases = 4
				changed = true
			case "unknown-key":
				body, err := json.Marshal(state)
				if err != nil {
					test.Fatal(err)
				}
				body = append(body[:len(body)-1], []byte(`,"unknown":0}`)...)
				ref, err = legacyNativeCheckpointWrite(context.Background(), storage.io(), body)
				if err != nil {
					test.Fatal(err)
				}
			case "root-body-hash":
				read := io.Read
				io.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
					body, err := read(ctx, hash, length)
					if err == nil && hash == request.Root.Hash {
						body[0] ^= 1
					}
					return body, err
				}
			}
			if changed {
				var err error
				ref, err = legacyNativeCheckpointWriteJSON(context.Background(), storage.io(), state)
				if err != nil {
					test.Fatal(err)
				}
			}
			work, err := newLegacyNativeBlockListCheckpointRestore(context.Background(), ref, io, current, binding, legacyNativeBlockListW81Budget)
			if err != nil {
				if !errors.Is(err, ErrIntegrity) {
					test.Fatal(err)
				}
				return
			}
			defer work.close()
			for attempt := 0; attempt < legacyNativeBlockListW81CheckpointSteps; attempt++ {
				ready, err := work.step(context.Background())
				if err != nil {
					if !errors.Is(err, ErrIntegrity) {
						test.Fatal(err)
					}
					if again, err := work.step(context.Background()); again || !errors.Is(err, ErrIntegrity) {
						test.Fatal("restore error was not terminal", err)
					}
					if retained, err := work.retainedBytes(); err != nil || retained != 0 {
						test.Fatal("failed root retained owned storage", retained, err)
					}
					return
				}
				if ready {
					test.Fatal("invalid actual checkpoint became ready")
				}
			}
			test.Fatal("invalid actual checkpoint exhausted finite rejection schedule")
		})
	}
}
