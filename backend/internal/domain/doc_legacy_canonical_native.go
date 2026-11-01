package domain

import (
	"context"
	"reflect"
	"unsafe"
)

func newLegacyCanonicalEncodingNative(ctx context.Context, owner *legacyNativeCanonicalOwner) (*LegacyCanonicalEncoding, error) {
	event, heap, err := owner.claim(ctx)
	if err != nil {
		return nil, err
	}
	value, err := legacyCanonicalNativeEventViewJSONObject(event, owner.replacement, owner.blocks)
	if err != nil {
		owner.release()
		return nil, err
	}
	heap.include(legacyCanonicalNativeProjectionHeap(event, value))
	if heap.err != nil {
		owner.release()
		return nil, heap.err
	}
	return finishLegacyCanonicalEncodingNative(ctx, owner, value, heap)
}

func finishLegacyCanonicalEncodingNative(ctx context.Context, owner *legacyNativeCanonicalOwner, value map[string]any, heap docRangeHeaderHeapEstimate) (*LegacyCanonicalEncoding, error) {
	encoding := &LegacyCanonicalEncoding{root: value, rootHeap: heap, nativeOwner: owner, initialized: true}
	encoding.preparation = newLegacyCanonicalPreparation(value, 2, true)
	if _, err := encoding.RetainedBytes(); err != nil {
		encoding.release()
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		encoding.release()
		return nil, err
	}
	return encoding, nil
}

func legacyCanonicalNativeProjectionHeap(event CIREvent, value map[string]any) docRangeHeaderHeapEstimate {
	var heap docRangeHeaderHeapEstimate
	addMap := func(fields map[string]any) {
		heap.allocation(1, 64)
		heap.allocation(int64(max(8, len(fields))), int64(unsafe.Sizeof("")+unsafe.Sizeof(any(nil)))+8)
		for key, value := range fields {
			heap.allocation(int64(len(key)), 1)
			if value != nil {
				heap.allocation(1, int64(reflect.TypeOf(value).Size()))
			}
		}
	}
	addMap(value)
	for key, field := range value {
		if replacement, ok := field.(*legacyCanonicalReplacement); ok {
			heap.allocation(1, int64(unsafe.Sizeof(*replacement)))
		}
		if key == "provider_metadata" || key == "locked" || key == "input" && event.Input == nil {
			if nested, typed := field.(map[string]any); typed {
				addMap(nested)
			}
		}
	}
	return heap
}

func legacyCanonicalInitialState(encoding *LegacyCanonicalEncoding) bool {
	if encoding == nil || !encoding.initialized || encoding.complete || encoding.err != nil || encoding.nativeOwner == nil ||
		encoding.nativeOwner.seal != nil || encoding.preparation == nil || encoding.inspectWork != 0 || encoding.keyWork != 0 ||
		encoding.mapKeys.length() != 0 || encoding.frames.length() != 0 || encoding.ordering || encoding.eagerNumbers ||
		encoding.pendingKeysHeap != (docRangeHeaderHeapEstimate{}) || encoding.numbers.pending.length() != 0 || encoding.numbers.proven.length() != 0 ||
		encoding.numbers.next != 0 || encoding.numbers.position != 0 || encoding.numbers.mode != legacyJSONValueIdle {
		return false
	}
	preparation := encoding.preparation
	if preparation.complete || preparation.heap != (docRangeHeaderHeapEstimate{}) || preparation.frames.length() != 1 ||
		preparation.lastActions != 0 || preparation.lastBytes != 0 || preparation.lastStorageBytes != 0 {
		return false
	}
	frame := preparation.frames.at(0)
	if frame == nil || frame.kind != 0 || frame.phase != 0 || frame.depth != 2 || !frame.prepare || frame.position != 0 ||
		frame.length != 0 || frame.count != 0 || frame.mapPosition != 0 || frame.keyBytes != 0 || frame.iterator != nil || frame.mapResume != nil ||
		frame.heap != (docRangeHeaderHeapEstimate{}) {
		return false
	}
	root, rootOK := encoding.root.(map[string]any)
	value, valueOK := frame.value.(map[string]any)
	return rootOK && valueOK && root != nil && reflect.ValueOf(root).Pointer() == reflect.ValueOf(value).Pointer()
}

func (encoding *LegacyCanonicalEncoding) finishPreparation(ctx context.Context) ([]byte, bool, error) {
	if err := ctx.Err(); err != nil {
		return encoding.fail(err)
	}
	return nil, false, nil
}
