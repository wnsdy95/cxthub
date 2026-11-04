package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyNativeBlockListInlineRecordOracle(test *testing.T) {
	for _, fixture := range []struct {
		name       string
		block      ContentBlock
		capacities [2]int
	}{
		{name: "empty"},
		{name: "exact-unrounded", block: ContentBlock{Type: "t", Text: "abc"}, capacities: [2]int{1, 3}},
		{name: "retained-empty", capacities: [2]int{32, 224}},
		{name: "capacity-boundary", block: ContentBlock{Type: "text", Text: strings.Repeat("x", 127)}, capacities: [2]int{16, 240}},
		{name: "byte-boundary", block: ContentBlock{Type: strings.Repeat("\xff", 128), Text: strings.Repeat("\x00", 128)}, capacities: [2]int{128, 128}},
		{name: "opaque-bytes", block: ContentBlock{Type: "\xff\x00", Text: "\xfe\xff\x00\ud55c\U0001f642"}, capacities: [2]int{8, 32}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			ctx := context.Background()
			record := legacyNativeBlockRecordW81Record(fixture.block, fixture.capacities)
			saved, packed, err := legacyNativeBlockInlineRecord(ctx, record, true)
			if err != nil || !packed || saved.Inline == nil || saved.Values != (legacyNativeCheckpointStrings{}) {
				test.Fatal("small record was not packed", packed, err)
			}
			body, err := json.Marshal(saved)
			if err != nil || len(body) > 1024 {
				test.Fatal("inline row escaped encoded bound", len(body), err)
			}
			storage := legacyNativeBlockRecordW81Storage(test)
			legacy := legacyNativeBlockRecordW81Capture(test, record, true, storage)
			legacyBody, err := json.Marshal(legacy)
			if err != nil || legacy.Inline != nil || bytes.Contains(legacyBody, []byte(`"inline"`)) {
				test.Fatal("standalone record wire format changed", err)
			}
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(ctx, legacy, storage.io, 0, legacyNativeBlockRecordW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			expected := legacyNativeBlockRecordW81Finish[legacyNativeBlockValue](test, restore, storage, legacyNativeBlockRecordW81Budget)
			actual, err := legacyNativeBlockInlineRestore(ctx, saved, 0, legacyNativeBlockRecordW81Budget)
			if err != nil || !reflect.DeepEqual(actual, expected) || !reflect.DeepEqual(actual, record) {
				test.Fatal("inline record changed bytes, exact capacities, heap, or excess", actual, expected, err)
			}
			for _, value := range saved.Inline.Values {
				clear(value)
			}
			if !reflect.DeepEqual(actual, expected) {
				test.Fatal("restored strings alias checkpoint page bytes")
			}
		})
	}
	for _, fixture := range []legacyNativeBlockValue{
		legacyNativeBlockRecordW81Record(ContentBlock{Text: strings.Repeat("x", 257)}, [2]int{0, 257}),
		legacyNativeBlockRecordW81Record(ContentBlock{Type: "t", Text: "x"}, [2]int{16, 256}),
		legacyNativeBlockRecordW81Record(ContentBlock{Text: "x"}, [2]int{0, 512}),
	} {
		if saved, packed, err := legacyNativeBlockInlineRecord(context.Background(), fixture, true); err != nil || packed || saved != (legacyNativeCheckpointBlockRecord{}) {
			test.Fatal("large payload or capacity did not retain legacy fallback", packed, err)
		}
	}
	owned := []byte("borrowed")
	record := legacyNativeBlockValue{block: ContentBlock{Text: unsafe.String(unsafe.SliceData(owned), len(owned))}}
	saved, packed, err := legacyNativeBlockInlineRecord(context.Background(), record, false)
	clear(owned)
	if err != nil || !packed || string(saved.Inline.Values[1]) != "borrowed" {
		test.Fatal("inline capture retained a borrowed source alias", err)
	}
}

func TestLegacyNativeBlockListInlineStandaloneFence(test *testing.T) {
	ctx := context.Background()
	storage := legacyNativeBlockRecordW81Storage(test)
	legacy := legacyNativeBlockRecordW81Capture(test, legacyNativeBlockRecordW81Record(ContentBlock{Type: "t", Text: "abc"}, [2]int{1, 3}), true, storage)
	entry := legacyNativeEventValuesCheckpointEntry{Kind: legacyNativeEventValuesEntryBlock, Block: legacy}
	if err := entry.validate(legacyNativeEventValuesEntryBlock, 32); err != nil {
		test.Fatal("original standalone event entry is invalid", err)
	}
	for _, populated := range []bool{false, true} {
		mixed := legacy
		mixed.Inline = &legacyNativeCheckpointBlockInlineRecord{Values: [2][]byte{[]byte{}, []byte{}}}
		if populated {
			mixed.Inline.Values, mixed.Inline.Capacities = [2][]byte{[]byte("x"), []byte("y")}, [2]int{1, 1}
		}
		test.Run(fmt.Sprintf("standalone/populated=%t", populated), func(test *testing.T) {
			reads, writes := storage.reads, storage.writes
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(ctx, mixed, storage.io, 32, legacyNativeBlockRecordW81Budget)
			if restore != nil {
				restore.close()
			}
			if restore != nil || !errors.Is(err, ErrIntegrity) || storage.reads != reads || storage.writes != writes {
				test.Fatal("standalone/object restore accepted inline data alongside valid legacy values", err)
			}
		})
		test.Run(fmt.Sprintf("event-entry/populated=%t", populated), func(test *testing.T) {
			changed := entry
			changed.Block = mixed
			if err := changed.validate(legacyNativeEventValuesEntryBlock, 32); !errors.Is(err, ErrIntegrity) {
				test.Fatal("event entry accepted inline data alongside valid legacy values", err)
			}
		})
	}
}

func legacyNativeBlockInlineFinish[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], storage *legacyNativeBlockRecordW81Store) Value {
	test.Helper()
	defer work.close()
	for step := 0; step < legacyNativeBlockListW81CheckpointSteps; step++ {
		reads, writes, transferred := storage.reads, storage.writes, storage.transferred
		ready, err := work.step(context.Background())
		if err != nil || storage.reads-reads+storage.writes-writes > legacyNativeCheckpointTreeHeight+2 || storage.transferred-transferred > LegacyCanonicalEncodingBytes {
			test.Fatal("inline list escaped original bounded CAS quantum", step, err, storage.transferred-transferred)
		}
		if retained, err := work.retainedBytes(); err != nil || retained <= 0 || retained > legacyNativeBlockListW81Budget {
			test.Fatal("inline list escaped retained heap admission", retained, err)
		}
		if ready {
			value, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("inline transfer retained ownership", retained, err)
			}
			return value
		}
	}
	test.Fatal("inline list exceeded finite checkpoint schedule")
	var empty Value
	return empty
}

func TestLegacyNativeBlockListInlineMixedRows(test *testing.T) {
	for _, mixed := range []bool{false, true} {
		test.Run(fmt.Sprintf("legacy-fallback=%t", mixed), func(test *testing.T) {
			prior := make([]ContentBlock, 33)
			for index := range prior {
				prior[index] = ContentBlock{Type: "text", Text: fmt.Sprintf("%03d/", index) + strings.Repeat("\xff", 248)}
			}
			if mixed {
				prior[32].Text = strings.Repeat("x", 257)
			}
			fixture := legacyNativeBlockListW81Fixture{name: "inline-mixed", raw: `[null]`, prior: prior[:2]}
			request, list, source := legacyNativeBlockListW81Prepare(test, fixture)
			before := legacyNativeBlockListW81StateOf(test, list)
			storage := legacyNativeBlockRecordW81Storage(test)
			writer, err := newLegacyNativeBlockListCheckpoint(context.Background(), list, request.Binding, source, storage.io, legacyNativeBlockListW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			root := legacyNativeBlockInlineFinish[legacyNativeCheckpointRef](test, writer, storage)
			if !reflect.DeepEqual(before, legacyNativeBlockListW81StateOf(test, list)) || storage.reads != 0 {
				test.Fatal("inline list capture mutated source or read CAS")
			}
			if !mixed && storage.writes != 4 {
				test.Fatal("33 inline rows must write two row pages, one branch, and one list root, not per-field objects", storage.writes)
			}
			var state legacyNativeCheckpointBlockListState
			if err := legacyNativeCheckpointReadJSON(context.Background(), storage.io, root, &state); err != nil {
				test.Fatal(err)
			}
			reader, err := newLegacyNativeCheckpointListReader[legacyNativeCheckpointBlockRecord](state.Decoded.Records, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			defer reader.close()
			for index := range prior {
				row, ok, err := reader.next(context.Background())
				if err != nil || !ok || (row.Inline != nil) != (!mixed || index < 32) {
					test.Fatal("list lost inline/legacy selection or hidden tail", index, err)
				}
			}
			source.Read = func(context.Context, int, int) ([]byte, error) {
				test.Fatal("inline restore replayed source bytes")
				return nil, ErrIntegrity
			}
			restore, err := newLegacyNativeBlockListCheckpointRestore(context.Background(), root, storage.io, source, request.Binding, legacyNativeBlockListW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			actual := legacyNativeBlockInlineFinish[legacyNativeBlockListRestored](test, restore, storage)
			if !reflect.DeepEqual(before, legacyNativeBlockListW81StateOf(test, actual.List)) || actual.Source.authority != source.authority {
				test.Fatal("mixed restore changed list shape, hidden values, or source authority")
			}
		})
	}
}

func TestLegacyNativeBlockListInlinePrefixAndAliases(test *testing.T) {
	fixtures := legacyNativeBlockListW81Fixtures()
	for _, ordinal := range []int{1, 2, 9} {
		fixture := fixtures[ordinal]
		test.Run(fixture.name, func(test *testing.T) {
			storage := legacyNativeAnyW78Store(test)
			request, list, source := legacyNativeBlockListW81Capture(test, fixture, storage)
			before := legacyNativeBlockListW81StateOf(test, list)
			_, originalErr := legacyNativeBlockListW81Continue(test, list, source)
			after := legacyNativeBlockListW81StateOf(test, list)
			var reads legacyNativeAnyW78Reads
			fresh := legacyNativeAnyW78Source(test, request, &reads)
			restore, err := newLegacyNativeBlockListCheckpointRestore(context.Background(), request.Root, storage.io(), fresh, request.Binding, legacyNativeBlockListW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer restore.close()
			legacyNativeBlockListW81Drain(test, restore, storage)
			actual, err := restore.take(context.Background())
			if err != nil || len(reads.Ranges) != 0 || actual.Source.authority != fresh.authority || !reflect.DeepEqual(before, legacyNativeBlockListW81StateOf(test, actual.List)) {
				test.Fatal("inline restore lost prefix boundary, aliases, or fresh authority", err)
			}
			_, restoredErr := legacyNativeBlockListW81Continue(test, actual.List, actual.Source)
			if reads.Forbidden != 0 || !reflect.DeepEqual(after, legacyNativeBlockListW81StateOf(test, actual.List)) || !reflect.DeepEqual(legacyNativeObjectW79ErrorState(originalErr), legacyNativeObjectW79ErrorState(restoredErr)) {
				test.Fatal("inline continuation replayed consumed prefix or changed native semantics", restoredErr)
			}
		})
	}
}

func TestLegacyNativeBlockListInlineMalformedAndAdmission(test *testing.T) {
	ctx := context.Background()
	fixture := legacyNativeBlockListW81Fixture{name: "inline-invalid", raw: `[null]`, prior: []ContentBlock{{Type: "t", Text: "abc"}}}
	request, list, source := legacyNativeBlockListW81Prepare(test, fixture)
	storage := legacyNativeBlockRecordW81Storage(test)
	writer, err := newLegacyNativeBlockListCheckpoint(ctx, list, request.Binding, source, storage.io, legacyNativeBlockListW81Budget)
	if err != nil {
		test.Fatal(err)
	}
	root := legacyNativeBlockInlineFinish[legacyNativeCheckpointRef](test, writer, storage)
	var state legacyNativeCheckpointBlockListState
	if err := legacyNativeCheckpointReadJSON(ctx, storage.io, root, &state); err != nil {
		test.Fatal(err)
	}
	var page legacyNativeCheckpointListPage[legacyNativeCheckpointBlockRecord]
	if err := legacyNativeCheckpointReadJSON(ctx, storage.io, state.Decoded.Records.Ref, &page); err != nil || len(page.Values) != 1 || page.Values[0].Inline == nil {
		test.Fatal("missing inline malformed fixture", err)
	}
	for _, fixture := range []struct {
		name   string
		change func(*legacyNativeCheckpointBlockRecord)
	}{
		{name: "schema", change: func(row *legacyNativeCheckpointBlockRecord) { row.Schema = HashContent([]byte("wrong schema")) }},
		{name: "mixed", change: func(row *legacyNativeCheckpointBlockRecord) { row.Values.Length = 2 }},
		{name: "missing", change: func(row *legacyNativeCheckpointBlockRecord) { row.Inline = nil }},
		{name: "nil-both", change: func(row *legacyNativeCheckpointBlockRecord) {
			row.Inline.Values, row.Inline.Capacities = [2][]byte{}, [2]int{}
		}},
		{name: "nil-type", change: func(row *legacyNativeCheckpointBlockRecord) {
			row.Inline.Values[0], row.Inline.Capacities[0] = nil, 0
		}},
		{name: "nil-text", change: func(row *legacyNativeCheckpointBlockRecord) {
			row.Inline.Values[1], row.Inline.Capacities[1] = nil, 0
		}},
		{name: "negative-capacity", change: func(row *legacyNativeCheckpointBlockRecord) { row.Inline.Capacities[0] = -1 }},
		{name: "short-capacity", change: func(row *legacyNativeCheckpointBlockRecord) { row.Inline.Capacities[1] = 2 }},
		{name: "total-capacity", change: func(row *legacyNativeCheckpointBlockRecord) { row.Inline.Capacities = [2]int{128, 129} }},
		{name: "non-realizable-capacity", change: func(row *legacyNativeCheckpointBlockRecord) { row.Inline.Capacities[1] = 9 }},
		{name: "oversized-payload", change: func(row *legacyNativeCheckpointBlockRecord) { row.Inline.Values[1] = make([]byte, 257) }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			row := page.Values[0]
			inline := *row.Inline
			row.Inline = &inline
			fixture.change(&row)
			changed := state
			changed.Decoded.Records.Ref, err = legacyNativeCheckpointWriteJSON(ctx, storage.io, legacyNativeCheckpointListPage[legacyNativeCheckpointBlockRecord]{Version: 1, Values: []legacyNativeCheckpointBlockRecord{row}})
			if err != nil {
				test.Fatal(err)
			}
			ref, err := legacyNativeCheckpointWriteJSON(ctx, storage.io, changed)
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyNativeBlockListCheckpointRestore(ctx, ref, storage.io, source, request.Binding, legacyNativeBlockListW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockListRestored](test, restore, err, ErrIntegrity)
		})
	}
	for _, name := range []string{"hash", "length", "missing"} {
		test.Run("page-"+name, func(test *testing.T) {
			current := storage.io
			current.Read = func(ctx context.Context, hash ContentHash, length int) ([]byte, error) {
				body, err := storage.io.Read(ctx, hash, length)
				if err != nil || hash != state.Decoded.Records.Ref.Hash {
					return body, err
				}
				switch name {
				case "hash":
					body[0] ^= 1
				case "length":
					body = body[:len(body)-1]
				case "missing":
					return nil, ErrNotFound
				}
				return body, nil
			}
			failure := ErrPreparedDocRangeObject
			if name == "missing" {
				failure = ErrNotFound
			}
			restore, err := newLegacyNativeBlockListCheckpointRestore(ctx, root, current, source, request.Binding, legacyNativeBlockListW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockListRestored](test, restore, err, failure)
		})
	}
	for _, name := range []string{"invalid-base64", "unknown-inline-key"} {
		test.Run(name, func(test *testing.T) {
			body, err := json.Marshal(page)
			if err != nil {
				test.Fatal(err)
			}
			if name == "invalid-base64" {
				body = bytes.Replace(body, []byte(`"YWJj"`), []byte(`"!!!!"`), 1)
			} else {
				body = bytes.Replace(body, []byte(`"capacities"`), []byte(`"unknown":0,"capacities"`), 1)
			}
			changed := state
			changed.Decoded.Records.Ref, err = legacyNativeCheckpointWrite(ctx, storage.io, body)
			if err != nil {
				test.Fatal(err)
			}
			ref, err := legacyNativeCheckpointWriteJSON(ctx, storage.io, changed)
			if err != nil {
				test.Fatal(err)
			}
			restore, err := newLegacyNativeBlockListCheckpointRestore(ctx, ref, storage.io, source, request.Binding, legacyNativeBlockListW81Budget)
			legacyNativeBlockRecordW81Reject[legacyNativeBlockListRestored](test, restore, err, ErrIntegrity)
		})
	}
	for _, name := range []string{"heap", "negative-excess", "odd-excess"} {
		test.Run("source-"+name, func(test *testing.T) {
			record := legacyNativeBlockRecordW81Record(ContentBlock{Text: "abc"}, [2]int{0, 8})
			switch name {
			case "heap":
				record.heaps[1].bytes++
			case "negative-excess":
				record.excess[1] = -2
			case "odd-excess":
				record.excess[1]++
			}
			if _, packed, err := legacyNativeBlockInlineRecord(ctx, record, true); packed || !errors.Is(err, ErrIntegrity) {
				test.Fatal("inline capture accepted inconsistent source heap/excess", packed, err)
			}
		})
	}
	test.Run("inline-allocation-admission", func(test *testing.T) {
		if value, err := legacyNativeBlockInlineRestore(ctx, page.Values[0], source.Bytes, 1); !errors.Is(err, ErrDocumentResourceLimit) || !reflect.DeepEqual(value, legacyNativeBlockValue{}) {
			test.Fatal("inline restored payload without admission", err)
		}
	})
	for _, mode := range []string{"heap", "freeze", "cancel"} {
		test.Run(mode, func(test *testing.T) {
			request, list, source := legacyNativeBlockListW81Prepare(test, fixture)
			work, err := newLegacyNativeBlockListCheckpoint(ctx, list, request.Binding, source, storage.io, legacyNativeBlockListW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			failure := ErrDocumentResourceLimit
			current, cancel := context.WithCancel(ctx)
			defer cancel()
			switch mode {
			case "heap":
				remaining, err := work.remaining()
				if err != nil {
					test.Fatal(err)
				}
				work.budget -= remaining - legacyNativeAnyCheckpointScratch + 1
			case "freeze":
				list.parentDepth++
				failure = ErrConflict
			case "cancel":
				cancel()
				failure = context.Canceled
			}
			reads, writes := storage.reads, storage.writes
			if ready, err := work.step(current); ready || !errors.Is(err, failure) || storage.reads != reads || storage.writes != writes {
				test.Fatal("inline capture ignored admission, frozen binding, or cancellation", ready, err)
			}
			legacyNativeBlockRecordW81Failed[legacyNativeCheckpointRef](test, work, failure)
		})
	}
}
