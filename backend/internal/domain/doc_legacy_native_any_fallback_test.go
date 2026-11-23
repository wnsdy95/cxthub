package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeAnyFallbackNamedMap map[string]any

type legacyNativeAnyFallbackJSONMap map[string]any

var errLegacyNativeAnyFallbackJSON = errors.New("any fallback map JSON rejection")

func (value *legacyNativeAnyFallbackJSONMap) UnmarshalJSON(raw []byte) error {
	if *value == nil {
		*value = make(legacyNativeAnyFallbackJSONMap)
	}
	calls, _ := (*value)["calls"].([]string)
	(*value)["calls"] = append(calls, string(raw))
	(*value)["raw"] = string(raw)
	if string(raw) == `"reject"` {
		return errLegacyNativeAnyFallbackJSON
	}
	return nil
}

type legacyNativeAnyFallbackCase struct {
	name            string
	fieldType       reflect.Type
	tag             reflect.StructTag
	initial         func() any
	raw             string
	rejected        bool
	preservePointer bool
	wantCalls       []string
	wantError       error
}

func TestLegacyNativeObjectAnyFallbackStringOptionParity(test *testing.T) {
	for _, fixture := range []legacyNativeAnyFallbackCase{
		{name: "exact-map-string-option-keeps-native-map", fieldType: reflect.TypeFor[map[string]any](), raw: `{"value":{"number":7,"nested":[]}}`},
		{name: "exact-map-string-option-rejects-encoded-object", fieldType: reflect.TypeFor[map[string]any](), raw: `{"value":"{\"number\":7}"}`, rejected: true},
		{name: "named-map-string-option-keeps-native-map", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), raw: `{"value":{"number":7}}`},
		{name: "typed-map-string-option-keeps-element-errors", fieldType: reflect.TypeFor[map[string]string](), raw: `{"value":{"wrong":7}}`, rejected: true},
		{name: "any-string-option-keeps-object", raw: `{"value":{"number":7,"nested":[]}}`},
		{name: "any-string-option-keeps-array", raw: `{"value":[{},1,null]}`},
		{name: "any-string-option-does-not-parse-encoded-object", raw: `{"value":"{\"number\":7}"}`},
		{name: "any-string-option-keeps-map-pointer", initial: func() any { value := map[string]any{"seed": true}; return &value }, raw: `{"value":{"fresh":1}}`, preservePointer: true},
		{name: "any-string-option-keeps-nil-pointer-replacement", initial: func() any { return (*legacyNativeAnyFallbackJSONMap)(nil) }, raw: `{"value":{"fresh":1}}`},
		{name: "custom-map-string-option-receives-raw-object", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":{ "overflow":1e999 }}`, wantCalls: []string{`{ "overflow":1e999 }`}},
		{name: "custom-map-string-option-receives-quoted-bytes", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":"\u0061"}`, wantCalls: []string{`"\u0061"`}},
		{name: "any-custom-pointer-string-option-calls-decoder", initial: func() any { return new(legacyNativeAnyFallbackJSONMap) }, raw: `{"value":[1e999]}`, preservePointer: true, wantCalls: []string{`[1e999]`}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, tag := range []reflect.StructTag{`json:"value,string"`, `json:"value,omitempty,string"`} {
				test.Run(strings.TrimPrefix(string(tag), `json:`), func(test *testing.T) {
					fixture.tag = tag
				})
			}
		})
	}
}
