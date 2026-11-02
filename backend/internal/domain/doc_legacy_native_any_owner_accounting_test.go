package domain

import (
	"context"
	"errors"
	"math"
	"testing"
	"unsafe"
)

func legacyNativeAnyOwnerMetadataOracle(test *testing.T, owner legacyNativeAnyValueOwner) docRangeHeaderHeapEstimate {
	test.Helper()
	var estimate docRangeHeaderHeapEstimate
	if owner.object != nil {
		object := owner.object
		estimate.allocation(1, int64(unsafe.Sizeof(*object)))
		estimate.allocation(1, 64)
		estimate.allocation(int64(max(8, len(object.ownership))), int64(unsafe.Sizeof("")+unsafe.Sizeof(legacyNativeAnyMapOwnership{}))+8)
		object.links.retained(&estimate)
		for _, row := range object.ownership {
			estimate.include(legacyNativeAnyOwnerMetadataOracle(test, row.owner))
		}
	}
	if owner.array != nil {
		array := owner.array
		estimate.allocation(1, int64(unsafe.Sizeof(*array)))
		estimate.include(legacyNativeAnyArrayBacking(cap(array.previous)))
		array.children.retained(&estimate)
		for position := 0; position < array.children.length(); position++ {
			child := array.children.at(position)
			if child == nil {
				test.Fatal("metadata oracle found a missing array child", position)
			}
			estimate.include(legacyNativeAnyOwnerMetadataOracle(test, child.owner))
		}
	}
	if estimate.err != nil || estimate.bytes != owner.bytes {
		test.Fatal("captured child metadata differs from independent owned-structure accounting", estimate, owner.bytes)
	}
	return estimate
}

func TestLegacyNativeAnyOwnerFrameAtomicity(test *testing.T) {
	for _, kind := range []string{"object", "array", "key"} {
		test.Run(kind, func(test *testing.T) {
			frame := &legacyNativeAnyFrame{phase: legacyNativeAnyValueNext}
			parser := &legacyNativeAny{frame: frame, pending: true, live: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}}
			parser.node = legacyNativeAnyValue{value: true, heap: legacyNativeAnyMapTestValueHeap(test, true)}
			if kind == "array" {
				var err error
				frame.array, err = newLegacyNativeAnyArray(1)
				if err != nil {
					test.Fatal(err)
				}
			} else {
				frame.object = newLegacyNativeAnyMap()
				frame.key, frame.keyCapacity = "slot", 4
			}
			if kind == "key" {
				parser.keyToken = true
				parser.node = legacyNativeAnyValue{value: "next", capacity: 4}
			}
			before := *frame
			input := &legacyNormalizationInput{ctx: context.Background()}
			complete, err := parser.commit(input.ctx, input)
			if complete || !errors.Is(err, ErrIntegrity) || !parser.pending || input.values != 0 || input.workBytes != 0 ||
				parser.live.bytes != math.MaxInt64 || *frame != before {
				test.Fatal("frame overflow published or consumed a pending value", complete, err, parser.pending, input, frame, before)
			}
			if frame.object != nil && (len(frame.object.values()) != 0 || frame.object.revision != 0 || frame.object.childBytes != 0) {
				test.Fatal("frame overflow changed map payload or metadata")
			}
			if frame.array != nil && (len(frame.array.values()) != 0 || cap(frame.array.decoded) != 0 || frame.array.revision != 0 || frame.array.children.length() != 0) {
				test.Fatal("frame overflow allocated or published array state")
			}
		})
	}
	for _, first := range []byte{'{', '['} {
		test.Run("open-"+string(first), func(test *testing.T) {
			frame := &legacyNativeAnyFrame{object: newLegacyNativeAnyMap(), phase: legacyNativeAnyValueNext}
			parser := &legacyNativeAny{frame: frame, depth: 1, end: 2, live: docRangeHeaderHeapEstimate{bytes: math.MaxInt64}}
			before := *parser
			input := &legacyNormalizationInput{ctx: context.Background()}
			ready, err := parser.open(first, input)
			if ready || !errors.Is(err, ErrIntegrity) || input.workBytes != 0 || parser.frame != before.frame ||
				parser.rootMap != before.rootMap || parser.rootArray != before.rootArray || parser.rootReserved != before.rootReserved ||
				parser.depth != before.depth || parser.position != before.position || parser.live != before.live {
				test.Fatal("frame overflow published an opened container before its accounting", ready, err, input)
			}
		})
	}
}

func TestLegacyNativeAnyOwnerPayloadProvenance(test *testing.T) {
	for _, raw := range []string{`[1]`, `{"nested":[1]}`} {
		for _, corrupt := range []string{"payload", "excess"} {
			test.Run(raw+corrupt, func(test *testing.T) {
				value, _ := legacyNativeValueCheckpointW76Accept(test, raw)
				if corrupt == "payload" {
					value.heap.bytes = 0
				} else {
					value.excess++
				}
				object := newLegacyNativeAnyMap()
				if _, err := object.putValue("child", 5, value); !errors.Is(err, ErrIntegrity) || object.revision != 0 || len(object.values()) != 0 {
					test.Fatal("map accepted forged child payload accounting", err)
				}
				array, err := newLegacyNativeAnyArray(1)
				if err != nil {
					test.Fatal(err)
				}
				input := &legacyNormalizationInput{ctx: context.Background()}
				if ready, err := array.appendValue(input.ctx, input, value); ready || !errors.Is(err, ErrIntegrity) || array.revision != 0 || len(array.values()) != 0 {
					test.Fatal("array accepted forged child payload accounting", ready, err)
				}
			})
		}
	}
}
