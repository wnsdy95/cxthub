package domain

import (
	"context"
	"reflect"
)

var legacyLockedNativeFields = newLegacyNativeObjectFields(reflect.TypeFor[LockedBlob]())
var legacyProviderNativeFields = newLegacyNativeObjectFields(reflect.TypeFor[ProviderMetadata]())

func legacyNativeNestedMetadata(model reflect.Type) *legacyNativeObjectFields {
	switch model {
	case reflect.TypeFor[*LockedBlob]():
		return &legacyLockedNativeFields
	case reflect.TypeFor[*ProviderMetadata]():
		return &legacyProviderNativeFields
	default:
		return nil
	}
}

func (native *legacyNativeObject) decodeNestedObject(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	if native.nested == nil {
		native.nested = make([]*legacyNativeObject, len(native.metadata.fields))
	}
	field := native.metadata.fields[native.field]
	value := native.destination.Elem().Field(field.index)
	child := native.nested[native.field]
	if !native.nestedActive {
		if child == nil {
			metadata := legacyNativeNestedMetadata(value.Type())
			if metadata == nil || metadata.err != nil {
				return false, ErrIntegrity
			}
			target := value
			if value.IsNil() {
				target = reflect.New(value.Type().Elem())
			}
			var err error
			child, err = newLegacyNativeObject(target, metadata, native.parentDepth+1, false)
			if err != nil {
				return false, err
			}
			child.mutation = native.mutation
			for index, member := range metadata.fields {
				heap, err := legacyNativeValueHeap(ctx, target.Elem().Field(member.index))
				if err != nil {
					return false, err
				}
				child.heaps[index] = heap
			}
			if err := ctx.Err(); err != nil {
				return false, err
			}
			if value.IsNil() {
				value.Set(target)
			}
			native.nested[native.field] = child
		}
		if err := child.start(native.valueStart, native.valueEnd); err != nil {
			return false, err
		}
		child.prefix = true
		child.deferTypeErrors = true
		child.fieldLimit = native.fieldActionLimit()
		native.nestedActive = true
	}
	complete, err := child.step(ctx, input, frame)
	if err != nil || !complete {
		return false, err
	}
	excess, err := child.backingExcessBytes(nil)
	if err != nil {
		return false, err
	}
	heap, err := legacyNativeValueHeap(ctx, value)
	if err != nil {
		return false, err
	}
	heap.include(docRangeHeaderHeapEstimate{bytes: excess})
	if heap.err != nil {
		return false, heap.err
	}
	native.heaps[native.field], native.backingExcess[native.field] = heap, excess
	native.position, native.valueEnd = child.position, child.position
	native.nestedActive = false
	return true, nil
}
