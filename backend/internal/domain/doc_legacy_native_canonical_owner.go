package domain

import (
	"context"
	"reflect"
)

type legacyNativeCanonicalOwner struct {
	event       CIREvent
	replacement *legacyNativeEventValues
	blocks      *legacyNativeBlockValues
	heap        docRangeHeaderHeapEstimate
	seal        *legacyNativeCanonicalOwner
}

func (native *legacyNormalizationNativeEvent) takeCanonicalOwner(ctx context.Context) (*legacyNativeCanonicalOwner, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if native == nil || !native.canonicalReady || native.object == nil {
		return nil, ErrIntegrity
	}
	if err := native.object.mutation.enter(); err != nil {
		return nil, err
	}
	defer native.object.mutation.leave()
	object := native.object
	if object.metadata != &legacyEventNativeFields || object.metadata.err != nil || object.metadata.model != reflect.TypeFor[wire]() ||
		object.destination != reflect.ValueOf(&native.decoded) || object.phase != legacyNativeObjectDone ||
		object.position < 0 || object.position > object.end || object.position != object.end && !object.prefix ||
		!object.trackPresence || object.parentDepth != 3 || object.typeError != nil ||
		object.listActive || object.blockActive || object.eventActive || object.nestedActive || object.containerActive ||
		object.scalar != nil || object.number != nil || object.typedNumber != nil ||
		len(object.heaps) != len(legacyEventNativeFields.fields) || len(object.backingExcess) != len(legacyEventNativeFields.fields) {
		return nil, ErrIntegrity
	}
	var heap docRangeHeaderHeapEstimate
	for index, field := range legacyEventNativeFields.fields {
		value := object.destination.Elem().Field(field.index)
		fieldHeap := object.heaps[index]
		if fieldHeap.err != nil {
			return nil, fieldHeap.err
		}
		if fieldHeap.bytes == 0 {
			if !value.IsZero() {
				return nil, ErrIntegrity
			}
		} else {
			var minimum docRangeHeaderHeapEstimate
			minimum.allocation(1, int64(value.Type().Size()))
			if minimum.err != nil || fieldHeap.bytes < minimum.bytes {
				return nil, ErrIntegrity
			}
		}
		heap.include(fieldHeap)
		if heap.err != nil {
			return nil, heap.err
		}
	}
	replacement, err := object.pagedReplacement()
	if err != nil {
		return nil, err
	}
	blocks, err := object.pagedBlocks()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	event := CIREvent(native.decoded)
	event.fieldPresence = object.presence
	owner := &legacyNativeCanonicalOwner{event: event, replacement: replacement, blocks: blocks, heap: heap}
	owner.seal = owner
	if err := native.releaseValues(); err != nil {
		owner.release()
		return nil, err
	}
	return owner, nil
}

func (owner *legacyNativeCanonicalOwner) claim(ctx context.Context) (CIREvent, docRangeHeaderHeapEstimate, error) {
	if err := ctx.Err(); err != nil {
		return CIREvent{}, docRangeHeaderHeapEstimate{}, err
	}
	if owner == nil || owner.seal != owner {
		return CIREvent{}, docRangeHeaderHeapEstimate{}, ErrIntegrity
	}
	owner.seal = nil
	return owner.event, owner.heap, nil
}

func (owner *legacyNativeCanonicalOwner) release() {
	if owner == nil {
		return
	}
	owner.event = CIREvent{}
	owner.replacement = nil
	owner.blocks = nil
	owner.heap = docRangeHeaderHeapEstimate{}
	owner.seal = nil
}
