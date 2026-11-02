package domain

import (
	"context"
	"errors"
	"testing"
)

func TestLegacyCanonicalMapContinuationCheckpointCurrentFences(test *testing.T) {
	for _, name := range []string{"cancel", "capture", "source", "frame", "resume", "owner", "budget", "borrowed-payload", "callback-storage", "pending-value", "encode-fence", "put-fence"} {
		test.Run(name, func(test *testing.T) {
			stage, grant, fields := legacyCanonicalMapContinuationCheckpointFixture(test, 2, 0, true)
			codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
			store, _ := legacyNativeCheckpointStoreFixture(test)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			work, err := newLegacyCanonicalMapContinuationCheckpoint(ctx, stage, 0, grant, codec, HashContent([]byte("fences")), store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			want := error(ErrConflict)
			switch name {
			case "cancel":
				cancel()
				want = context.Canceled
			case "capture":
				stage.mutation.stop()
			case "source":
				work.frame.value = map[string]any{"different": 1, "source": 2}
			case "frame":
				work.frame.position++
			case "resume":
				work.frame.mapResume.position++
			case "owner":
				stage.pending[0].Encoding = &LegacyCanonicalEncoding{}
			case "budget":
				work.budget, want = 1, ErrDocumentResourceLimit
			case "borrowed-payload":
				codec.ownerBytes, want = codec.budget, ErrDocumentResourceLimit
			case "callback-storage":
				codec.callbackBytes, want = codec.budget, ErrDocumentResourceLimit
			case "pending-value":
				codec.encodeError, want = ErrDocRangeIndexPending, ErrDocRangeIndexPending
			case "encode-fence":
				codec.onEncode = func() { codec.checkError = ErrConflict }
			case "put-fence":
				put := work.store.Put
				work.store.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
					if err := put(ctx, hash, body); err != nil {
						return err
					}
					codec.checkError = ErrConflict
					return nil
				}
			}
			for steps := 0; steps < 128; steps++ {
				ready, failure := work.step(ctx)
				if failure != nil {
					if !errors.Is(failure, want) || ready || !work.closed {
						test.Fatal("writer did not fail closed at current fence", failure, want)
					}
					if ref, err := work.take(context.Background()); ref.valid() || !errors.Is(err, want) {
						test.Fatal("failed suffix acquired a durable root", ref, err)
					}
					return
				}
				if ready {
					test.Fatal("invalid capture became ready")
				}
			}
			test.Fatal("writer did not reach its expected fence")
		})
	}
}

func TestLegacyCanonicalMapContinuationRestoreCurrentFences(test *testing.T) {
	for _, name := range []string{"binding", "position", "count", "capture", "cancel-read", "read-fence", "decode-fence", "pending-value", "callback-growth", "page-allocation", "ready-budget", "unknown-key", "key-descriptor", "key-length", "pending-key", "key-pre-cancel", "cancel-key", "key-fence", "key-capture", "key-owner-budget"} {
		test.Run(name, func(test *testing.T) {
			stage, grant, fields := legacyCanonicalMapContinuationCheckpointFixture(test, 2, 0, true)
			codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
			store, _ := legacyNativeCheckpointStoreFixture(test)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			binding := HashContent([]byte("restore-fences"))
			writer, err := newLegacyCanonicalMapContinuationCheckpoint(ctx, stage, 0, grant, codec, binding, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			ref := legacyCanonicalMapContinuationCheckpointDrain(test, writer)
			restore, err := newLegacyCanonicalMapContinuationRestore(ctx, ref, binding, 2, 0, grant, codec, store, legacyDecoderCheckpointBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			want := error(ErrConflict)
			switch name {
			case "binding":
				restore.binding, want = HashContent([]byte("foreign")), ErrIntegrity
			case "position":
				restore.position, want = 1, ErrIntegrity
			case "count":
				restore.count, want = 3, ErrIntegrity
			case "capture":
				stage.mutation.stop()
			case "cancel-read", "read-fence":
				read := restore.store.Read
				restore.store.Read = func(current context.Context, hash ContentHash, length int) ([]byte, error) {
					body, err := read(current, hash, length)
					if name == "cancel-read" {
						cancel()
					} else {
						codec.checkError = ErrConflict
					}
					return body, err
				}
				if name == "cancel-read" {
					want = context.Canceled
				}
			case "decode-fence":
				codec.onDecode = func() { codec.checkError = ErrConflict }
			case "pending-value":
				codec.decodeError, want = ErrDocRangeIndexPending, ErrDocRangeIndexPending
			case "callback-growth":
				codec.onDecode = func() { codec.callbackBytes = codec.budget }
				want = ErrDocumentResourceLimit
			case "page-allocation", "ready-budget":
				want = ErrDocumentResourceLimit
			case "unknown-key":
				clear(codec.keys)
				want = ErrIntegrity
			case "key-descriptor":
				keys := make(map[legacyNativeCheckpointText]string, len(codec.keys))
				for text, key := range codec.keys {
					text.Tree.Height++
					keys[text] = key
				}
				codec.keys, want = keys, ErrIntegrity
			case "key-length":
				for text, key := range codec.keys {
					codec.keys[text] = key[:len(key)-1]
				}
				want = ErrIntegrity
			case "pending-key":
				codec.keyError, want = ErrDocRangeIndexPending, ErrDocRangeIndexPending
			case "key-pre-cancel":
				want = context.Canceled
			case "cancel-key":
				codec.onDecodeKey, want = cancel, context.Canceled
			case "key-fence":
				codec.onDecodeKey = func() { codec.checkError = ErrConflict }
			case "key-capture":
				codec.onDecodeKey = stage.mutation.stop
			case "key-owner-budget":
				codec.onDecodeKey = func() { codec.ownerBytes = codec.budget }
				want = ErrDocumentResourceLimit
			}
			for steps := 0; steps < 128; steps++ {
				if name == "page-allocation" && restore.phase == 4 {
					retained, err := restore.retainedBytes()
					if err != nil {
						test.Fatal(err)
					}
					restore.budget = retained + legacyNativeCheckpointStringValuesWorkspace().bytes
				}
				if name == "key-pre-cancel" && restore.phase == 2 {
					cancel()
				}
				ready, failure := restore.step(ctx)
				if ready && name == "ready-budget" {
					codec.ownerBytes = codec.budget
					result, err := restore.take(ctx)
					if result.continuation != nil || !errors.Is(err, want) {
						test.Fatal("ready result ignored current owner budget", err)
					}
					return
				}
				if failure != nil {
					if !errors.Is(failure, want) || ready || !restore.closed {
						test.Fatal("restore did not fail closed at current fence", failure, want)
					}
					if name == "key-pre-cancel" && codec.decodedKeys != 0 ||
						(name == "unknown-key" || name == "key-descriptor" || name == "key-length" || name == "pending-key" || name == "cancel-key" || name == "key-fence" || name == "key-capture" || name == "key-owner-budget") && codec.decoded != 0 {
						test.Fatal("failed key resolution advanced a callback")
					}
					if _, err := restore.step(context.Background()); !errors.Is(err, want) {
						test.Fatal("failed restore did not retain its failure", err)
					}
					if result, err := restore.take(context.Background()); result.continuation != nil || !errors.Is(err, want) {
						test.Fatal("failed restore returned values", err)
					}
					return
				}
				if ready {
					test.Fatal("invalid restore became ready")
				}
			}
			test.Fatal("restore did not reach its expected fence")
		})
	}
}

func TestLegacyCanonicalMapContinuationCheckpointRejectsMalformedEntries(test *testing.T) {
	stage, grant, fields := legacyCanonicalMapContinuationCheckpointFixture(test, 1, 0, true)
	codec := legacyCanonicalMapContinuationCodecFixture(test, fields)
	store, _ := legacyNativeCheckpointStoreFixture(test)
	binding := HashContent([]byte("malformed-suffix"))
	for _, entry := range []legacyCanonicalMapContinuationEntry{
		{},
		{Value: codec.refs[0], Key: legacyNativeCheckpointText{Bytes: 1}},
		{Value: codec.refs[0], Key: legacyNativeCheckpointText{Bytes: -1}},
		{Value: codec.refs[0], Key: legacyNativeCheckpointText{Bytes: 1, Tree: legacyNativeCheckpointTree{Count: 1}}},
	} {
		var entries legacyNativeCheckpointListWriter[legacyCanonicalMapContinuationEntry]
		if err := entries.append(context.Background(), store, entry); err != nil {
			test.Fatal(err)
		}
		tree, err := entries.finish(context.Background(), store)
		if err != nil {
			test.Fatal(err)
		}
		ref, err := legacyNativeCheckpointWriteJSON(context.Background(), store, legacyCanonicalMapContinuationState{
			Kind: "map-continuation", Version: legacyNativeCheckpointVersion, Binding: binding, Count: 1, Entries: tree})
		if err != nil {
			test.Fatal(err)
		}
		restore, err := newLegacyCanonicalMapContinuationRestore(context.Background(), ref, binding, 1, 0, grant, codec, store, legacyDecoderCheckpointBudget)
		if err != nil {
			test.Fatal(err)
		}
		if ready, err := restore.step(context.Background()); ready || err != nil {
			test.Fatal("fixture root did not load", err)
		}
		if ready, err := restore.step(context.Background()); ready || !errors.Is(err, ErrIntegrity) || codec.decoded != 0 || codec.decodedKeys != 0 {
			test.Fatal("malformed key/value descriptor reached resolver", err)
		}
	}
	if stage.pending[0].Encoding.preparation.frames.at(0).position != 0 {
		test.Fatal("malformed restore advanced preparation")
	}
}
