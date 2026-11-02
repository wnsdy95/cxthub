package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func legacyNativePublicRejectExemptionCanonical(test *testing.T, document string) []byte {
	test.Helper()
	raw := []byte(document)
	expected, expectedErr := legacyNormalizationOracle(raw)
	if expectedErr != nil {
		test.Fatal("original native CIR oracle rejected an acceptance exemption", expectedErr)
	}
	buffer := &legacySourceProofBuffer{body: raw}
	actual, actualErr := normalizeLegacyFixture(context.Background(), buffer.input())
	if actualErr != nil {
		test.Fatal("public normalization rejected an original native CIR acceptance exemption", actualErr)
	}
	if !bytes.Equal(actual, expected) {
		test.Fatal("public normalization changed original canonical bytes for an acceptance exemption", len(actual), len(expected))
	}
	return actual
}

func legacyNativePublicRejectExemptionProvider(test *testing.T, event, turnID, number string) {
	test.Helper()
	canonical := legacyNativePublicRejectExemptionCanonical(test, `{"envelope":{"cir_version":"2"},"events":[`+event+`]}`)
	var decoded CIRDocument
	if err := json.Unmarshal(canonical, &decoded); err != nil {
		test.Fatal("canonical acceptance exemption no longer decodes as native CIR", err)
	}
	if len(decoded.Events) != 1 || decoded.Events[0].ProviderMetadata == nil {
		test.Fatal("canonical acceptance exemption lost its provider metadata object")
	}
	metadata := decoded.Events[0].ProviderMetadata
	if metadata.TurnID != turnID || (metadata.CreateTime == nil) != (number == "") {
		test.Fatal("canonical acceptance exemption changed provider duplicate or null semantics")
	}
	if number != "" && (metadata.CreateTime.String() != number || !bytes.Contains(canonical, []byte(`"create_time":`+number))) {
		test.Fatal("canonical provider json.Number was rounded, quoted, or changed its original spelling")
	}
}

func TestLegacyNativePublicRejectExemptionsProviderNumberSpellings(test *testing.T) {
	longDigits := strings.Repeat("9", LegacyCanonicalEncodingBytes+17)
	for _, fixture := range []struct {
		name  string
		token string
	}{
		{name: "small-overflow-exponent", token: "1e9999"},
		{name: "small-underflow-exponent", token: "1e-9999"},
		{name: "negative-zero-spelling", token: "-0.000E+03"},
		{name: "trailing-zeroes-and-exponent-spelling", token: "1.2300E+003"},
		{name: "integer-beyond-float64-precision", token: "9007199254740993"},
		{name: "precise-fraction", token: "1787683260.123456789"},
		{name: "over-64k-positive-exponent", token: "1E+" + longDigits},
		{name: "over-64k-negative-exponent", token: "-1e-" + longDigits},
		{name: "over-64k-fraction", token: "-0." + strings.Repeat("0", LegacyCanonicalEncodingBytes+17) + "1E+003"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			quoted, err := json.Marshal(fixture.token)
			if err != nil {
				test.Fatal(err)
			}
			escaped := strings.NewReplacer("-", `\u002d`, ".", `\u002e`, "e", `\u0065`, "E", `\u0045`, "+", `\u002b`).Replace(fixture.token)
			for _, spelling := range []struct {
				name string
				raw  string
			}{
				{name: "number", raw: fixture.token},
				{name: "quoted-number", raw: string(quoted)},
				{name: "escaped-quoted-number", raw: `"` + escaped + `"`},
			} {
				test.Run(spelling.name, func(test *testing.T) {
					event := `{"kind":"turn","seq":7,"role":"user","provider_metadata":{"turn_id":"lexical","create_time":` + spelling.raw + `}}`
					legacyNativePublicRejectExemptionProvider(test, event, "lexical", fixture.token)
				})
			}
		})
	}
}

func TestLegacyNativePublicRejectExemptionsProviderNullDuplicates(test *testing.T) {
	for _, size := range []struct {
		name  string
		token string
	}{
		{name: "small-control", token: "1e9999"},
		{name: "over-64k-exponent", token: "1e+" + strings.Repeat("9", LegacyCanonicalEncodingBytes+17)},
	} {
		test.Run(size.name, func(test *testing.T) {
			initial := `"provider_metadata":{"turn_id":"retained","create_time":` + size.token + `}`
			for _, fixture := range []struct {
				name   string
				fields string
				turnID string
				number string
			}{
				{
					name:   "outer-object-merges-and-string-null-retains",
					fields: initial + `,"provider_metadata":{},"PROVIDER_METADATA":{"turn_id":null}`,
					turnID: "retained",
					number: size.token,
				},
				{
					name:   "inner-number-null-clears-pointer",
					fields: initial + `,"provider_metadata":{"create_time":null}`,
					turnID: "retained",
				},
				{
					name:   "folded-quoted-duplicate-replaces-long-number",
					fields: initial + `,"PROVIDER_METADATA":{"CREATE_TIME":"-0.000E+03"}`,
					turnID: "retained",
					number: "-0.000E+03",
				},
				{
					name:   "inner-null-then-escaped-key-number",
					fields: `"provider_metadata":{"turn_id":"retained","create_time":null,"create_\u0074ime":` + size.token + `}`,
					turnID: "retained",
					number: size.token,
				},
				{
					name:   "outer-null-then-quoted-number",
					fields: initial + `,"provider_metadata":null,"provider_\u006detadata":{"turn_id":"after-null","create_time":"` + size.token + `"}`,
					turnID: "after-null",
					number: size.token,
				},
				{
					name:   "outer-null-discards-number-before-new-object",
					fields: initial + `,"PROVIDER_METADATA":null,"provider_metadata":{"turn_id":"after-null"}`,
					turnID: "after-null",
				},
				{
					name:   "outer-null-then-empty-object",
					fields: initial + `,"provider_metadata":null,"provider_metadata":{}`,
				},
			} {
				test.Run(fixture.name, func(test *testing.T) {
					event := `{"kind":"turn","seq":7,"role":"user",` + fixture.fields + `}`
					legacyNativePublicRejectExemptionProvider(test, event, fixture.turnID, fixture.number)
				})
			}
		})
	}
}

func TestLegacyNativePublicRejectExemptionsQuotedPayloadNumbers(test *testing.T) {
	for _, size := range []struct {
		name  string
		token string
	}{
		{name: "small-control", token: "1e9999"},
		{name: "over-64k-exponent", token: "1e+" + strings.Repeat("9", LegacyCanonicalEncodingBytes+17)},
	} {
		test.Run(size.name, func(test *testing.T) {
			quoted, err := json.Marshal(size.token)
			if err != nil {
				test.Fatal(err)
			}
			for _, fixture := range []struct {
				name     string
				event    string
				fragment string
			}{
				{
					name:     "root-output-string",
					event:    `{"kind":"tool_result","seq":7,"output":` + string(quoted) + `}`,
					fragment: `"output":` + string(quoted),
				},
				{
					name:     "input-map-string",
					event:    `{"kind":"tool_call","seq":7,"input":{"quoted":` + string(quoted) + `}}`,
					fragment: `"quoted":` + string(quoted),
				},
				{
					name:     "nested-output-string-and-typed-provider-number",
					event:    `{"kind":"tool_result","seq":7,"output":[null,{"create_time":` + string(quoted) + `}],"provider_metadata":{"create_time":-0.000E+03}}`,
					fragment: `"output":[null,{"create_time":` + string(quoted) + `}]`,
				},
			} {
				test.Run(fixture.name, func(test *testing.T) {
					canonical := legacyNativePublicRejectExemptionCanonical(test, `{"envelope":{"cir_version":"2"},"events":[`+fixture.event+`]}`)
					if !bytes.Contains(canonical, []byte(fixture.fragment)) {
						test.Fatal("public canonical output interpreted a quoted payload number as a numeric token")
					}
				})
			}
		})
	}
}

func TestLegacyNativePublicRejectExemptionsIgnoredOverflow(test *testing.T) {
	control := `{"envelope":{"cir_version":"2"},"events":[{"kind":"message","seq":7,"role":"user","blocks":[{"type":"text","text":"kept"}],"provider_metadata":{"turn_id":"kept","create_time":-0.000E+03}}]}`
	expected := legacyNativePublicRejectExemptionCanonical(test, control)
	for _, size := range []struct {
		name  string
		token string
	}{
		{name: "small-control", token: "1e9999"},
		{name: "over-64k-exponent", token: "1e+" + strings.Repeat("9", LegacyCanonicalEncodingBytes+17)},
	} {
		test.Run(size.name, func(test *testing.T) {
			for _, fixture := range []struct {
				name     string
				anchor   string
				addition string
			}{
				{name: "root-unknown", anchor: `"envelope":`, addition: `"future":` + size.token + `,`},
				{name: "envelope-unknown", anchor: `"cir_version":`, addition: `"future":` + size.token + `,`},
				{name: "event-unknown-known-looking-children", anchor: `"kind":`, addition: `"future":{"input":[` + size.token + `],"output":` + size.token + `,"seq":"not-an-int"},`},
				{name: "provider-unknown", anchor: `"turn_id":`, addition: `"future":` + size.token + `,`},
				{name: "provider-number-lookalikes", anchor: `"turn_id":`, addition: `"create_time ":` + size.token + `,"create_time\u0000":` + size.token + `,`},
				{name: "block-unknown", anchor: `"type":`, addition: `"future":` + size.token + `,`},
			} {
				test.Run(fixture.name, func(test *testing.T) {
					document := strings.Replace(control, fixture.anchor, fixture.addition+fixture.anchor, 1)
					actual := legacyNativePublicRejectExemptionCanonical(test, document)
					if !bytes.Equal(actual, expected) {
						test.Fatal("ignored numeric overflow changed the selected canonical document")
					}
				})
			}
		})
	}
}
