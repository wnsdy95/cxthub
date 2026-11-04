package domain

import (
	"context"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestLegacyNativeCheckpointShortStringsPreservePagedFormat(test *testing.T) {
	for _, size := range []int{0, 1, LegacyCanonicalEncodingBytes - 1, LegacyCanonicalEncodingBytes, LegacyCanonicalEncodingBytes + 1} {
		test.Run(strconv.Itoa(size), func(test *testing.T) {
			ctx := context.Background()
			value := strings.Repeat("x", size)
			storage := legacyNativeCheckpointStringsOracleStorage(test)
			text, err := newLegacyNativeCheckpointTextWriter(ctx, size, func(_ context.Context, offset, length int) ([]byte, error) {
				return []byte(value[offset : offset+length]), nil
			}, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			saved, _ := legacyNativeCheckpointStringsOracleFinish(test, text, storage, legacyNativeCheckpointStringsOracleBudget)
			rows := legacyNativeCheckpointListWriter[legacyNativeCheckpointStringValueRow]{}
			defer rows.close()
			if err := rows.append(ctx, storage.io, legacyNativeCheckpointStringValueRow{Capacity: size + 8, Text: saved}); err != nil {
				test.Fatal(err)
			}
			expected, err := rows.finish(ctx, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			actualStorage := legacyNativeCheckpointStringsOracleStorage(test)
			work, err := newLegacyNativeCheckpointStringValuesWriter(ctx, []string{value}, func(context.Context, int) (int, error) { return size + 8, nil }, actualStorage.io, legacyNativeCheckpointStringsOracleBudget)
			if err != nil {
				test.Fatal(err)
			}
			if size <= LegacyCanonicalEncodingBytes {
				if ready, err := work.step(ctx); ready || err != nil || !work.pending || work.text != nil || actualStorage.bytes != size || actualStorage.writes != min(size, 1) {
					test.Fatal("single-leaf capture exceeded its bounded write or created unnecessary continuation", ready, err, actualStorage.bytes, actualStorage.writes)
				}
			}
			actual, _ := legacyNativeCheckpointStringsOracleFinish(test, work, actualStorage, legacyNativeCheckpointStringsOracleBudget)
			if actual.Items != expected || actual.Length != 1 || actual.Capacity != 1 || actual.Nil || !reflect.DeepEqual(actualStorage.objects, storage.objects) {
				test.Fatal("single-leaf capture changed stored format, tree, capacity or bytes", actual, expected)
			}
		})
	}
}

func TestLegacyNativeCheckpointShortStringsPreserveFailureFences(test *testing.T) {
	for _, scenario := range []string{"write", "cancellation", "capacity", "budget"} {
		test.Run(scenario, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			storage := legacyNativeCheckpointStringsOracleStorage(test)
			store := storage.io
			capacity := 4
			want := ErrConflict
			switch scenario {
			case "write":
				store.Put = func(context.Context, ContentHash, []byte) error { return want }
			case "cancellation":
				want = context.Canceled
			case "capacity":
				capacity, want = 3, ErrIntegrity
			case "budget":
				want = ErrDocumentResourceLimit
			}
			work, err := newLegacyNativeCheckpointStringValuesWriter(ctx, []string{"text"}, func(context.Context, int) (int, error) { return capacity, nil }, store, legacyNativeCheckpointStringsOracleBudget)
			if err != nil {
				test.Fatal(err)
			}
			defer work.close()
			if scenario == "cancellation" {
				cancel()
			}
			if scenario == "budget" {
				work.budget, err = work.retainedBytes()
				if err != nil {
					test.Fatal(err)
				}
			}
			if ready, err := work.step(ctx); ready || !errors.Is(err, want) {
				test.Fatal("short capture bypassed its original fence", ready, err, want)
			}
			legacyNativeCheckpointStringsOracleFailed(test, work, want)
		})
	}
}
