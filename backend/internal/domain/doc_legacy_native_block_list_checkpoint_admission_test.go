package domain

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"unsafe"
)

func TestLegacyNativeBlockListCheckpointW81Admission(test *testing.T) {
	test.Run("controller-before-allocation", func(test *testing.T) {
		request, list, source := legacyNativeBlockListW81Prepare(test, legacyNativeBlockListW81Fixtures()[0])
		storage := legacyNativeAnyW78Store(test)
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		work, err := newLegacyNativeBlockListCheckpoint(context.Background(), list, request.Binding, source, storage.io(), 1)
		runtime.ReadMemStats(&after)
		if work != nil || !errors.Is(err, ErrDocumentResourceLimit) {
			test.Fatal("underfunded controller accepted", err)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated >= uint64(unsafe.Sizeof(legacyNativeBlockListCheckpoint{})) {
			test.Fatal("controller escaped pre-allocation admission", allocated)
		}
	})
	test.Run("separate-array-planning", func(test *testing.T) {
		request, list, source := legacyNativeBlockListW81Prepare(test, legacyNativeBlockListW81Fixtures()[1])
		for attempt := 0; attempt < legacyNativeBlockListW81SourceSteps && !legacyNativeBlockListW81Stop(list, "copy"); attempt++ {
			base, cancel := context.WithCancel(context.Background())
			current := &legacyNativeAnyW78CutContext{Context: base, cancel: cancel, stop: func() bool { return legacyNativeBlockListW81Stop(list, "copy") }}
			_, err := legacyNativeBlockListW81Step(test, current, list, source, true)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) {
				test.Fatal(err)
			}
		}
		if list.previous == nil {
			test.Fatal("missing actual migration")
		}
		state, err := legacyNativeCheckpointBlockListControlFor(list, request.Binding, source.Bytes)
		if err != nil {
			test.Fatal(err)
		}
		work := &legacyNativeBlockListCheckpointRestore{state: state, source: source, binding: request.Binding, budget: legacyNativeBlockListW81Budget}
		defer work.close()
		retained, err := work.retainedBytes()
		if err != nil {
			test.Fatal(err)
		}
		var expected docRangeHeaderHeapEstimate
		expected.allocation(1, int64(unsafe.Sizeof(legacyNativeBlockList{})))
		for _, capacity := range []int{state.Decoded.Capacity, state.Previous.Capacity} {
			expected.include(legacyNativeBlocksBacking(capacity))
			expected.allocation(2*int64(capacity), int64(unsafe.Sizeof(docRangeHeaderHeapEstimate{})))
			expected.allocation(2*int64(capacity), int64(unsafe.Sizeof(int64(0))))
		}
		if expected.err != nil {
			test.Fatal(expected.err)
		}
		work.budget = retained + expected.bytes - 1
		if err := work.allocate(); !errors.Is(err, ErrDocumentResourceLimit) || work.list != nil {
			test.Fatal("separate arrays allocated below their full existing policy estimate", err)
		}
	})
}
