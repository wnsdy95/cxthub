package domain

import (
	"strings"
	"testing"
)

func legacyNativePublicRejectDifferentialObject(fields []string, rotation int) string {
	pivot := rotation % len(fields)
	ordered := append([]string(nil), fields[pivot:]...)
	ordered = append(ordered, fields[:pivot]...)
	return "{" + strings.Join(ordered, ",") + "}"
}

func legacyNativePublicRejectDifferentialDocument(kind, selected, ignored, provider string, rotation int) []byte {
	event := legacyNativePublicRejectDifferentialObject([]string{
		`"kind":"` + kind + `"`,
		`"seq":7`,
		`"call_id":"generated"`,
		selected,
		`"provider_metadata":{"turn_id":"selected","create_time":9007199254740993}`,
		`"future":{"input":[` + ignored + `],"output":{"provider_metadata":{"create_time":"not-a-number"},"seq":"not-an-int"}}`,
	}, rotation)
	nested := legacyNativePublicRejectDifferentialObject([]string{
		`"kind":"turn"`,
		`"seq":2`,
		`"role":"assistant"`,
		`"provider_metadata":{"turn_id":"nested","create_time":` + provider + `,"future":{"output":1e9999}}`,
	}, rotation+1)
	compaction := legacyNativePublicRejectDifferentialObject([]string{
		`"kind":"compaction"`,
		`"seq":11`,
		`"replacement":[` + nested + `]`,
		`"replacement_complete":true`,
		`"provider_metadata":{"turn_id":"parent","create_time":-0.000E+03}`,
	}, rotation+2)
	events := event + "," + compaction
	if rotation%2 != 0 {
		events = compaction + "," + event
	}
	return []byte(legacyNativePublicRejectDifferentialObject([]string{
		`"envelope":{"cir_version":"2","future":{"input":1e9999}}`,
		`"events":[` + events + `]`,
		`"future":{"output":1e9999,"provider_metadata":{"create_time":"not-a-number"}}`,
	}, rotation+3))
}

func TestLegacyNativePublicRejectGeneratedDifferential(test *testing.T) {
	generated := 0
	var outcomes [2][3]int
	for sizeIndex, size := range []struct {
		name   string
		digits int
	}{
		{name: "small-control", digits: 3},
		{name: "over-64k", digits: (64 << 10) + 17},
	} {
		zeroes := strings.Repeat("0", size.digits)
		nines := strings.Repeat("9", size.digits)
		finite := "1." + zeroes + "25"
		overflow := "1E+" + nines
		malformed := "1." + zeroes + "E+"
		provider := "1.2300E+" + nines
		for familyIndex, family := range []struct {
			name     string
			token    string
			ignored  string
			accepted bool
			syntax   bool
		}{
			{name: "finite-fraction", token: finite, ignored: overflow, accepted: true},
			{name: "negative-underflow", token: "-1e-" + nines, ignored: overflow, accepted: true},
			{name: "exponent-overflow", token: overflow, ignored: overflow},
			{name: "coefficient-overflow", token: "9" + strings.Repeat("7", size.digits+310), ignored: overflow},
			{name: "incomplete-selected-exponent", token: malformed, ignored: overflow, syntax: true},
			{name: "malformed-ignored-sibling", token: finite, ignored: malformed, syntax: true},
		} {
			for _, token := range []string{family.token, family.ignored, provider} {
				if (len(token) > 64<<10) != (sizeIndex == 1) {
					test.Fatalf("%s/%s: generated token escaped its small/over-64KiB size class", size.name, family.name)
				}
			}
			outputTail := ""
			if familyIndex%2 == 0 {
				outputTail = `,"output":{"replacement":[0,false,null]}`
			}
			for shapeIndex, shape := range []struct {
				name   string
				kind   string
				fields string
			}{
				{
					name:   "input-map-merge",
					kind:   "tool_call",
					fields: `"input":{"keep":"merge","replace":{"old":true}},"INP\u0055T":{"number":` + family.token + `,"replace":{"fresh":false}},"input":{"tail":null}`,
				},
				{
					name:   "input-nested-arrays",
					kind:   "tool_call",
					fields: `"input":{"keep":true},"INPUT":{"tree":[false,[{"number":` + family.token + `}],null]},"input":{}`,
				},
				{
					name:   "output-scalar-duplicates",
					kind:   "tool_result",
					fields: `"output":{"discard":true},"OUTP\u0055T":` + family.token + outputTail,
				},
				{
					name:   "output-nested-arrays",
					kind:   "tool_result",
					fields: `"output":{"discard":true},"outp\u0075t":[false,[{"number":` + family.token + `}],null]`,
				},
			} {
				_ = size.name + "/" + family.name + "/" + shape.name
				_ = legacyNativePublicRejectDifferentialDocument(shape.kind, shape.fields, family.ignored, provider, 4*familyIndex+shapeIndex)
				outcome := 0
				if family.syntax {
					outcome = 2
				} else if !family.accepted {
					outcome = 1
				}
				outcomes[sizeIndex][outcome]++
				generated++
			}
		}
	}
	if generated != 48 || outcomes != [2][3]int{{8, 8, 8}, {8, 8, 8}} {
		test.Fatalf("generated differential mix changed: comparisons=%d per-size accepted/range/syntax=%v", generated, outcomes)
	}
	test.Logf("generated differential comparisons=%d; per-size accepted/range/syntax=%v", generated, outcomes)
}
