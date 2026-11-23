package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLegacyNativePublicRejectExemptionsProviderNumberSpellings(test *testing.T) {
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
					_ = `{"kind":"turn","seq":7,"role":"user","provider_metadata":{"turn_id":"lexical","create_time":` + spelling.raw + `}}`
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
					_ = `{"kind":"turn","seq":7,"role":"user",` + fixture.fields + `}`
				})
			}
		})
	}
}
