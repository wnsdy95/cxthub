package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestLegacyNativeFloat64ReviewOverflowOffset(test *testing.T) {
	token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
	for _, fixture := range []struct {
		name     string
		fragment string
	}{
		{name: "direct-output", fragment: `{"output":` + token + `}`},
		{name: "input-map", fragment: `{"input":{"number":` + token + `,"tail":true}}`},
		{name: "output-object", fragment: `{"output":{"number":` + token + `,"tail":true}}`},
		{name: "output-array", fragment: `{"output":[` + token + `,true]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var expected legacyNativeAnyReviewOwner
			expectedErr := json.Unmarshal([]byte(fixture.fragment), &expected)
			var expectedTypeError *json.UnmarshalTypeError
			if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "number "+token {
				test.Fatal("native field-fragment oracle did not report exact overflow text", expectedErr)
			}
			raw := []byte(`{"before":"earlier",` + fixture.fragment[1:])
			var actual legacyNativeAnyReviewOwner
			actualErr := legacyNativeAnyReviewDecode(test, raw, &actual, false)
			var actualTypeError *json.UnmarshalTypeError
			if !errors.As(actualErr, &actualTypeError) {
				test.Fatal("cooperative numeric route did not report a type error", actualErr)
			}
			if actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type ||
				actualTypeError.Struct != expectedTypeError.Struct || actualTypeError.Field != expectedTypeError.Field {
				test.Fatal("cooperative numeric route changed exact overflow text, type, or owner")
			}
			if actualTypeError.Offset != expectedTypeError.Offset {
				test.Fatalf("field-fragment overflow offset = %d, want native read-ahead offset %d", actualTypeError.Offset, expectedTypeError.Offset)
			}
		})
	}
}

func TestLegacyNativeFloat64ReviewRejectedInterfacePreservesDestination(test *testing.T) {
	token := "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)
	fragment := []byte(`{"output":` + token + `}`)
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{name: "string", value: "kept"},
		{name: "number", value: float64(7)},
		{name: "object", value: map[string]any{"kept": true}},
		{name: "array", value: []any{"kept"}},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			expected := legacyNativeAnyReviewOwner{Output: fixture.value}
			expectedErr := json.Unmarshal(fragment, &expected)
			var expectedTypeError *json.UnmarshalTypeError
			if !errors.As(expectedErr, &expectedTypeError) || !reflect.DeepEqual(expected.Output, fixture.value) {
				test.Fatal("native overflow fixture no longer preserves an existing interface", expectedErr)
			}
			actual := legacyNativeAnyReviewOwner{Output: fixture.value}
			actualErr := legacyNativeAnyReviewDecode(test, fragment, &actual, false)
			var actualTypeError *json.UnmarshalTypeError
			if !errors.As(actualErr, &actualTypeError) || actualTypeError.Value != expectedTypeError.Value {
				test.Fatal("cooperative numeric route changed overflow rejection", actualErr)
			}
			if !reflect.DeepEqual(actual.Output, expected.Output) {
				test.Fatalf("rejected numeric assignment changed Output: got %#v, want preserved %#v", actual.Output, expected.Output)
			}
		})
	}
}

func TestLegacyNativeFloat64ReviewCancellationRollsBackDecimal(test *testing.T) {
	buffer := &legacySourceProofBuffer{body: []byte("1e309")}
	source := buffer.input()
	parser, err := newLegacyNativeFloat64(0, len(buffer.body))
	if err != nil {
		test.Fatal(err)
	}
	input := &legacyNormalizationInput{ctx: context.Background(), source: source}
	_, complete, _, err := parser.step(context.Background(), input)
	if err != nil || complete || parser.decimal == nil {
		test.Fatal("overflow fixture did not park at the decimal conversion boundary", complete, err)
	}
	before := *parser
	beforeDecimal := *parser.decimal
	probe := before
	probeDecimal := beforeDecimal
	probe.decimal = &probeDecimal
	checks := 0
	probeContext := canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}
	probeInput := &legacyNormalizationInput{ctx: probeContext, source: source}
	if _, _, _, err := probe.step(probeContext, probeInput); err != nil || checks < 2 || *probe.decimal == beforeDecimal {
		test.Fatal("decimal transition did not expose its final driver cancellation check", checks, err)
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	observed := 0
	stepContext := canonicalAdmissionContext{Context: base, check: func() {
		observed++
		if observed == checks {
			cancel()
		}
	}}
	input = &legacyNormalizationInput{ctx: stepContext, source: source}
	bits, complete, rejected, err := parser.step(stepContext, input)
	if !errors.Is(err, context.Canceled) || bits != 0 || complete || rejected {
		test.Fatal("canceled numeric step returned a usable result", bits, complete, rejected, err)
	}
	if !reflect.DeepEqual(*parser, before) || *parser.decimal != beforeDecimal {
		test.Fatal("driver final-check cancellation committed aliased decimal state")
	}
}

func TestLegacyNativeFloat64ReviewSourceSwitchRejected(test *testing.T) {
	first := &legacySourceProofBuffer{body: []byte("1." + strings.Repeat("0", 2*LegacyCanonicalEncodingBytes) + "1")}
	second := &legacySourceProofBuffer{body: []byte("2." + strings.Repeat("0", 2*LegacyCanonicalEncodingBytes) + "9")}
	parser, err := newLegacyNativeFloat64(0, len(first.body))
	if err != nil {
		test.Fatal(err)
	}
	input := &legacyNormalizationInput{ctx: context.Background(), source: first.input()}
	bits, complete, rejected, err := parser.step(context.Background(), input)
	if err != nil || bits != 0 || complete || rejected || parser.position <= 0 || parser.position >= len(first.body) {
		test.Fatal("source fixture did not park a valid numeric prefix", bits, complete, rejected, parser.position, err)
	}
	before := *parser
	input = &legacyNormalizationInput{ctx: context.Background(), source: second.input()}
	bits, complete, rejected, err = parser.step(context.Background(), input)
	if !errors.Is(err, ErrIntegrity) || bits != 0 || complete || rejected {
		test.Fatal("numeric prefix resumed against a different authenticated source", bits, complete, rejected, err)
	}
	if !reflect.DeepEqual(*parser, before) {
		test.Fatal("rejected source switch changed the numeric checkpoint")
	}
}
