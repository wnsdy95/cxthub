package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func legacyNativeAnyNumberDecode(test *testing.T, raw []byte) (wire, error) {
	test.Helper()
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("native event constructor failed", err)
	}
	buffer := &legacySourceProofBuffer{body: raw}
	source := buffer.input()
	ctx := context.Background()
	var frame legacyJSONValueScanner
	native.reset(0, len(raw))
	for stepIndex := 0; stepIndex < len(raw)+16; stepIndex++ {
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		_, complete, err := native.step(ctx, input, &frame)
		if input.failure != nil {
			return native.decoded, input.failure
		}
		if err != nil || complete {
			return native.decoded, err
		}
	}
	test.Fatal("native numeric fixture did not finish")
	return wire{}, ErrIntegrity
}

func legacyNativeAnyNumberTree(test *testing.T, path string, actual, expected any) {
	test.Helper()
	switch expectedValue := expected.(type) {
	case float64:
		actualValue, ok := actual.(float64)
		if !ok {
			test.Fatalf("%s: generic numeric leaf has dynamic type %T, want float64", path, actual)
		}
		if math.Float64bits(actualValue) != math.Float64bits(expectedValue) {
			test.Fatalf("%s: numeric bits = %016x, want %016x", path, math.Float64bits(actualValue), math.Float64bits(expectedValue))
		}
	case map[string]any:
		actualValue, ok := actual.(map[string]any)
		if !ok || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
			test.Fatalf("%s: native object shape differs: got %T, want map[string]any with %d keys", path, actual, len(expectedValue))
		}
		for key, value := range expectedValue {
			child, found := actualValue[key]
			if !found {
				test.Fatalf("%s: missing native object key %q", path, key)
			}
			legacyNativeAnyNumberTree(test, path+"["+strconv.Quote(key)+"]", child, value)
		}
	case []any:
		actualValue, ok := actual.([]any)
		if !ok || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
			test.Fatalf("%s: native array shape differs: got %T, want []any with %d elements", path, actual, len(expectedValue))
		}
		for index, value := range expectedValue {
			legacyNativeAnyNumberTree(test, path+"["+strconv.Itoa(index)+"]", actualValue[index], value)
		}
	default:
		if !reflect.DeepEqual(actual, expected) {
			test.Fatalf("%s: nonnumeric leaf differs: got %#v, want %#v", path, actual, expected)
		}
	}
}

func legacyNativeAnyNumberParity(test *testing.T, event string, input map[string]any, output any) (wire, []byte) {
	test.Helper()
	raw := []byte(event)
	var expected wire
	if err := json.Unmarshal(raw, &expected); err != nil {
		test.Fatal("encoding/json rejected the numeric oracle fixture", err)
	}
	legacyNativeAnyNumberTree(test, "oracle.input", expected.Input, input)
	legacyNativeAnyNumberTree(test, "oracle.output", expected.Output, output)
	actual, err := legacyNativeAnyNumberDecode(test, raw)
	if err != nil {
		test.Fatal("native generic numeric decode rejected an accepted wire", err)
	}
	legacyNativeAnyNumberTree(test, "native.input", actual.Input, expected.Input)
	legacyNativeAnyNumberTree(test, "native.output", actual.Output, expected.Output)
	if !reflect.DeepEqual(actual, expected) {
		test.Fatal("native numeric decode changed other wire fields")
	}
	canonical, err := legacyNativeEventCanonicalCheck(test, legacyNativeEventDocument("2", event), true)
	if err != nil {
		test.Fatal(err)
	}
	return actual, canonical
}

func legacyNativeAnyNumberContainers(test *testing.T, token string, bits uint64) {
	test.Helper()
	number := math.Float64frombits(bits)
	object := map[string]any{"number": number, "nested": []any{nil, true, "1.2300", map[string]any{"number": number}}}
	objectRaw := `{"number":` + token + `,"nested":[null,true,"1.2300",{"number":` + token + `}]}`
	test.Run("map-string-any-input", func(test *testing.T) {
		legacyNativeAnyNumberParity(test, `{"kind":"tool_call","input":`+objectRaw+`}`, object, nil)
	})
	test.Run("any-output-object", func(test *testing.T) {
		legacyNativeAnyNumberParity(test, `{"kind":"tool_result","output":`+objectRaw+`}`, nil, object)
	})
	test.Run("any-output-array", func(test *testing.T) {
		legacyNativeAnyNumberParity(test, `{"kind":"tool_result","output":[`+token+`,`+objectRaw+`]}`, nil, []any{number, object})
	})
	test.Run("any-output-scalar", func(test *testing.T) {
		legacyNativeAnyNumberParity(test, `{"kind":"tool_result","output":`+token+`}`, nil, number)
	})
}

func TestLegacyNativeAnyNumberFloat64TypeAndBits(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
		bits  uint64
	}{
		{name: "positive-zero", token: "0", bits: 0},
		{name: "negative-zero", token: "-0", bits: 0x8000000000000000},
		{name: "negative-fractional-zero", token: "-0.000E+300", bits: 0x8000000000000000},
		{name: "negative-zero-with-saturated-exponent", token: "-0e999999999999999999999", bits: 0x8000000000000000},
		{name: "smallest-subnormal", token: "5e-324", bits: 1},
		{name: "negative-smallest-subnormal", token: "-5e-324", bits: 0x8000000000000001},
		{name: "below-half-subnormal", token: "2e-324", bits: 0},
		{name: "negative-underflow-keeps-sign", token: "-2e-324", bits: 0x8000000000000000},
		{name: "above-half-subnormal", token: "3e-324", bits: 1},
		{name: "deep-underflow", token: "1e-9999", bits: 0},
		{name: "smallest-normal", token: "2.2250738585072014e-308", bits: 0x0010000000000000},
		{name: "inexact-fraction", token: "0.1", bits: 0x3fb999999999999a},
		{name: "beyond-two-to-53-rounds-down", token: "9007199254740993", bits: 0x4340000000000000},
		{name: "beyond-two-to-53-rounds-up", token: "9007199254740995", bits: 0x4340000000000002},
		{name: "even-midpoint-rounds-down", token: "1.00000000000000011102230246251565404236316680908203125", bits: 0x3ff0000000000000},
		{name: "above-even-midpoint-rounds-up", token: "1.000000000000000111022302462515654042363166809082031251", bits: 0x3ff0000000000001},
		{name: "odd-midpoint-rounds-up", token: "1.00000000000000033306690738754696212708950042724609375", bits: 0x3ff0000000000002},
		{name: "large-finite-exponent", token: "1e308", bits: 0x7fe1ccf385ebc8a0},
		{name: "maximum-finite", token: "1.7976931348623157e308", bits: 0x7fefffffffffffff},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeAnyNumberContainers(test, fixture.token, fixture.bits)
		})
	}
}

func TestLegacyNativeAnyNumberReplacementBits(test *testing.T) {
	negativeZero := math.Float64frombits(0x8000000000000000)
	rounded := math.Float64frombits(0x4340000000000000)
	for _, fixture := range []struct {
		name   string
		event  string
		input  map[string]any
		output any
	}{
		{
			name:  "input-map-merges-outer-fields-and-replaces-nested-map",
			event: `{"kind":"tool_call","input":{"keep":-0,"replace":{"stale":1}},"INPUT":{"replace":{"value":9007199254740993}}}`,
			input: map[string]any{"keep": negativeZero, "replace": map[string]any{"value": rounded}},
		},
		{
			name:  "input-null-removes-old-numeric-leaves",
			event: `{"kind":"tool_call","input":{"stale":-0},"input":null,"input":{"value":9007199254740993}}`,
			input: map[string]any{"value": rounded},
		},
		{
			name:   "output-object-is-replaced-not-merged",
			event:  `{"kind":"tool_result","output":{"stale":9007199254740993},"OUTPUT":{"value":-0}}`,
			output: map[string]any{"value": negativeZero},
		},
		{
			name:   "output-array-replaces-object-and-preserves-new-bits",
			event:  `{"kind":"tool_result","output":{"stale":1},"output":[9007199254740993,-0]}`,
			output: []any{rounded, negativeZero},
		},
		{
			name:   "duplicate-key-replaces-array-with-negative-zero",
			event:  `{"kind":"tool_result","output":{"value":[1,2],"value":-0}}`,
			output: map[string]any{"value": negativeZero},
		},
		{
			name:   "outer-null-then-array-allocates-fresh-output",
			event:  `{"kind":"tool_result","output":[1,2],"output":null,"output":[-0]}`,
			output: []any{negativeZero},
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeAnyNumberParity(test, fixture.event, fixture.input, fixture.output)
		})
	}
}

func TestLegacyNativeAnyNumberOverflowStaysRejected(test *testing.T) {
	for _, token := range []struct {
		name string
		raw  string
	}{
		{name: "positive", raw: "1e309"},
		{name: "negative", raw: "-1e9999"},
		{name: "giant-token-semantic-characterization-only", raw: "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)},
	} {
		test.Run(token.name, func(test *testing.T) {
			for _, fixture := range []struct {
				name  string
				event string
			}{
				{name: "input-duplicate-key", event: `{"kind":"tool_call","input":{"value":` + token.raw + `,"value":0}}`},
				{name: "input-outer-null-and-object", event: `{"kind":"tool_call","input":{"value":` + token.raw + `},"input":null,"input":{}}`},
				{name: "input-nested-array-replaced", event: `{"kind":"tool_call","input":{"value":[` + token.raw + `],"value":[]}}`},
				{name: "output-duplicate-key", event: `{"kind":"tool_result","output":{"value":` + token.raw + `,"value":null}}`},
				{name: "output-object-replaced", event: `{"kind":"tool_result","output":{"value":` + token.raw + `},"output":{}}`},
				{name: "output-outer-null", event: `{"kind":"tool_result","output":[` + token.raw + `],"output":null}`},
				{name: "output-array-replaced", event: `{"kind":"tool_result","output":[` + token.raw + `],"output":[]}`},
			} {
				test.Run(fixture.name, func(test *testing.T) {
					raw := []byte(fixture.event)
					var expected wire
					expectedErr := json.Unmarshal(raw, &expected)
					var expectedTypeError *json.UnmarshalTypeError
					if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Type != reflect.TypeFor[float64]() || expectedTypeError.Value != "number "+token.raw {
						test.Fatal("encoding/json no longer reports sticky generic float64 overflow", expectedErr)
					}
					_, actualErr := legacyNativeAnyNumberDecode(test, raw)
					var actualTypeError *json.UnmarshalTypeError
					if !errors.As(actualErr, &actualTypeError) || actualTypeError.Type != expectedTypeError.Type || actualTypeError.Value != expectedTypeError.Value {
						test.Fatal("native generic overflow was repaired or changed error category", actualErr)
					}
					legacyNativeEventCanonicalCheck(test, legacyNativeEventDocument("2", fixture.event), false)
					for _, replacement := range []string{"[]", "null"} {
						document := []byte(`{"envelope":{"cir_version":"2"},"events":[` + fixture.event + `],"events":` + replacement + `}`)
						legacyNativeEventCanonicalCheck(test, document, false)
					}
				})
			}
		})
	}
}

func TestLegacyNativeAnyNumberGo12610GiantSemanticCharacterization(test *testing.T) {
	test.Log("Go 1.26.10 encoding/json oracle; giant tokens characterize semantics only, not conversion latency, heap, or work boundedness")
	const midpoint = "1.00000000000000011102230246251565404236316680908203125"
	fixtures := []struct {
		name   string
		token  string
		oracle string
	}{
		{name: "d06-long-exponent-saturates-before-compensation", token: "0." + strings.Repeat("0", 100000) + "1e100001", oracle: "0"},
		{name: "d06-negative-long-exponent-preserves-underflow-sign", token: "-0." + strings.Repeat("0", 100000) + "1e100001", oracle: "-0"},
		{name: "d06-short-mathematical-equivalent-is-not-native-equivalent", token: "1e0", oracle: "1"},
		{name: "unsaturated-exponent-compensates-leading-zeroes", token: "0." + strings.Repeat("0", 9998) + "1e9999", oracle: "1"},
		{name: "no-dot-900-digits-native-compensation", token: "1" + strings.Repeat("0", 899) + "e-899", oracle: "1e-100"},
		{name: "dot-after-900-digits-native-compensation", token: "1" + strings.Repeat("0", 899) + ".0e-899", oracle: "1e-100"},
		{name: "giant-exact-midpoint", token: midpoint + strings.Repeat("0", LegacyCanonicalEncodingBytes+73), oracle: "1"},
		{name: "giant-midpoint-distant-sticky-digit", token: midpoint + strings.Repeat("0", LegacyCanonicalEncodingBytes+73) + "1", oracle: "1.0000000000000002"},
	}
	midpointDigits := strings.ReplaceAll(midpoint, ".", "")
	for _, digits := range []int{801, 900, 1200} {
		coefficient := midpointDigits + strings.Repeat("0", digits-len(midpointDigits))
		exponent := "e-" + strconv.Itoa(digits-1)
		oracle := midpoint + "e" + strconv.Itoa(800-digits)
		fixtures = append(fixtures,
			struct{ name, token, oracle string }{name: "no-dot-compensated-" + strconv.Itoa(digits) + "-digits", token: coefficient + exponent, oracle: oracle},
			struct{ name, token, oracle string }{name: "dot-after-compensated-" + strconv.Itoa(digits) + "-digits", token: coefficient + ".0" + exponent, oracle: oracle},
		)
	}
	for _, fixture := range fixtures {
		test.Run(fixture.name, func(test *testing.T) {
			var oracle any
			if err := json.Unmarshal([]byte(fixture.oracle), &oracle); err != nil {
				test.Fatal("short Go numeric oracle was rejected", err)
			}
			number, ok := oracle.(float64)
			if !ok {
				test.Fatalf("Go numeric oracle returned %T, want float64", oracle)
			}
			legacyNativeAnyNumberContainers(test, fixture.token, math.Float64bits(number))
		})
	}
}

func TestLegacyNativeAnyNumberProviderNumberRemainsLexical(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "negative-zero", token: "-0.000E+03"},
		{name: "beyond-two-to-53", token: "9007199254740993"},
		{name: "trailing-zeroes-and-exponent", token: "1.2300E+003"},
		{name: "beyond-float64-range", token: "1e99999"},
		{name: "giant-integer-semantic-characterization-only", token: "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)},
		{name: "d06-lexeme-does-not-underflow", token: "0." + strings.Repeat("0", 100000) + "1e100001"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			quoted, err := json.Marshal(fixture.token)
			if err != nil {
				test.Fatal(err)
			}
			for _, spelling := range []struct {
				name string
				raw  string
			}{
				{name: "number", raw: fixture.token},
				{name: "quoted-number", raw: string(quoted)},
			} {
				test.Run(spelling.name, func(test *testing.T) {
					event := `{"kind":"tool_call","input":{"value":9007199254740993,"negative_zero":-0},"provider_metadata":{"turn_id":"lexical","create_time":` + spelling.raw + `}}`
					input := map[string]any{"value": math.Float64frombits(0x4340000000000000), "negative_zero": math.Float64frombits(0x8000000000000000)}
					actual, canonical := legacyNativeAnyNumberParity(test, event, input, nil)
					if actual.ProviderMetadata == nil || actual.ProviderMetadata.CreateTime == nil || actual.ProviderMetadata.CreateTime.String() != fixture.token {
						test.Fatal("typed json.Number provider field was converted or lost its lexeme")
					}
					if !bytes.Contains(canonical, []byte(`"create_time":`+fixture.token)) {
						test.Fatal("canonical provider json.Number was quoted or changed spelling")
					}
				})
			}
		})
	}
}
