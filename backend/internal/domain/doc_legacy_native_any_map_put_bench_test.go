package domain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

var legacyNativeAnyMapPutBenchmarkSink *legacyNativeAnyMap

func BenchmarkLegacyNativeAnyMapPutIsolated(benchmark *testing.B) {
	fixtures := []struct {
		name     string
		keyCount int
		putCount int
	}{
		{name: "tiny8", keyCount: 8, putCount: 8},
		{name: "medium4096", keyCount: 4096, putCount: 4096},
		{name: "overwrite64key4096puts", keyCount: 64, putCount: 4096},
	}
	for _, fixture := range fixtures {
		keys := make([]string, fixture.keyCount)
		keyCapacities := make([]int, fixture.keyCount)
		keyIdentities := make(map[string]*byte, fixture.keyCount)
		for position := range keys {
			text := fmt.Sprintf("key-%04d", position)
			var builder strings.Builder
			builder.Grow(len(text) + 16)
			builder.WriteString(text)
			keys[position] = builder.String()
			keyCapacities[position] = builder.Cap()
			keyIdentities[keys[position]] = unsafe.StringData(keys[position])
		}
		puts := make([]struct {
			key         string
			keyCapacity int
			value       any
			valueHeap   docRangeHeaderHeapEstimate
			valueExcess int64
		}, fixture.putCount)
		expectedValues := make(map[string]any, fixture.keyCount)
		finalValueHeaps := make([]docRangeHeaderHeapEstimate, fixture.keyCount)
		for position := range puts {
			keyPosition := position % fixture.keyCount
			put := &puts[position]
			put.key = keys[keyPosition]
			put.keyCapacity = keyCapacities[keyPosition]
			if position >= fixture.keyCount {
				put.key = strings.Clone(put.key)
				put.keyCapacity = len(put.key)
			}
			if fixture.putCount == fixture.keyCount || position < fixture.putCount-fixture.keyCount {
				switch (position/fixture.keyCount + keyPosition) % 3 {
				case 0:
					put.value = float64(position) + 0.25
					put.valueHeap.allocation(1, int64(unsafe.Sizeof(float64(0))))
				case 1:
					text := fmt.Sprintf("value-%08d", position)
					var builder strings.Builder
					builder.Grow(len(text) + 16)
					builder.WriteString(text)
					put.value = builder.String()
					put.valueHeap.allocation(1, int64(unsafe.Sizeof("")))
					put.valueHeap.allocation(int64(builder.Cap()), 1)
					put.valueExcess = 2 * int64(builder.Cap()-len(text))
				}
			}
			if put.valueHeap.err != nil {
				benchmark.Fatal(put.valueHeap.err)
			}
			expectedValues[keys[keyPosition]] = put.value
			finalValueHeaps[keyPosition] = put.valueHeap
		}
		var expectedHeap docRangeHeaderHeapEstimate
		expectedHeap.allocation(1, int64(unsafe.Sizeof(expectedValues)))
		expectedHeap.allocation(1, 64)
		expectedHeap.allocation(int64(max(8, fixture.keyCount)), int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8)
		for position, capacity := range keyCapacities {
			expectedHeap.allocation(int64(capacity), 1)
			expectedHeap.include(finalValueHeaps[position])
		}
		var minimumRetained docRangeHeaderHeapEstimate
		minimumRetained.allocation(1, int64(unsafe.Sizeof(legacyNativeAnyMap{})))
		minimumRetained.allocation(1, 64)
		minimumRetained.allocation(int64(max(8, fixture.keyCount)), int64(unsafe.Sizeof(""))+8)
		if expectedHeap.err != nil || minimumRetained.err != nil {
			benchmark.Fatal("invalid fixture accounting", expectedHeap, minimumRetained)
		}
		benchmark.Run(fixture.name, func(benchmark *testing.B) {
			benchmark.ReportAllocs()
			var result *legacyNativeAnyMap
			benchmark.ResetTimer()
			for operation := 0; operation < benchmark.N; operation++ {
				native := newLegacyNativeAnyMap()
				for _, put := range puts {
					if err := native.put(put.key, put.keyCapacity, put.value, put.valueHeap, put.valueExcess); err != nil {
						benchmark.Fatal(err)
					}
				}
				result = native
			}
			benchmark.StopTimer()
			legacyNativeAnyMapPutBenchmarkSink = result
			if result == nil || !reflect.DeepEqual(result.values(), expectedValues) {
				benchmark.Fatal("final map changed value types, values, or keys")
			}
			for key := range result.values() {
				if unsafe.StringData(key) != keyIdentities[key] {
					benchmark.Fatalf("final map replaced canonical key backing for %q", key)
				}
			}
			actualHeap := result.heap()
			if actualHeap.err != nil || actualHeap.bytes != expectedHeap.bytes {
				benchmark.Fatalf("final map capacity accounting: got %+v, want %+v", actualHeap, expectedHeap)
			}
			var retained docRangeHeaderHeapEstimate
			result.retained(&retained)
			if retained.err != nil || retained.bytes < minimumRetained.bytes {
				benchmark.Fatalf("final retained metadata: got %+v, want at least %+v", retained, minimumRetained)
			}
			actualHeap.include(retained)
			if actualHeap.err != nil {
				benchmark.Fatal(actualHeap.err)
			}
			benchmark.ReportMetric(float64(fixture.putCount), "puts/op")
			benchmark.ReportMetric(float64(actualHeap.bytes), "final-native-owner-B")
		})
	}
}
