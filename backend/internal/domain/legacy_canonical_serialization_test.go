package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
)

func TestLegacyDecodedCanonicalDocumentParity(t *testing.T) {
	cases := canonicalEventCases()
	cases = append(cases,
		canonicalEventCase{"number-boundaries", []byte(`{"kind":"tool_result","seq":9223372036854775807,"output":[9007199254740993,-0,1e-7,1e21,5e-324,1.7976931348623157e308],"provider_metadata":{"create_time":1E+3}}`)},
		canonicalEventCase{"number-overflow", []byte(`{"kind":"tool_result","seq":0,"output":1e309}`)},
		canonicalEventCase{"sequence-overflow", []byte(`{"kind":"turn","seq":9223372036854775808}`)},
		canonicalEventCase{"number-duplicates", []byte(`{"kind":"tool_result","seq":-9223372036854775808,"output":{"n":1,"n":9007199254740993},"provider_metadata":{"create_time":1787683260.123456789}}`)},
		canonicalEventCase{"unicode-order", []byte(`{"kind":"message","seq":3,"role":"custom-\uac80","blocks":[{"type":"text","text":"<&>\u2028\u2029\ud83d\ude42"}]}`)},
	)
	for _, version := range []string{"1", "2", "999"} {
		for _, tc := range cases {
			t.Run(version+"/"+tc.name, func(t *testing.T) {
				raw := []byte(fmt.Sprintf(`{"envelope":{"cir_version":%q},"events":[%s]}`, version, tc.raw))
				assertLegacyCanonicalParity(t, raw)
			})
		}
	}
	assertLegacyCanonicalParity(t, []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","seq":2,"role":"last"},{"kind":"compaction","seq":1,"replacement_complete":true,"replacement":[{"kind":"message","seq":9,"role":"later","blocks":[]},{"kind":"turn","seq":0,"role":"first"}]},{"kind":"turn","seq":2,"role":"equal-seq-stable"}]}`))
}

func assertLegacyCanonicalParity(t *testing.T, raw []byte) {
	t.Helper()
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		if proof, err := VerifyStoredDocBytes(HashContent(raw), raw); err == nil || proof.Valid() {
			t.Fatal("malformed typed input accepted")
		}
		return
	}
	want, oldErr := CanonicalBytes(doc)
	got, newErr := canonicalBytes(doc, canonicalDecodedEventJSON)
	if (oldErr == nil) != (newErr == nil) {
		t.Fatalf("canonical acceptance differs: %v / %v", oldErr, newErr)
	}
	if oldErr != nil {
		if proof, err := VerifyStoredDocBytes(HashContent(raw), raw); err == nil || proof.Valid() {
			t.Fatal("invalid schema/union accepted despite matching raw hash")
		}
		return
	}
	if !bytes.Equal(want, got) {
		t.Fatal("canonical document bytes differ")
	}
	hash := HashContent(want)
	proof, err := VerifyStoredDocBytes(hash, raw)
	if err != nil || !proof.Valid() || proof.Hash() != hash || proof.DocumentRef().Identity != DocumentIdentityLegacy {
		t.Fatal("legacy stored representation lost its exact identity", err)
	}
	if HashContent(raw) != hash {
		if proof, err := VerifyStoredDocBytes(HashContent(raw), raw); err == nil || proof.Valid() {
			t.Fatal("raw-byte hash bypassed canonical validation")
		}
	}
}

type legacyCanonicalCustomValue struct{}

func (legacyCanonicalCustomValue) MarshalJSON() ([]byte, error) {
	return []byte(`{"z":1E+3,"a":9007199254740993}`), nil
}

func TestLegacyCanonicalPublicGoValuesRemainGeneric(t *testing.T) {
	type nested struct {
		Z string `json:"z"`
		A string `json:"a"`
	}
	for _, value := range []any{
		nested{Z: "last", A: "first"},
		map[string]any{"typed": nested{}, "custom": legacyCanonicalCustomValue{}, "number": json.Number("9007199254740993")},
		[]any{legacyCanonicalCustomValue{}, &nested{}},
	} {
		doc := CIRDocument{Events: []CIREvent{{Kind: EventToolResult, Output: value}}}
		want, err := canonicalJSON(doc)
		if err != nil {
			t.Fatal(err)
		}
		got, err := CanonicalBytes(doc)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("public arbitrary-Go-value normalization changed", err)
		}
		validated, err := ValidatedSessionDocBytes(SessionDoc{Hash: HashContent(want), CIR: doc})
		if err != nil || !bytes.Equal(validated, want) {
			t.Fatal("public validated writer changed", err)
		}
	}
}
