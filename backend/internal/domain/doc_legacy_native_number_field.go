package domain

import (
	"context"
	"reflect"
)

func (native *legacyNativeObject) numberField(field legacyNativeObjectField) bool {
	return field.interfaceString && native.valueEnd-native.valueStart > LegacyCanonicalEncodingBytes && native.scalarStringField(field) &&
		(native.valueFirst == '-' || native.valueFirst >= '0' && native.valueFirst <= '9')
}

func (native *legacyNativeObject) decodeNumber(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if native.number == nil {
		var err error
		native.number, err = newLegacyNativeNumberValue(native.valueStart, native.valueEnd, input.numberDiagnostics)
		if err != nil {
			return false, err
		}
	}
	complete, err := native.number.step(ctx, input)
	if !complete {
		return false, err
	}
	field := native.metadata.fields[native.field]
	value := native.destination.Elem().Field(field.index)
	if native.number.rejected {
		if native.number.semantic != nil {
			native.number.semantic.Struct, native.number.semantic.Field = native.metadata.model.Name(), field.name
			native.number.semantic.Offset += int64(len(field.prefix))
		}
		native.number = nil
		return true, err
	} else {
		value.Set(reflect.ValueOf(native.number.value))
		native.clearPublishedInitialAlias()
	}
	native.number = nil
	if heapErr := native.refreshDecodedFieldHeap(ctx); heapErr != nil {
		return false, heapErr
	}
	return true, err
}
