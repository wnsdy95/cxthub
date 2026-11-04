package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeNestedFallbackLocked LockedBlob

type legacyNativeNestedFallbackMetadata ProviderMetadata

type legacyNativeNestedFallbackLockedPointer *LockedBlob

type legacyNativeNestedFallbackStringPointer *string

type legacyNativeNestedFallbackEmbedded struct {
	LockedBlob
	Label string `json:"label"`
}

type legacyNativeNestedFallbackEmbeddedPointer struct {
	*LockedBlob
	Label string `json:"label"`
}

type legacyNativeNestedFallbackPlain struct {
	Blob  string `json:"blob"`
	Count int    `json:"count,string"`
}

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

type legacyNativeNestedFallbackEmbeddedJSON struct {
	legacyNativeNestedFallbackJSON
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

func legacyNativeNestedFallbackCalls(value any) []string {
	switch value := value.(type) {
	case legacyNativeNestedFallbackJSON:
		return value.Calls
	case *legacyNativeNestedFallbackJSON:
		if value != nil {
			return value.Calls
		}
	case legacyNativeNestedFallbackText:
		return value.Calls
	case *legacyNativeNestedFallbackText:
		if value != nil {
			return value.Calls
		}
	case *legacyNativeNestedFallbackEmbeddedJSON:
		if value != nil {
			return value.Calls
		}
	}
	return nil
}

func legacyNativeNestedFallbackCheck(test *testing.T, fixture legacyNativeNestedFallbackCase) {
	test.Helper()
	tag := fixture.tag
	if tag == "" {
		tag = `json:"value"`
	}
	model := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: fixture.fieldType, Tag: tag}})
	newDestination := func() reflect.Value {
		destination := reflect.New(model)
		if fixture.initial != nil {
			destination.Elem().Field(0).Set(reflect.ValueOf(fixture.initial()))
		}
		return destination
	}
	expected, actual := newDestination(), newDestination()
	expectedInitial := expected.Elem().Field(0).Interface()
	actualInitial := actual.Elem().Field(0).Interface()
	raw := []byte(fixture.raw)
	expectedErr := json.Unmarshal(raw, expected.Interface())
	if (expectedErr != nil) != fixture.rejected {
		test.Fatal("fixture changed native acceptance", fixture.rejected, expectedErr)
	}
	presence, actualErr := legacyNativeMetadataDecode(test, raw, actual.Interface(), false)
	if reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) || presence != 0 {
		test.Fatal("fallback changed native error type or disabled presence", actualErr, expectedErr, presence)
	}
	if expectedErr != nil && actualErr.Error() != expectedErr.Error() {
		test.Fatal("fallback changed native error details", actualErr, expectedErr)
	}
	if fixture.wantError != nil && (!errors.Is(expectedErr, fixture.wantError) || !errors.Is(actualErr, fixture.wantError)) {
		test.Fatal("fallback lost the custom unmarshaler error", actualErr, expectedErr, fixture.wantError)
	}
	if !reflect.DeepEqual(actual.Interface(), expected.Interface()) {
		test.Fatalf("fallback changed native values or call history: got=%#v want=%#v", actual.Interface(), expected.Interface())
	}
	if !reflect.DeepEqual(actualInitial, expectedInitial) {
		test.Fatal("fallback changed mutations through the original pointer", actualInitial, expectedInitial)
	}
	if fixture.preservePointer && (actual.Elem().Field(0).Interface() != actualInitial || expected.Elem().Field(0).Interface() != expectedInitial) {
		test.Fatal("fallback replaced a pointer preserved by encoding/json")
	}
	if calls := legacyNativeNestedFallbackCalls(actual.Elem().Field(0).Interface()); !reflect.DeepEqual(calls, fixture.wantCalls) {
		test.Fatal("fallback changed custom call count, order, or argument bytes", calls, fixture.wantCalls)
	}
}

func TestLegacyNativeObjectNestedFallbackMetadata(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		fieldType reflect.Type
		value     string
		initial   func() any
	}{
		{name: "named-locked-struct", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackLocked](), value: `{"blob":"opaque","scheme":"signature"}`},
		{name: "named-provider-metadata-struct", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackMetadata](), value: `{"turn_id":"turn","create_time":123.40}`},
		{name: "named-pointer-to-real-locked", fieldType: reflect.TypeFor[legacyNativeNestedFallbackLockedPointer](), value: `{"blob":"opaque"}`},
		{name: "json-unmarshaler-struct", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), value: `{"blob":"opaque"}`},
		{name: "text-unmarshaler-struct", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), value: `"opaque"`},
		{name: "anonymous-embedded-struct", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackEmbedded](), value: `{"blob":"opaque","label":"kept"}`},
		{name: "anonymous-embedded-pointer", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackEmbeddedPointer](), value: `{"blob":"opaque","label":"kept"}`},
		{name: "promoted-json-unmarshaler", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackEmbeddedJSON](), value: `{"blob":"opaque"}`},
		{name: "arbitrary-struct-pointer", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackPlain](), value: `{"blob":"opaque","count":"7"}`},
		{name: "double-pointer-to-real-locked", fieldType: reflect.TypeFor[**LockedBlob](), value: `{"blob":"opaque"}`},
		{name: "string-pointer", fieldType: reflect.TypeFor[*string](), value: `"opaque"`},
		{name: "slice-pointer", fieldType: reflect.TypeFor[*[]string](), value: `["opaque"]`},
		{name: "map-pointer", fieldType: reflect.TypeFor[*map[string]string](), value: `{"blob":"opaque"}`},
		{name: "number-pointer", fieldType: reflect.TypeFor[*json.Number](), value: `123.40`},
		{name: "interface-holding-real-locked", fieldType: reflect.TypeFor[any](), initial: func() any { return new(LockedBlob) }, value: `{"blob":"opaque"}`},
		{name: "interface-holding-real-metadata", fieldType: reflect.TypeFor[any](), initial: func() any { return new(ProviderMetadata) }, value: `{"turn_id":"turn"}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			model := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: fixture.fieldType, Tag: `json:"locked"`}})
			metadata := newLegacyNativeObjectFields(model)
			if metadata.err != nil || len(metadata.fields) != 1 {
				test.Fatal("ineligible child type poisoned parent metadata", metadata.err)
			}
			field := metadata.fields[0]
			if field.index != 0 || field.name != "locked" || field.prefix != `{"locked":` || field.scalarString || field.stringList || field.blockList ||
				field.interfaceString != (fixture.fieldType == reflect.TypeFor[any]()) {
				test.Fatal("ineligible pointer changed field metadata", field)
			}
			destination := reflect.New(model)
			if fixture.initial != nil {
				destination.Elem().Field(0).Set(reflect.ValueOf(fixture.initial()))
			}
			native, err := newLegacyNativeObject(destination, &metadata, 1, false)
			if err != nil {
				test.Fatal(err)
			}
			raw := []byte(`{"locked":` + fixture.value + `}`)
			buffer := &legacySourceProofBuffer{body: raw}
			input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input()}
			var frame legacyJSONValueScanner
			native.start(0, len(raw))
			complete, err := native.step(input.ctx, input, &frame)
			if err != nil || input.failure != nil || !complete {
				test.Fatal("small fallback did not complete", complete, err, input.failure)
			}
			if input.semanticBytes != len(fixture.value) || input.fields != 1 {
				test.Fatal("ineligible pointer bypassed whole-field native fallback", input.semanticBytes, len(fixture.value), input.fields)
			}
		})
	}
}

func TestLegacyNativeObjectNestedFallbackPointerParity(test *testing.T) {
	locked := func() any { return &LockedBlob{Provider: "claude", Scheme: "signature", Blob: "seed"} }
	metadata := func() any { return &ProviderMetadata{TurnID: "seed"} }
	for _, fixture := range []legacyNativeNestedFallbackCase{
		{name: "named-locked-merges-duplicate-objects", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackLocked](), tag: `json:"locked"`, raw: `{"locked":{"provider":"claude","blob":"first"},"LOCKED":{"scheme":"signature","blob":null}}`},
		{name: "named-locked-null-resets-before-reallocation", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackLocked](), raw: `{"value":{"provider":"claude","blob":"first"},"value":null,"value":{"scheme":"signature"}}`},
		{name: "named-metadata-merges-duplicate-objects", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackMetadata](), tag: `json:"provider_metadata"`, raw: `{"provider_metadata":{"turn_id":"first"},"PROVIDER_METADATA":{"create_time":123.40,"turn_id":null}}`},
		{name: "named-metadata-null-resets-before-reallocation", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackMetadata](), raw: `{"value":{"turn_id":"first"},"value":null,"value":{"create_time":"12.50"}}`},
		{name: "named-pointer-retains-identity", fieldType: reflect.TypeFor[legacyNativeNestedFallbackLockedPointer](), initial: func() any { return legacyNativeNestedFallbackLockedPointer(locked().(*LockedBlob)) }, raw: `{"value":{"blob":"fresh"},"VALUE":{"scheme":"changed"}}`, preservePointer: true},
		{name: "embedded-fields-follow-native-promotion", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackEmbedded](), raw: `{"value":{"BLOB":"first","label":"kept"},"value":{"scheme":"signature","blob":null}}`},
		{name: "embedded-pointer-allocates-and-merges", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackEmbeddedPointer](), raw: `{"value":{"blob":"first"},"value":{"scheme":"signature","label":"kept"}}`},
		{name: "arbitrary-struct-keeps-child-string-option", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackPlain](), raw: `{"value":{"blob":"first","count":"7"},"VALUE":{"blob":null,"count":"9"}}`},
		{name: "arbitrary-struct-rejects-unquoted-count", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackPlain](), raw: `{"value":{"count":7,"blob":"after-error"}}`, rejected: true},
		{name: "double-pointer-allocates-and-merges", fieldType: reflect.TypeFor[**LockedBlob](), raw: `{"value":{"blob":"first"},"value":{"scheme":"signature"}}`},
		{name: "scalar-pointer-preserves-identity", fieldType: reflect.TypeFor[*string](), initial: func() any { value := "seed"; return &value }, raw: `{"value":"first","VALUE":"last"}`, preservePointer: true},
		{name: "slice-pointer-follows-native-replacement", fieldType: reflect.TypeFor[*[]string](), raw: `{"value":["first","second"],"value":["last"]}`},
		{name: "map-pointer-merges-entries", fieldType: reflect.TypeFor[*map[string]string](), raw: `{"value":{"first":"kept"},"value":{"last":"fresh"}}`},
		{name: "array-pointer-keeps-native-zeroing", fieldType: reflect.TypeFor[*[2]string](), raw: `{"value":["first","second"],"value":["last"]}`},
		{name: "number-pointer-keeps-native-lexeme", fieldType: reflect.TypeFor[*json.Number](), raw: `{"value":123.40e-2}`},
		{name: "interface-real-locked-keeps-concrete-pointer", fieldType: reflect.TypeFor[any](), initial: locked, raw: `{"value":{"blob":"first"},"VALUE":{"scheme":"changed"}}`, preservePointer: true},
		{name: "interface-real-metadata-keeps-concrete-pointer", fieldType: reflect.TypeFor[any](), initial: metadata, raw: `{"value":{"turn_id":"fresh"},"value":{"create_time":123.40}}`, preservePointer: true},
		{name: "interface-real-locked-null-discards-pointer", fieldType: reflect.TypeFor[any](), initial: locked, raw: `{"value":null,"value":{"blob":"fresh"}}`},
		{name: "interface-real-metadata-null-discards-pointer", fieldType: reflect.TypeFor[any](), initial: metadata, raw: `{"value":null,"value":{"turn_id":"fresh"}}`},
		{name: "interface-struct-pointer-rejects-array", fieldType: reflect.TypeFor[any](), initial: locked, raw: `{"value":[]}`, rejected: true, preservePointer: true},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeNestedFallbackCheck(test, fixture) })
	}
}

func TestLegacyNativeObjectNestedFallbackCustomCalls(test *testing.T) {
	jsonPointer := func() any { return &legacyNativeNestedFallbackJSON{Blob: "seed"} }
	textPointer := func() any { return &legacyNativeNestedFallbackText{Blob: "seed"} }
	for _, fixture := range []legacyNativeNestedFallbackCase{
		{name: "json-struct-receives-entire-object-once", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), raw: `{"value":{ "blob":"first", "BLOB":"last", "unknown":1e999 }}`, wantCalls: []string{`{ "blob":"first", "BLOB":"last", "unknown":1e999 }`}},
		{name: "json-takes-priority-over-text-and-keeps-escapes", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), raw: `{"value":"\u0061"}`, wantCalls: []string{`"\u0061"`}},
		{name: "json-duplicate-objects-call-in-order", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), initial: jsonPointer, raw: `{"value":{"blob":"first"},"VALUE":{},"value":{"blob":"last"}}`, preservePointer: true, wantCalls: []string{`{"blob":"first"}`, `{}`, `{"blob":"last"}`}},
		{name: "json-pointer-null-skips-call-and-resets", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), initial: jsonPointer, raw: `{"value":null}`},
		{name: "json-nil-pointer-null-skips-allocation", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), raw: `{"value":null}`},
		{name: "json-null-reallocation-keeps-old-calls-detached", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), initial: jsonPointer, raw: `{"value":{"blob":"first"},"value":null,"value":{"blob":"last"}}`, wantCalls: []string{`{"blob":"last"}`}},
		{name: "json-struct-value-receives-null", fieldType: reflect.TypeFor[legacyNativeNestedFallbackJSON](), initial: func() any { return legacyNativeNestedFallbackJSON{Blob: "seed"} }, raw: `{"value":null}`, wantCalls: []string{"null"}},
		{name: "json-custom-error-is-returned-once", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackJSON](), initial: jsonPointer, raw: `{"value":"reject"}`, rejected: true, preservePointer: true, wantCalls: []string{`"reject"`}, wantError: errLegacyNativeNestedFallbackJSON},
		{name: "promoted-json-method-still-receives-whole-object", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackEmbeddedJSON](), raw: `{"value":{"blob":"fresh"}}`, wantCalls: []string{`{"blob":"fresh"}`}},
		{name: "text-struct-receives-unquoted-decoded-string", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), raw: `{"value":"fresh\n\u2603"}`, wantCalls: []string{"fresh\n☃"}},
		{name: "text-duplicate-strings-call-in-order", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), initial: textPointer, raw: `{"value":"first","VALUE":"last"}`, preservePointer: true, wantCalls: []string{"first", "last"}},
		{name: "text-pointer-null-skips-call-and-resets", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), initial: textPointer, raw: `{"value":null}`},
		{name: "text-struct-value-null-retains-value", fieldType: reflect.TypeFor[legacyNativeNestedFallbackText](), initial: func() any { return legacyNativeNestedFallbackText{Blob: "seed"} }, raw: `{"value":null}`},
		{name: "text-rejects-object-without-calling-method", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), initial: textPointer, raw: `{"value":{"blob":"not-native-text"}}`, rejected: true, preservePointer: true},
		{name: "text-custom-error-is-returned-once", fieldType: reflect.TypeFor[*legacyNativeNestedFallbackText](), initial: textPointer, raw: `{"value":"reject"}`, rejected: true, preservePointer: true, wantCalls: []string{"reject"}, wantError: errLegacyNativeNestedFallbackText},
		{name: "interface-json-pointer-keeps-method-and-identity", fieldType: reflect.TypeFor[any](), initial: jsonPointer, raw: `{"value":{"blob":"first"},"value":"last"}`, preservePointer: true, wantCalls: []string{`{"blob":"first"}`, `"last"`}},
		{name: "interface-text-pointer-keeps-method-and-identity", fieldType: reflect.TypeFor[any](), initial: textPointer, raw: `{"value":"first","value":"last"}`, preservePointer: true, wantCalls: []string{"first", "last"}},
		{name: "interface-json-pointer-null-skips-method", fieldType: reflect.TypeFor[any](), initial: jsonPointer, raw: `{"value":null}`},
		{name: "interface-text-pointer-null-skips-method", fieldType: reflect.TypeFor[any](), initial: textPointer, raw: `{"value":null}`},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeNestedFallbackCheck(test, fixture) })
	}
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
					legacyNativeNestedFallbackCheck(test, fixture)
				})
			}
		})
	}
}
