package domain

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeBlockRecordQuantumFinish[Value any](test *testing.T, work legacyNativeBlockRecordW81Work[Value], storage *legacyNativeBlockRecordW81Store) (Value, int) {
	test.Helper()
	defer work.close()
	for step := 1; step <= 64; step++ {
		reads, writes, transferred := storage.reads, storage.writes, storage.transferred
		ready, err := work.step(context.Background())
		if err != nil || storage.reads != reads || storage.writes-writes > 1 || storage.transferred-transferred > LegacyCanonicalEncodingBytes {
			test.Fatal("block record combined leaf IO or lost its bounded write", step, storage.reads-reads, storage.writes-writes, storage.transferred-transferred, err)
		}
		if retained, err := work.retainedBytes(); err != nil || retained <= 0 || retained > legacyNativeBlockRecordW81Budget {
			test.Fatal("block record escaped original heap admission", retained, err)
		}
		if ready {
			value, err := work.take(context.Background())
			if err != nil {
				test.Fatal(err)
			}
			if retained, err := work.retainedBytes(); err != nil || retained != 0 {
				test.Fatal("block record transfer retained ownership", retained, err)
			}
			return value, step
		}
	}
	test.Fatal("two-field block record exhausted its finite schedule")
	var empty Value
	return empty, 0
}

func TestLegacyNativeCheckpointBlockRecordUnflushedRowQuantum(test *testing.T) {
	for _, size := range []int{0, 1, LegacyCanonicalEncodingBytes - 1, LegacyCanonicalEncodingBytes, LegacyCanonicalEncodingBytes + 1} {
		test.Run(fmt.Sprintf("bytes=%d", size), func(test *testing.T) {
			ctx := context.Background()
			values := [2]string{strings.Repeat("t", size), strings.Repeat("x", size)}
			var capacities [2]int
			for ordinal, text := range values {
				var builder strings.Builder
				builder.Grow(size + 8*(ordinal+1))
				builder.WriteString(text)
				values[ordinal], capacities[ordinal] = builder.String(), builder.Cap()
			}
			oracleStore := legacyNativeBlockRecordW81Storage(test)
			oracle, err := newLegacyNativeCheckpointStringValuesWriter(ctx, values[:], func(_ context.Context, ordinal int) (int, error) {
				return capacities[ordinal], nil
			}, oracleStore.io, legacyNativeBlockRecordW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			expected, oracleSteps := legacyNativeBlockRecordQuantumFinish[legacyNativeCheckpointStrings](test, oracle, oracleStore)
			storage := legacyNativeBlockRecordW81Storage(test)
			record := legacyNativeBlockRecordW81Record(ContentBlock{Type: values[0], Text: values[1]}, capacities)
			before := record
			work, err := newLegacyNativeCheckpointBlockRecordWriter(ctx, record, true, storage.io, legacyNativeBlockRecordW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			actual, steps := legacyNativeBlockRecordQuantumFinish[legacyNativeCheckpointBlockRecord](test, work, storage)
			if actual.Values != expected || !reflect.DeepEqual(storage.objects, oracleStore.objects) || !reflect.DeepEqual(record, before) {
				test.Fatal("coalesced block rows changed exact bytes, hashes, capacity, or borrowed input", actual.Values, expected)
			}
			if size <= LegacyCanonicalEncodingBytes && (oracleSteps != 5 || steps != 3) {
				test.Fatal("small block record did not retain the five-step string helper and finish in three steps", oracleSteps, steps)
			}
			restore, err := newLegacyNativeCheckpointBlockRecordRestore(ctx, actual, storage.io, 0, legacyNativeBlockRecordW81Budget)
			if err != nil {
				test.Fatal(err)
			}
			decoded := legacyNativeBlockRecordW81Finish[legacyNativeBlockValue](test, restore, storage, legacyNativeBlockRecordW81Budget)
			if !reflect.DeepEqual(decoded, record) {
				test.Fatal("coalesced block record changed restored bytes, capacities, heap or excess")
			}
		})
	}
	test.Run("no-flush-boundary", func(test *testing.T) {
		storage := legacyNativeBlockRecordW81Storage(test)
		work, err := newLegacyNativeCheckpointBlockRecordWriter(context.Background(), legacyNativeBlockValue{block: ContentBlock{Type: "text", Text: "body"}}, false, storage.io, legacyNativeBlockRecordW81Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		work.strings.rows.values = make([]legacyNativeCheckpointStringValueRow, legacyNativeCheckpointPageItems-1, legacyNativeCheckpointPageItems)
		ready, err := work.step(context.Background())
		if err != nil || ready || !work.strings.pending || work.strings.position != 0 || len(work.strings.rows.values) != legacyNativeCheckpointPageItems-1 || storage.writes != 1 || storage.transferred != len("text") {
			test.Fatal("record combined a possible row flush with its text leaf", ready, err, storage.writes, storage.transferred)
		}
	})
}
