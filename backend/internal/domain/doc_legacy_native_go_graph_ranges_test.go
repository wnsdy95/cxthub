package domain

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestLegacyNativeCheckpointGoGraphW85RangeIndex(test *testing.T) {
	for _, order := range []string{"ascending", "descending", "alternating", "stride"} {
		test.Run(order, func(test *testing.T) {
			const count = 1021
			backing := make([]any, count)
			for position := range backing {
				backing[position] = float64(position)
			}
			source := make([]any, count)
			for position := range source {
				ordinal := position
				switch order {
				case "descending":
					ordinal = count - 1 - position
				case "alternating":
					ordinal = position / 2
					if position%2 != 0 {
						ordinal = count - 1 - position/2
					}
				case "stride":
					ordinal = (position * 509) % count
				}
				source[position] = backing[ordinal : ordinal+1 : ordinal+1]
			}
			registry := newLegacyNativeCheckpointGoGraphRegistry()
			defer registry.close()
			value, _ := legacyNativeGoGraphW85Import(test, source, registry)
			(&legacyNativeGoGraphW85Inspector{}).check(test, source, value)
			visited := 0
			var inspect func(*legacyNativeCheckpointGoGraphEntry, *legacyNativeCheckpointGoGraphEntry) int
			inspect = func(entry, parent *legacyNativeCheckpointGoGraphEntry) int {
				if entry == nil {
					return 0
				}
				visited++
				if visited > count+1 || entry.rangeParent != parent || entry.key.kind != legacyNativeCheckpointArray || entry.key.capacity <= 0 {
					test.Fatal("range index lost membership, parent, or acyclic ownership")
				}
				if entry.rangeLeft != nil && entry.rangeLeft.key.address >= entry.key.address || entry.rangeRight != nil && entry.rangeRight.key.address <= entry.key.address {
					test.Fatal("range index lost address ordering")
				}
				left := inspect(entry.rangeLeft, entry)
				right := inspect(entry.rangeRight, entry)
				height := 1 + max(left, right)
				if entry.rangeHeight != height || left-right < -1 || left-right > 1 {
					test.Fatal("range index lost its logarithmic height", entry.rangeHeight, left, right)
				}
				return height
			}
			height := inspect(registry.ranges, nil)
			if visited != count+1 {
				test.Fatal("range index omitted a physical source range", visited)
			}
			test.Logf("order=%s ranges=%d height=%d", order, visited, height)
		})
	}
}

func TestLegacyNativeGoGraphSetW85CarrierRetention(test *testing.T) {
	binding := HashContent([]byte("W85 roots carrier is not a logical graph root"))
	for _, capacity := range []int{33, 8193} {
		test.Run(fmt.Sprintf("excess-carrier-capacity-%d", capacity), func(test *testing.T) {
			storage := legacyNativeGoGraphSetW85Storage(test)
			roots := make([]any, 1, capacity)
			work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), roots, binding, storage.io, legacyNativeGoGraphSetW85Budget)
			if work != nil || !errors.Is(err, ErrIntegrity) || storage.reads != 0 || storage.writes != 0 {
				if work != nil {
					work.close()
				}
				test.Fatal("unbounded borrowed carrier was retained before admission", err)
			}
		})
	}
	test.Run("hidden-payload-before-capture", func(test *testing.T) {
		storage := legacyNativeGoGraphSetW85Storage(test)
		backing := []any{nil, map[string]any{"payload": make([]any, 8193)}}
		work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), backing[:1], binding, storage.io, legacyNativeGoGraphSetW85Budget)
		if work != nil || !errors.Is(err, ErrIntegrity) || storage.reads != 0 || storage.writes != 0 {
			if work != nil {
				work.close()
			}
			test.Fatal("hidden payload escaped source retention accounting", err)
		}
		if backing[1].(map[string]any)["payload"] == nil {
			test.Fatal("capture cleared the borrowed source to evade retention")
		}
	})
	test.Run("hidden-payload-after-construction", func(test *testing.T) {
		storage := legacyNativeGoGraphSetW85Storage(test)
		backing := make([]any, 2)
		work, err := newLegacyNativeCheckpointGoGraphSet(context.Background(), backing[:1], binding, storage.io, legacyNativeGoGraphSetW85Budget)
		if err != nil {
			test.Fatal(err)
		}
		defer work.close()
		backing[1] = map[string]any{"payload": true}
		ready, err := legacyNativeGoGraphSetW85Step(test, work, storage)
		if ready || !errors.Is(err, ErrConflict) || storage.reads != 0 || storage.writes != 0 {
			test.Fatal("hidden source replacement survived the current capture fence", ready, err)
		}
		legacyNativeGoGraphSetW85Failed(test, work, storage, err)
	})
	test.Run("bounded-empty-tail", func(test *testing.T) {
		storage := legacyNativeGoGraphSetW85Storage(test)
		roots := make([]any, 1, 32)
		saved := legacyNativeGoGraphSetW85Capture(test, roots, binding, storage)
		values := legacyNativeGoGraphSetW85Restore(test, saved, binding, storage, 0)
		if len(values) != 1 || values[0].value != nil {
			test.Fatal("roots carrier capacity became logical context data")
		}
	})
}
