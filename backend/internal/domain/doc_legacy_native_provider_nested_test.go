package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeProviderNestedFixture struct {
	name           string
	event          string
	metadata       *ProviderMetadata
	nativeRejected bool
	rejected       bool
}

func legacyNativeProviderNestedParity(test *testing.T, fixture legacyNativeProviderNestedFixture) {
	test.Helper()
	raw := []byte(fixture.event)
	var expected wire
	expectedErr := json.Unmarshal(raw, &expected)
	if (expectedErr != nil) != fixture.nativeRejected {
		test.Fatal("provider fixture changed native wire acceptance", expectedErr)
	}
	if expectedErr == nil && !reflect.DeepEqual(expected.ProviderMetadata, fixture.metadata) {
		test.Fatal("native wire lost expected provider pointer merge, reset, or number spelling")
	}
	var actual wire
	presence, actualErr := legacyNativeMetadataDecode(test, raw, &actual, true)
	if (actualErr != nil) != fixture.nativeRejected {
		test.Fatal("direct provider decoder changed native wire acceptance", actualErr, expectedErr)
	}
	if json.Valid(raw) && reflect.TypeOf(actualErr) != reflect.TypeOf(expectedErr) {
		test.Fatal("direct provider decoder changed native error type", actualErr, expectedErr)
	}
	if actualErr == nil && (!reflect.DeepEqual(actual, expected) || presence != eventFieldProviderMetadata) {
		test.Fatal("direct provider decoder changed native fields or exact outer presence", presence)
	}
	document := []byte(`{"envelope":{"cir_version":"2"},"events":[` + fixture.event + `]}`)
	want, wantErr := legacyNormalizationOracle(document)
	if (wantErr != nil) != fixture.rejected {
		test.Fatal("provider fixture changed full typed acceptance", wantErr)
	}
	buffer := &legacySourceProofBuffer{body: document}
	got, err := normalizeLegacyFixture(context.Background(), buffer.input())
	if (err != nil) != fixture.rejected || !errors.Is(err, wantErr) || !bytes.Equal(got, want) {
		test.Fatal("provider normalizer changed acceptance or canonical bytes", err, wantErr, len(got), len(want))
	}
	if errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("authenticated provider rejection became a private-source failure", err)
	}
	if err == nil {
		var normalized struct {
			Events []wire `json:"events"`
		}
		if decodeErr := json.Unmarshal(got, &normalized); decodeErr != nil || len(normalized.Events) != 1 ||
			!reflect.DeepEqual(normalized.Events[0].ProviderMetadata, fixture.metadata) {
			test.Fatal("canonical provider output lost native metadata or exact number spelling", decodeErr)
		}
		if fixture.metadata != nil && fixture.metadata.CreateTime != nil &&
			!bytes.Contains(got, []byte(`"create_time":`+fixture.metadata.CreateTime.String())) {
			test.Fatal("canonical provider create_time was quoted or its lexical spelling changed")
		}
	}
}

func TestLegacyNativeProviderNestedGiantTurnIDParity(test *testing.T) {
	giantText := strings.Repeat("x", ConversationManifestChunkBytes+73) + "🙂<&>\u2028\u2029\"\\\n\x00"
	encoded, err := json.Marshal(giantText)
	if err != nil {
		test.Fatal(err)
	}
	giant := string(encoded)
	initialTime := json.Number("1787683260.123456789")
	changedTime := json.Number("-0.000E+03")
	for _, fixture := range []legacyNativeProviderNestedFixture{
		{
			name:     "giant-turn-id",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"create_time":1787683260.123456789}}`,
			metadata: &ProviderMetadata{TurnID: giantText, CreateTime: &initialTime},
		},
		{
			name:     "duplicate-outer-object-retains-giant-turn-id",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"create_time":1787683260.123456789},"PROVIDER_METADATA":{"CREATE_TIME":"-0.000E+03"}}`,
			metadata: &ProviderMetadata{TurnID: giantText, CreateTime: &changedTime},
		},
		{
			name:     "duplicate-outer-object-retains-number",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":"old","create_time":1787683260.123456789},"provider_\u006detadata":{"TURN_ID":` + giant + `}}`,
			metadata: &ProviderMetadata{TurnID: giantText, CreateTime: &initialTime},
		},
		{
			name:     "empty-object-and-inner-string-null-preserve-fields",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"create_time":1787683260.123456789},"provider_metadata":{},"PROVIDER_METADATA":{"turn_\u0069d":null}}`,
			metadata: &ProviderMetadata{TurnID: giantText, CreateTime: &initialTime},
		},
		{
			name:     "inner-number-null-clears-only-number-pointer",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"create_time":1787683260.123456789},"provider_metadata":{"create_time":null}}`,
			metadata: &ProviderMetadata{TurnID: giantText},
		},
		{
			name:     "duplicate-inner-turn-id-replaces-giant-and-null-keeps-replacement",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"TURN_ID":"fresh","turn_\u0069d":null,"create_time":1787683260.123456789}}`,
			metadata: &ProviderMetadata{TurnID: "fresh", CreateTime: &initialTime},
		},
		{
			name:     "outer-null-then-object-allocates-giant-turn-id",
			event:    `{"kind":"turn","provider_metadata":null,"PROVIDER_METADATA":{"turn_id":` + giant + `,"create_time":"-0.000E+03"}}`,
			metadata: &ProviderMetadata{TurnID: giantText, CreateTime: &changedTime},
		},
		{
			name:     "outer-null-discards-number-before-giant-object",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":"stale","create_time":1787683260.123456789},"provider_metadata":null,"provider_metadata":{"turn_id":` + giant + `}}`,
			metadata: &ProviderMetadata{TurnID: giantText},
		},
		{
			name:     "outer-null-discards-giant-before-number-object",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"create_time":1787683260.123456789},"PROVIDER_METADATA":null,"provider_metadata":{"create_time":"-0.000E+03"}}`,
			metadata: &ProviderMetadata{CreateTime: &changedTime},
		},
		{
			name:     "outer-null-then-empty-object-is-non-nil-and-empty",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `,"create_time":1787683260.123456789},"provider_metadata":null,"provider_metadata":{}}`,
			metadata: &ProviderMetadata{},
		},
		{
			name:     "final-outer-null-decodes-natively-but-is-not-a-valid-event",
			event:    `{"kind":"turn","provider_metadata":{"turn_id":` + giant + `},"provider_metadata":null}`,
			rejected: true,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeProviderNestedParity(test, fixture)
		})
	}
}

func TestLegacyNativeProviderNestedCreateTimeLexicalParity(test *testing.T) {
	for _, fixture := range []struct {
		name   string
		lexeme string
	}{
		{name: "zero", lexeme: "0"},
		{name: "ordinary-integer", lexeme: "123"},
		{name: "negative-integer", lexeme: "-17"},
		{name: "beyond-float64-exact-integer", lexeme: "9007199254740993"},
		{name: "precise-fraction", lexeme: "1787683260.123456789"},
		{name: "fraction-trailing-zeroes", lexeme: "1.2300"},
		{name: "uppercase-exponent-with-leading-zero", lexeme: "1E+03"},
		{name: "lowercase-negative-exponent", lexeme: "1e-03"},
		{name: "exponent-beyond-float64", lexeme: "1e9999"},
		{name: "exponent-below-float64", lexeme: "1e-9999"},
		{name: "negative-zero", lexeme: "-0"},
		{name: "negative-fractional-zero", lexeme: "-0.000"},
		{name: "negative-zero-exponent", lexeme: "-0E+00"},
		{name: "large-number-semantic-parity-only", lexeme: "1" + strings.Repeat("0", LegacyCanonicalEncodingBytes+17)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			quoted, err := json.Marshal(fixture.lexeme)
			if err != nil {
				test.Fatal(err)
			}
			for _, spelling := range []struct {
				name string
				raw  string
			}{
				{name: "number", raw: fixture.lexeme},
				{name: "quoted-number", raw: string(quoted)},
			} {
				test.Run(spelling.name, func(test *testing.T) {
					number := json.Number(fixture.lexeme)
					legacyNativeProviderNestedParity(test, legacyNativeProviderNestedFixture{
						event:    `{"kind":"turn","provider_metadata":{"turn_id":"lexical","create_time":` + spelling.raw + `}}`,
						metadata: &ProviderMetadata{TurnID: "lexical", CreateTime: &number},
					})
				})
			}
		})
	}
	test.Run("quoted-number-decodes-json-escapes-before-validating-lexeme", func(test *testing.T) {
		number := json.Number("-0.00E+03")
		legacyNativeProviderNestedParity(test, legacyNativeProviderNestedFixture{
			event:    `{"kind":"turn","provider_metadata":{"create_time":"\u002d0\u002e00\u0045\u002b03"}}`,
			metadata: &ProviderMetadata{CreateTime: &number},
		})
	})
	test.Run("number-pointer-null-then-quoted-number", func(test *testing.T) {
		number := json.Number("-0E+00")
		legacyNativeProviderNestedParity(test, legacyNativeProviderNestedFixture{
			event:    `{"kind":"turn","provider_metadata":{"turn_id":"kept","create_time":1.2300,"create_time":null,"CREATE_TIME":"-0E+00"}}`,
			metadata: &ProviderMetadata{TurnID: "kept", CreateTime: &number},
		})
	})
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
		{name: "large-invalid-number-semantic-parity-only", lexeme: "0" + strings.Repeat("1", LegacyCanonicalEncodingBytes+17)},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			quoted, err := json.Marshal(fixture.lexeme)
			if err != nil {
				test.Fatal(err)
			}
			legacyNativeProviderNestedParity(test, legacyNativeProviderNestedFixture{
				event:          `{"kind":"turn","provider_metadata":{"create_time":` + string(quoted) + `}}`,
				nativeRejected: true,
				rejected:       true,
			})
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
			legacyNativeProviderNestedParity(test, fixture)
		})
	}
}
