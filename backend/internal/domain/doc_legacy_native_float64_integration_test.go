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

const Float64IntegrationMidpoint = "1.00000000000000011102230246251565404236316680908203125"

func Float64IntegrationTree(test *testing.T, path string, actual, expected any) {
	test.Helper()
	switch expectedValue := expected.(type) {
	case float64:
		actualValue, ok := actual.(float64)
		if !ok || math.Float64bits(actualValue) != math.Float64bits(expectedValue) {
			test.Fatalf("%s changed native float64 type or bits: got=%T %016x want=%016x", path, actual, math.Float64bits(actualValue), math.Float64bits(expectedValue))
		}
	case map[string]any:
		actualValue, ok := actual.(map[string]any)
		if !ok || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
			test.Fatalf("%s changed native map type, nilness, or length", path)
		}
		for key, child := range expectedValue {
			actualChild, exists := actualValue[key]
			if !exists {
				test.Fatalf("%s lost native map key %q", path, key)
			}
			Float64IntegrationTree(test, path+"["+strconv.Quote(key)+"]", actualChild, child)
		}
	case []any:
		actualValue, ok := actual.([]any)
		if !ok || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
			test.Fatalf("%s changed native array type, nilness, or length", path)
		}
		for index, child := range expectedValue {
			Float64IntegrationTree(test, path+"["+strconv.Itoa(index)+"]", actualValue[index], child)
		}
	default:
		if !reflect.DeepEqual(actual, expected) {
			test.Fatalf("%s changed nonnumeric type or value: got=%T want=%T", path, actual, expected)
		}
	}
}

func Float64IntegrationNative(test *testing.T, raw []byte) (wire, CIREvent, error) {
	test.Helper()
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("native event constructor failed", err)
	}
	native.reset(0, len(raw))
	native.object.prefix, native.object.deferTypeErrors = true, true
	buffer := &legacySourceProofBuffer{body: raw}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	readSource := source.Read
	var activeInput *legacyNormalizationInput
	source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
		if activeInput == nil || activeInput.unbounded {
			test.Fatal("native numeric decoding entered the unbounded source fallback", offset, length)
		}
		return readSource(ctx, offset, length)
	}
	ctx := context.Background()
	var frame legacyJSONValueScanner
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+64*parts; stepIndex++ {
		before := len(buffer.reads)
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		activeInput = input
		event, complete, err := native.step(ctx, input, &frame)
		activeInput = nil
		if input.workBytes < 0 || input.workBytes > ConversationManifestChunkBytes || input.loads < 0 || input.loads > 2 || input.unbounded ||
			input.fields > LegacyNormalizationStepEvents*len(native.object.metadata.fields) {
			test.Fatal("native numeric step exceeded shared 512 KiB, two-load, or field limits", stepIndex, input.workBytes, input.loads, input.fields, input.unbounded)
		}
		if len(buffer.reads)-before != input.loads {
			test.Fatal("native numeric source loads escaped the actual input accounting", stepIndex, len(buffer.reads)-before, input.loads)
		}
		for _, read := range buffer.reads[before:] {
			if read[0] < 0 || read[0] >= len(raw) || read[0]%ConversationManifestChunkBytes != 0 ||
				read[1] != min(ConversationManifestChunkBytes, len(raw)-read[0]) {
				test.Fatal("native numeric decoding bypassed authenticated full-part reads", read)
			}
		}
		if input.failure != nil {
			test.Fatal("native numeric fixture lost its authenticated source", input.failure)
		}
		if retained, retainedErr := native.retained(); retainedErr != nil || retained <= 0 {
			test.Fatal("native numeric decoding lost retained-byte accounting", retained, retainedErr)
		}
		if err != nil {
			if complete || !reflect.DeepEqual(event, CIREvent{}) {
				test.Fatal("rejected native numeric event exposed provisional output")
			}
			return native.decoded, event, err
		}
		if complete {
			if native.object.position != len(raw) {
				test.Fatal("native numeric event completed before its raw source extent", native.object.position, len(raw))
			}
			return native.decoded, event, nil
		}
	}
	test.Fatal("native numeric event exhausted its finite per-part step allowance")
	return wire{}, CIREvent{}, ErrIntegrity
}

func Float64IntegrationCanonical(test *testing.T, raw []byte, accepted bool) []byte {
	test.Helper()
	expected, expectedErr := legacyNormalizationOracle(raw)
	if (expectedErr == nil) != accepted {
		test.Fatal("numeric fixture changed original CIR acceptance", accepted, expectedErr)
	}
	normalizer, source := legacyNativeScalarReviewStart(test, raw)
	actual, actualErr := legacyNativeScalarReviewDrain(test, normalizer, source)
	if !errors.Is(actualErr, expectedErr) || (actualErr == nil) != accepted || !bytes.Equal(actual, expected) {
		test.Fatal("native numeric normalization changed original acceptance or canonical bytes", len(actual), len(expected), actualErr, expectedErr)
	}
	if errors.Is(actualErr, ErrPreparedDocRangeObject) {
		test.Fatal("numeric semantic rejection became a private-source failure", actualErr)
	}
	return actual
}

func Float64IntegrationParity(test *testing.T, event string) (wire, []byte) {
	test.Helper()
	raw := []byte(event)
	var expectedWire wire
	var expectedEvent CIREvent
	if err := json.Unmarshal(raw, &expectedWire); err != nil {
		test.Fatal("original encoding/json rejected the wire fixture", err)
	}
	if err := json.Unmarshal(raw, &expectedEvent); err != nil {
		test.Fatal("original encoding/json rejected the CIR event fixture", err)
	}
	actualWire, actualEvent, err := Float64IntegrationNative(test, raw)
	if err != nil {
		test.Fatal("bounded native decoding rejected an accepted number", err)
	}
	Float64IntegrationTree(test, "wire.input", actualWire.Input, expectedWire.Input)
	Float64IntegrationTree(test, "wire.output", actualWire.Output, expectedWire.Output)
	Float64IntegrationTree(test, "CIREvent.input", actualEvent.Input, expectedEvent.Input)
	Float64IntegrationTree(test, "CIREvent.output", actualEvent.Output, expectedEvent.Output)
	if !reflect.DeepEqual(actualWire, expectedWire) || !reflect.DeepEqual(actualEvent, expectedEvent) {
		test.Fatal("native numeric integration changed other wire or CIR event fields")
	}
	canonical := Float64IntegrationCanonical(test, legacyNativeEventDocument("2", event), true)
	return actualWire, canonical
}

func Float64IntegrationContainers(test *testing.T, token string) {
	test.Helper()
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
	}{
		{name: "selected-input-map", prefix: `{"Kind":"tool_call","seq":7,"call_id":"number","INP\u0055T":{"number":`, suffix: `,"nested":[null,{"unchanged":true}],"text":"-0"}}`},
		{name: "nested-output-arrays", prefix: `{"kind":"tool_result","seq":7,"call_id":"number","OUTPUT":[false,[{"number":`, suffix: `}],null]}`},
		{name: "root-scalar-output", prefix: `{"kind":"tool_result","seq":7,"call_id":"number","outp\u0075t":`, suffix: `}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			Float64IntegrationParity(test, fixture.prefix+token+fixture.suffix)
		})
	}
}

func Float64IntegrationBits(test *testing.T, token string) uint64 {
	test.Helper()
	var value any
	if err := json.Unmarshal([]byte(token), &value); err != nil {
		test.Fatal("original encoding/json rejected the scalar numeric oracle", err)
	}
	number, ok := value.(float64)
	if !ok || math.IsInf(number, 0) || math.IsNaN(number) {
		test.Fatalf("original native numeric oracle has unusable type or value: %T", value)
	}
	return math.Float64bits(number)
}

func TestLegacyNativeFloat64IntegrationNativeBits(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
		bits  uint64
	}{
		{name: "positive-zero", token: "0", bits: 0},
		{name: "negative-zero", token: "-0.000E+300", bits: 0x8000000000000000},
		{name: "negative-underflow", token: "-2e-324", bits: 0x8000000000000000},
		{name: "minimum-subnormal", token: "5e-324", bits: 1},
		{name: "negative-minimum-subnormal", token: "-5e-324", bits: 0x8000000000000001},
		{name: "minimum-normal", token: "2.2250738585072014e-308", bits: 0x0010000000000000},
		{name: "inexact-fraction", token: "0.1", bits: 0x3fb999999999999a},
		{name: "integer-rounds-down", token: "9007199254740993", bits: 0x4340000000000000},
		{name: "integer-rounds-up", token: "9007199254740995", bits: 0x4340000000000002},
		{name: "even-midpoint", token: Float64IntegrationMidpoint, bits: 0x3ff0000000000000},
		{name: "above-midpoint", token: Float64IntegrationMidpoint + "1", bits: 0x3ff0000000000001},
		{name: "odd-midpoint", token: "1.00000000000000033306690738754696212708950042724609375", bits: 0x3ff0000000000002},
		{name: "maximum-finite", token: "1.7976931348623157e308", bits: 0x7fefffffffffffff},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if bits := Float64IntegrationBits(test, fixture.token); bits != fixture.bits {
				test.Fatalf("original numeric oracle changed bits: got=%016x want=%016x", bits, fixture.bits)
			}
			Float64IntegrationContainers(test, fixture.token)
		})
	}
}

func TestLegacyNativeFloat64IntegrationGiantSelectedValues(test *testing.T) {
	const digits = 2*ConversationManifestChunkBytes + 17
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "negative-zero-giant-exponent", token: "-0e+" + strings.Repeat("9", digits)},
		{name: "negative-underflow-giant-exponent", token: "-1e-" + strings.Repeat("9", digits)},
		{name: "leading-exponent-zeroes", token: "1e+" + strings.Repeat("0", digits) + "1"},
		{name: "midpoint-zero-tail", token: Float64IntegrationMidpoint + strings.Repeat("0", digits)},
		{name: "midpoint-distant-sticky", token: Float64IntegrationMidpoint + strings.Repeat("0", digits) + "1"},
		{name: "negative-midpoint-distant-sticky", token: "-" + Float64IntegrationMidpoint + strings.Repeat("0", digits) + "1"},
		{name: "huge-fractional-coefficient", token: "2." + strings.Repeat("2", digits) + "e+1"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if len(fixture.token) <= 2*ConversationManifestChunkBytes {
				test.Fatal("giant numeric fixture no longer crosses two authenticated source parts")
			}
			Float64IntegrationContainers(test, fixture.token)
		})
	}
}

func TestLegacyNativeFloat64IntegrationSourcePathQuirks(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "exponent-before-freeze", token: "0." + strings.Repeat("0", 9998) + "1e9999"},
		{name: "exponent-frozen-before-compensation", token: "0." + strings.Repeat("0", 100000) + "1e100001"},
		{name: "negative-frozen-compensation", token: "-0." + strings.Repeat("0", 100000) + "1e100001"},
		{name: "retained-no-dot-position", token: "1" + strings.Repeat("0", 899) + "e-899"},
		{name: "retained-explicit-dot-position", token: "1" + strings.Repeat("0", 899) + ".0e-899"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			Float64IntegrationContainers(test, fixture.token)
		})
	}
	midpointDigits := strings.ReplaceAll(Float64IntegrationMidpoint, ".", "")
	for _, digits := range []int{801, 900, 1200, 2*ConversationManifestChunkBytes + 17} {
		for _, dot := range []string{"", ".0"} {
			test.Run("compensated-coefficient-"+strconv.Itoa(digits)+"-dot-"+strconv.Itoa(len(dot)), func(test *testing.T) {
				token := midpointDigits + strings.Repeat("0", digits-len(midpointDigits)) + dot + "e-" + strconv.Itoa(digits-1)
				Float64IntegrationContainers(test, token)
			})
		}
	}
}

func TestLegacyNativeFloat64IntegrationOverflowDiagnostics(test *testing.T) {
	for _, token := range []string{"1e999", "-1e999", "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)} {
		for _, fixture := range []struct {
			name     string
			kind     string
			field    string
			fragment string
		}{
			{name: "input-map", kind: "tool_call", field: "input", fragment: `{"input":{"number":` + token + `,"tail":true}}`},
			{name: "nested-output-arrays", kind: "tool_result", field: "output", fragment: `{"output":[false,[{"number":` + token + `}],true]}`},
			{name: "root-scalar-output", kind: "tool_result", field: "output", fragment: `{"output":` + token + `}`},
		} {
			test.Run(fixture.name+"-"+strconv.Itoa(len(token))+"-"+token[:1], func(test *testing.T) {
				var expected wire
				expectedErr := json.Unmarshal([]byte(fixture.fragment), &expected)
				var expectedTypeError *json.UnmarshalTypeError
				if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "number "+token ||
					expectedTypeError.Type != reflect.TypeFor[float64]() || expectedTypeError.Struct != "wire" || expectedTypeError.Field != fixture.field {
					test.Fatal("field-fragment overflow oracle lost the original literal or owner context")
				}
				raw := []byte(`{"kind":"` + fixture.kind + `","seq":7,"call_id":"earlier-fields",` + fixture.fragment[1:])
				var whole wire
				wholeErr := json.Unmarshal(raw, &whole)
				var wholeTypeError *json.UnmarshalTypeError
				if !errors.As(wholeErr, &wholeTypeError) || wholeTypeError.Offset == expectedTypeError.Offset {
					test.Fatal("overflow fixture no longer distinguishes full-event and field-fragment offsets")
				}
				_, _, actualErr := Float64IntegrationNative(test, raw)
				var actualTypeError *json.UnmarshalTypeError
				if !errors.As(actualErr, &actualTypeError) {
					test.Fatalf("native overflow lost its private type error: %T", actualErr)
				}
				if actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type ||
					actualTypeError.Struct != expectedTypeError.Struct || actualTypeError.Field != expectedTypeError.Field || actualTypeError.Offset != expectedTypeError.Offset {
					test.Fatal("native overflow changed full literal, type, owner context, or field-fragment offset", len(actualTypeError.Value), len(expectedTypeError.Value), actualTypeError.Offset, expectedTypeError.Offset)
				}
				if actualErr.Error() != expectedErr.Error() {
					test.Fatal("native overflow abbreviated or changed the original error text", len(actualErr.Error()), len(expectedErr.Error()))
				}
			})
		}
	}
}

func TestLegacyNativeFloat64IntegrationStickyOverflow(test *testing.T) {
	for _, token := range []string{"1e999", "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)} {
		for _, fixture := range []struct {
			name  string
			event string
		}{
			{name: "input-duplicate-member", event: `{"kind":"tool_call","input":{"number":` + token + `,"number":-0}}`},
			{name: "input-null-reset", event: `{"kind":"tool_call","input":{"number":` + token + `},"INPUT":null,"input":{}}`},
			{name: "output-nested-null-reset", event: `{"kind":"tool_result","output":[[{"number":` + token + `,"number":0}]],"OUTPUT":null,"output":[]}`},
			{name: "output-scalar-reset", event: `{"kind":"tool_result","output":` + token + `,"OUTPUT":-0,"output":null,"output":{}}`},
		} {
			test.Run(fixture.name+"-"+strconv.Itoa(len(token)), func(test *testing.T) {
				var expected wire
				expectedErr := json.Unmarshal([]byte(fixture.event), &expected)
				var expectedTypeError *json.UnmarshalTypeError
				if !errors.As(expectedErr, &expectedTypeError) || expectedTypeError.Value != "number "+token {
					test.Fatal("original wire overflow was repaired by a duplicate or null reset")
				}
				actual, _, actualErr := Float64IntegrationNative(test, []byte(fixture.event))
				var actualTypeError *json.UnmarshalTypeError
				if !errors.As(actualErr, &actualTypeError) || actualTypeError.Value != expectedTypeError.Value || actualTypeError.Type != expectedTypeError.Type {
					test.Fatal("native overflow was repaired by a duplicate or null reset")
				}
				Float64IntegrationTree(test, "rejected-wire.input", actual.Input, expected.Input)
				Float64IntegrationTree(test, "rejected-wire.output", actual.Output, expected.Output)
				Float64IntegrationCanonical(test, legacyNativeEventDocument("2", fixture.event), false)
				for _, reset := range []string{"[]", "null"} {
					document := []byte(`{"envelope":{"cir_version":"2"},"events":[` + fixture.event + `],"EVENTS":` + reset + `,"events":[]}`)
					Float64IntegrationCanonical(test, document, false)
				}
			})
		}
	}
}

func TestLegacyNativeFloat64IntegrationIgnoredOverflow(test *testing.T) {
	for _, token := range []string{"1e999", "1e+" + strings.Repeat("9", 2*ConversationManifestChunkBytes+17)} {
		test.Run(strconv.Itoa(len(token)), func(test *testing.T) {
			for _, event := range []string{
				`{"kind":"tool_call","future":{"numbers":[` + token + `]},"input":{"number":-0}}`,
				`{"kind":"tool_result","future":` + token + `,"output":[[{"number":9007199254740993}]]}`,
				`{"kind":"tool_result","provider_metadata":{"future":` + token + `,"turn_id":"kept"},"output":-0}`,
			} {
				Float64IntegrationParity(test, event)
			}
			document := []byte(`{"future":{"number":` + token + `},"envelope":{"cir_version":"2"},"events":[{"kind":"tool_result","output":-0}]}`)
			Float64IntegrationCanonical(test, document, true)
		})
	}
}

func TestLegacyNativeFloat64IntegrationProviderNumberLexicalCharacterization(test *testing.T) {
	test.Log("Typed provider json.Number lexical characterization only; no giant provider-number cutover or cooperative conversion claim")
	for _, token := range []string{"-0.000E+03", "9007199254740993", "1.2300E+003", "1e99999"} {
		test.Run(token, func(test *testing.T) {
			quoted, err := json.Marshal(token)
			if err != nil {
				test.Fatal(err)
			}
			for _, spelling := range []struct {
				name string
				raw  string
			}{
				{name: "number", raw: token},
				{name: "quoted-number", raw: string(quoted)},
			} {
				test.Run(spelling.name, func(test *testing.T) {
					event := `{"kind":"tool_call","input":{"negative_zero":-0,"rounded":9007199254740993},"provider_metadata":{"turn_id":"lexical","create_time":` + spelling.raw + `}}`
					actual, canonical := Float64IntegrationParity(test, event)
					if actual.ProviderMetadata == nil || actual.ProviderMetadata.CreateTime == nil || actual.ProviderMetadata.CreateTime.String() != token {
						test.Fatal("typed provider json.Number was converted or lost its exact lexeme")
					}
					if !bytes.Contains(canonical, []byte(`"create_time":`+token)) {
						test.Fatal("canonical provider number was quoted or changed spelling")
					}
				})
			}
		})
	}
}
