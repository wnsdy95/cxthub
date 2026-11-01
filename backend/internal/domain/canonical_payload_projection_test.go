package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func canonicalPayloadConcreteOracle(raw []byte) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	return err == nil && bytes.Equal(raw, canonical)
}

func assertCanonicalPayloadMarker(t testing.TB, raw, marker []byte) {
	t.Helper()
	var original, projected any
	if json.Unmarshal(raw, &original) != nil || json.Unmarshal(marker, &projected) != nil {
		t.Fatal("successful projection returned invalid JSON")
	}
	if len(marker) > 16 || !canonicalPayloadConcreteOracle(marker) {
		t.Fatalf("marker is not tiny canonical JSON: %q", marker)
	}
	switch value := original.(type) {
	case nil:
		if !bytes.Equal(marker, []byte(`null`)) {
			t.Fatalf("null changed kind: %q", marker)
		}
	case map[string]any:
		if !bytes.Equal(marker, []byte(`{}`)) {
			t.Fatalf("object changed kind: %q", marker)
		}
	case []any:
		if !bytes.Equal(marker, []byte(`[]`)) {
			t.Fatalf("array changed kind: %q", marker)
		}
	case float64:
		if !bytes.Equal(marker, []byte(`0`)) {
			t.Fatalf("number changed kind: %q", marker)
		}
	case bool:
		if _, ok := projected.(bool); !ok {
			t.Fatalf("boolean changed kind: %q", marker)
		}
	case string:
		text, ok := projected.(string)
		if !ok || (value == "") != (text == "") {
			t.Fatalf("string kind or emptiness changed: %q", marker)
		}
	default:
		t.Fatalf("unexpected concrete JSON type %T", original)
	}
}

func assertCanonicalPayloadOracle(t testing.TB, raw []byte) {
	t.Helper()
	original := bytes.Clone(raw)
	want := canonicalPayloadConcreteOracle(raw)
	end, marker, err := canonicalPayloadProjection(context.Background(), raw, 0, 10000)
	if (err == nil && end == len(raw)) != want {
		t.Fatalf("concrete acceptance differs for %d bytes: end=%d, error=%v, oracle=%t", len(raw), end, err, want)
	}
	if err != nil {
		if !errors.Is(err, ErrIntegrity) || end != 0 || marker != nil {
			t.Fatalf("failure leaked a projection: end=%d, marker=%q, error=%v", end, marker, err)
		}
	} else {
		if end <= 0 || end > len(raw) || !canonicalPayloadConcreteOracle(raw[:end]) {
			t.Fatalf("success did not identify a canonical prefix: end=%d", end)
		}
		assertCanonicalPayloadMarker(t, raw[:end], marker)
	}
	if !bytes.Equal(raw, original) {
		t.Fatal("payload projection mutated the source")
	}
}

func TestCanonicalPayloadProjectionConcreteOracle(t *testing.T) {
	for name, raw := range map[string]string{
		"empty_object":            `{}`,
		"empty_array":             `[]`,
		"empty_string":            `""`,
		"nonempty_string":         `"payload"`,
		"true":                    `true`,
		"false":                   `false`,
		"null":                    `null`,
		"nested_arrays":           `[[[],{}],[null,false,true,-0,"",{"a":[1,2,3]}]]`,
		"generic_unknown_shape":   `{"agent_message":false,"kind":"not-a-cir-event","provider_metadata":null,"replacement":[null,7]}`,
		"generic_number_string":   `{"create_time":"not-a-number","provider_metadata":{"create_time":"1e9999"}}`,
		"decoded_control_order":   `{"\u0000":0,"\b":0,"\t":0,"\n":0,"\u000b":0,"\f":0,"\r":0," ":0}`,
		"decoded_html_order":      `{"\"":0,"\u0026":0,"\u003c":0,"\u003e":0,"A":0,"\\":0,"a":0}`,
		"decoded_utf8_order":      `{"z":0,"é":0,"한":0,"😀":0}`,
		"decoded_not_wire_order":  `{"\n":0," ":0,"\u003c":0,"A":0}`,
		"unicode_not_utf16_order": "{\"\ue000\":0,\"😀\":0}",
		"unsorted_keys":           `{"z":0,"a":0}`,
		"unsorted_controls":       `{" ":0,"\n":0}`,
		"unsorted_html":           `{"A":0,"\u003c":0}`,
		"unsorted_utf8":           `{"😀":0,"한":0}`,
		"unsorted_nested":         `{"a":[{"z":0,"a":0}],"z":0}`,
		"duplicate_key":           `{"a":1,"a":2}`,
		"duplicate_escaped_key":   `{"a":1,"\u0061":2}`,
		"duplicate_html_key":      `{"\u003c":1,"\u003c":2}`,
		"duplicate_nested":        `[{"a":null,"a":false}]`,
		"noncanonical_key":        `{"\u0061":0}`,
		"noncanonical_value":      `{"a":"\u0061"}`,
		"raw_html":                `"<>&"`,
		"escaped_html":            `"\u003c\u003e\u0026"`,
		"uppercase_html_hex":      `"\u003C"`,
		"raw_utf8":                `"한글😀"`,
		"escaped_utf8":            `"\ud55c\uae00"`,
		"escaped_surrogate_pair":  `"\ud83d\ude00"`,
		"lone_surrogate":          `"\ud800"`,
		"lone_low_surrogate":      `"\udc00"`,
		"replacement_rune":        `"�"`,
		"escaped_replacement":     `"\ufffd"`,
		"escaped_slash":           `"\/"`,
		"raw_slash":               `"/"`,
		"escaped_controls":        `"\u0000\b\t\n\u000b\f\r\"\\"`,
		"long_form_newline":       `"\u000a"`,
		"escaped_separators":      `"\u2028\u2029"`,
		"raw_separators":          "\"\u2028\u2029\"",
		"invalid_utf8":            "\"" + string([]byte{0xff}) + "\"",
		"truncated_utf8":          "\"" + string([]byte{0xf0, 0x9f}) + "\"",
	} {
		t.Run(name, func(t *testing.T) {
			assertCanonicalPayloadOracle(t, []byte(raw))
		})
	}
}

func TestCanonicalPayloadProjectionFloat64Oracle(t *testing.T) {
	for _, token := range []string{
		`0`, `-0`, `0.0`, `-0.0`, `1`, `1.0`, `1e0`, `1E+0`,
		`0.000001`, `1e-6`, `1e-7`, `1e-07`, `1e+21`, `1e21`, `1E+21`,
		`100000000000000000000`, `1e20`, `1000000000000000000000`,
		`9007199254740992`, `9007199254740993`, `9007199254740994`,
		`1.2345678901234567`, `1.234567890123456789`,
		`1.7976931348623157e+308`, `1.7976931348623159e+308`, `1e9999`, `-1e9999`,
		`5e-324`, `4e-324`, `1e-9999`, `-1e-9999`,
		`01`, `-01`, `+1`, `.1`, `1.`, `1e`, `1e+`, `NaN`, `Infinity`,
	} {
		t.Run(token, func(t *testing.T) {
			assertCanonicalPayloadOracle(t, []byte(token))
			assertCanonicalPayloadOracle(t, []byte(`{"n":[`+token+`]}`))
		})
	}
}

func TestCanonicalPayloadProjectionPrefixEnd(t *testing.T) {
	for _, value := range []string{`{}`, `[]`, `true`, `false`, `null`, `-0`, `1e+21`, `""`, `"😀\u003c"`, `{"a":[0,false],"z":{}}`} {
		for _, suffix := range []string{"", `,not-json`, `]}`, `}`, `,{"broken":`} {
			for _, prefix := range []string{"", `ignored:`, `{"output":`} {
				raw := []byte(prefix + value + suffix)
				original := bytes.Clone(raw)
				end, marker, err := canonicalPayloadProjection(context.Background(), raw, len(prefix), 10000)
				if err != nil || end != len(prefix)+len(value) {
					t.Fatalf("wrong prefix boundary for %q + %q: end=%d, error=%v", value, suffix, end, err)
				}
				assertCanonicalPayloadMarker(t, []byte(value), marker)
				if !bytes.Equal(raw, original) {
					t.Fatal("prefix projection mutated unrelated bytes")
				}
			}
		}
	}
	for _, value := range []string{`{}`, `[]`, `true`, `false`, `null`, `-0`, `"x"`} {
		for _, suffix := range []string{" ", "\n", "\t", `x`, `0`, `:`, `"`, `[`, `{`} {
			raw := []byte(`ignored:` + value + suffix)
			end, marker, err := canonicalPayloadProjection(context.Background(), raw, len(`ignored:`), 10000)
			if !errors.Is(err, ErrIntegrity) || end != 0 || marker != nil {
				t.Fatalf("non-delimiter after %q accepted %q: end=%d, marker=%q, error=%v", value, suffix, end, marker, err)
			}
		}
	}
	for _, start := range []int{-1, 4, 5} {
		end, marker, err := canonicalPayloadProjection(context.Background(), []byte(`null`), start, 10000)
		if !errors.Is(err, ErrIntegrity) || end != 0 || marker != nil {
			t.Fatalf("invalid start %d returned end=%d, marker=%q, error=%v", start, end, marker, err)
		}
	}
}

func TestCanonicalPayloadProjectionMalformedPrefixes(t *testing.T) {
	for _, value := range []string{
		``, ` `, ` true`, "\n[]", `{`, `[`, `"`, `tru`, `fals`, `nul`,
		`[0,]`, `[,0]`, `[0 1]`, `[0,,1]`, `[0}`, `{"a":0]`,
		`{a:0}`, `{"a"}`, `{"a",0}`, `{"a":}`, `{"a":0,}`, `{"a":0 "b":1}`,
		`{"a":[0,1}`, `{"a":0,"b":[1,2}`, `{"a":0,"z":"unterminated}`,
		`{"a":0,"z":"\q"}`, `{"a":0,"z":"\u12"}`, `{"a":0,"z":"\uZZZZ"}`,
		"[\"raw\ncontrol\"]", `{"a":truex}`, `{"a":nul}`, `[1e+]`,
	} {
		for _, prefix := range []string{"", `unused:`} {
			raw := []byte(prefix + value)
			end, marker, err := canonicalPayloadProjection(context.Background(), raw, len(prefix), 10000)
			if !errors.Is(err, ErrIntegrity) || end != 0 || marker != nil {
				t.Fatalf("malformed prefix %q returned end=%d, marker=%q, error=%v", value, end, marker, err)
			}
		}
	}
}

func TestCanonicalPayloadProjectionLongKeyOrdering(t *testing.T) {
	common := strings.Repeat("key", canonicalCheckBytes)
	for _, pair := range [][2]string{{"\x00", "\t"}, {"\n", " "}, {"<", "A"}, {"A", "\\"}, {"한", "😀"}, {"\ue000", "😀"}} {
		t.Run(fmt.Sprintf("%q-before-%q", pair[0], pair[1]), func(t *testing.T) {
			first := giantCanonicalString(t, common+pair[0])
			second := giantCanonicalString(t, common+pair[1])
			for _, raw := range []string{
				`{` + first + `:0,` + second + `:false}`,
				`{` + second + `:false,` + first + `:0}`,
				`{` + first + `:0,` + first + `:false}`,
				`[{` + first + `:null},{` + first + `:0}]`,
			} {
				assertCanonicalPayloadOracle(t, []byte(raw))
			}
		})
	}
}

func TestCanonicalPayloadProjectionStringAndKeySeams(t *testing.T) {
	for shift := -8; shift <= 8; shift++ {
		text := strings.Repeat("a", canonicalCheckBytes+shift) + "\x00\b\t\n\f\r\"\\/<>&\u2028\u2029한😀�"
		quoted := giantCanonicalString(t, text)
		assertCanonicalPayloadOracle(t, []byte(`[`+quoted+`]`))
		assertCanonicalPayloadOracle(t, []byte(`{`+quoted+`:0}`))
	}
	for name, suffix := range map[string]string{
		"raw_html": "<", "uppercase_hex": `\u003C`, "escaped_ascii": `\u0061`,
		"surrogate_pair": `\ud83d\ude00`, "lone_surrogate": `\ud800`,
		"raw_separator": "\u2028", "escaped_slash": `\/`, "bad_escape": `\q`,
		"bad_hex": `\uZZZZ`, "short_escape": `\u00`, "raw_control": "\n",
		"bad_utf8": string([]byte{0xff}), "short_utf8": string([]byte{0xf0, 0x9f}),
	} {
		t.Run(name, func(t *testing.T) {
			text := strings.Repeat("x", canonicalCheckBytes-3) + suffix + strings.Repeat("y", 32)
			assertCanonicalPayloadOracle(t, []byte(`["`+text+`"]`))
			assertCanonicalPayloadOracle(t, []byte(`{"`+text+`":0}`))
		})
	}
}

func TestCanonicalPayloadProjectionDepthBudget(t *testing.T) {
	for _, depth := range []int{9998, 9999, 10000, 10001} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			raw := []byte(strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth))
			want := canonicalPayloadConcreteOracle(raw)
			for _, budget := range []int{9997, 9998, 9999, 10000} {
				end, marker, err := canonicalPayloadProjection(context.Background(), raw, 0, budget)
				if want && depth <= budget {
					if err != nil || end != len(raw) || !bytes.Equal(marker, []byte(`[]`)) {
						t.Fatalf("depth=%d budget=%d: end=%d, marker=%q, error=%v", depth, budget, end, marker, err)
					}
				} else if !errors.Is(err, ErrIntegrity) || end != 0 || marker != nil {
					t.Fatalf("depth=%d budget=%d escaped limit: end=%d, marker=%q, error=%v", depth, budget, end, marker, err)
				}
			}
		})
	}
	for _, item := range []struct {
		raw    string
		budget int
		valid  bool
	}{
		{`0`, 0, true}, {`null`, 0, true}, {`""`, 0, true},
		{`[]`, 0, false}, {`{}`, 0, false}, {`[]`, 1, true},
		{`{"a":[{}]}`, 2, false}, {`{"a":[{}]}`, 3, true},
	} {
		end, marker, err := canonicalPayloadProjection(context.Background(), []byte(item.raw), 0, item.budget)
		if item.valid {
			if err != nil || end != len(item.raw) {
				t.Fatalf("raw=%s budget=%d: end=%d, error=%v", item.raw, item.budget, end, err)
			}
			assertCanonicalPayloadMarker(t, []byte(item.raw), marker)
		} else if !errors.Is(err, ErrIntegrity) || end != 0 || marker != nil {
			t.Fatalf("raw=%s budget=%d escaped limit: end=%d, marker=%q, error=%v", item.raw, item.budget, end, marker, err)
		}
	}
}

func TestCanonicalPayloadProjectionCancellation(t *testing.T) {
	key := giantCanonicalString(t, strings.Repeat("k", 4*canonicalCheckBytes))
	value := giantCanonicalString(t, strings.Repeat("v", 4*canonicalCheckBytes))
	for name, raw := range map[string][]byte{
		"long_key":   []byte(`{` + key + `:0}`),
		"long_value": []byte(`[` + value + `]`),
		"wide_array": []byte(`[` + strings.Repeat("0,", 2*canonicalCheckBytes) + `null]`),
	} {
		t.Run(name, func(t *testing.T) {
			checks := 0
			probe := canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}
			if _, _, err := canonicalPayloadProjection(probe, raw, 0, 10000); err != nil || checks < 2 {
				t.Fatalf("large payload lacks cancellation checkpoints: checks=%d, error=%v", checks, err)
			}
			for _, stop := range []int{1, (checks + 1) / 2, checks} {
				base, cancel := context.WithCancel(context.Background())
				calls := 0
				ctx := canonicalAdmissionContext{Context: base, check: func() {
					calls++
					if calls == stop {
						cancel()
					}
				}}
				end, marker, err := canonicalPayloadProjection(ctx, raw, 0, 10000)
				cancel()
				if !errors.Is(err, context.Canceled) || end != 0 || marker != nil {
					t.Fatalf("checkpoint %d/%d leaked projection: end=%d, marker=%q, error=%v", stop, checks, end, marker, err)
				}
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, raw := range [][]byte{[]byte(`{}`), []byte(`{"bad":"\q"}`)} {
		end, marker, err := canonicalPayloadProjection(ctx, raw, 0, 10000)
		if !errors.Is(err, context.Canceled) || end != 0 || marker != nil {
			t.Fatalf("entry cancellation lost priority: end=%d, marker=%q, error=%v", end, marker, err)
		}
	}
}

func assertCanonicalPayloadDocumentParity(t testing.TB, raw []byte) {
	t.Helper()
	original := bytes.Clone(raw)
	hash := HashContent(raw)
	want := canonicalVerificationOracle(raw)
	_, archivedWant := archivedColdCanonicalVerify(context.Background(), hash, raw)
	if (want == nil) != (archivedWant == nil) {
		t.Fatalf("fixture oracles disagree: typed=%v, archived=%v", want, archivedWant)
	}
	var verifier CanonicalDocVerifier
	for attempt := 0; attempt < 2; attempt++ {
		proof, err := verifier.Verify(context.Background(), hash, raw)
		if (err == nil) != (want == nil) || errors.Is(err, ErrUnsupportedCIRVersion) != errors.Is(archivedWant, ErrUnsupportedCIRVersion) {
			t.Fatalf("acceptance/precedence differs on attempt %d: got=%v, typed=%v, archived=%v", attempt, err, want, archivedWant)
		}
		if err != nil {
			if proof.Valid() || len(verifier.events) != 0 || len(verifier.order) != 0 {
				t.Fatal("failed projection published proof/cache state")
			}
			continue
		}
		if !proof.Valid() || proof.Hash() != hash || !bytes.Equal(proof.Bytes(), original) {
			t.Fatal("projection changed original proof bytes/hash")
		}
		exported := proof.Bytes()
		exported[0] = '!'
		if !bytes.Equal(proof.Bytes(), original) {
			t.Fatal("proof bytes alias exported storage")
		}
		envelopeRaw, stream, ok := canonicalStream(raw)
		var envelope CIREnvelope
		if !ok || json.Unmarshal(envelopeRaw, &envelope) != nil {
			t.Fatal("accepted document did not retain its canonical frame")
		}
		if err := visitCanonicalEvents(context.Background(), stream, func(event []byte) error {
			if _, ok := verifier.events[canonicalEventKey{hash: HashContent(event), version: envelope.CIRVersion}]; !ok {
				t.Fatal("cache remembered projected rather than original event bytes")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	if !bytes.Equal(raw, original) || HashContent(raw) != hash {
		t.Fatal("verification mutated original document bytes/hash")
	}
}

func TestCanonicalPayloadProjectionVerifierShapeParity(t *testing.T) {
	wide := `[` + strings.Repeat("0,", canonicalCheckBytes/2) + `null]`
	object := `{"a":` + wide + `,"provider_metadata":{"create_time":"not-a-number"},"replacement":[{"kind":"unknown","seq":null}],"z":0}`
	largeID := giantCanonicalString(t, strings.Repeat("c", canonicalCheckBytes+1))
	cases := map[string]string{
		"generic_input":          `{"call_id":"tool","input":` + object + `,"kind":"tool_call","seq":1,"tool_name":"tool"}`,
		"generic_output":         `{"call_id":"tool","kind":"tool_result","output":` + object + `,"seq":1}`,
		"wide_output_array":      `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"seq":1}`,
		"wrong_union_input":      `{"input":` + object + `,"kind":"turn","role":"user","seq":1}`,
		"wrong_union_output":     `{"kind":"turn","output":` + wide + `,"role":"user","seq":1}`,
		"unknown_payload_field":  `{"kind":"turn","role":"user","seq":1,"unknown":` + object + `}`,
		"unknown_kind":           `{"call_id":"tool","kind":"not-a-kind","output":` + wide + `,"seq":1}`,
		"null_metadata":          `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"provider_metadata":null,"seq":1}`,
		"null_lock":              `{"call_id":"tool","kind":"tool_result","locked":null,"output":` + wide + `,"seq":1}`,
		"wrong_call_id_type":     `{"call_id":{},"kind":"tool_result","output":` + wide + `,"seq":1}`,
		"null_call_id":           `{"call_id":null,"kind":"tool_result","output":` + wide + `,"seq":1}`,
		"wrong_seq_type":         `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"seq":"1"}`,
		"wrong_error_type":       `{"call_id":"tool","is_error":{},"kind":"tool_result","output":` + wide + `,"seq":1}`,
		"wrong_metadata_type":    `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"provider_metadata":[],"seq":1}`,
		"wrong_metadata_field":   `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"provider_metadata":{"turn_id":{}},"seq":1}`,
		"metadata_unknown_field": `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"provider_metadata":{"unknown":` + object + `},"seq":1}`,
		"duplicate_output":       `{"call_id":"tool","kind":"tool_result","output":` + wide + `,"output":[],"seq":1}`,
		"duplicate_input":        `{"call_id":"tool","input":` + object + `,"input":{},"kind":"tool_call","seq":1,"tool_name":"tool"}`,
		"payload_looking_string": `{"call_id":"tool","kind":"tool_result","output":` + giantCanonicalString(t, strings.Repeat(`"output":{"create_time":"x"},`, 3000)) + `,"seq":1}`,
	}
	for index, payload := range []string{`{}`, `[]`, `true`, `false`, `0`, `-0`, `null`, `""`, `"nonempty"`} {
		cases[fmt.Sprintf("output_kind_%d", index)] = `{"call_id":` + largeID + `,"kind":"tool_result","output":` + payload + `,"seq":1}`
		cases[fmt.Sprintf("input_kind_%d", index)] = `{"call_id":` + largeID + `,"input":` + payload + `,"kind":"tool_call","seq":1,"tool_name":"tool"}`
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, event))
		})
	}
}

func TestCanonicalPayloadProjectionNestedReplacementParity(t *testing.T) {
	wide := `[` + strings.Repeat("false,", canonicalCheckBytes/6+1) + `0]`
	result := `{"call_id":"result","kind":"tool_result","output":` + wide + `,"seq":1}`
	call := `{"call_id":"call","input":{"a":` + wide + `},"kind":"tool_call","seq":2,"tool_name":"tool"}`
	for name, replacement := range map[string]string{
		"sorted":            result + `,` + call,
		"unsorted":          call + `,` + result,
		"nested_compaction": `{"kind":"compaction","replacement":[` + result + `,` + call + `],"replacement_complete":false,"seq":1}`,
		"null_input":        strings.Replace(call, `"input":{"a":`+wide+`}`, `"input":null`, 1),
		"wrong_input_kind":  strings.Replace(call, `"input":{"a":`+wide+`}`, `"input":`+wide, 1),
		"unknown_field":     strings.Replace(result, `"seq":1}`, `"seq":1,"unknown":`+wide+`}`, 1),
		"null_metadata":     strings.Replace(result, `"seq":1}`, `"provider_metadata":null,"seq":1}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			event := `{"kind":"compaction","replacement":[` + replacement + `],"replacement_complete":true,"seq":3}`
			assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, event))
		})
	}
}

func TestCanonicalPayloadProjectionVersionErrorPrecedence(t *testing.T) {
	padding := strings.Repeat("x", canonicalCheckBytes+1)
	for name, payload := range map[string]string{
		"sorted":          `{"a":"` + padding + `","z":0}`,
		"unsorted":        `{"z":0,"a":"` + padding + `"}`,
		"duplicate":       `{"a":"` + padding + `","a":0}`,
		"precision":       `{"a":"` + padding + `","n":9007199254740993}`,
		"overflow":        `{"a":"` + padding + `","n":1e9999}`,
		"bad_escape":      `{"a":"` + padding + `\q"}`,
		"noncanonical":    `{"a":"` + padding + `<"}`,
		"missing_value":   `{"a":"` + padding + `","z":}`,
		"generic_version": `{"a":"` + padding + `","agent_message":false,"provider_metadata":null}`,
	} {
		for _, version := range []string{"1", "2", "99"} {
			t.Run(name+"/v"+version, func(t *testing.T) {
				event := `{"call_id":"tool","kind":"tool_result","output":` + payload + `,"provider_metadata":{},"seq":1}`
				raw := bytes.Replace(giantCanonicalDocument(t, event), []byte(`"cir_version":"2"`), []byte(`"cir_version":"`+version+`"`), 1)
				assertCanonicalPayloadDocumentParity(t, raw)
			})
		}
	}
}

func TestCanonicalPayloadProjectionVerifierDepthParity(t *testing.T) {
	for _, depth := range []int{9995, 9997, 9998, 9999, 10000, 10001} {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			payload := strings.Repeat("[", depth) + "0" + strings.Repeat("]", depth)
			event := `{"call_id":"tool","kind":"tool_result","output":` + payload + `,"seq":1}`
			assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, event))
			nested := `{"kind":"compaction","replacement":[` + event + `],"replacement_complete":true,"seq":2}`
			assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, nested))
		})
	}
}

func TestCanonicalPayloadProjectionCanceledStrictEntryUsesLegacyPriority(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	large := strings.Repeat("x", canonicalCheckBytes+1)
	for _, event := range []string{
		`{"call_id":"tool","kind":"tool_result","output":["` + large + `"],"seq":1}`,
		`{"call_id":"tool","kind":"tool_result","output":["` + large + `"],"provider_metadata":{},"seq":1}`,
		`{"call_id":"tool","kind":"tool_result","output":["` + large + `<"],"provider_metadata":{},"seq":1}`,
		`{"call_id":"tool","kind":"tool_result","output":["` + large + `",1e9999],"provider_metadata":{},"seq":1}`,
		`{"call_id":"tool","kind":"tool_result","output":["` + large + `\q"],"provider_metadata":{},"seq":1}`,
	} {
		for _, version := range []string{"1", "2"} {
			wantSeq, want := verifyCanonicalEventBytes(ctx, version, []byte(event))
			seq, err := verifyCanonicalEvent(ctx, version, []byte(event))
			if seq != wantSeq || (err == nil) != (want == nil) {
				t.Fatalf("canceled strict entry changed legacy result: seq=%d/%d, error=%v/%v", seq, wantSeq, err, want)
			}
			for _, sentinel := range []error{ErrIntegrity, ErrUnsupportedCIRVersion, context.Canceled} {
				if errors.Is(err, sentinel) != errors.Is(want, sentinel) {
					t.Fatalf("canceled strict entry changed %v priority: got=%v, legacy=%v", sentinel, err, want)
				}
			}
		}
	}
}

func TestCanonicalPayloadProjectionProviderCreateTimeParity(t *testing.T) {
	numericString := giantCanonicalString(t, "1"+strings.Repeat("0", canonicalCheckBytes+1))
	output := `{"create_time":` + numericString + `,"provider_metadata":{"create_time":"not-a-number"},"values":[0,false,null]}`
	assertCanonicalPayloadOracle(t, []byte(output))
	for name, createTime := range map[string]string{
		"overflow_number": `1e9999`, "typed_precision": `9007199254740993`, "typed_spelling": `1E+03`,
		"negative_zero": `-0`, "numeric_string": `"1"`, "large_numeric_string": numericString,
		"invalid_number_string": `"not-a-number"`, "empty_string": `""`, "null": `null`,
		"wrong_array": `[]`, "wrong_object": `{}`, "wrong_boolean": `true`,
	} {
		t.Run(name, func(t *testing.T) {
			event := `{"call_id":"tool","kind":"tool_result","output":` + output + `,"provider_metadata":{"create_time":` + createTime + `,"turn_id":"turn"},"seq":1}`
			assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, event))
		})
	}
	for _, key := range []string{`"CREATE_TIME"`, `"\u0063reate_time"`} {
		event := `{"call_id":"tool","kind":"tool_result","output":` + output + `,"provider_metadata":{` + key + `:` + numericString + `},"seq":1}`
		assertCanonicalPayloadDocumentParity(t, giantCanonicalDocument(t, event))
	}
}

func TestCanonicalPayloadProjectionVerifierCancellationAndProofOwnership(t *testing.T) {
	output := `[` + strings.Repeat("0,", canonicalCheckBytes) + `false]`
	raw := giantCanonicalDocument(t, `{"call_id":"tool","kind":"tool_result","output":`+output+`,"seq":1}`)
	original := bytes.Clone(raw)
	hash := HashContent(raw)
	checks := 0
	var probe CanonicalDocVerifier
	if _, err := probe.Verify(canonicalAdmissionContext{Context: context.Background(), check: func() { checks++ }}, hash, raw); err != nil {
		t.Fatal(err)
	}
	for _, stop := range []int{1, (checks + 1) / 2, checks} {
		base, cancel := context.WithCancel(context.Background())
		calls := 0
		ctx := canonicalAdmissionContext{Context: base, check: func() {
			calls++
			if calls == stop {
				cancel()
			}
		}}
		var verifier CanonicalDocVerifier
		proof, err := verifier.Verify(ctx, hash, raw)
		cancel()
		if !errors.Is(err, context.Canceled) || proof.Valid() || len(verifier.events) != 0 || len(verifier.order) != 0 {
			t.Fatalf("cancellation checkpoint %d/%d admitted a projection: %v", stop, checks, err)
		}
		if !bytes.Equal(raw, original) || HashContent(raw) != hash {
			t.Fatal("canceled verification mutated original bytes/hash")
		}
		proof, err = verifier.Verify(context.Background(), hash, raw)
		if err != nil || !proof.Valid() || proof.Hash() != hash || !bytes.Equal(proof.Bytes(), original) {
			t.Fatal("canceled attempt prevented a complete retry", err)
		}
	}
	var verifier CanonicalDocVerifier
	proof, err := verifier.Verify(context.Background(), hash, raw)
	if err != nil || !proof.Valid() {
		t.Fatal(err)
	}
	if forged, err := verifier.Verify(context.Background(), HashContent([]byte(output)), raw); !errors.Is(err, ErrIntegrity) || forged.Valid() {
		t.Fatal("warm proof accepted a hash of payload/projected bytes", err)
	}
	raw[0] = '!'
	if proof.Hash() != hash || !bytes.Equal(proof.Bytes(), original) {
		t.Fatal("proof aliases original input bytes")
	}
}

func FuzzCanonicalPayloadProjectionConcreteOracle(f *testing.F) {
	for _, seed := range []string{`{}`, `[]`, `null`, `true`, `-0`, `9007199254740993`, `1e9999`, `{"a":["한😀\u003c",null],"z":false}`, `{"a":1,"a":2}`, `{"z":0,"a":1}`, `"\ud800"`} {
		f.Add(seed, uint8(0), uint16(0))
	}
	f.Fuzz(func(t *testing.T, text string, mutation uint8, position uint16) {
		if len(text) > 8192 {
			return
		}
		raw := []byte(text)
		if mutation != 0 && len(raw) != 0 {
			raw[int(position)%len(raw)] = mutation
		}
		assertCanonicalPayloadOracle(t, raw)
	})
}

func FuzzCanonicalPayloadProjectionVerifierParity(f *testing.F) {
	for _, text := range []string{"plain", "한😀<>&\u2028", "\x00\n\\\"", string([]byte{0xff})} {
		for variant := uint8(0); variant < 3; variant++ {
			f.Add(text, variant, uint8(0), uint16(0))
		}
	}
	f.Fuzz(func(t *testing.T, text string, variant, mutation uint8, position uint16) {
		if len(text) > 2048 {
			return
		}
		payload, err := json.Marshal(map[string]any{
			"a":           strings.Repeat("x", canonicalCheckBytes) + text,
			"create_time": "not-a-number",
			"z":           []any{float64(0), false, nil},
		})
		if err != nil {
			t.Fatal(err)
		}
		event := `{"call_id":"tool","kind":"tool_result","output":` + string(payload) + `,"seq":1}`
		switch variant % 3 {
		case 1:
			event = `{"call_id":"tool","input":` + string(payload) + `,"kind":"tool_call","seq":1,"tool_name":"tool"}`
		case 2:
			event = `{"kind":"compaction","replacement":[` + event + `],"replacement_complete":true,"seq":2}`
		}
		raw := giantCanonicalDocument(t, event)
		if len(raw) > 128<<10 {
			t.Fatal("fuzz fixture exceeded its byte bound")
		}
		if mutation != 0 {
			raw[int(position)%len(raw)] = mutation
		}
		assertCanonicalPayloadDocumentParity(t, raw)
	})
}
