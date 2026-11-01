package domain

import (
	"context"
	"fmt"
	"unsafe"
)

const legacyCanonicalReplacementProjectionBytes = 16 << 10
const legacyCanonicalReplacementProjectionActions = 64

type legacyCanonicalReplacement struct {
	events []CIREvent
	native *legacyNativeEventValues
	order  legacyCanonicalKeyOrder
}

func legacyCanonicalNativeEventJSONObject(event CIREvent) (map[string]any, error) {
	return canonicalEventJSONObjectWithReplacement(event, true, true)
}

func legacyCanonicalNativeEventViewJSONObject(event CIREvent, replacement *legacyNativeEventValues, blocks *legacyNativeBlockValues) (map[string]any, error) {
	if replacement != nil {
		if event.Replacement != nil {
			return nil, ErrIntegrity
		}
		event.Replacement = []CIREvent{}
	}
	if blocks != nil {
		if event.Blocks != nil {
			return nil, ErrIntegrity
		}
		event.Blocks = []ContentBlock{}
	}
	value, err := legacyCanonicalNativeEventJSONObject(event)
	if err != nil {
		return nil, err
	}
	if _, emitted := value["replacement"]; emitted && replacement != nil {
		value["replacement"] = &legacyCanonicalReplacement{native: replacement}
	}
	if _, emitted := value["blocks"]; emitted && blocks != nil {
		value["blocks"] = blocks
	}
	return value, nil
}

func (replacement *legacyCanonicalReplacement) length() int {
	if replacement == nil || replacement.native != nil && replacement.events != nil {
		return -1
	}
	if replacement.native != nil {
		return replacement.native.length()
	}
	if replacement.events == nil {
		return -1
	}
	return len(replacement.events)
}

func (replacement *legacyCanonicalReplacement) at(index int) (CIREvent, *legacyNativeEventValues, error) {
	if index < 0 || index >= replacement.length() {
		return CIREvent{}, nil, ErrIntegrity
	}
	if replacement.native != nil {
		value := replacement.native.at(index)
		if value == nil {
			return CIREvent{}, nil, ErrIntegrity
		}
		return value.event, value.replacement, nil
	}
	return replacement.events[index], nil, nil
}

func legacyCanonicalReplacementKey(sequence, ordinal int) string {
	var encoded [32]byte
	const digits = "0123456789abcdef"
	values := [2]uint64{uint64(int64(sequence)) ^ (uint64(1) << 63), uint64(ordinal)}
	for index, value := range values {
		for position := 15; position >= 0; position-- {
			encoded[index*16+position] = digits[value&15]
			value >>= 4
		}
	}
	return string(encoded[:])
}

func (preparation *legacyCanonicalPreparation) enterReplacement(encoding *LegacyCanonicalEncoding, frame *legacyCanonicalPreparationFrame, replacement *legacyCanonicalReplacement) (bool, error) {
	count := replacement.length()
	if count < 0 || encoding.nativeOwner == nil || !frame.prepare {
		return false, ErrIntegrity
	}
	if frame.depth >= 10000 {
		return false, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
	}
	storageBytes, err := encoding.mapKeys.appendBytes()
	if err != nil || !preparation.reserve(storageBytes) {
		return false, err
	}
	frame.mapPosition = encoding.mapKeys.length()
	if err := encoding.mapKeys.append(legacyCanonicalEncodingKeys{source: replacement}); err != nil {
		return false, err
	}
	frame.kind, frame.count = 'r', count
	frame.heap.allocation(1, int64(unsafe.Sizeof(*replacement)))
	if replacement.native != nil {
		frame.heap.include(replacement.native.backing())
	} else {
		frame.heap.allocation(int64(cap(replacement.events)), int64(unsafe.Sizeof(CIREvent{})))
	}
	preparation.lastStorageBytes += storageBytes
	return true, frame.heap.err
}

func (preparation *legacyCanonicalPreparation) stepReplacement(ctx context.Context, encoding *LegacyCanonicalEncoding) (bool, error) {
	frame := preparation.frames.at(preparation.frames.length() - 1)
	replacement, ok := frame.value.(*legacyCanonicalReplacement)
	if !ok || replacement.length() < 0 || replacement.length() != frame.count || encoding.nativeOwner == nil ||
		frame.phase > 2 || frame.position < 0 || frame.position > frame.count {
		return false, ErrIntegrity
	}
	keys := encoding.mapKeys.at(frame.mapPosition)
	if keys == nil || keys.fields.length() > frame.count || frame.phase == 0 && keys.fields.length() != frame.position ||
		frame.phase != 0 && keys.fields.length() != frame.count {
		return false, ErrIntegrity
	}
	if frame.phase == 0 {
		if frame.position == frame.count {
			replacement.order = newLegacyCanonicalKeyOrder(keys.fields.length(), false)
			frame.phase, frame.position = 1, 0
			return true, nil
		}
		if legacyCanonicalReplacementProjectionActions > legacyCanonicalEncodingActions-preparation.lastActions {
			return false, nil
		}
		storageBytes, err := keys.fields.appendBytes()
		if err != nil || !preparation.reserve(storageBytes+legacyCanonicalReplacementProjectionBytes+int(unsafe.Sizeof(legacyCanonicalKeyField{}))) {
			return false, err
		}
		event, childReplacement, err := replacement.at(frame.position)
		if err != nil {
			return false, err
		}
		var childBlocks *legacyNativeBlockValues
		if replacement.native != nil {
			childBlocks = replacement.native.at(frame.position).blocks
		}
		value, err := legacyCanonicalNativeEventViewJSONObject(event, childReplacement, childBlocks)
		if err != nil {
			return false, err
		}
		projection := legacyCanonicalNativeProjectionHeap(event, value)
		projection.allocation(32, 1)
		if projection.err != nil || projection.bytes > legacyCanonicalReplacementProjectionBytes {
			return false, ErrIntegrity
		}
		key := legacyCanonicalReplacementKey(event.Seq, frame.position)
		if err := keys.fields.append(legacyCanonicalKeyField{key: key, value: value}); err != nil {
			return false, err
		}
		bytes, err := keys.fields.ownedBytes()
		if err != nil || bytes < keys.heap {
			return false, ErrIntegrity
		}
		encoding.pendingKeysHeap.include(docRangeHeaderHeapEstimate{bytes: bytes - keys.heap})
		encoding.rootHeap.include(projection)
		if encoding.pendingKeysHeap.err != nil || encoding.rootHeap.err != nil {
			return false, ErrIntegrity
		}
		keys.heap = bytes
		frame.position++
		preparation.lastActions += legacyCanonicalReplacementProjectionActions
		encoding.inspectWork += legacyCanonicalReplacementProjectionActions
		preparation.lastStorageBytes += storageBytes + int(projection.bytes)
		return true, nil
	}
	if frame.phase == 1 {
		if preparation.lastBytes == LegacyCanonicalEncodingBytes {
			return false, nil
		}
		before := replacement.order.work
		used, complete, _, err := replacement.order.advance(&keys.fields, LegacyCanonicalEncodingBytes-preparation.lastBytes)
		if err != nil || !preparation.reserve(used) || replacement.order.work < before {
			return false, ErrIntegrity
		}
		encoding.keyWork += replacement.order.work - before
		if complete {
			keys.sorted = true
			replacement.order = legacyCanonicalKeyOrder{}
			frame.phase = 2
		}
		return true, nil
	}
	return preparation.stepMap(ctx, encoding)
}
