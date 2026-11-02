package domain

import (
	"context"
	"encoding/json"
	"reflect"
)

func (native *legacyNativeObject) wrongNumberField(field legacyNativeObjectField) bool {
	return field.scalarString && native.metadata.model.Field(field.index).Type != reflect.TypeFor[json.Number]() &&
		native.valueEnd-native.valueStart > LegacyCanonicalEncodingBytes &&
		(native.valueFirst == '-' || native.valueFirst >= '0' && native.valueFirst <= '9')
}

func (native *legacyNativeObject) decodeWrongNumber(ctx context.Context, input *legacyNormalizationInput) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if input == nil {
		return false, ErrIntegrity
	}
	if input.failure != nil {
		return false, input.failure
	}
	if input.unbounded || native.phase != legacyNativeObjectApply || native.valueStart < 0 ||
		native.valueEnd > input.source.Bytes || native.position != native.valueEnd {
		return false, ErrIntegrity
	}
	field := native.metadata.fields[native.field]
	if !native.wrongNumberField(field) {
		return false, ErrIntegrity
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return true, &json.UnmarshalTypeError{
		Value: "number", Type: native.metadata.model.Field(field.index).Type,
		Offset: int64(len(field.prefix)) + int64(native.valueEnd-native.valueStart),
		Struct: native.metadata.model.Name(), Field: field.name,
	}
}
