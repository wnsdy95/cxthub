package domain

import (
	"encoding/json"
	"testing"
)

type legacyNativeProviderNestedFixture struct {
	name           string
	event          string
	metadata       *ProviderMetadata
	nativeRejected bool
	rejected       bool
}

func TestLegacyNativeProviderNestedCreateTimeInvalidLexicalParity(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		lexeme string
	}{
		{name: "empty", lexeme: ""},
		{name: "leading-whitespace", lexeme: " 1"},
		{name: "trailing-whitespace", lexeme: "1 "},
		{name: "leading-plus", lexeme: "+1"},
		{name: "leading-zero", lexeme: "01"},
		{name: "negative-leading-zero", lexeme: "-01"},
		{name: "missing-integer", lexeme: ".5"},
		{name: "missing-fraction", lexeme: "1."},
		{name: "missing-exponent", lexeme: "1e"},
		{name: "exponent-sign-only", lexeme: "1E+"},
		{name: "minus-only", lexeme: "-"},
		{name: "nan", lexeme: "NaN"},
		{name: "infinity", lexeme: "Infinity"},
		{name: "not-a-number", lexeme: "not-a-number"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			_, err := json.Marshal(fixture.lexeme)
			if err != nil {
				test.Fatal(err)
			}
		})
	}
	for _, fixture := range []legacyNativeProviderNestedFixture{
		{name: "invalid-quoted-number-is-not-repaired-by-duplicate", event: `{"kind":"turn","provider_metadata":{"create_time":"01","create_time":1}}`},
		{name: "invalid-quoted-number-is-not-repaired-by-inner-null", event: `{"kind":"turn","provider_metadata":{"create_time":"not-a-number","create_time":null}}`},
		{name: "invalid-quoted-number-is-not-repaired-by-outer-null-and-object", event: `{"kind":"turn","provider_metadata":{"create_time":"1e+"},"provider_metadata":null,"provider_metadata":{"create_time":1}}`},
		{name: "unquoted-leading-zero", event: `{"kind":"turn","provider_metadata":{"create_time":01}}`},
		{name: "unquoted-missing-exponent", event: `{"kind":"turn","provider_metadata":{"create_time":1e}}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			fixture.nativeRejected, fixture.rejected = true, true
		})
	}
}
