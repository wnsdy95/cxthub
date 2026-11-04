package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

type legacyNativeAnyReviewOwner struct {
	Before string         `json:"before"`
	Input  map[string]any `json:"input"`
	Output any            `json:"output"`
}

type legacyNativeAnyReviewDeferredAny struct {
	Before string `json:"before"`
	Value  any    `json:"value"`
	After  string `json:"after"`
}

type legacyNativeAnyReviewDeferredMap struct {
	Before string         `json:"before"`
	Value  map[string]any `json:"value"`
	After  string         `json:"after"`
}

func legacyNativeAnyReviewDecode(test *testing.T, raw []byte, destination any, deferTypeErrors bool) error {
	test.Helper()
	if len(raw) == 0 || len(raw) > ConversationManifestChunkBytes {
		test.Fatal("review fixture must fit one authenticated source part")
	}
	metadata := newLegacyNativeObjectFields(reflect.TypeOf(destination).Elem())
	native, err := newLegacyNativeObject(reflect.ValueOf(destination), &metadata, 1, false)
	if err != nil {
		test.Fatal("review native-object construction failed", err)
	}
	source := (&legacySourceProofBuffer{body: raw}).input()
	native.start(0, len(raw))
	native.deferTypeErrors = deferTypeErrors
	ctx := context.Background()
	var frame legacyJSONValueScanner
	for range 16 {
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		complete, err := native.step(ctx, input, &frame)
		if input.failure != nil {
			test.Fatal("review fixture lost authenticated source access", input.failure)
		}
		if err != nil || complete {
			return err
		}
	}
	test.Fatal("small direct-native review fixture exhausted sixteen steps")
	return ErrIntegrity
}

func legacyNativeAnyReviewCompareTypeError(test *testing.T, actualErr, expectedErr error) {
	test.Helper()
	var actualTypeError, expectedTypeError *json.UnmarshalTypeError
	if !errors.As(expectedErr, &expectedTypeError) {
		test.Fatal("field-fragment oracle did not return a recoverable type error", expectedErr)
	}
	if !errors.As(actualErr, &actualTypeError) {
		test.Fatalf("native error is not the expected recoverable type: got=%T %v want=%v", actualErr, actualErr, expectedErr)
	}
	if actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type ||
		actualTypeError.Struct != expectedTypeError.Struct || actualTypeError.Field != expectedTypeError.Field ||
		actualTypeError.Offset != expectedTypeError.Offset {
		test.Errorf("native type error differs from prior field-fragment basis: got=%#v want=%#v", actualTypeError, expectedTypeError)
	}
	if actualErr.Error() != expectedErr.Error() {
		test.Errorf("native error message lost owner context: got=%q want=%q", actualErr.Error(), expectedErr.Error())
	}
}

func TestLegacyNativeAnyReviewOverflowOwnerContext(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		field    string
		fragment string
	}{
		{name: "input-object", field: "input", fragment: `{"input":{"overflow":1e999,"tail":true}}`},
		{name: "output-object", field: "output", fragment: `{"output":{"overflow":1e999,"tail":true}}`},
		{name: "output-array", field: "output", fragment: `{"output":[1e999,true]}`},
		{name: "input-duplicate-keeps-error", field: "input", fragment: `{"input":{"overflow":1e999,"overflow":0,"tail":true}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var expected legacyNativeAnyReviewOwner
			expectedErr := json.Unmarshal([]byte(fixture.fragment), &expected)
			var expectedTypeError *json.UnmarshalTypeError
			if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "number 1e999" ||
				expectedTypeError.Type != reflect.TypeFor[float64]() || expectedTypeError.Struct != "legacyNativeAnyReviewOwner" ||
				expectedTypeError.Field != fixture.field {
				test.Fatal("overflow field-fragment fixture lost its owner context", expectedErr)
			}
			raw := []byte(`{"before":"earlier",` + fixture.fragment[1:])
			var wholeDocument legacyNativeAnyReviewOwner
			wholeErr := json.Unmarshal(raw, &wholeDocument)
			var wholeTypeError *json.UnmarshalTypeError
			if !errors.As(wholeErr, &wholeTypeError) || wholeTypeError.Offset == expectedTypeError.Offset {
				test.Fatal("earlier field no longer distinguishes document and prior field-fragment offsets", wholeErr, expectedErr)
			}
			expected.Before = "earlier"
			var actual legacyNativeAnyReviewOwner
			actualErr := legacyNativeAnyReviewDecode(test, raw, &actual, false)
			if !reflect.DeepEqual(actual, expected) {
				test.Errorf("recoverable overflow did not install the complete generic container: got=%#v want=%#v", actual, expected)
			}
			legacyNativeAnyReviewCompareTypeError(test, actualErr, expectedErr)
		})
	}
}

func TestLegacyNativeAnyReviewDeferredTypeErrors(test *testing.T) {
	for _, fixture := range []struct {
		name        string
		destination func() any
	}{
		{
			name: "any",
			destination: func() any {
				return &legacyNativeAnyReviewDeferredAny{Before: "kept", After: "kept"}
			},
		},
		{
			name: "map",
			destination: func() any {
				return &legacyNativeAnyReviewDeferredMap{Before: "kept", After: "kept"}
			},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			for _, scenario := range []struct {
				name      string
				raw       string
				fragments []string
				first     string
			}{
				{
					name:      "earlier-type-error-keeps-priority",
					raw:       `{"before":7,"value":{"overflow":1e999,"tail":true},"after":"written"}`,
					fragments: []string{`{"before":7}`, `{"value":{"overflow":1e999,"tail":true}}`, `{"after":"written"}`},
					first:     "before",
				},
				{
					name:      "overflow-still-writes-later-field",
					raw:       `{"value":{"overflow":1e999,"tail":true},"after":"written"}`,
					fragments: []string{`{"value":{"overflow":1e999,"tail":true}}`, `{"after":"written"}`},
					first:     "value",
				},
			} {
				test.Run(scenario.name, func(test *testing.T) {
					expected := fixture.destination()
					var expectedErr error
					for _, fragment := range scenario.fragments {
						if err := json.Unmarshal([]byte(fragment), expected); err != nil {
							var recoverable *json.UnmarshalTypeError
							if !errors.As(err, &recoverable) {
								test.Fatal("baseline field-fragment policy encountered a non-recoverable error", err)
							}
							if expectedErr == nil {
								expectedErr = err
							}
						}
					}
					var expectedTypeError *json.UnmarshalTypeError
					if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Field != scenario.first {
						test.Fatal("baseline policy did not preserve the first recoverable field error", expectedErr)
					}
					actual := fixture.destination()
					actualErr := legacyNativeAnyReviewDecode(test, []byte(scenario.raw), actual, true)
					actualFields := reflect.ValueOf(actual).Elem()
					if actualFields.FieldByName("Before").String() != "kept" || actualFields.FieldByName("After").String() != "written" {
						test.Errorf("deferred error stopped owner writes or changed the rejected string: got=%#v", actual)
					}
					value, ok := actualFields.FieldByName("Value").Interface().(map[string]any)
					if !ok || !reflect.DeepEqual(value, map[string]any{"overflow": nil, "tail": true}) {
						test.Errorf("recoverable error did not install the complete generic value: got=%#v", actualFields.FieldByName("Value").Interface())
					}
					if !reflect.DeepEqual(actual, expected) {
						test.Errorf("deferred native destination differs from prior field-fragment policy: got=%#v want=%#v", actual, expected)
					}
					legacyNativeAnyReviewCompareTypeError(test, actualErr, expectedErr)
				})
			}
		})
	}
}
