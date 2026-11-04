package domain

import (
	"context"
	"reflect"
)

func (native *legacyNativeObject) genericContainerField(field legacyNativeObjectField, first byte) bool {
	if first != '{' && first != '[' {
		return false
	}
	if field.genericMap {
		value := native.destination.Elem().Field(field.index)
		return first == '{' && (value.IsNil() || native.containers != nil && native.containers[native.field] != nil)
	}
	return field.interfaceString && native.scalarStringField(field)
}

func (native *legacyNativeObject) decodeAnyContainer(ctx context.Context, input *legacyNormalizationInput, frame *legacyJSONValueScanner) (bool, error) {
	if native.containers == nil {
		native.containers = make([]*legacyNativeAny, len(native.metadata.fields))
	}
	field := native.metadata.fields[native.field]
	value := native.destination.Elem().Field(field.index)
	parser := native.containers[native.field]
	if !native.containerActive {
		var err error
		if field.genericMap && parser != nil {
			err = parser.start(native.valueStart, native.valueEnd, native.parentDepth, native.fieldActionLimit())
		} else {
			previous := parser
			parser, err = newLegacyNativeAny(native.valueStart, native.valueEnd, native.parentDepth, native.fieldActionLimit())
			if err == nil && previous != nil {
				parser.visible = previous.result.owner
			}
		}
		if err != nil {
			return false, err
		}
		native.containers[native.field], native.containerActive = parser, true
	}
	complete, err := parser.step(ctx, input, frame)
	if field.genericMap && parser.rootMap != nil {
		value.Set(reflect.ValueOf(parser.rootMap.values()))
	}
	if !complete {
		return false, err
	}
	if field.genericMap {
		value.Set(reflect.ValueOf(parser.result.value.(map[string]any)))
	} else {
		value.Set(reflect.ValueOf(parser.result.value))
		native.clearPublishedInitialAlias()
	}
	heap := parser.result.heap
	if !field.genericMap {
		heap.allocation(1, int64(value.Type().Size()))
	}
	if heap.err != nil {
		return false, heap.err
	}
	native.heaps[native.field], native.backingExcess[native.field] = heap, parser.result.excess
	parser.visible = legacyNativeAnyValueOwner{}
	native.position, native.valueEnd, native.containerActive = parser.position, parser.position, false
	if parser.semanticError != nil && !parser.errorContextSet {
		parser.semanticError.Struct = native.metadata.model.Name()
		parser.semanticError.Field = field.name
		parser.semanticError.Offset += int64(len(field.prefix))
		parser.errorContextSet = true
	}
	return true, err
}
