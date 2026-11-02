package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestLegacyCanonicalBlocksReplacementReviewStableRecursiveOrder(test *testing.T) {
	const outerFirst = `{"kind":"message","id":"outer-first","seq":8,"role":"user","blocks":[{"type":"text","text":"outer"}]}`
	raw := []byte(`{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","id":"root","seq":17,"provider_metadata":{},"replacement_complete":false,"replacement":[` +
		outerFirst + `,{"kind":"compaction","id":"inner","seq":-3,"replacement_complete":true,"replacement":[` +
		`{"kind":"message","id":"inner-first","seq":4,"role":"user","blocks":[null,{"type":"text","text":"first"}]},` +
		`{"kind":"message","id":"inner-low","seq":-2,"role":"user","blocks":null,"provider_metadata":{}},` +
		`{"kind":"compaction","id":"empty","seq":0,"replacement":[]},` +
		`{"kind":"message","id":"inner-second","seq":4,"role":"assistant","blocks":[{"type":"text","text":"<&>"}],"provider_metadata":{"turn_id":"nested","create_time":-0.000E+03}}]},` +
		`{"kind":"message","id":"outer-second","seq":8,"role":"user"},` + outerFirst + `]}]}`)
	const canonicalOuterFirst = `{"blocks":[{"text":"outer","type":"text"}],"id":"outer-first","kind":"message","role":"user","seq":8}`
	const expected = `{"id":"root","kind":"compaction","provider_metadata":{},"replacement":[` +
		`{"id":"inner","kind":"compaction","replacement":[` +
		`{"blocks":[],"id":"inner-low","kind":"message","provider_metadata":{},"role":"user","seq":-2},` +
		`{"id":"empty","kind":"compaction","replacement":[],"replacement_complete":false,"seq":0},` +
		`{"blocks":[{"text":"","type":""},{"text":"first","type":"text"}],"id":"inner-first","kind":"message","role":"user","seq":4},` +
		`{"blocks":[{"text":"\u003c\u0026\u003e","type":"text"}],"id":"inner-second","kind":"message","provider_metadata":{"create_time":-0.000E+03,"turn_id":"nested"},"role":"assistant","seq":4}` +
		`],"replacement_complete":true,"seq":-3},` + canonicalOuterFirst + `,` +
		`{"blocks":[],"id":"outer-second","kind":"message","role":"user","seq":8},` + canonicalOuterFirst +
		`],"replacement_complete":false,"seq":17}`
	buffer := &legacySourceProofBuffer{body: raw}
	canonical, err := normalizeLegacyFixture(context.Background(), buffer.input())
	if err != nil {
		test.Fatal("public normalization rejected nested message replacements", err)
	}
	var document struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(canonical, &document); err != nil {
		test.Fatal(err)
	}
	if len(document.Events) != 1 || !bytes.Equal(document.Events[0], []byte(expected)) {
		test.Fatalf("recursive stable ties, duplicate retention, block bytes or metadata changed: got=%s want=%s", document.Events, expected)
	}
}

func TestLegacyCanonicalBlocksReplacementReviewInvalidChildAndReset(test *testing.T) {
	const invalidEvents = `[{"kind":"compaction","seq":17,"replacement":[` +
		`{"kind":"turn","seq":-3,"role":"user"},` +
		`{"kind":"compaction","seq":-3,"replacement":[` +
		`{"kind":"turn","id":"tie-valid","seq":4,"role":"user"},` +
		`{"kind":"turn","id":"tie-invalid","seq":4,"agent_message":false},` +
		`{"kind":"turn","id":"tie-valid","seq":4,"role":"user"}]}]}]`
	for _, fixture := range []struct {
		name   string
		raw    string
		reject bool
	}{
		{
			name:   "retained-invalid-child-cannot-hide-in-sequence-ties",
			raw:    `{"envelope":{"cir_version":"2"},"events":` + invalidEvents + `}`,
			reject: true,
		},
		{
			name: "discarded-invalid-child-clears-error-and-v2-requirement",
			raw: `{"envelope":{"cir_version":"1"},"events":` + invalidEvents +
				`,"events":[{"kind":"turn","id":"survivor","seq":17,"role":"user"}]}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			buffer := &legacySourceProofBuffer{body: []byte(fixture.raw)}
			canonical, err := normalizeLegacyFixture(context.Background(), buffer.input())
			if fixture.reject {
				if !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) || canonical != nil {
					test.Fatalf("invalid nested union completed or changed error class: bytes=%s error=%v", canonical, err)
				}
				return
			}
			if err != nil {
				test.Fatal("discarded nested union contaminated the replacement events array", err)
			}
			var document struct {
				Events []json.RawMessage `json:"events"`
			}
			if err := json.Unmarshal(canonical, &document); err != nil {
				test.Fatal(err)
			}
			const expected = `{"id":"survivor","kind":"turn","role":"user","seq":17}`
			if len(document.Events) != 1 || !bytes.Equal(document.Events[0], []byte(expected)) {
				test.Fatalf("discarded nested union survived the events reset: got=%s want=%s", document.Events, expected)
			}
		})
	}
}
