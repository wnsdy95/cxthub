package domain

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unsafe"
)

func TestLegacyNativeScalarCheckpointReviewBoundaries(test *testing.T) {
	test.Run("current_allocator_bounds", func(test *testing.T) {
		for _, requested := range []int{0, 1, 7, 8, 9, 63, 64, 65, 128, 129, 256, 257, 4096, 4097, 32768, 32769, 65536, 65537, 512 << 10} {
			var builder strings.Builder
			builder.Grow(requested)
			page := bytes.Clone(make([]byte, requested))
			bound := legacyNativeScalarAllocationBound(requested)
			if int64(builder.Cap()) > bound || int64(cap(page)) > bound {
				test.Fatalf("allocation request=%d builder=%d page=%d bound=%d", requested, builder.Cap(), cap(page), bound)
			}
		}
	})
	test.Run("rounded_workspace_admission", func(test *testing.T) {
		var rounded strings.Builder
		rounded.Grow(129)
		if rounded.Cap() <= 129 {
			test.Fatal("fixture requires actual allocator rounding")
		}
		state := legacyNativeScalarCheckpoint{Version: 1, Maximum: 129, Capacity: 129, Present: true}
		var oldPlan docRangeHeaderHeapEstimate
		oldPlan.allocation(1, int64(unsafe.Sizeof(legacyNativeScalarRestore{})))
		oldPlan.allocation(1, int64(unsafe.Sizeof(legacyNativeScalar{})))
		oldPlan.allocation(1, int64(unsafe.Sizeof(strings.Builder{})))
		oldPlan.allocation(129, 1)
		work, err := newLegacyNativeScalarRestore(context.Background(), state, LegacyByteSource{}, 0, oldPlan.bytes)
		if work != nil {
			ready, delayed := work.step(context.Background())
			work.close()
			test.Fatalf("rounded allocation admitted before rejection: ready=%v delayed=%v error=%v", ready, delayed, err)
		}
		if !errors.Is(err, ErrDocumentResourceLimit) {
			test.Fatalf("undersized rounded workspace: %v", err)
		}
	})
	test.Run("premature_take_releases_and_stays_failed", func(test *testing.T) {
		state := legacyNativeScalarCheckpoint{Version: 1}
		work, err := newLegacyNativeScalarRestore(context.Background(), state, LegacyByteSource{}, 0, 64<<20)
		if err != nil {
			test.Fatal(err)
		}
		if value, err := work.take(context.Background()); value != nil || !errors.Is(err, ErrIntegrity) {
			test.Fatalf("premature take: value=%v error=%v", value, err)
		}
		if ready, err := work.step(context.Background()); ready || !errors.Is(err, ErrIntegrity) {
			test.Fatalf("premature take resumed: ready=%v error=%v", ready, err)
		}
		if retained, err := work.retainedBytes(); err != nil || retained != 0 {
			test.Fatalf("failed restore still owns memory: retained=%d error=%v", retained, err)
		}
	})
}
