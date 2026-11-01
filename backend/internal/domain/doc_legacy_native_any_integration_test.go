package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

const legacyNativeAnyIntegrationDocumentPrefix = `{"ENVE\u004cOPE":{"CIR_VERſION":"2"},"event\u017f":[`

const legacyNativeAnyIntegrationNumbers = `[-0,-0.0,0,-1e-999,1e-999,9007199254740993,9007199254740995,1.234567890123456789,0.1,1e21,1e-7,5e-324,1.7976931348623157e308]`

func legacyNativeAnyIntegrationValueCheck(test *testing.T, path string, actual, expected any) {
	test.Helper()
	switch expectedValue := expected.(type) {
	case map[string]any:
		actualValue, ok := actual.(map[string]any)
		if !ok || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
			test.Fatalf("%s changed native map type, nilness, or length: got %T, want %T", path, actual, expected)
		}
		for key, child := range expectedValue {
			actualChild, exists := actualValue[key]
			if !exists {
				test.Fatalf("%s lost native map key %q", path, key)
			}
			legacyNativeAnyIntegrationValueCheck(test, fmt.Sprintf("%s[%q]", path, key), actualChild, child)
		}
	case []any:
		actualValue, ok := actual.([]any)
		if !ok || (actualValue == nil) != (expectedValue == nil) || len(actualValue) != len(expectedValue) {
			test.Fatalf("%s changed native array type, nilness, or length: got %T, want %T", path, actual, expected)
		}
		for index, child := range expectedValue {
			legacyNativeAnyIntegrationValueCheck(test, fmt.Sprintf("%s[%d]", path, index), actualValue[index], child)
		}
	case float64:
		actualValue, ok := actual.(float64)
		if !ok || math.Float64bits(actualValue) != math.Float64bits(expectedValue) {
			test.Fatalf("%s changed native float64 bits: got type %T bits=%016x, want bits=%016x", path, actual, math.Float64bits(actualValue), math.Float64bits(expectedValue))
		}
	default:
		if !reflect.DeepEqual(actual, expected) {
			test.Fatalf("%s changed native scalar type or value: got %T, want %T", path, actual, expected)
		}
	}
}

func legacyNativeAnyIntegrationWire(test *testing.T, event string) wire {
	test.Helper()
	raw := []byte(event)
	var expected wire
	if err := json.Unmarshal(raw, &expected); err != nil {
		test.Fatal("generic fixture lost its encoding/json wire acceptance", err)
	}
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("generic native event constructor failed", err)
	}
	native.reset(0, len(raw))
	native.object.prefix = true
	buffer := &legacySourceProofBuffer{body: raw}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	ctx := context.Background()
	var frame legacyJSONValueScanner
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		_, complete, err := native.step(ctx, input, &frame)
		if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields > LegacyNormalizationStepEvents*len(native.object.metadata.fields) {
			test.Fatal("generic giant-string decoding exceeded shared byte, load, or action bounds", stepIndex, input.workBytes, input.loads, input.fields, input.unbounded)
		}
		if err != nil || input.failure != nil {
			test.Fatal("generic native decoding rejected an accepted wire event", stepIndex, err, input.failure)
		}
		if retained, retainedErr := native.retained(); retainedErr != nil || retained <= 0 {
			test.Fatal("generic native decoding lost retained-byte accounting", retained, retainedErr)
		}
		if !complete {
			continue
		}
		if !reflect.DeepEqual(native.decoded, expected) {
			test.Fatal("generic native decoding changed the original wire event")
		}
		legacyNativeAnyIntegrationValueCheck(test, "wire.input", native.decoded.Input, expected.Input)
		legacyNativeAnyIntegrationValueCheck(test, "wire.output", native.decoded.Output, expected.Output)
		return native.decoded
	}
	test.Fatal("generic native decoding exhausted its finite per-part step allowance")
	return wire{}
}

func legacyNativeAnyIntegrationCheck(test *testing.T, event string) wire {
	test.Helper()
	raw := []byte(legacyNativeAnyIntegrationDocumentPrefix + event + `]}`)
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	actual, err := legacyNativeScalarReviewDrain(test, normalizer, input)
	legacyNativeScalarReviewCompare(test, raw, actual, err, true)
	return legacyNativeAnyIntegrationWire(test, event)
}

func TestLegacyNativeAnyIntegrationGiantMixedValues(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		prefix string
		suffix string
	}{
		{
			name:   "input-object-in-array",
			prefix: `{"Kind":"tool_call","ſeq":7,"call_id":"call","INP\u0055T":{"mixed":[false,null,{},[],{"text":`,
			suffix: `,"numbers":` + legacyNativeAnyIntegrationNumbers + `,"empty_key":{"":null},"duplicate":{"old":1},"dupli\u0063ate":{"new":2}}],"tail":true}}`,
		},
		{
			name:   "input-array-in-object",
			prefix: `{"\u006bind":"tool_call","seq":7,"inpu\u0074":{"outer":{"mixed":[{"text":`,
			suffix: `},` + legacyNativeAnyIntegrationNumbers + `,null,[],{},false]},"":{},"끝":[]}}`,
		},
		{
			name:   "output-object-in-array",
			prefix: `{"Kind":"tool_result","ſeq":7,"call_id":"call","OUTP\u0055T":[false,null,[],{}, {"nested":{"text":`,
			suffix: `,"numbers":` + legacyNativeAnyIntegrationNumbers + `,"duplicate":[1,2],"dupli\u0063ate":[null]}}]}`,
		},
		{
			name:   "output-array-in-object",
			prefix: `{"\u006bind":"tool_result","seq":7,"outp\u0075t":{"outer":{"mixed":[`,
			suffix: `,` + legacyNativeAnyIntegrationNumbers + `,null,[],{},false]},"":null,"한🙂":{"empty":{}}}}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			giant := legacyNativeOutputReviewString(test, legacyNativeAnyIntegrationDocumentPrefix+fixture.prefix)
			legacyNativeAnyIntegrationCheck(test, fixture.prefix+giant+fixture.suffix)
		})
	}
}

func TestLegacyNativeAnyIntegrationDuplicateContainers(test *testing.T) {
	giant := legacyNativeOutputReviewString(test, legacyNativeAnyIntegrationDocumentPrefix)
	var nativeGiant string
	if err := json.Unmarshal([]byte(giant), &nativeGiant); err != nil {
		test.Fatal("generic duplicate fixture lost native string acceptance", err)
	}
	for _, fixture := range []struct {
		name       string
		kind       string
		fields     string
		wantInput  map[string]any
		wantOutput any
	}{
		{
			name:      "outer-input-merges-but-nested-object-replaces",
			kind:      "tool_call",
			fields:    `"input":{"keep":` + giant + `,"replace":{"old":1},"same":1},"INP\u0055T":{"replace":{"new":2},"same":null}`,
			wantInput: map[string]any{"keep": nativeGiant, "replace": map[string]any{"new": float64(2)}, "same": nil},
		},
		{
			name:      "outer-input-empty-object-preserves-entries",
			kind:      "tool_call",
			fields:    `"input":{"keep":` + giant + `},"INPUT":{}`,
			wantInput: map[string]any{"keep": nativeGiant},
		},
		{
			name:      "outer-input-null-clears-before-empty-object",
			kind:      "tool_call",
			fields:    `"input":{"stale":` + giant + `},"INPUT":null,"inpu\u0074":{}`,
			wantInput: map[string]any{},
		},
		{
			name:   "outer-input-null-is-nil",
			kind:   "tool_call",
			fields: `"input":{"stale":` + giant + `},"INPUT":null`,
		},
		{
			name:      "duplicate-nested-array-replaces-not-appends",
			kind:      "tool_call",
			fields:    `"input":{"replace":[` + giant + `,{"old":1}],"repl\u0061ce":[{"new":2}],"":[],"KEEP":1,"keep":2}`,
			wantInput: map[string]any{"replace": []any{map[string]any{"new": float64(2)}}, "": []any{}, "KEEP": float64(1), "keep": float64(2)},
		},
		{
			name:      "outer-input-nested-null-then-object-starts-fresh",
			kind:      "tool_call",
			fields:    `"input":{"replace":{"stale":` + giant + `}},"INPUT":{"replace":null},"inpu\u0074":{"replace":{"fresh":true}}`,
			wantInput: map[string]any{"replace": map[string]any{"fresh": true}},
		},
		{
			name:       "outer-output-object-replaces-not-merges",
			kind:       "tool_result",
			fields:     `"output":{"stale":` + giant + `,"replace":{"old":1}},"OUTP\u0055T":{"replace":{"new":2}}`,
			wantOutput: map[string]any{"replace": map[string]any{"new": float64(2)}},
		},
		{
			name:       "duplicate-nested-output-object-replaces",
			kind:       "tool_result",
			fields:     `"output":{"keep":` + giant + `,"replace":{"old":1},"repl\u0061ce":{"new":2}}`,
			wantOutput: map[string]any{"keep": nativeGiant, "replace": map[string]any{"new": float64(2)}},
		},
		{
			name:       "outer-output-array-replaces-without-hidden-tail",
			kind:       "tool_result",
			fields:     `"output":[{"old":1},` + giant + `],"OUTPUT":[{}]`,
			wantOutput: []any{map[string]any{}},
		},
		{
			name:       "outer-output-object-then-array-replaces",
			kind:       "tool_result",
			fields:     `"output":{"stale":` + giant + `},"OUTPUT":[null,{},[]]`,
			wantOutput: []any{nil, map[string]any{}, []any{}},
		},
		{
			name:       "outer-output-array-then-object-replaces",
			kind:       "tool_result",
			fields:     `"output":[` + giant + `,{"stale":1}],"outp\u0075t":{"fresh":[]}`,
			wantOutput: map[string]any{"fresh": []any{}},
		},
		{
			name:       "outer-output-empty-object-is-not-nil",
			kind:       "tool_result",
			fields:     `"output":{"stale":` + giant + `},"OUTPUT":{}`,
			wantOutput: map[string]any{},
		},
		{
			name:       "outer-output-empty-array-is-not-nil",
			kind:       "tool_result",
			fields:     `"output":[` + giant + `],"OUTPUT":[]`,
			wantOutput: []any{},
		},
		{
			name:   "outer-output-null-clears-generic-object",
			kind:   "tool_result",
			fields: `"output":{"stale":` + giant + `},"OUTPUT":null`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			event := `{"kind":"` + fixture.kind + `","seq":7,"call_id":"call",` + fixture.fields + `}`
			actual := legacyNativeAnyIntegrationCheck(test, event)
			legacyNativeAnyIntegrationValueCheck(test, "wire.input", actual.Input, fixture.wantInput)
			legacyNativeAnyIntegrationValueCheck(test, "wire.output", actual.Output, fixture.wantOutput)
		})
	}
}

func TestLegacyNativeAnyIntegrationFloat64Bits(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		event string
	}{
		{name: "input", event: `{"kind":"tool_call","input":{"numbers":` + legacyNativeAnyIntegrationNumbers + `}}`},
		{name: "output", event: `{"kind":"tool_result","output":[{"numbers":` + legacyNativeAnyIntegrationNumbers + `}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			actual := legacyNativeAnyIntegrationCheck(test, fixture.event)
			var values []any
			if fixture.name == "input" {
				values = actual.Input["numbers"].([]any)
			} else {
				values = actual.Output.([]any)[0].(map[string]any)["numbers"].([]any)
			}
			expected := []float64{
				math.Copysign(0, -1), math.Copysign(0, -1), 0, math.Copysign(0, -1), 0,
				9007199254740992, 9007199254740996, 1.2345678901234567, 0.1,
				1e21, 1e-7, math.SmallestNonzeroFloat64, math.MaxFloat64,
			}
			if len(values) != len(expected) {
				test.Fatal("numeric fixture changed its finite ordinary-token count", len(values))
			}
			for index, number := range expected {
				legacyNativeAnyIntegrationValueCheck(test, fmt.Sprintf("wire.numbers[%d]", index), values[index], number)
			}
		})
	}
}

func TestLegacyNativeAnyIntegrationDeepMixedShapes(test *testing.T) {
	for _, depth := range []int{65, 257} {
		for _, field := range []string{"input", "output"} {
			test.Run(fmt.Sprintf("%s/depth-%d", field, depth), func(test *testing.T) {
				var prefix strings.Builder
				if field == "input" {
					prefix.WriteString(`{"kind":"tool_call","input":{"root":`)
				} else {
					prefix.WriteString(`{"kind":"tool_result","output":`)
				}
				for level := 0; level < depth; level++ {
					if level%2 == 0 {
						prefix.WriteString(`{"한🙂":`)
					} else {
						prefix.WriteString(`[0.1,null,{},[],`)
					}
				}
				prefix.WriteString(`{"text":`)
				giant := legacyNativeOutputReviewString(test, legacyNativeAnyIntegrationDocumentPrefix+prefix.String())
				var suffix strings.Builder
				suffix.WriteString(`,"numbers":` + legacyNativeAnyIntegrationNumbers + `,"nil":null,"empty":{}}`)
				for level := depth - 1; level >= 0; level-- {
					if level%2 == 0 {
						suffix.WriteByte('}')
					} else {
						suffix.WriteByte(']')
					}
				}
				if field == "input" {
					suffix.WriteByte('}')
				}
				suffix.WriteByte('}')
				legacyNativeAnyIntegrationCheck(test, prefix.String()+giant+suffix.String())
			})
		}
	}
}
