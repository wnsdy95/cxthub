package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

type canonicalInputJSONMarshaler string

func (value canonicalInputJSONMarshaler) MarshalJSON() ([]byte, error) {
	if value == "" {
		return nil, fmt.Errorf("synthetic marshal failure")
	}
	return []byte(value), nil
}

func canonicalInputEventCases(testingContext testing.TB) []canonicalEventCase {
	testingContext.Helper()
	cases := canonicalEventCases()
	for index, raw := range []string{
		`{"kind":"turn","seq":9223372036854775807,"role":"custom"}`,
		`{"kind":"message","blocks":null,"agent_message":false}`,
		`{"kind":"message","blocks":[],"agent_message":null}`,
		`{"kind":"message","agent_message":true,"agent_author":"","agent_recipient":"","locked":{"provider":"codex","scheme":"s","blob":"b"},"provider_metadata":{}}`,
		`{"kind":"tool_call","input":null}`,
		`{"kind":"tool_call","input":{}}`,
		`{"kind":"tool_result","output":[9007199254740993,-0,1e-7,1e21,5e-324,1.7976931348623157e308],"is_error":false}`,
		`{"kind":"tool_result","output":null,"is_error":null}`,
		`{"kind":"reasoning","locked":{"provider":"codex","scheme":"s","blob":"b"},"redacted_summary":"summary","cross_replayable":true}`,
		`{"kind":"compaction","replacement":null,"replacement_complete":false}`,
		`{"kind":"compaction","replacement":[],"replacement_complete":false}`,
		`{"kind":"compaction","locked":{"provider":"codex","scheme":"s","blob":"b"}}`,
		`{"kind":"compaction","replacement":[],"locked":{"provider":"codex"}}`,
	} {
		cases = append(cases, canonicalEventCase{fmt.Sprintf("union-%d", index), []byte(raw)})
	}
	_, events, err := splitCanonicalDocBytes(verifierFixture(testingContext))
	if err != nil {
		testingContext.Fatal(err)
	}
	for index, raw := range events {
		cases = append(cases, canonicalEventCase{fmt.Sprintf("fixture-%d", index), raw})
	}
	return cases
}

func assertCanonicalInputEventParity(test *testing.T, fresh func() CIREvent) CIREvent {
	test.Helper()
	want, oldErr := canonicalJSON(fresh())
	event := fresh()
	before, beforeErr := json.Marshal(event)
	got, newErr := canonicalInputEventJSON(event)
	if (oldErr == nil) != (newErr == nil) {
		test.Fatalf("canonical acceptance differs: oracle=%v candidate=%v", oldErr, newErr)
	}
	if oldErr == nil && (!bytes.Equal(want, got) || HashContent(want) != HashContent(got)) {
		test.Fatalf("canonical bytes/hash differ: oracle=%q candidate=%q", want, got)
	}
	after, afterErr := json.Marshal(event)
	if !bytes.Equal(before, after) || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
		test.Fatal("candidate mutated source event")
	}
	return event
}

func assertCanonicalInputRawParity(test *testing.T, raw []byte) {
	test.Helper()
	var original CIREvent
	if json.Unmarshal(raw, &original) != nil {
		return
	}
	event := assertCanonicalInputEventParity(test, func() CIREvent {
		var decoded CIREvent
		if err := json.Unmarshal(raw, &decoded); err != nil {
			test.Fatal(err)
		}
		return decoded
	})
	if !reflect.DeepEqual(original, event) {
		test.Fatal("candidate mutated typed-decoded event")
	}
}

func TestCanonicalInputEventDecodedParity(tests *testing.T) {
	for _, candidate := range canonicalInputEventCases(tests) {
		tests.Run(candidate.name, func(test *testing.T) { assertCanonicalInputRawParity(test, candidate.raw) })
	}
}

func TestCanonicalInputEventGoPayloadParity(tests *testing.T) {
	type record struct{ Z, A int }
	type payloadMap map[string]any
	type payloadSlice []any
	fresh := func() map[string]any {
		number := json.Number("1E+3")
		custom := canonicalInputJSONMarshaler(`{"z":1E+3,"a":9007199254740993}`)
		return map[string]any{
			"nil": nil, "bool": false, "int": int(9007199254740993),
			"struct": record{Z: 1, A: 2}, "pointer": &record{Z: 1, A: 2}, "nil-pointer": (*record)(nil),
			"array": [2]record{{Z: 1}, {A: 2}}, "typed-map": map[string]record{"item": {Z: 1}},
			"named-map": payloadMap{"z": 1, "a": 2}, "named-slice": payloadSlice{record{}}, "bytes": []byte("<&>"),
			"custom": custom, "custom-pointer": &custom, "custom-error": canonicalInputJSONMarshaler(""),
			"custom-invalid": canonicalInputJSONMarshaler(`{"bad":`), "raw": json.RawMessage(custom),
			"raw-invalid": json.RawMessage(`{"bad":`), "raw-nil": json.RawMessage(nil),
			"nested-custom":  map[string]any{"array": []any{map[string]any{"custom": custom, "raw": json.RawMessage(custom)}}},
			"number-pointer": &number, "number-nil": (*json.Number)(nil),
			"exact-integers": []any{int64(math.MinInt64), int64(math.MaxInt64), uint64(math.MaxUint64), int64(9007199254740993)},
			"floats":         []any{float64(9007199254740993), math.Copysign(0, -1), 1e-7, 1e21, math.SmallestNonzeroFloat64, math.MaxFloat64},
			"nan":            math.NaN(), "positive-infinity": math.Inf(1), "negative-infinity": math.Inf(-1),
			"plain":   []any{nil, true, 7, 1.25, EventMessage, RoleAssistant, ProviderKind("codex"), "한국🙂"},
			"maps":    []map[string]any{nil, {}, {"z": 1, "a": []any{false, nil}}},
			"nil-map": map[string]any(nil), "empty-map": map[string]any{},
			"nil-slice": []any(nil), "empty-slice": []any{}, "nil-maps": []map[string]any(nil),
			"function": func() {}, "channel": make(chan int),
		}
	}
	for name := range fresh() {
		for _, kind := range []EventKind{EventToolCall, EventToolResult} {
			for _, nested := range []bool{false, true} {
				tests.Run(fmt.Sprintf("%s/%s/nested-%t", name, kind, nested), func(test *testing.T) {
					assertCanonicalInputEventParity(test, func() CIREvent {
						value := fresh()[name]
						event := CIREvent{Kind: kind, Input: map[string]any{"value": value}, Output: value}
						if nested {
							event = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{Kind: EventCompaction, Replacement: []CIREvent{event}}}}
						}
						return event
					})
				})
			}
		}
	}
}

func TestCanonicalInputEventTypedNilOutputParity(tests *testing.T) {
	cases := []struct {
		name       string
		output     any
		unreadable bool
	}{
		{"nil-content-blocks", []ContentBlock(nil), true},
		{"empty-content-blocks", []ContentBlock{}, false},
		{"nil-events", []CIREvent(nil), true},
		{"empty-events", []CIREvent{}, false},
		{"nil-provider-metadata", (*ProviderMetadata)(nil), true},
		{"empty-provider-metadata", &ProviderMetadata{}, false},
		{"nil-locked-blob", (*LockedBlob)(nil), true},
		{"empty-locked-blob", &LockedBlob{}, false},
	}
	for _, candidate := range cases {
		for _, nested := range []bool{false, true} {
			tests.Run(fmt.Sprintf("%s/nested-%t", candidate.name, nested), func(test *testing.T) {
				event := assertCanonicalInputEventParity(test, func() CIREvent {
					event := CIREvent{Kind: EventToolResult, Seq: 1, CallID: "call", Output: candidate.output}
					if nested {
						event = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{Kind: EventCompaction, Replacement: []CIREvent{event}}}}
					}
					return event
				})
				document := CIRDocument{Envelope: CIREnvelope{CIRVersion: CIRVersionV2}, Events: []CIREvent{event}}
				oldBytes, err := canonicalBytes(document, func(event CIREvent) ([]byte, error) {
					return canonicalJSON(event)
				})
				if err != nil {
					test.Fatal("predecessor canonicalization failed", err)
				}
				oldHash := HashContent(oldBytes)
				canonical, err := CanonicalBytes(document)
				if err != nil || !bytes.Equal(canonical, oldBytes) || HashContent(canonical) != oldHash {
					test.Fatalf("public canonical bytes/hash differ: oracle=%q candidate=%q err=%v", oldBytes, canonical, err)
				}
				validated, err := ValidatedSessionDocBytes(SessionDoc{Hash: oldHash, CIR: document})
				verifier := NewCanonicalDocVerifierWithoutCache()
				proof, wireErr := verifier.Verify(context.Background(), oldHash, oldBytes)
				if candidate.unreadable {
					if !errors.Is(wireErr, ErrIntegrity) || !errors.Is(err, ErrIntegrity) || len(validated) != 0 || proof.Valid() {
						test.Fatalf("unreadable typed-null representation was admitted: bytes=%q err=%v wire=%v", validated, err, wireErr)
					}
					return
				}
				if wireErr != nil || !proof.Valid() {
					test.Fatalf("unexpected wire oracle result: %v", wireErr)
				}
				if err != nil || !bytes.Equal(validated, oldBytes) || HashContent(validated) != oldHash {
					test.Fatalf("public validated writer rejected predecessor bytes/hash: oracle=%q candidate=%q err=%v", oldBytes, validated, err)
				}
			})
		}
	}
}

func TestCanonicalInputEventNumberLexemes(tests *testing.T) {
	for _, lexeme := range []string{"", "0", "-0", "1.0", "1e0", "1E+3", "1e-7", "1e21", "1e1000", "9007199254740993", "1787683260.123456789", "01", "+1", "NaN"} {
		tests.Run(lexeme, func(test *testing.T) {
			assertCanonicalInputEventParity(test, func() CIREvent {
				number := json.Number(lexeme)
				return CIREvent{Kind: EventToolCall, ProviderMetadata: &ProviderMetadata{CreateTime: &number}, Input: map[string]any{"value": number, "pointer": &number}}
			})
		})
	}
}

func TestCanonicalInputEventUnicodeParity(tests *testing.T) {
	for _, text := range []string{"<&>\u2028\u2029한국🙂", "\xff", "\x80", "\ufffd"} {
		fresh := func() []CIREvent {
			locked := &LockedBlob{Provider: ProviderKind(text), Scheme: text, Blob: text}
			return []CIREvent{
				{Kind: EventKind(text)},
				{Kind: EventTurn, Role: Role(text), ID: text, TS: text, ProviderMetadata: &ProviderMetadata{TurnID: text}},
				{Kind: EventMessage, Role: Role(text), Blocks: []ContentBlock{{Type: text, Text: text}}, CompactSummary: true, AgentMessage: true, AgentAuthor: text, AgentRecipient: text, Locked: locked},
				{Kind: EventToolCall, CallID: text, ToolName: text, ProviderToolName: text, Status: text, Input: map[string]any{text: text, "\ufffd": "collision", "enums": []any{EventKind(text), Role(text), ProviderKind(text)}}},
				{Kind: EventToolResult, Output: map[string]any{"nested": []any{map[string]any{text: "original", "\ufffd": "collision"}}}},
				{Kind: EventReasoning, Locked: locked, RedactedSummary: text},
				{Kind: EventCompaction, Locked: locked},
			}
		}
		for index := range fresh() {
			tests.Run(fmt.Sprintf("%x/%d", text, index), func(test *testing.T) {
				assertCanonicalInputEventParity(test, func() CIREvent { return fresh()[index] })
			})
		}
	}
}

func TestCanonicalInputEventPreservesAliases(tests *testing.T) {
	fresh := func() CIREvent {
		shared := map[string]any{"z": json.Number("9007199254740993"), "a": nil}
		slice := []any{shared, nil}
		blocks := []ContentBlock{{Type: "text", Text: "<&>한국"}}
		return CIREvent{Kind: EventCompaction, Replacement: []CIREvent{
			{Kind: EventToolCall, Seq: 9, Input: shared},
			{Kind: EventToolResult, Seq: 1, Output: []any{shared, slice, slice}},
			{Kind: EventMessage, Blocks: blocks}, {Kind: EventMessage, Blocks: blocks},
		}}
	}
	event := assertCanonicalInputEventParity(tests, fresh)
	if !reflect.DeepEqual(event, fresh()) {
		tests.Fatal("candidate mutated aliased data, types, nils or replacement order")
	}
	output := event.Replacement[1].Output.([]any)
	event.Replacement[0].Input["probe"] = true
	output[1].([]any)[0] = "probe"
	if output[0].(map[string]any)["probe"] != true || output[2].([]any)[0] != "probe" {
		tests.Fatal("candidate broke payload aliases")
	}
}

func TestCanonicalInputEventPayloadDepthAndCycles(tests *testing.T) {
	for _, shape := range []string{"deep-map", "deep-slice", "deep-maps", "cycle-map", "cycle-slice"} {
		for _, kind := range []EventKind{EventToolCall, EventToolResult} {
			tests.Run(shape+"/"+string(kind), func(test *testing.T) {
				assertCanonicalInputEventParity(test, func() CIREvent {
					var value any = true
					switch shape {
					case "cycle-map":
						payload := map[string]any{}
						payload["self"], value = payload, payload
					case "cycle-slice":
						payload := make([]any, 1)
						payload[0], value = payload, payload
					default:
						for depth := 0; depth < 80; depth++ {
							switch shape {
							case "deep-map":
								value = map[string]any{"next": value}
							case "deep-slice":
								value = []any{value}
							case "deep-maps":
								value = []map[string]any{{"next": value}}
							}
						}
					}
					return CIREvent{Kind: kind, Input: map[string]any{"payload": value}, Output: value}
				})
			})
		}
	}
}

func FuzzCanonicalInputEventJSON(fuzzer *testing.F) {
	for _, candidate := range canonicalInputEventCases(fuzzer) {
		if len(candidate.raw) <= 16<<10 {
			fuzzer.Add(candidate.raw)
		}
	}
	fuzzer.Fuzz(func(test *testing.T, raw []byte) {
		if len(raw) > 16<<10 {
			return
		}
		assertCanonicalInputRawParity(test, raw)
	})
}
