package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

func TestLegacyNativeEventValuesBlockInlineCallerProofs(test *testing.T) {
	decoder, buffer := legacyDecoderCheckpointCut(test, legacyEventValuesCheckpointFixtures()[0])
	list, _ := legacyEventValuesCheckpointList(test, decoder)
	storage := legacyNativeAnyW78Store(test)
	current := storage.io()
	put := current.Put
	var capture *legacyNativeEventValuesCheckpoint
	var fieldPuts, smallBlockPuts, largeBlockPuts, totalPuts int
	current.Put = func(ctx context.Context, hash ContentHash, body []byte) error {
		totalPuts++
		if capture.current != nil {
			switch capture.current.phase {
			case 2:
				fieldPuts++
			case 5:
				if capture.block != nil {
					frame := capture.current
					record := frame.record.blocks.records.at(frame.blockIndex)
					capacity := int64(len(record.block.Type)+len(record.block.Text)) + (record.excess[0]+record.excess[1])/2
					if capacity <= 256 {
						smallBlockPuts++
					} else {
						largeBlockPuts++
					}
				}
			}
		}
		return put(ctx, hash, body)
	}
	binding := HashContent(buffer.body)
	var err error
	capture, err = newLegacyNativeEventValuesCheckpoint(context.Background(), list, binding, decoder.source.Bytes, current, legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer capture.close()
	legacyNativeAnyW78Drain(test, capture, storage, legacyDecoderCheckpointBudget)
	root, err := capture.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	var state legacyNativeEventValuesCheckpointRoot
	if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io(), root, &state); err != nil {
		test.Fatal(err)
	}
	reader, err := newLegacyNativeCheckpointListReader[legacyNativeEventValuesCheckpointEntry](state.Entries, storage.io())
	if err != nil {
		test.Fatal(err)
	}
	defer reader.close()
	inlineRows := 0
	for {
		entry, ok, err := reader.next(context.Background())
		if err != nil {
			test.Fatal(err)
		}
		if !ok {
			break
		}
		if entry.Kind == legacyNativeEventValuesEntryBlock && entry.Block.Inline != nil {
			inlineRows++
			body, err := json.Marshal(entry)
			if err != nil || len(body) > 1024 {
				test.Fatal("inline event block row escaped its encoded bound", len(body), err)
			}
		}
	}
	test.Logf("actual EventValues caller: inline_rows=%d field_puts=%d small_block_puts=%d large_block_puts=%d total_puts=%d", inlineRows, fieldPuts, smallBlockPuts, largeBlockPuts, totalPuts)
	if inlineRows == 0 || smallBlockPuts != 0 {
		test.Error("small block rows were not packed or still emit standalone text/metadata CAS objects", inlineRows, smallBlockPuts)
	}
	restore, err := newLegacyNativeEventValuesCheckpointRestore(context.Background(), root, binding, decoder.source.Bytes, storage.io(), legacyDecoderCheckpointBudget)
	if err != nil {
		test.Fatal(err)
	}
	defer restore.close()
	legacyNativeAnyW78Drain(test, restore, storage, legacyDecoderCheckpointBudget)
	result, err := restore.take(context.Background())
	if err != nil {
		test.Fatal(err)
	}
	legacyEventValuesCheckpointCompare(test, list.values, result.Values)
	legacyNormalizationCaptureRQ87Idle(test, decoder.mutation)
}

func TestLegacyNativeEventValuesBlockInlineExclusiveEncoding(test *testing.T) {
	ctx := context.Background()
	record := legacyNativeBlockRecordW81Record(ContentBlock{Type: "\xff", Text: "\x00\xfe\xff"}, [2]int{8, 32})
	saved, packed, err := legacyNativeBlockInlineRecord(ctx, record, true)
	if err != nil || !packed {
		test.Fatal(err)
	}
	entry := legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryBlock, Block: saved}
	if err := entry.validate(legacyNativeEventValuesEntryBlock, 128); err != nil {
		test.Fatal("bounded inline event block rejected", err)
	}
	restored, err := legacyNativeBlockInlineRestore(ctx, saved, 128, legacyDecoderCheckpointBudget)
	if err != nil || !reflect.DeepEqual(restored, record) {
		test.Fatal("inline event block changed opaque bytes or capacity ledger", err)
	}
	storage := legacyNativeBlockRecordW81Storage(test)
	if work, err := newLegacyNativeCheckpointBlockRecordRestore(ctx, saved, storage.io, 128, legacyDecoderCheckpointBudget); work != nil || !errors.Is(err, ErrIntegrity) {
		if work != nil {
			work.close()
		}
		test.Fatal("standalone restore admitted inline event encoding", err)
	}
	legacy := legacyNativeBlockRecordW81Capture(test, record, true, storage)
	if err := (legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryBlock, Block: legacy}).validate(legacyNativeEventValuesEntryBlock, 128); err != nil {
		test.Fatal("legacy event encoding rejected", err)
	}
	for _, name := range []string{"mixed", "nil-both", "nil-type", "nil-text", "capacity", "short", "schema", "kind"} {
		test.Run(name, func(test *testing.T) {
			changed := entry
			inline := *saved.Inline
			changed.Block.Inline = &inline
			switch name {
			case "mixed":
				changed.Block.Values = legacy.Values
			case "nil-both":
				inline.Values, inline.Capacities = [2][]byte{}, [2]int{}
			case "nil-type":
				inline.Values[0], inline.Capacities[0] = nil, 0
			case "nil-text":
				inline.Values[1], inline.Capacities[1] = nil, 0
			case "capacity":
				inline.Capacities = [2]int{128, 129}
			case "short":
				inline.Capacities[1] = 2
			case "schema":
				changed.Block.Schema = HashContent([]byte("different block schema"))
			case "kind":
				changed.Kind = legacyNativeEventValuesEntryBlocks
			}
			if err := changed.validate(legacyNativeEventValuesEntryBlock, 128); !errors.Is(err, ErrIntegrity) {
				test.Fatal("invalid or mixed inline event entry accepted", err)
			}
		})
	}
}
