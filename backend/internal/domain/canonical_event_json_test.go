package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type canonicalEventCase struct {
	name string
	raw  []byte
}

func canonicalEventCases() []canonicalEventCase {
	cases := []canonicalEventCase{
		{"plain", []byte(`{"kind":"message","seq":0,"role":"user","blocks":[]}`)},
		{"null-event", []byte(`null`)},
		{"array-event", []byte(`[]`)},
		{"number-event", []byte(`1`)},
		{"truncated", []byte(`{"kind":"message"`)},
		{"trailing-object", []byte(`{} {}`)},
		{"bad-escape", []byte(`{"kind":"message","unknown":"\q"}`)},
		{"bad-number", []byte(`{"kind":"tool_call","input":{"n":01}}`)},
		{"overflow-number", []byte(`{"kind":"tool_call","input":{"n":1e1000}}`)},
		{"precision", []byte(`{"kind":"tool_call","seq":1,"call_id":"x","tool_name":"synthetic","input":{"n":9007199254740993,"m":1.0,"e":1e0}}`)},
		{"duplicate-kind", []byte(`{"kind":"tool_call","kind":"message","seq":0,"role":"user","blocks":[]}`)},
		{"duplicate-null", []byte(`{"kind":"message","seq":0,"agent_message":true,"agent_message":null,"agent_author":""}`)},
		{"case-typed-map", []byte(`{"kind":"message","seq":0,"AGENT_MESSAGE":false,"Agent_Author":""}`)},
		{"escaped-key", []byte(`{"kind":"message","seq":0,"\u0061gent_message":false}`)},
		{"nested-compaction", []byte(`{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":[{"kind":"message","seq":9,"role":"assistant","agent_message":false},{"kind":"compaction","seq":1,"replacement_complete":false,"replacement":[{"kind":"message","seq":0,"role":"user","AGENT_MESSAGE":false,"unknown":null}]}]}`)},
		{"duplicate-replacement", []byte(`{"kind":"compaction","seq":0,"replacement":[{"kind":"message","seq":0,"agent_message":true}],"replacement":[],"replacement_complete":false}`)},
		{"locked-null", []byte(`{"kind":"reasoning","seq":0,"locked":null,"cross_replayable":false}`)},
		{"invalid-utf8", append(append([]byte(`{"kind":"message","seq":0,"role":"user","blocks":[{"type":"text","text":"`), 0xff), []byte(`"}]}`)...)},
		{"huge-unknown-value", []byte(`{"kind":"message","seq":0,"role":"user","blocks":[],"unknown":"` + strings.Repeat("x", 4<<20) + `","agent_message":false}`)},
		{"huge-known-value", []byte(`{"kind":"message","seq":0,"role":"user","blocks":[{"type":"text","text":"` + strings.Repeat("x", 1<<20) + `"}],"agent_message":false}`)},
	}
	fields := []string{"provider_metadata", "agent_message", "agent_author", "agent_recipient", "replacement", "replacement_complete", "locked"}
	values := []string{`null`, `false`, `true`, `""`, `[]`, `{}`, `0`, `"synthetic"`, `{"x":null}`, `[1,null,false]`}
	for _, field := range fields {
		for i, value := range values {
			cases = append(cases, canonicalEventCase{fmt.Sprintf("%s/value-%d", field, i), []byte(`{"kind":"message","seq":0,"role":"user","blocks":[],"` + field + `":` + value + `}`)})
		}
		cases = append(cases, canonicalEventCase{field + "/duplicate", []byte(`{"kind":"message","seq":0,"` + field + `":false,"` + field + `":null}`)})
		cases = append(cases, canonicalEventCase{field + "/uppercase", []byte(`{"kind":"message","seq":0,"` + strings.ToUpper(field) + `":null}`)})
	}
	for _, depth := range []int{9995, 9998, 10001} {
		cases = append(cases, canonicalEventCase{fmt.Sprintf("depth-%d", depth), []byte(`{"kind":"message","seq":0,"unknown":` + strings.Repeat("[", depth) + `0` + strings.Repeat("]", depth) + `}`)})
	}
	return cases
}

func TestCanonicalDecodedEventEquivalence(t *testing.T) {
	cases := canonicalEventCases()
	for _, item := range []canonicalEventCase{
		{"message-fields", []byte(`{"kind":"message","seq":3,"id":"id<&>","ts":"synthetic","role":"arbitrary-\uac80\uc0c9","blocks":[{"type":"text","text":"<&>\u2028\u2029\ud83d\ude42\n\"\\"},{"type":"text","text":""}],"agent_message":true,"agent_author":"","agent_recipient":"/agent","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"},"provider_metadata":{"turn_id":"turn","create_time":1787683260.123456789}}`)},
		{"provider-empty", []byte(`{"kind":"message","seq":0,"role":"user","provider_metadata":{},"blocks":[]}`)},
		{"turn", []byte(`{"kind":"turn","seq":9007199254740993,"role":"custom"}`)},
		{"tool-numbers", []byte(`{"kind":"tool_call","seq":0,"call_id":"c","tool_name":"tool","input":{"z":-0,"a":1e-7,"b":1e21,"c":9007199254740993,"nested":[null,false,{"z":1,"a":2}]},"provider_tool_name":"native","status":"ok"}`)},
		{"tool-result-map", []byte(`{"kind":"tool_result","seq":1,"call_id":"c","output":{"z":{},"a":[]},"is_error":true}`)},
		{"tool-result-array", []byte(`{"kind":"tool_result","seq":1,"call_id":"c","output":[{"z":1,"a":2},null,-0]}`)},
		{"reasoning", []byte(`{"kind":"reasoning","seq":2,"redacted_summary":"summary","locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"},"cross_replayable":true}`)},
		{"nested-known-types", []byte(`{"kind":"compaction","seq":0,"replacement_complete":true,"replacement":[{"kind":"message","seq":9,"role":"user","provider_metadata":{"turn_id":"t","create_time":1E+3},"blocks":[{"type":"text","text":"later"}]},{"kind":"compaction","seq":1,"replacement_complete":false,"replacement":[{"kind":"reasoning","seq":0,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"nested"}}]}]}`)},
	} {
		cases = append(cases, item)
	}
	_, fixtureEvents, err := splitCanonicalDocBytes(verifierFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range fixtureEvents {
		cases = append(cases, canonicalEventCase{fmt.Sprintf("existing-verifier-%d", i), raw})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var event CIREvent
			if err := json.Unmarshal(tc.raw, &event); err != nil {
				return
			} // unchanged typed boundary
			event = canonicalEvents([]CIREvent{event})[0]
			before, marshalErr := json.Marshal(event)
			want, oldErr := canonicalJSON(event)
			got, newErr := canonicalDecodedEventJSON(event)
			if (oldErr == nil) != (newErr == nil) {
				t.Fatalf("canonical acceptance differs: %v / %v", oldErr, newErr)
			}
			if oldErr == nil && (!bytes.Equal(got, want) || HashContent(got) != HashContent(want)) {
				t.Fatal("canonical bytes/hash differ")
			}
			after, err := json.Marshal(event)
			if fmt.Sprint(err) != fmt.Sprint(marshalErr) || !bytes.Equal(before, after) {
				t.Fatal("candidate mutated source event")
			}
		})
	}
}

// Pin key ordering, typed integer precision and current float64 payload behavior.
func TestCanonicalDecodedEventHashPitfalls(t *testing.T) {
	raw := []byte(`{"blocks":[{"text":"<&>","type":"text"}],"kind":"message","provider_metadata":{"create_time":1E+3,"turn_id":"t"},"role":"user","seq":9007199254740993}`)
	var event CIREvent
	if err := json.Unmarshal(raw, &event); err != nil {
		t.Fatal(err)
	}
	got, err := canonicalDecodedEventJSON(event)
	want := bytes.Replace(raw, []byte(`<&>`), []byte(`\u003c\u0026\u003e`), 1)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("struct ordering, typed sequence, number lexeme or HTML escaping drift", err)
	}
	var tool CIREvent
	if err := json.Unmarshal([]byte(`{"kind":"tool_call","input":{"n":9007199254740993},"seq":0}`), &tool); err != nil {
		t.Fatal(err)
	}
	got, err = canonicalDecodedEventJSON(tool)
	if err != nil || !bytes.Contains(got, []byte(`"n":9007199254740992`)) {
		t.Fatal("legacy float64 payload normalization changed", err)
	}
}

// Unknown emitted shapes, including inside replacement history, use the full
// generic canonicalizer; ordinary MarshalJSON would retain declaration order.
func TestCanonicalDecodedEventFallsBackForUnhandledTypedValues(t *testing.T) {
	type future struct {
		Z string `json:"z"`
		A string `json:"a"`
	}
	for _, value := range []any{future{}, &future{}, []future{{}}, map[string]future{"item": {}}} {
		for _, nested := range []bool{false, true} {
			event := CIREvent{Kind: EventToolResult, Output: value}
			if nested {
				event = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{
					Kind: EventCompaction, Replacement: []CIREvent{event},
				}}}
			}
			if _, err := canonicalEventJSONObject(event); !errors.Is(err, errCanonicalEventShape) {
				t.Fatal("unknown shape sentinel did not propagate to entry point")
			}
			want, err := canonicalJSON(event)
			if err != nil {
				t.Fatal(err)
			}
			plain, err := json.Marshal(event)
			if err != nil || bytes.Equal(plain, want) {
				t.Fatal("fixture must distinguish canonical and ordinary marshal")
			}
			got, err := canonicalDecodedEventJSON(event)
			if err != nil || !bytes.Equal(got, want) || HashContent(got) != HashContent(want) {
				t.Fatal("unknown shape fallback changed canonical bytes/hash", err)
			}
		}
	}
}

// Adding/changing a field in a mapped struct requires updating its conversion.
func TestCanonicalEventKnownStructFields(t *testing.T) {
	type field struct {
		name string
		typ  reflect.Type
		tag  string
	}
	stringType := reflect.TypeOf("")
	for typ, fields := range map[reflect.Type][]field{
		reflect.TypeOf(ContentBlock{}): {
			{"Type", stringType, "type"}, {"Text", stringType, "text"},
		},
		reflect.TypeOf(ProviderMetadata{}): {
			{"TurnID", stringType, "turn_id,omitempty"}, {"CreateTime", reflect.TypeOf((*json.Number)(nil)), "create_time,omitempty"},
		},
		reflect.TypeOf(LockedBlob{}): {
			{"Provider", reflect.TypeOf(ProviderKind("")), "provider"}, {"Scheme", stringType, "scheme"}, {"Blob", stringType, "blob"},
		},
	} {
		if typ.NumField() != len(fields) {
			t.Fatalf("update canonical conversion for %s", typ.Name())
		}
		for i, want := range fields {
			got := typ.Field(i)
			if got.Name != want.name || got.Type != want.typ || got.Tag.Get("json") != want.tag {
				t.Fatalf("update canonical conversion for %s field %d", typ.Name(), i)
			}
		}
	}
}
