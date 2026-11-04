package domain

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

func TestLegacyCanonicalPreparedShortTextMatchesPagedWriter(test *testing.T) {
	for _, size := range []int{0, 1, LegacyCanonicalEncodingBytes - 1, LegacyCanonicalEncodingBytes, LegacyCanonicalEncodingBytes + 1} {
		test.Run(strconv.Itoa(size), func(test *testing.T) {
			ctx := context.Background()
			text := strings.Repeat("x", size)
			storage := legacyCanonicalPreparedValueTestStorage(test)
			writer, err := newLegacyNativeCheckpointTextWriter(ctx, size, func(_ context.Context, offset, length int) ([]byte, error) {
				return []byte(text[offset : offset+length]), nil
			}, storage.io)
			if err != nil {
				test.Fatal(err)
			}
			defer writer.close()
			ready := false
			for steps := 0; steps < 32 && !ready; steps++ {
				ready, err = writer.step(ctx)
				if err != nil {
					test.Fatal(err)
				}
			}
			tree, err := writer.take(ctx)
			if err != nil {
				test.Fatal(err)
			}
			binding := HashContent([]byte("short-text-parity"))
			record := legacyCanonicalPreparedValueRecord{Kind: legacyCanonicalPreparedValueKind, Version: legacyNativeCheckpointVersion,
				Binding: binding, Tag: legacyCanonicalPreparedString}
			record.Texts[0] = tree
			expected, err := legacyNativeCheckpointWriteJSON(ctx, storage.io, record)
			if err != nil {
				test.Fatal(err)
			}
			actualStorage := legacyCanonicalPreparedValueTestStorage(test)
			actual := legacyCanonicalPreparedValueTestWrite(test, text, nil, binding, actualStorage, legacyCanonicalPreparedValueTestBudget)
			if actual != expected {
				test.Fatal("single-leaf fast path changed the existing checkpoint format", actual, expected)
			}
			if size <= LegacyCanonicalEncodingBytes {
				value, err := newLegacyCanonicalPreparedValueWrite(ctx, text, nil, binding, actualStorage.io, legacyCanonicalPreparedValueTestBudget)
				if err != nil {
					test.Fatal(err)
				}
				defer value.close()
				for step := 0; step < 2; step++ {
					actualStorage.bytes, actualStorage.writes = 0, 0
					complete, err := value.step(ctx)
					if err != nil || complete != (step == 1) || actualStorage.bytes > LegacyCanonicalEncodingBytes || actualStorage.writes > 1 {
						test.Fatal("short text did not preserve separate bounded text and record steps", step, complete, actualStorage.bytes, actualStorage.writes, err)
					}
				}
			}
		})
	}
}
