package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"unsafe"
)

type legacyCanonicalPreparation struct {
	frames           legacyCanonicalPages[legacyCanonicalPreparationFrame]
	heap             docRangeHeaderHeapEstimate
	complete         bool
	lastActions      int
	lastBytes        int
	lastStorageBytes int
}

type legacyCanonicalPreparationFrame struct {
	value       any
	heap        docRangeHeaderHeapEstimate
	iterator    *reflect.MapIter
	mapResume   *legacyCanonicalMapContinuation
	depth       int
	position    int
	length      int
	count       int
	mapPosition int
	keyBytes    int
	kind        byte
	phase       uint8
	prepare     bool
}

func newLegacyCanonicalPreparation(value any, depth int, prepare bool) *legacyCanonicalPreparation {
	preparation := &legacyCanonicalPreparation{}
	preparation.heap.err = preparation.frames.append(legacyCanonicalPreparationFrame{value: value, depth: depth, prepare: prepare})
	return preparation
}

func (preparation *legacyCanonicalPreparation) reserve(bytes int) bool {
	if bytes < 0 || bytes > LegacyCanonicalEncodingBytes-preparation.lastBytes {
		return false
	}
	preparation.lastBytes += bytes
	return true
}

func (preparation *legacyCanonicalPreparation) pop() error {
	last := preparation.frames.length() - 1
	heap := preparation.frames.at(last).heap
	if err := preparation.frames.pop(); err != nil {
		return err
	}
	if last == 0 {
		preparation.heap = heap
		preparation.complete = true
	} else {
		preparation.frames.at(last - 1).heap.include(heap)
	}
	return heap.err
}

func (encoding *LegacyCanonicalEncoding) inspectLeaf(ctx context.Context, input any, depth int, prepare bool) (docRangeHeaderHeapEstimate, bool, error) {
	var heap docRangeHeaderHeapEstimate
	switch value := input.(type) {
	case nil:
	case bool:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case int:
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case float64:
		if _, err := json.Marshal(value); err != nil {
			return heap, true, err
		}
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	case string, EventKind, Role, ProviderKind, FidelityTier:
		text := reflect.ValueOf(value).String()
		heap.allocation(1, int64(unsafe.Sizeof(text)))
		heap.allocation(int64(len(text)), 1)
	case json.Number:
		if err := encoding.prepareNumber(ctx, string(value)); err != nil {
			return heap, true, err
		}
		heap.allocation(1, int64(unsafe.Sizeof(value)))
		heap.allocation(int64(len(value)), 1)
	case *json.Number:
		if value != nil {
			if err := encoding.prepareNumber(ctx, string(*value)); err != nil {
				return heap, true, err
			}
			heap.allocation(1, int64(unsafe.Sizeof(*value)))
			heap.allocation(int64(len(*value)), 1)
		}
	case ContentBlock:
		child, err := encoding.inspectContentBlock(ctx, value, depth, prepare)
		if err != nil {
			return heap, true, err
		}
		heap.include(child)
		heap.allocation(1, int64(unsafe.Sizeof(value)))
	default:
		return heap, false, nil
	}
	return heap, true, heap.err
}

func (preparation *legacyCanonicalPreparation) enter(ctx context.Context, encoding *LegacyCanonicalEncoding) (bool, error) {
	frame := preparation.frames.at(preparation.frames.length() - 1)
	var number string
	var numbered bool
	switch value := frame.value.(type) {
	case json.Number:
		number, numbered = string(value), true
	case *json.Number:
		if value != nil {
			number, numbered = string(*value), true
		}
	}
	if numbered {
		bytes, err := encoding.numbers.prepareBytes(number)
		if err != nil || !preparation.reserve(bytes) {
			return false, err
		}
		preparation.lastStorageBytes += bytes
	}
	heap, leaf, err := encoding.inspectLeaf(ctx, frame.value, frame.depth, frame.prepare)
	if err != nil {
		return false, err
	}
	if leaf {
		frame.heap = heap
		return true, preparation.pop()
	}
	switch value := frame.value.(type) {
	case map[string]any:
		if value == nil {
			return true, preparation.pop()
		}
		if frame.depth >= 10000 {
			return false, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
		}
		if frame.prepare {
			bytes, err := encoding.mapKeys.appendBytes()
			if err != nil || !preparation.reserve(bytes) {
				return false, err
			}
			frame.mapPosition = encoding.mapKeys.length()
			if err := encoding.mapKeys.append(legacyCanonicalEncodingKeys{source: value}); err != nil {
				return false, err
			}
			preparation.lastStorageBytes += bytes
		}
		frame.kind, frame.count = 'o', len(value)
		frame.heap.allocation(1, 64)
		frame.heap.allocation(int64(max(8, len(value))), int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8)
		frame.iterator = reflect.ValueOf(value).MapRange()
	case []any:
		if value != nil && frame.depth >= 10000 {
			return false, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
		}
		frame.kind, frame.length, frame.count = 'a', len(value), cap(value)
		frame.heap.allocation(1, int64(unsafe.Sizeof(value)))
		frame.heap.allocation(int64(cap(value)), int64(unsafe.Sizeof(any(nil))))
	case []map[string]any:
		if value != nil && frame.depth >= 10000 {
			return false, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
		}
		frame.kind, frame.length, frame.count = 'm', len(value), cap(value)
		frame.heap.allocation(1, int64(unsafe.Sizeof(value)))
		frame.heap.allocation(int64(cap(value)), int64(unsafe.Sizeof(map[string]any(nil))))
	case []ContentBlock:
		if value != nil && frame.depth >= 10000 {
			return false, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
		}
		frame.kind, frame.length, frame.count = 'c', len(value), cap(value)
		frame.heap.allocation(1, int64(unsafe.Sizeof(value)))
		frame.heap.allocation(int64(cap(value)), int64(unsafe.Sizeof(ContentBlock{})))
	case *legacyNativeBlockValues:
		if value == nil {
			return true, preparation.pop()
		}
		if frame.depth >= 10000 {
			return false, fmt.Errorf("%w: legacy canonical container depth", ErrValidation)
		}
		frame.kind, frame.length, frame.count = 'd', value.count, value.records.length()
		if frame.length < 0 || frame.length > frame.count {
			return false, ErrIntegrity
		}
		frame.heap.include(value.backing())
	case *legacyCanonicalReplacement:
		return preparation.enterReplacement(encoding, frame, value)
	default:
		return false, fmt.Errorf("%w: unsupported legacy canonical value %T", ErrValidation, frame.value)
	}
	return true, frame.heap.err
}

func (preparation *legacyCanonicalPreparation) stepMap(ctx context.Context, encoding *LegacyCanonicalEncoding) (bool, error) {
	frame := preparation.frames.at(preparation.frames.length() - 1)
	if frame.phase == 0 {
		if !preparation.reserve(int(unsafe.Sizeof(legacyCanonicalKeyField{}))) {
			return false, nil
		}
		var storageBytes int
		var err error
		if frame.prepare {
			storageBytes, err = encoding.mapKeys.at(frame.mapPosition).fields.appendBytes()
		} else {
			storageBytes, err = preparation.frames.appendBytes()
		}
		if err != nil || !preparation.reserve(storageBytes) {
			return false, err
		}
		key, value, more, err := frame.nextMapValue()
		if err != nil {
			return false, err
		}
		if more {
			if frame.position >= frame.count {
				return false, ErrIntegrity
			}
			frame.heap.allocation(int64(len(key)), 1)
			if frame.heap.err != nil {
				return false, frame.heap.err
			}
			frame.position++
			if frame.prepare {
				keys := encoding.mapKeys.at(frame.mapPosition)
				if err := keys.fields.append(legacyCanonicalKeyField{key: key, value: value}); err != nil {
					return false, err
				}
				bytes, err := keys.fields.ownedBytes()
				if err != nil || bytes < keys.heap {
					return false, ErrIntegrity
				}
				encoding.pendingKeysHeap.include(docRangeHeaderHeapEstimate{bytes: bytes - keys.heap})
				keys.heap = bytes
				if encoding.pendingKeysHeap.err != nil {
					return false, encoding.pendingKeysHeap.err
				}
				frame.keyBytes = min(1025, frame.keyBytes+min(1025, len(key)))
			} else {
				depth := frame.depth
				if err := preparation.frames.append(legacyCanonicalPreparationFrame{value: value, depth: depth + 1}); err != nil {
					return false, err
				}
			}
			preparation.lastStorageBytes += storageBytes
			return true, nil
		}
		if frame.position != frame.count || len(frame.value.(map[string]any)) != frame.count {
			return false, ErrIntegrity
		}
		frame.iterator = nil
		frame.mapResume = nil
		if !frame.prepare {
			return true, preparation.pop()
		}
		frame.phase, frame.position = 1, 0
		return true, nil
	}
	if frame.phase == 1 {
		keys := encoding.mapKeys.at(frame.mapPosition)
		if keys.fields.length() <= 8 && frame.keyBytes <= 1024 {
			actions := keys.fields.length() * keys.fields.length()
			bytes := 2 * keys.fields.length() * frame.keyBytes
			if actions > legacyCanonicalEncodingActions-preparation.lastActions || !preparation.reserve(bytes) {
				return false, nil
			}
			preparation.lastActions += actions
			encoding.inspectWork += uint64(actions)
			for position := 1; position < keys.fields.length(); position++ {
				for cursor := position; cursor > 0 && keys.fields.at(cursor).key < keys.fields.at(cursor-1).key; cursor-- {
					left, right := keys.fields.at(cursor), keys.fields.at(cursor-1)
					*left, *right = *right, *left
				}
			}
			keys.sorted = true
		}
		frame.phase = 2
		return true, nil
	}
	keys := encoding.mapKeys.at(frame.mapPosition)
	if frame.position == keys.fields.length() {
		keys.endKeys = encoding.mapKeys.length()
		return true, preparation.pop()
	}
	if !preparation.reserve(int(unsafe.Sizeof(legacyCanonicalKeyField{}))) {
		return false, nil
	}
	storageBytes, err := preparation.frames.appendBytes()
	if err != nil || !preparation.reserve(storageBytes) {
		return false, err
	}
	child := keys.fields.at(frame.position)
	child.childKeys = encoding.mapKeys.length()
	value, depth := child.value, frame.depth
	frame.position++
	if err := preparation.frames.append(legacyCanonicalPreparationFrame{value: value, depth: depth + 1, prepare: true}); err != nil {
		return false, err
	}
	preparation.lastStorageBytes += storageBytes
	return true, nil
}

func (preparation *legacyCanonicalPreparation) step(ctx context.Context, encoding *LegacyCanonicalEncoding) (bool, error) {
	preparation.lastActions, preparation.lastBytes, preparation.lastStorageBytes = 0, 0, 0
	if preparation.heap.err != nil {
		return false, preparation.heap.err
	}
	if (preparation.frames.length() == 0) != preparation.complete {
		return false, ErrIntegrity
	}
	for preparation.frames.length() != 0 && preparation.lastActions < legacyCanonicalEncodingActions {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		preparation.lastActions++
		encoding.inspectWork++
		frame := preparation.frames.at(preparation.frames.length() - 1)
		if frame.kind == 0 {
			if !preparation.reserve(int(unsafe.Sizeof(any(nil)))) {
				preparation.lastActions--
				encoding.inspectWork--
				break
			}
			advanced, err := preparation.enter(ctx, encoding)
			if err != nil {
				return false, err
			}
			if !advanced {
				preparation.lastActions--
				encoding.inspectWork--
				break
			}
			continue
		}
		if frame.kind == 'o' || frame.kind == 'r' {
			var advanced bool
			var err error
			if frame.kind == 'r' {
				advanced, err = preparation.stepReplacement(ctx, encoding)
			} else {
				advanced, err = preparation.stepMap(ctx, encoding)
			}
			if err != nil {
				return false, err
			}
			if !advanced {
				preparation.lastActions--
				encoding.inspectWork--
				break
			}
			continue
		}
		if frame.position == frame.count {
			if err := preparation.pop(); err != nil {
				return false, err
			}
			continue
		}
		if !preparation.reserve(int(unsafe.Sizeof(ContentBlock{}))) {
			preparation.lastActions--
			encoding.inspectWork--
			break
		}
		storageBytes := 0
		if frame.kind != 'c' && frame.kind != 'd' {
			var err error
			storageBytes, err = preparation.frames.appendBytes()
			if err != nil {
				return false, err
			}
			if !preparation.reserve(storageBytes) {
				preparation.lastActions--
				encoding.inspectWork--
				break
			}
		}
		index, depth, prepare := frame.position, frame.depth, frame.prepare && frame.position < frame.length
		frame.position++
		if frame.kind == 'c' || frame.kind == 'd' {
			var block ContentBlock
			if frame.kind == 'c' {
				block = frame.value.([]ContentBlock)[:frame.count][index]
			} else {
				record := frame.value.(*legacyNativeBlockValues).records.at(index)
				if record == nil {
					return false, ErrIntegrity
				}
				block = record.block
			}
			heap, err := encoding.inspectContentBlock(ctx, block, depth+1, prepare)
			if err != nil {
				return false, err
			}
			frame.heap.include(heap)
			if frame.heap.err != nil {
				return false, frame.heap.err
			}
			continue
		}
		var value any
		if frame.kind == 'a' {
			value = frame.value.([]any)[:frame.count][index]
		} else {
			value = frame.value.([]map[string]any)[:frame.count][index]
		}
		if err := preparation.frames.append(legacyCanonicalPreparationFrame{value: value, depth: depth + 1, prepare: prepare}); err != nil {
			return false, err
		}
		preparation.lastStorageBytes += storageBytes
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return preparation.frames.length() == 0, preparation.heap.err
}

func (preparation *legacyCanonicalPreparation) retained(heap *docRangeHeaderHeapEstimate) {
	if preparation.heap.err != nil || (preparation.frames.length() == 0) != preparation.complete {
		heap.include(docRangeHeaderHeapEstimate{err: ErrIntegrity})
		return
	}
	heap.allocation(1, int64(unsafe.Sizeof(*preparation)))
	preparation.frames.retained(heap)
	for position := 0; position < preparation.frames.length(); position++ {
		frame := preparation.frames.at(position)
		if frame.iterator != nil {
			heap.allocation(1, int64(unsafe.Sizeof(reflect.MapIter{})))
		}
		if frame.mapResume != nil {
			heap.allocation(1, int64(unsafe.Sizeof(*frame.mapResume)))
			frame.mapResume.fields.retained(heap)
		}
	}
}
