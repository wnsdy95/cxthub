package domain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalPayloadScalarCheckpointBudget(t *testing.T) {
	for _, suffix := range []string{`10]`, `true]`, `false]`, `null]`, `-0.0000012345678901234567]`} {
		raw := []byte(`[` + strings.Repeat("0,", 32767) + suffix)
		parser := canonicalPayloadParser{raw: raw, maxDepth: 10000}
		var positions []int
		parser.ctx = canonicalAdmissionContext{Context: context.Background(), check: func() { positions = append(positions, parser.offset) }}
		if err := parser.ctx.Err(); err != nil {
			t.Fatal(err)
		}
		if err := parser.value(); err != nil {
			t.Fatal(err)
		}
		for len(parser.frames) > 0 {
			if err := parser.step(); err != nil {
				t.Fatal(err)
			}
		}
		if err := parser.ctx.Err(); err != nil {
			t.Fatal(err)
		}
		if len(positions) < 3 {
			t.Fatal("no checkpoint inside the real scalar loop", positions)
		}
		for index := 1; index < len(positions); index++ {
			if positions[index]-positions[index-1] > canonicalCheckBytes {
				t.Fatal("scalar/literal advance exceeded byte checkpoint budget", positions)
			}
		}
	}
}

func TestCanonicalPayloadOriginalParentDepthCannotBeProjectedAway(t *testing.T) {
	identity := giantCanonicalString(t, strings.Repeat("i", canonicalCheckBytes+1))
	for _, depth := range []int{9994, 9995, 9996, 9997, 9998, 9999, 10000, 10001} {
		payload := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
		child := `{"call_id":` + identity + `,"kind":"tool_result","output":` + payload + `,"seq":1}`
		for _, event := range []string{child, `{"kind":"compaction","replacement":[` + child + `],"replacement_complete":true,"seq":2}`} {
			raw := []byte(event)
			if len(raw) <= canonicalCheckBytes {
				t.Fatal("fixture does not activate scratch projection")
			}
			wantSeq, want := strictCanonicalEventReference(context.Background(), "2", raw)
			seq, err := verifyCanonicalEvent(context.Background(), "2", raw)
			if seq != wantSeq || fmt.Sprint(err) != fmt.Sprint(want) ||
				errors.Is(err, ErrIntegrity) != errors.Is(want, ErrIntegrity) {
				t.Fatalf("original depth %d was lost by projection: %v / %v", depth, err, want)
			}
			assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, event))
		}
	}
}

func TestCanonicalPayloadMetadataKeepsProjection(t *testing.T) {
	output := `[` + strings.Repeat("false,", canonicalCheckBytes/3) + `null]`
	for _, field := range []string{`"create_time":1.0`, `"create_time":1E+3`, `"create_time":1e1000`, `"CREATE_TIME":"1"`, `"\u0063reate_time":"1"`} {
		raw := []byte(`{"call_id":"tool","kind":"tool_result","output":` + output + `,"provider_metadata":{` + field + `},"seq":1}`)
		projected, err := canonicalEventProjection(context.Background(), raw)
		if err != nil || len(projected) >= 1024 || !bytes.Contains(projected, []byte(field)) ||
			!bytes.Contains(projected, []byte(`"output":[]`)) {
			t.Fatal("provider metadata disabled projection or changed its original scalar", len(projected), err)
		}
		assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, string(raw)))
	}
}
