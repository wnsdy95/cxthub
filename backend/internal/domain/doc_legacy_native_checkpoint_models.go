package domain

import (
	"encoding/json"
	"reflect"
)

const (
	legacyNativeCheckpointEnvelopeModel uint8 = iota + 1
	legacyNativeCheckpointEventModel
	legacyNativeCheckpointBlockModel
	legacyNativeCheckpointLockedModel
	legacyNativeCheckpointProviderModel
)

func legacyNativeCheckpointModelMetadata(model uint8) *legacyNativeObjectFields {
	switch model {
	case legacyNativeCheckpointEnvelopeModel:
		return &legacyEnvelopeNativeFields
	case legacyNativeCheckpointEventModel:
		return &legacyEventNativeFields
	case legacyNativeCheckpointBlockModel:
		return &legacyContentBlockNativeFields
	case legacyNativeCheckpointLockedModel:
		return &legacyLockedNativeFields
	case legacyNativeCheckpointProviderModel:
		return &legacyProviderNativeFields
	default:
		return nil
	}
}

func legacyNativeCheckpointModel(metadata *legacyNativeObjectFields) (uint8, error) {
	if metadata == nil || metadata.err != nil {
		return 0, ErrIntegrity
	}
	for model := uint8(1); model <= legacyNativeCheckpointProviderModel; model++ {
		expected := legacyNativeCheckpointModelMetadata(model)
		if metadata.model != expected.model {
			continue
		}
		if metadata.keyBytes != expected.keyBytes || metadata.rawBytes != expected.rawBytes || len(metadata.fields) != len(expected.fields) {
			return 0, ErrIntegrity
		}
		for index, field := range expected.fields {
			if metadata.fields[index] != field {
				return 0, ErrIntegrity
			}
		}
		return model, nil
	}
	return 0, ErrIntegrity
}

func legacyNativeCheckpointModelSchema(metadata *legacyNativeObjectFields) (ContentHash, error) {
	model, err := legacyNativeCheckpointModel(metadata)
	if err != nil {
		return "", err
	}
	binding := &legacyNativeCheckpointModelSchemas[model-1]
	if binding.err != nil || metadata.model != binding.model || metadata.keyBytes != binding.keyBytes || metadata.rawBytes != binding.rawBytes || len(metadata.fields) != binding.count {
		return "", ErrIntegrity
	}
	for index, field := range metadata.fields {
		if field != binding.fields[index] {
			return "", ErrIntegrity
		}
	}
	return binding.schema, nil
}

type legacyNativeCheckpointModelSchemaBinding struct {
	model    reflect.Type
	fields   [32]legacyNativeObjectField
	count    int
	keyBytes int
	rawBytes int
	schema   ContentHash
	err      error
}

var legacyNativeCheckpointModelSchemas = func() [5]legacyNativeCheckpointModelSchemaBinding {
	var bindings [5]legacyNativeCheckpointModelSchemaBinding
	models := [...]reflect.Type{reflect.TypeFor[CIREnvelope](), reflect.TypeFor[wire](), reflect.TypeFor[ContentBlock](), reflect.TypeFor[LockedBlob](), reflect.TypeFor[ProviderMetadata]()}
	for index, model := range models {
		metadata := newLegacyNativeObjectFields(model)
		binding := &bindings[index]
		binding.model, binding.count = model, len(metadata.fields)
		binding.keyBytes, binding.rawBytes = metadata.keyBytes, metadata.rawBytes
		if metadata.err != nil || binding.count > len(binding.fields) {
			binding.err = ErrIntegrity
			continue
		}
		copy(binding.fields[:], metadata.fields)
		binding.schema, binding.err = legacyNativeCheckpointModelSchemaValue(&metadata, uint8(index+1))
	}
	return bindings
}()

func legacyNativeCheckpointModelSchemaValue(metadata *legacyNativeObjectFields, model uint8) (ContentHash, error) {
	type fieldShape struct {
		Name   string `json:"name"`
		Prefix string `json:"prefix"`
		Index  int    `json:"index"`
		Type   uint8  `json:"type"`
		Flags  uint16 `json:"flags"`
	}
	type modelShape struct {
		Model  uint8        `json:"model"`
		Key    int          `json:"key"`
		Raw    int          `json:"raw"`
		Fields []fieldShape `json:"fields"`
	}
	shape := modelShape{Model: model, Key: metadata.keyBytes, Raw: metadata.rawBytes, Fields: make([]fieldShape, len(metadata.fields))}
	for index, field := range metadata.fields {
		typeID, err := legacyNativeCheckpointErrorTypeID(metadata.model.Field(field.index).Type)
		if err != nil {
			return "", err
		}
		var flags uint16
		for bit, enabled := range []bool{field.scalarString, field.interfaceString, field.stringList, field.blockList, field.eventList, field.fixedScalar, field.nestedObject, field.genericMap, field.typedNumber} {
			if enabled {
				flags |= 1 << bit
			}
		}
		shape.Fields[index] = fieldShape{Name: field.name, Prefix: field.prefix, Index: field.index, Type: typeID, Flags: flags}
	}
	body, err := json.Marshal(shape)
	if err != nil || len(body) > LegacyCanonicalEncodingBytes {
		return "", ErrIntegrity
	}
	return HashContent(body), nil
}

var legacyNativeCheckpointErrorTypes = [...]reflect.Type{
	reflect.TypeFor[string](), reflect.TypeFor[ProviderKind](), reflect.TypeFor[FidelityTier](),
	reflect.TypeFor[EventKind](), reflect.TypeFor[Role](), reflect.TypeFor[int](), reflect.TypeFor[bool](),
	reflect.TypeFor[any](), reflect.TypeFor[map[string]any](), reflect.TypeFor[[]ContentBlock](),
	reflect.TypeFor[[]CIREvent](), reflect.TypeFor[*bool](), reflect.TypeFor[*LockedBlob](),
	reflect.TypeFor[*ProviderMetadata](), reflect.TypeFor[*json.Number](), reflect.TypeFor[json.Number](),
	reflect.TypeFor[wire](), reflect.TypeFor[CIREnvelope](), reflect.TypeFor[ContentBlock](),
	reflect.TypeFor[LockedBlob](), reflect.TypeFor[ProviderMetadata](), reflect.TypeFor[float64](),
	reflect.TypeFor[[]string](), reflect.TypeFor[[]any](),
}

func legacyNativeCheckpointErrorTypeID(model reflect.Type) (uint8, error) {
	for index, expected := range legacyNativeCheckpointErrorTypes {
		if model == expected {
			return uint8(index + 1), nil
		}
	}
	return 0, ErrIntegrity
}

func legacyNativeCheckpointErrorType(identity uint8) (reflect.Type, error) {
	if identity == 0 || int(identity) > len(legacyNativeCheckpointErrorTypes) {
		return nil, ErrIntegrity
	}
	return legacyNativeCheckpointErrorTypes[identity-1], nil
}
