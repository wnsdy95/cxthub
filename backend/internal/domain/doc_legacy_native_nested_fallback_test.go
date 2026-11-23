package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeNestedFallbackLocked LockedBlob

type legacyNativeNestedFallbackMetadata ProviderMetadata

type legacyNativeNestedFallbackStringPointer *string

var errLegacyNativeNestedFallbackJSON = errors.New("nested fallback JSON rejection")
var errLegacyNativeNestedFallbackText = errors.New("nested fallback text rejection")

type legacyNativeNestedFallbackJSON struct {
	Blob  string   `json:"blob"`
	Calls []string `json:"-"`
}

func (value *legacyNativeNestedFallbackJSON) UnmarshalJSON(raw []byte) error {
	value.Calls = append(value.Calls, string(raw))
	if string(raw) == `"reject"` {
		return errLegacyNativeNestedFallbackJSON
	}
	if len(raw) > 0 && raw[0] == '"' {
		var decoded string
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return err
		}
		value.Blob = "json:" + decoded
		return nil
	}
	type plain legacyNativeNestedFallbackJSON
	return json.Unmarshal(raw, (*plain)(value))
}

func (value *legacyNativeNestedFallbackJSON) UnmarshalText(raw []byte) error {
	value.Calls = append(value.Calls, "text:"+string(raw))
	value.Blob = "text:" + string(raw)
	return nil
}

type legacyNativeNestedFallbackText struct {
	Blob  string   `json:"blob"`
	Calls []string `json:"-"`
}

func (value *legacyNativeNestedFallbackText) UnmarshalText(raw []byte) error {
	value.Calls = append(value.Calls, string(raw))
	if string(raw) == "reject" {
		return errLegacyNativeNestedFallbackText
	}
	value.Blob = "text:" + string(raw)
	return nil
}

type legacyNativeNestedFallbackCase struct {
	name            string
	fieldType       reflect.Type
	tag             reflect.StructTag
	raw             string
	initial         func() any
	rejected        bool
	preservePointer bool
	wantCalls       []string
	wantError       error
}

func TestLegacyNativeObjectNestedFallbackStringOptionParity(test *testing.T) {
	for _, fixture := range []legacyNativeNestedFallbackCase{
		{name: "string-pointer-requires-inner-quotes", fieldType: reflect.TypeFor[*string](), raw: `{"value":"\"fresh\""}`},
		{name: "string-pointer-rejects-missing-inner-quotes", fieldType: reflect.TypeFor[*string](), raw: `{"value":"fresh"}`, rejected: true},
		{name: "integer-pointer-decodes-quoted-number", fieldType: reflect.TypeFor[*int](), raw: `{"value":"7"}`},
		{name: "integer-pointer-rejects-unquoted-number", fieldType: reflect.TypeFor[*int](), raw: `{"value":7}`, rejected: true},
		{name: "boolean-pointer-decodes-quoted-boolean", fieldType: reflect.TypeFor[*bool](), raw: `{"value":"true"}`},
		{name: "number-pointer-keeps-quoted-lexeme", fieldType: reflect.TypeFor[*json.Number](), raw: `{"value":"123.40e-2"}`},
		{name: "quoted-null-resets-scalar-pointer", fieldType: reflect.TypeFor[*string](), initial: func() any { value := "seed"; return &value }, raw: `{"value":"null"}`},
		{name: "named-pointer-string-option-is-ignored", fieldType: reflect.TypeFor[legacyNativeNestedFallbackStringPointer](), raw: `{"value":"fresh"}`},
		{name: "struct-pointer-string-option-is-ignored", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackLocked](), raw: `{"value":{"blob":"fresh"}}`},
		{name: "metadata-pointer-string-option-is-ignored", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackMetadata](), raw: `{"value":{"turn_id":"fresh","create_time":123.40}}`},
		{name: "json-struct-string-option-is-ignored", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), raw: `{"value":"fresh"}`, wantCalls: []string{`"fresh"`}},
		{name: "text-struct-string-option-is-ignored", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), raw: `{"value":"fresh"}`, wantCalls: []string{"fresh"}},
		{name: "interface-string-option-keeps-typed-pointer", fieldType: reflect.TypeFor[any](), initial: func() any { return new(LockedBlob) }, raw: `{"value":{"blob":"fresh"}}`, preservePointer: true},
		{name: "json-string-alias-still-unquotes-twice", fieldType: reflect.TypeFor[*legacyNativeMetadataJSONAlias](), raw: `{"value":"\"fresh\""}`},
		{name: "text-string-alias-still-unquotes-twice", fieldType: reflect.TypeFor[*legacyNativeMetadataTextAlias](), raw: `{"value":"\"fresh\""}`},
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
