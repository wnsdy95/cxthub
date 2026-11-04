package domain

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeInterfaceMetadataAny interface{}

type legacyNativeInterfaceMetadataStringer interface {
	String() string
}

type legacyNativeInterfaceMetadataString string

func (value legacyNativeInterfaceMetadataString) String() string {
	return string(value)
}

type legacyNativeInterfaceMetadataCase struct {
	name            string
	fieldType       reflect.Type
	tag             reflect.StructTag
	initial         func() any
	raw             string
	rejected        bool
	preservePointer bool
}

func legacyNativeInterfaceMetadataCheck(test *testing.T, fixture legacyNativeInterfaceMetadataCase) {
	test.Helper()
	fieldType := fixture.fieldType
	if fieldType == nil {
		fieldType = reflect.TypeFor[any]()
	}
	tag := fixture.tag
	if tag == "" {
		tag = `json:"value"`
	}
	model := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: fieldType, Tag: tag}})
	newDestination := func() reflect.Value {
		destination := reflect.New(model)
		if fixture.initial != nil {
			if initial := fixture.initial(); initial != nil {
				destination.Elem().Field(0).Set(reflect.ValueOf(initial))
			}
		}
		return destination
	}
	expected, actual := newDestination(), newDestination()
	expectedInitial := expected.Elem().Field(0).Interface()
	actualInitial := actual.Elem().Field(0).Interface()
	raw := []byte(fixture.raw)
	expectedErr := json.Unmarshal(raw, expected.Interface())
	if (expectedErr != nil) != fixture.rejected {
		test.Fatal("fixture lost its native acceptance", fixture.rejected, expectedErr)
	}
	presence, actualErr := legacyNativeMetadataDecode(test, raw, actual.Interface(), false)
	if reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) || presence != 0 {
		test.Fatal("interface field changed native acceptance, error type, or disabled presence", actualErr, expectedErr, presence)
	}
	if !reflect.DeepEqual(actual.Interface(), expected.Interface()) {
		test.Fatalf("interface field changed native decoded values: got type %T, want type %T", actual.Elem().Field(0).Interface(), expected.Elem().Field(0).Interface())
	}
	if !reflect.DeepEqual(actualInitial, expectedInitial) {
		test.Fatal("interface field changed writes through the original concrete value")
	}
	if fixture.preservePointer {
		if expected.Elem().Field(0).Interface() != expectedInitial {
			test.Fatal("fixture no longer preserves its native concrete pointer")
		}
		if actual.Elem().Field(0).Interface() != actualInitial {
			test.Fatal("interface field replaced its concrete pointer instead of decoding through it")
		}
	}
}

func TestLegacyNativeObjectInterfaceMetadataLargeStringParity(test *testing.T) {
	value := `"` + strings.Repeat("x", LegacyCanonicalEncodingBytes+1) +
		`\uD83D\uDE42\uD800x\uDC00\"\\\/\b\f\n\r\t\u0000` + "\ud55c🙂\xff\xc0\x80" + `"`
	raw := `{"value":` + value + `}`
	for _, fixture := range []legacyNativeInterfaceMetadataCase{
		{name: "nil-empty-interface"},
		{name: "named-empty-interface", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataAny]()},
		{name: "existing-string", initial: func() any { return "previous" }},
		{name: "existing-string-alias", initial: func() any { return legacyNativeInterfaceMetadataString("previous") }},
		{name: "existing-number", initial: func() any { return 7 }},
		{name: "existing-map", initial: func() any { return map[string]any{"previous": true} }},
		{name: "existing-slice", initial: func() any { return []any{"previous"} }},
		{name: "typed-nil-string-pointer", initial: func() any { return (*string)(nil) }},
		{name: "typed-nil-json-pointer", initial: func() any { return (*legacyNativeMetadataJSONAlias)(nil) }},
		{name: "typed-nil-text-pointer", initial: func() any { return (*legacyNativeMetadataTextAlias)(nil) }},
		{name: "concrete-string-pointer", initial: func() any { return new(string) }, preservePointer: true},
		{name: "concrete-interface-pointer", initial: func() any { return new(any) }, preservePointer: true},
		{name: "concrete-pointer-to-nil-string-pointer", initial: func() any { return new(*string) }, preservePointer: true},
		{name: "concrete-json-pointer", initial: func() any { return new(legacyNativeMetadataJSONAlias) }, preservePointer: true},
		{name: "concrete-text-pointer", initial: func() any { return new(legacyNativeMetadataTextAlias) }, preservePointer: true},
		{name: "concrete-number-pointer-rejects-string", initial: func() any { return new(int) }, rejected: true, preservePointer: true},
		{name: "empty-interface-string-option-is-ignored", tag: `json:"value,omitempty,string"`},
		{name: "nil-nonempty-interface", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](), rejected: true},
		{
			name: "nonempty-interface-concrete-string", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](),
			initial: func() any { return legacyNativeInterfaceMetadataString("previous") }, rejected: true,
		},
		{
			name: "nonempty-interface-typed-nil-pointer", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](),
			initial: func() any { return (*legacyNativeInterfaceMetadataString)(nil) }, rejected: true,
		},
		{
			name: "nonempty-interface-concrete-string-pointer", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](),
			initial: func() any { return new(legacyNativeInterfaceMetadataString) }, preservePointer: true,
		},
		{name: "nil-json-decoder-interface", fieldType: reflect.TypeFor[json.Unmarshaler](), rejected: true},
		{
			name: "json-decoder-interface", fieldType: reflect.TypeFor[json.Unmarshaler](),
			initial: func() any { return new(legacyNativeMetadataJSONAlias) }, preservePointer: true,
		},
		{
			name: "text-decoder-interface", fieldType: reflect.TypeFor[encoding.TextUnmarshaler](),
			initial: func() any { return new(legacyNativeMetadataTextAlias) }, preservePointer: true,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			fixture.raw = raw
			legacyNativeInterfaceMetadataCheck(test, fixture)
		})
	}
}

func TestLegacyNativeObjectInterfaceMetadataPointerAndNullParity(test *testing.T) {
	stringPointer := func() any {
		value := "previous"
		return &value
	}
	interfacePointer := func() any {
		var value any = "previous"
		return &value
	}
	stringPointerPointer := func() any {
		value := "previous"
		pointer := &value
		return &pointer
	}
	for _, fixture := range []legacyNativeInterfaceMetadataCase{
		{name: "nil-interface-null", raw: `{"value":null}`},
		{name: "typed-nil-pointer-null", initial: func() any { return (*string)(nil) }, raw: `{"value":null}`},
		{name: "concrete-string-pointer-null-clears-interface", initial: stringPointer, raw: `{"value":null}`},
		{name: "concrete-interface-pointer-null-clears-interface", initial: interfacePointer, raw: `{"value":null}`},
		{name: "pointer-to-pointer-null-clears-inner-pointer", initial: stringPointerPointer, raw: `{"value":null}`, preservePointer: true},
		{name: "pointer-to-pointer-allocates-inner-pointer", initial: func() any { return new(*string) }, raw: `{"value":"fresh"}`, preservePointer: true},
		{name: "pointer-to-interface-writes-through", initial: interfacePointer, raw: `{"value":"fresh"}`, preservePointer: true},
		{name: "pointer-null-then-string-replaces-interface", initial: stringPointer, raw: `{"value":null,"value":"fresh"}`},
		{name: "duplicate-string-writes-through-pointer", initial: stringPointer, raw: `{"value":"first","VALUE":"second"}`, preservePointer: true},
		{name: "duplicate-interface-types-and-null", raw: `{"value":{},"value":[1],"value":true,"value":"fresh","value":null}`},
		{name: "nil-direct-string-pointer-allocates", fieldType: reflect.TypeFor[*string](), raw: `{"value":"fresh"}`},
		{name: "direct-string-pointer-null", fieldType: reflect.TypeFor[*string](), initial: stringPointer, raw: `{"value":null}`},
		{name: "concrete-json-pointer-null-clears-interface", initial: func() any { return new(legacyNativeMetadataJSONAlias) }, raw: `{"value":null}`},
		{
			name: "nil-nonempty-interface-null", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](),
			raw: `{"value":null}`,
		},
		{
			name: "nonempty-interface-typed-nil-pointer-null", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](),
			initial: func() any { return (*legacyNativeInterfaceMetadataString)(nil) }, raw: `{"value":null}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeInterfaceMetadataCheck(test, fixture)
		})
	}
}

func TestLegacyNativeObjectInterfaceMetadataFieldOptionsAndDecoders(test *testing.T) {
	for _, fixture := range []legacyNativeInterfaceMetadataCase{
		{name: "empty-interface-string-option", tag: `json:"value,omitempty,string"`, raw: `{"value":"native"}`},
		{name: "empty-interface-string-option-does-not-unquote-twice", tag: `json:"value,string"`, raw: `{"value":"\"native\""}`},
		{name: "empty-interface-string-option-keeps-number", tag: `json:"value,string"`, raw: `{"value":7}`},
		{name: "empty-interface-string-option-keeps-boolean", tag: `json:"value,string"`, raw: `{"value":true}`},
		{name: "quoted-string", fieldType: reflect.TypeFor[string](), tag: `json:"value,omitempty,string"`, raw: `{"value":"\"native\""}`},
		{name: "quoted-string-rejects-missing-inner-quotes", fieldType: reflect.TypeFor[string](), tag: `json:"value,string"`, raw: `{"value":"native"}`, rejected: true},
		{name: "json-decoder-field", fieldType: reflect.TypeFor[legacyNativeMetadataJSONAlias](), raw: `{"value":"native"}`},
		{name: "text-decoder-field", fieldType: reflect.TypeFor[legacyNativeMetadataTextAlias](), raw: `{"value":"native"}`},
		{name: "quoted-json-decoder-field", fieldType: reflect.TypeFor[legacyNativeMetadataJSONAlias](), tag: `json:"value,omitempty,string"`, raw: `{"value":"\"native\""}`},
		{name: "quoted-text-decoder-field", fieldType: reflect.TypeFor[legacyNativeMetadataTextAlias](), tag: `json:"value,omitempty,string"`, raw: `{"value":"\"native\""}`},
		{name: "quoted-json-decoder-rejects-missing-inner-quotes", fieldType: reflect.TypeFor[legacyNativeMetadataJSONAlias](), tag: `json:"value,string"`, raw: `{"value":"native"}`, rejected: true},
		{name: "quoted-text-decoder-rejects-missing-inner-quotes", fieldType: reflect.TypeFor[legacyNativeMetadataTextAlias](), tag: `json:"value,string"`, raw: `{"value":"native"}`, rejected: true},
		{name: "json-decoder-observes-null", fieldType: reflect.TypeFor[legacyNativeMetadataJSONAlias](), initial: func() any { return legacyNativeMetadataJSONAlias("previous") }, raw: `{"value":null}`},
		{name: "text-decoder-null-keeps-value", fieldType: reflect.TypeFor[legacyNativeMetadataTextAlias](), initial: func() any { return legacyNativeMetadataTextAlias("previous") }, raw: `{"value":null}`},
		{name: "json-decoder-rejects-native-number", initial: func() any { return new(legacyNativeMetadataJSONAlias) }, raw: `{"value":7}`, rejected: true, preservePointer: true},
		{name: "text-decoder-rejects-native-number", initial: func() any { return new(legacyNativeMetadataTextAlias) }, raw: `{"value":7}`, rejected: true, preservePointer: true},
		{
			name: "nonempty-interface-string-option-is-ignored", fieldType: reflect.TypeFor[legacyNativeInterfaceMetadataStringer](),
			tag: `json:"value,string"`, initial: func() any { return new(legacyNativeInterfaceMetadataString) },
			raw: `{"value":"native"}`, preservePointer: true,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeInterfaceMetadataCheck(test, fixture)
		})
	}
}
