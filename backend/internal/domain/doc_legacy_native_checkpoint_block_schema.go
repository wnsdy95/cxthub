package domain

import "reflect"

type legacyNativeCheckpointBlockSchemaBinding struct {
	model    reflect.Type
	fields   [2]legacyNativeObjectField
	keyBytes int
	rawBytes int
	schema   ContentHash
	err      error
}

var legacyNativeCheckpointBlockSchemaCompiled = func() legacyNativeCheckpointBlockSchemaBinding {
	metadata := newLegacyNativeObjectFields(reflect.TypeFor[ContentBlock]())
	binding := legacyNativeCheckpointBlockSchemaBinding{
		model: metadata.model, keyBytes: metadata.keyBytes, rawBytes: metadata.rawBytes,
	}
	if metadata.err != nil || len(metadata.fields) != len(binding.fields) || metadata.model.NumField() != len(binding.fields) {
		binding.err = ErrIntegrity
		return binding
	}
	for ordinal, field := range metadata.fields {
		if field.index != ordinal || !field.scalarString || metadata.model.Field(field.index).Type != reflect.TypeFor[string]() {
			binding.err = ErrIntegrity
			return binding
		}
		binding.fields[ordinal] = field
	}
	binding.schema, binding.err = legacyNativeCheckpointModelSchema(&metadata)
	return binding
}()

func legacyNativeCheckpointBlockSchema() (ContentHash, error) {
	binding := &legacyNativeCheckpointBlockSchemaCompiled
	metadata := &legacyContentBlockNativeFields
	if binding.err != nil || metadata.err != nil || metadata.model != binding.model ||
		len(metadata.fields) != len(binding.fields) || metadata.keyBytes != binding.keyBytes || metadata.rawBytes != binding.rawBytes {
		return "", ErrIntegrity
	}
	for ordinal, field := range binding.fields {
		if metadata.fields[ordinal] != field {
			return "", ErrIntegrity
		}
	}
	return binding.schema, nil
}
