package domain

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

func BenchmarkLegacyNativeAnyMapCheckpointPreparation(benchmark *testing.B) {
	fixtures := []struct {
		name     string
		keyCount int
		putCount int
	}{
		{name: "tiny8keys", keyCount: 8, putCount: 8},
		{name: "medium4096keys", keyCount: 4096, putCount: 4096},
		{name: "overwrite64keys/4096puts", keyCount: 64, putCount: 4096},
	}
	ctx := context.Background()
	for _, fixture := range fixtures {
		keys := make([]string, fixture.keyCount)
		for position := range keys {
			keys[position] = fmt.Sprintf("key-%04d", position)
		}
		puts := make([]struct {
			key               string
			value             any
			valueHeap         docRangeHeaderHeapEstimate
			expectedHeapBytes int64
		}, fixture.putCount)
		expectedValues := make(map[string]any, fixture.keyCount)
		previousHeaps := make([]docRangeHeaderHeapEstimate, fixture.keyCount)
		var expectedPayload docRangeHeaderHeapEstimate
		for position := range puts {
			keyPosition := position % fixture.keyCount
			key := keys[keyPosition]
			var value any
			switch (position/fixture.keyCount + keyPosition) % 3 {
			case 0:
				value = float64(position) + 0.25
			case 1:
				value = fmt.Sprintf("value-%08d", position)
			}
			if fixture.putCount > fixture.keyCount && position >= fixture.putCount-fixture.keyCount {
				value = nil
			}
			valueHeap, err := legacyNativeValueHeap(ctx, reflect.ValueOf(value))
			if err != nil {
				benchmark.Fatal(err)
			}
			if position < fixture.keyCount {
				expectedPayload.allocation(int64(len(key)), 1)
			} else {
				expectedPayload.bytes -= previousHeaps[keyPosition].bytes
			}
			expectedPayload.include(valueHeap)
			previousHeaps[keyPosition] = valueHeap
			expectedValues[key] = value
			expectedHeap := legacyNativeAnyMapBacking(len(expectedValues), int64(unsafe.Sizeof(any(nil))))
			expectedHeap.allocation(1, int64(unsafe.Sizeof(expectedValues)))
			expectedHeap.include(expectedPayload)
			if expectedHeap.err != nil {
				benchmark.Fatal(expectedHeap.err)
			}
			puts[position].key = key
			puts[position].value = value
			puts[position].valueHeap = valueHeap
			puts[position].expectedHeapBytes = expectedHeap.bytes
		}
		expectedHeap, err := legacyNativeValueHeap(ctx, reflect.ValueOf(expectedValues))
		if err != nil {
			benchmark.Fatal(err)
		}
		expectedRetained := legacyNativeAnyMapBacking(fixture.keyCount, int64(unsafe.Sizeof(legacyNativeAnyMapOwnership{})))
		expectedRetained.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyMap{})))
		if expectedRetained.err != nil {
			benchmark.Fatal(expectedRetained.err)
		}
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			var totalPeakOwner, totalRetainedOwner float64
			benchmark.ResetTimer()
			for operation := 0; operation < benchmark.N; operation++ {
				native := newLegacyNativeAnyMap()
				var peakOwner int64
				for position, put := range puts {
					if err := native.put(put.key, len(put.key), put.value, put.valueHeap, 0); err != nil {
						benchmark.Fatal(err)
					}
					values := native.values()
					actualValue, found := values[put.key]
					if !found || actualValue != put.value || len(values) != min(position+1, fixture.keyCount) {
						benchmark.Fatalf("put %d changed values or key count", position)
					}
					owner := native.heap()
					if owner.err != nil || owner.bytes != put.expectedHeapBytes {
						benchmark.Fatalf("put %d heap: got %+v, want %d bytes", position, owner, put.expectedHeapBytes)
					}
					native.retained(&owner)
					if owner.err != nil {
						benchmark.Fatal(owner.err)
					}
					peakOwner = max(peakOwner, owner.bytes)
				}
				benchmark.StopTimer()
				if !reflect.DeepEqual(native.values(), expectedValues) {
					benchmark.Fatal("operation changed final values")
				}
				actualHeap := native.heap()
				if actualHeap.err != nil || actualHeap.bytes != expectedHeap.bytes {
					benchmark.Fatalf("operation heap: got %+v, want %+v", actualHeap, expectedHeap)
				}
				var retained docRangeHeaderHeapEstimate
				native.retained(&retained)
				if retained.err != nil || retained.bytes < expectedRetained.bytes {
					benchmark.Fatalf("operation retained metadata: got %+v, want %+v", retained, expectedRetained)
				}
				actualHeap.include(retained)
				if actualHeap.err != nil || peakOwner < actualHeap.bytes {
					benchmark.Fatal("operation lost its peak owner estimate", peakOwner, actualHeap)
				}
				totalPeakOwner += float64(peakOwner)
				totalRetainedOwner += float64(actualHeap.bytes)
				benchmark.StartTimer()
			}
			benchmark.StopTimer()
			benchmark.ReportMetric(totalPeakOwner/float64(benchmark.N), "peak-native-owner-B/op")
			benchmark.ReportMetric(totalRetainedOwner/float64(benchmark.N), "retained-native-owner-B/op")
		})
	}
}

func BenchmarkLegacyNativeAnyMapCheckpointLongTailUpdates(benchmark *testing.B) {
	for _, width := range []int{64, 1 << 20} {
		benchmark.Run(fmt.Sprintf("tail-%d", width), func(benchmark *testing.B) {
			native := newLegacyNativeAnyMap()
			tail := strings.Repeat("x", width)
			heap, err := legacyNativeValueHeap(context.Background(), reflect.ValueOf(false))
			if err != nil {
				benchmark.Fatal(err)
			}
			for _, key := range []string{"short", tail} {
				if err := native.put(key, len(key), false, heap, 0); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for operation := 0; operation < benchmark.N; operation++ {
				if err := native.put("short", 5, operation%2 == 0, heap, 0); err != nil {
					benchmark.Fatal(err)
				}
			}
			benchmark.StopTimer()
			if len(native.values()) != 2 || native.values()[tail] != false {
				benchmark.Fatal("tail update changed unrelated values")
			}
		})
	}
}
