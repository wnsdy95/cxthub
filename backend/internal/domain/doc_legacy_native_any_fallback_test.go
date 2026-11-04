package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeAnyFallbackMapAlias = map[string]any

type legacyNativeAnyFallbackNamedMap map[string]any

type legacyNativeAnyFallbackNamedKey string

type legacyNativeAnyFallbackInterface interface{}

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

func legacyNativeAnyFallbackCalls(value any) []string {
	switch value := value.(type) {
	case legacyNativeAnyFallbackJSONMap:
		calls, _ := value["calls"].([]string)
		return calls
	case *legacyNativeAnyFallbackJSONMap:
		if value != nil {
			calls, _ := (*value)["calls"].([]string)
			return calls
		}
	}
	return legacyNativeNestedFallbackCalls(value)
}

func legacyNativeAnyFallbackCheck(test *testing.T, fixture legacyNativeAnyFallbackCase) {
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
		test.Fatal("fixture changed native acceptance", fixture.rejected, expectedErr)
	}
	presence, actualErr := legacyNativeMetadataDecode(test, raw, actual.Interface(), false)
	if reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) || presence != 0 {
		test.Fatal("generic eligibility changed native error type or disabled presence", actualErr, expectedErr, presence)
	}
	var actualTypeError, expectedTypeError *json.UnmarshalTypeError
	if errors.As(expectedErr, &expectedTypeError) &&
		(!errors.As(actualErr, &actualTypeError) || actualTypeError.Type != expectedTypeError.Type || actualTypeError.Value != expectedTypeError.Value) {
		test.Fatal("generic eligibility changed native type-error details", actualErr, expectedErr)
	}
	if fixture.wantError != nil && (!errors.Is(actualErr, fixture.wantError) || !errors.Is(expectedErr, fixture.wantError)) {
		test.Fatal("generic eligibility lost the custom decoder error", actualErr, expectedErr, fixture.wantError)
	}
	if !reflect.DeepEqual(actual.Interface(), expected.Interface()) {
		test.Fatalf("generic eligibility changed native values, nilness, or calls: got=%#v want=%#v", actual.Interface(), expected.Interface())
	}
	if !reflect.DeepEqual(actualInitial, expectedInitial) {
		test.Fatalf("generic eligibility changed mutations through the original value: got=%#v want=%#v", actualInitial, expectedInitial)
	}
	if fixture.preservePointer &&
		(actual.Elem().Field(0).Interface() != actualInitial || expected.Elem().Field(0).Interface() != expectedInitial) {
		test.Fatal("generic eligibility replaced a concrete pointer preserved by encoding/json")
	}
	if calls := legacyNativeAnyFallbackCalls(actual.Elem().Field(0).Interface()); !reflect.DeepEqual(calls, fixture.wantCalls) {
		test.Fatal("generic eligibility changed custom call count, order, or raw bytes", calls, fixture.wantCalls)
	}
}

func TestLegacyNativeObjectAnyFallbackMetadata(test *testing.T) {
	for _, fixture := range []struct {
		name            string
		fieldType       reflect.Type
		genericMap      bool
		interfaceString bool
	}{
		{name: "exact-map", fieldType: reflect.TypeFor[map[string]any](), genericMap: true},
		{name: "exact-map-alias", fieldType: reflect.TypeFor[legacyNativeAnyFallbackMapAlias](), genericMap: true},
		{name: "named-map", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap]()},
		{name: "custom-json-map", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap]()},
		{name: "custom-json-map-pointer", fieldType: reflect.TypeFor[*legacyNativeAnyFallbackJSONMap]()},
		{name: "string-map", fieldType: reflect.TypeFor[map[string]string]()},
		{name: "integer-map", fieldType: reflect.TypeFor[map[string]int]()},
		{name: "integer-key-map", fieldType: reflect.TypeFor[map[int]any]()},
		{name: "named-key-map", fieldType: reflect.TypeFor[map[legacyNativeAnyFallbackNamedKey]any]()},
		{name: "named-interface-element-map", fieldType: reflect.TypeFor[map[string]legacyNativeAnyFallbackInterface]()},
		{name: "exact-map-pointer", fieldType: reflect.TypeFor[*map[string]any]()},
		{name: "exact-any", fieldType: reflect.TypeFor[any](), interfaceString: true},
		{name: "named-empty-interface", fieldType: reflect.TypeFor[legacyNativeAnyFallbackInterface]()},
		{name: "decoder-interface", fieldType: reflect.TypeFor[json.Unmarshaler]()},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, tag := range []reflect.StructTag{`json:"value"`, `json:"value,omitempty"`} {
				model := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: fixture.fieldType, Tag: tag}})
				metadata := newLegacyNativeObjectFields(model)
				if metadata.err != nil || len(metadata.fields) != 1 {
					test.Fatal("generic eligibility poisoned enclosing struct metadata", metadata.err, len(metadata.fields))
				}
				field := metadata.fields[0]
				if field.name != "value" || field.index != 0 || field.prefix != `{"value":` || metadata.match("VALUE") != 0 ||
					field.genericMap != fixture.genericMap || field.interfaceString != fixture.interfaceString ||
					field.scalarString || field.stringList || field.blockList || field.nestedObject {
					test.Fatal("generic specialization selected an inexact type or changed field metadata", field, fixture)
				}
			}
		})
	}
}

func TestLegacyNativeObjectAnyFallbackUsesNativeFieldDecode(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		fieldType reflect.Type
		initial   func() any
		value     string
	}{
		{name: "named-map", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), value: `{"number":1,"nested":[]}`},
		{name: "custom-json-map", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), value: `{ "overflow":1e999, "escape":"\u0061" }`},
		{name: "string-map", fieldType: reflect.TypeFor[map[string]string](), value: `{"text":"kept","null":null}`},
		{name: "exact-map-pointer", fieldType: reflect.TypeFor[*map[string]any](), value: `{"number":1}`},
		{name: "any-map-pointer", fieldType: reflect.TypeFor[any](), initial: func() any { return new(map[string]any) }, value: `{"number":1}`},
		{name: "any-slice-pointer", fieldType: reflect.TypeFor[any](), initial: func() any { return new([]any) }, value: `[1,{},null]`},
		{name: "any-json-map-pointer", fieldType: reflect.TypeFor[any](), initial: func() any { return new(legacyNativeAnyFallbackJSONMap) }, value: `[1e999,"\u0061"]`},
		{name: "any-json-struct-pointer", fieldType: reflect.TypeFor[any](), initial: func() any { return new(legacyNativeNestedFallbackJSON) }, value: `{"blob":"kept"}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			model := reflect.StructOf([]reflect.StructField{{Name: "Value", Type: fixture.fieldType, Tag: `json:"value"`}})
			metadata := newLegacyNativeObjectFields(model)
			destination := reflect.New(model)
			if fixture.initial != nil {
				destination.Elem().Field(0).Set(reflect.ValueOf(fixture.initial()))
			}
			native, err := newLegacyNativeObject(destination, &metadata, 1, false)
			if err != nil {
				test.Fatal(err)
			}
			raw := []byte(`{"value":` + fixture.value + `}`)
			buffer := &legacySourceProofBuffer{body: raw}
			input := &legacyNormalizationInput{ctx: context.Background(), source: buffer.input()}
			var frame legacyJSONValueScanner
			native.start(0, len(raw))
			complete, err := native.step(input.ctx, input, &frame)
			if err != nil || input.failure != nil || !complete {
				test.Fatal("small native fallback did not complete", complete, err, input.failure)
			}
			if input.semanticBytes != len(fixture.value) || input.fields != 1 {
				test.Fatal("ineligible container bypassed whole-field native decoding", input.semanticBytes, len(fixture.value), input.fields)
			}
		})
	}
}

func TestLegacyNativeObjectAnyFallbackTypedMapParity(test *testing.T) {
	for _, fixture := range []legacyNativeAnyFallbackCase{
		{name: "named-map-retains-concrete-type", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), raw: `{"value":{"number":9007199254740993,"nested":{"empty":[]}}}`},
		{name: "named-map-merges-duplicate-objects", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), raw: `{"value":{"first":1,"same":{"before":true}},"VALUE":{"last":2,"same":{"after":true}}}`},
		{name: "named-map-null-discards-prior-object", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), raw: `{"value":{"before":true},"value":null,"value":{"after":true}}`},
		{name: "named-key-map-retains-key-type", fieldType: reflect.TypeFor[map[legacyNativeAnyFallbackNamedKey]any](), raw: `{"value":{"escaped\u006bey":123.40,"empty":{}}}`},
		{name: "named-interface-element-map-retains-map-type", fieldType: reflect.TypeFor[map[string]legacyNativeAnyFallbackInterface](), raw: `{"value":{"number":7,"nested":[{},null]}}`},
		{name: "integer-key-map-uses-native-key-conversion", fieldType: reflect.TypeFor[map[int]any](), raw: `{"value":{"12":1,"-3":[]}}`},
		{name: "integer-key-map-rejects-nonnumeric-key", fieldType: reflect.TypeFor[map[int]any](), raw: `{"value":{"invalid":1}}`, rejected: true},
		{name: "string-map-retains-strings-and-null-zero", fieldType: reflect.TypeFor[map[string]string](), raw: `{"value":{"text":"kept","nil":null,"empty":""}}`},
		{name: "string-map-preserves-native-partial-mutations", fieldType: reflect.TypeFor[map[string]string](), raw: `{"value":{"before":"kept","wrong":7,"after":"also-kept"}}`, rejected: true},
		{name: "string-map-rejects-nested-object", fieldType: reflect.TypeFor[map[string]string](), raw: `{"value":{"nested":{}}}`, rejected: true},
		{name: "string-map-rejects-nested-array", fieldType: reflect.TypeFor[map[string]string](), raw: `{"value":{"nested":[]}}`, rejected: true},
		{name: "integer-map-keeps-native-integer", fieldType: reflect.TypeFor[map[string]int](), raw: `{"value":{"count":7,"null":null}}`},
		{name: "integer-map-rejects-native-fraction", fieldType: reflect.TypeFor[map[string]int](), raw: `{"value":{"count":7.5}}`, rejected: true},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeAnyFallbackCheck(test, fixture) })
	}
	for _, fieldType := range []reflect.Type{
		reflect.TypeFor[map[string]any](), reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), reflect.TypeFor[map[string]string](),
	} {
		test.Run(fieldType.String(), func(test *testing.T) {
			for _, value := range []string{`[]`, `[1]`, `true`, `7`, `"text"`} {
				test.Run(value, func(test *testing.T) {
					legacyNativeAnyFallbackCheck(test, legacyNativeAnyFallbackCase{
						fieldType: fieldType, raw: `{"value":` + value + `}`, rejected: true,
					})
				})
			}
		})
	}
}

func TestLegacyNativeObjectAnyFallbackForeignMapParity(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		fieldType reflect.Type
		initial   func() any
	}{
		{name: "exact-map-without-capacity-ledger", fieldType: reflect.TypeFor[map[string]any](), initial: func() any {
			value := make(map[string]any, 128)
			value["seed"] = []any{"kept"}
			value["same"] = map[string]any{"before": true}
			return value
		}},
		{name: "empty-exact-map-without-capacity-ledger", fieldType: reflect.TypeFor[map[string]any](), initial: func() any { return make(map[string]any, 128) }},
		{name: "named-map", fieldType: reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), initial: func() any { return legacyNativeAnyFallbackNamedMap{"seed": []any{"kept"}} }},
		{name: "string-map", fieldType: reflect.TypeFor[map[string]string](), initial: func() any { return map[string]string{"seed": "kept"} }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, sequence := range []struct {
				name string
				raw  string
			}{
				{name: "absent", raw: `{}`},
				{name: "empty-keeps-map", raw: `{"value":{}}`},
				{name: "merge-mutates-original", raw: `{"value":{"fresh":"first"},"VALUE":{"last":"second"}}`},
				{name: "null-entry-overwrites", raw: `{"value":{"seed":null}}`},
				{name: "null-clears-map-not-alias", raw: `{"value":null}`},
				{name: "null-then-empty-allocates-distinct-map", raw: `{"value":null,"value":{}}`},
				{name: "mutate-detach-then-reallocate", raw: `{"value":{"fresh":"first"},"value":null,"value":{"after":"second"}}`},
			} {
				test.Run(sequence.name, func(test *testing.T) {
					legacyNativeAnyFallbackCheck(test, legacyNativeAnyFallbackCase{
						fieldType: fixture.fieldType, initial: fixture.initial, raw: sequence.raw,
					})
				})
			}
		})
	}
	legacyNativeAnyFallbackCheck(test, legacyNativeAnyFallbackCase{
		fieldType: reflect.TypeFor[map[string]any](), initial: func() any { return map[string]any{"same": map[string]any{"before": true}} },
		raw: `{"value":{"same":{"after":true}}}`,
	})
	for _, fieldType := range []reflect.Type{
		reflect.TypeFor[map[string]any](), reflect.TypeFor[legacyNativeAnyFallbackNamedMap](), reflect.TypeFor[map[string]string](),
	} {
		test.Run("nil-"+fieldType.String(), func(test *testing.T) {
			for _, raw := range []string{`{}`, `{"value":null}`, `{"value":{}}`, `{"value":{},"value":null}`, `{"value":null,"value":{}}`} {
				test.Run(raw, func(test *testing.T) {
					legacyNativeAnyFallbackCheck(test, legacyNativeAnyFallbackCase{fieldType: fieldType, raw: raw})
				})
			}
		})
	}
}

func TestLegacyNativeObjectAnyFallbackInterfaceContainerParity(test *testing.T) {
	for _, fixture := range []struct {
		name      string
		fieldType reflect.Type
		initial   func() any
	}{
		{name: "nil-any"},
		{name: "named-empty-interface", fieldType: reflect.TypeFor[legacyNativeAnyFallbackInterface]()},
		{name: "nil-concrete-map", initial: func() any { return map[string]any(nil) }},
		{name: "nil-concrete-slice", initial: func() any { return []any(nil) }},
		{name: "concrete-map-is-replaced", initial: func() any { return map[string]any{"seed": "must-not-merge"} }},
		{name: "concrete-named-map-is-replaced", initial: func() any { return legacyNativeAnyFallbackNamedMap{"seed": "must-not-merge"} }},
		{name: "concrete-custom-map-is-replaced-without-calls", initial: func() any { return legacyNativeAnyFallbackJSONMap{"seed": "must-not-merge"} }},
		{name: "concrete-slice-is-replaced", initial: func() any { return []any{"must-not-reuse", map[string]any{"tail": true}} }},
		{name: "concrete-number-is-replaced", initial: func() any { return 7 }},
		{name: "nil-struct-pointer-becomes-generic", initial: func() any { return (*LockedBlob)(nil) }},
		{name: "nil-map-pointer-becomes-generic", initial: func() any { return (*map[string]any)(nil) }},
		{name: "nil-slice-pointer-becomes-generic", initial: func() any { return (*[]any)(nil) }},
		{name: "nil-json-map-pointer-becomes-generic-without-calls", initial: func() any { return (*legacyNativeAnyFallbackJSONMap)(nil) }},
		{name: "nil-json-struct-pointer-becomes-generic-without-calls", initial: func() any { return (*legacyNativeNestedFallbackJSON)(nil) }},
		{name: "nil-text-pointer-becomes-generic-without-calls", initial: func() any { return (*legacyNativeNestedFallbackText)(nil) }},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, sequence := range []struct {
				name string
				raw  string
			}{
				{name: "absent", raw: `{}`},
				{name: "empty-object", raw: `{"value":{}}`},
				{name: "empty-array", raw: `{"value":[]}`},
				{name: "nested-object", raw: `{"value":{"number":9007199254740993,"nested":[123.40,{},[],null],"text":"\u0061\n"}}`},
				{name: "nested-array", raw: `{"value":[{"number":1.25},[],null,false,"text"]}`},
				{name: "duplicate-objects-replace-not-merge", raw: `{"value":{"first":1},"VALUE":{"last":2}}`},
				{name: "null-clears-interface", raw: `{"value":null}`},
				{name: "object-array-null-object", raw: `{"value":{"before":true},"value":[],"value":null,"value":{}}`},
			} {
				test.Run(sequence.name, func(test *testing.T) {
					legacyNativeAnyFallbackCheck(test, legacyNativeAnyFallbackCase{
						fieldType: fixture.fieldType, initial: fixture.initial, raw: sequence.raw,
					})
				})
			}
		})
	}
}

func TestLegacyNativeObjectAnyFallbackInterfacePointerParity(test *testing.T) {
	mapPointer := func() any {
		value := map[string]any{"seed": "kept", "same": map[string]any{"before": true}}
		return &value
	}
	slicePointer := func() any {
		value := []any{"head", "tail"}
		return &value
	}
	interfacePointer := func() any {
		var value any = map[string]any{"seed": "must-not-merge"}
		return &value
	}
	mapPointerPointer := func() any {
		value := map[string]any{"seed": "kept"}
		pointer := &value
		return &pointer
	}
	for _, fixture := range []legacyNativeAnyFallbackCase{
		{name: "map-pointer-mutates-original-and-merges", initial: mapPointer, raw: `{"value":{"fresh":1},"VALUE":{"same":{"after":true}}}`, preservePointer: true},
		{name: "map-pointer-empty-object-keeps-map", initial: mapPointer, raw: `{"value":{}}`, preservePointer: true},
		{name: "map-pointer-null-detaches-interface", initial: mapPointer, raw: `{"value":null}`},
		{name: "map-pointer-null-then-object-is-generic", initial: mapPointer, raw: `{"value":{"fresh":1},"value":null,"value":{"after":2}}`},
		{name: "map-pointer-rejects-array", initial: mapPointer, raw: `{"value":[]}`, rejected: true, preservePointer: true},
		{name: "typed-map-pointer-keeps-element-decoder", initial: func() any { value := map[string]string{"seed": "kept"}; return &value }, raw: `{"value":{"wrong":7,"after":"kept"}}`, rejected: true, preservePointer: true},
		{name: "slice-pointer-reuses-native-backing", initial: slicePointer, raw: `{"value":[{"fresh":1}],"value":[null,{}]}`, preservePointer: true},
		{name: "slice-pointer-empty-array-is-nonnil", initial: slicePointer, raw: `{"value":[]}`, preservePointer: true},
		{name: "slice-pointer-null-detaches-interface", initial: slicePointer, raw: `{"value":null}`},
		{name: "slice-pointer-rejects-object", initial: slicePointer, raw: `{"value":{}}`, rejected: true, preservePointer: true},
		{name: "struct-pointer-mutates-native-fields", initial: func() any { return &LockedBlob{Provider: "seed", Blob: "before"} }, raw: `{"value":{"blob":"after"},"VALUE":{"scheme":"signature"}}`, preservePointer: true},
		{name: "struct-pointer-rejects-array", initial: func() any { return new(LockedBlob) }, raw: `{"value":[]}`, rejected: true, preservePointer: true},
		{name: "interface-pointer-keeps-outer-pointer", initial: interfacePointer, raw: `{"value":{"first":1},"value":[2,{}]}`, preservePointer: true},
		{name: "pointer-to-nil-interface-keeps-outer-pointer", initial: func() any { return new(any) }, raw: `{"value":{"first":1},"value":[2,{}]}`, preservePointer: true},
		{name: "interface-pointer-null-detaches-outer-interface", initial: interfacePointer, raw: `{"value":null}`},
		{name: "pointer-to-nil-map-pointer-allocates-inner", initial: func() any { return new(*map[string]any) }, raw: `{"value":{"fresh":1}}`, preservePointer: true},
		{name: "double-pointer-null-clears-inner-not-outer", initial: mapPointerPointer, raw: `{"value":null}`, preservePointer: true},
		{name: "double-pointer-null-then-object-reallocates-inner", initial: mapPointerPointer, raw: `{"value":null,"value":{"fresh":1}}`, preservePointer: true},
		{name: "nil-decoder-interface-rejects-object", fieldType: reflect.TypeFor[json.Unmarshaler](), raw: `{"value":{}}`, rejected: true},
		{name: "typed-nil-nonempty-interface-is-not-any", fieldType: reflect.TypeFor[json.Unmarshaler](), initial: func() any { return (*legacyNativeAnyFallbackJSONMap)(nil) }, raw: `{"value":{}}`, rejected: true},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeAnyFallbackCheck(test, fixture) })
	}
}

func TestLegacyNativeObjectAnyFallbackCustomDecoderParity(test *testing.T) {
	jsonMapPointer := func() any {
		value := legacyNativeAnyFallbackJSONMap{"seed": "kept"}
		return &value
	}
	for _, fixture := range []legacyNativeAnyFallbackCase{
		{name: "custom-map-receives-object-once-with-original-bytes", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":{ "overflow":1e999, "escape":"\u0061", "overflow":-1e999 }}`, wantCalls: []string{`{ "overflow":1e999, "escape":"\u0061", "overflow":-1e999 }`}},
		{name: "custom-map-value-receives-null", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":null}`, wantCalls: []string{"null"}},
		{name: "custom-map-accepts-array-instead-of-generic-wrongkind", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":[1e999]}`, wantCalls: []string{`[1e999]`}},
		{name: "custom-map-duplicate-values-call-in-order", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":{"first":1},"VALUE":[],"value":null}`, wantCalls: []string{`{"first":1}`, `[]`, "null"}},
		{name: "custom-map-error-preserves-mutation", fieldType: reflect.TypeFor[legacyNativeAnyFallbackJSONMap](), raw: `{"value":"reject"}`, rejected: true, wantCalls: []string{`"reject"`}, wantError: errLegacyNativeAnyFallbackJSON},
		{name: "custom-map-pointer-allocates", fieldType: reflect.TypeFor[*legacyNativeAnyFallbackJSONMap](), raw: `{"value":{}}`, wantCalls: []string{`{}`}},
		{name: "custom-map-pointer-null-skips-allocation", fieldType: reflect.TypeFor[*legacyNativeAnyFallbackJSONMap](), raw: `{"value":null}`},
		{name: "any-custom-map-pointer-keeps-calls-and-identity", initial: jsonMapPointer, raw: `{"value":{ "first":1e999 },"VALUE":[2]}`, preservePointer: true, wantCalls: []string{`{ "first":1e999 }`, `[2]`}},
		{name: "any-custom-map-pointer-null-skips-call", initial: jsonMapPointer, raw: `{"value":null}`},
		{name: "any-custom-map-pointer-detaches-after-prior-call", initial: jsonMapPointer, raw: `{"value":{"first":1},"value":null,"value":{"last":2}}`},
		{name: "any-custom-map-pointer-error-is-returned-once", initial: jsonMapPointer, raw: `{"value":"reject"}`, rejected: true, preservePointer: true, wantCalls: []string{`"reject"`}, wantError: errLegacyNativeAnyFallbackJSON},
		{name: "decoder-interface-custom-map-pointer-keeps-method", fieldType: reflect.TypeFor[json.Unmarshaler](), initial: jsonMapPointer, raw: `{"value":[1e999]}`, preservePointer: true, wantCalls: []string{`[1e999]`}},
		{name: "any-json-struct-pointer-still-calls-custom-object-decoder", initial: func() any { return &legacyNativeNestedFallbackJSON{Blob: "seed"} }, raw: `{"value":{ "blob":"fresh", "unknown":1e999 }}`, preservePointer: true, wantCalls: []string{`{ "blob":"fresh", "unknown":1e999 }`}},
		{name: "any-json-struct-pointer-rejects-array-natively", initial: func() any { return &legacyNativeNestedFallbackJSON{Blob: "seed"} }, raw: `{"value":[]}`, rejected: true, preservePointer: true, wantCalls: []string{`[]`}},
		{name: "any-text-pointer-rejects-object-without-call", initial: func() any { return &legacyNativeNestedFallbackText{Blob: "seed"} }, raw: `{"value":{}}`, rejected: true, preservePointer: true},
		{name: "any-text-pointer-rejects-array-without-call", initial: func() any { return &legacyNativeNestedFallbackText{Blob: "seed"} }, raw: `{"value":[]}`, rejected: true, preservePointer: true},
	} {
		test.Run(fixture.name, func(test *testing.T) { legacyNativeAnyFallbackCheck(test, fixture) })
	}
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
					legacyNativeAnyFallbackCheck(test, fixture)
				})
			}
		})
	}
}
