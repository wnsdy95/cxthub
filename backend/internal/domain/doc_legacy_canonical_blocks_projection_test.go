package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func assertLegacyCanonicalBlocksProjectionShape(test testing.TB, event CIREvent, projected map[string]any) {
	test.Helper()
	union, err := eventJSONObject(event)
	if err != nil {
		test.Fatal(err)
	}
	if len(projected) != len(union) || projected["kind"] != event.Kind || projected["seq"] != event.Seq {
		test.Fatal("projection changed the event union or replacement order")
	}
	for key, value := range union {
		if _, present := projected[key]; !present {
			test.Fatalf("projection omitted union field %q", key)
		}
		switch value := value.(type) {
		case []ContentBlock:
			blocks, ok := projected[key].([]ContentBlock)
			if !ok || !reflect.DeepEqual(blocks, value) || cap(blocks) != cap(value) {
				test.Fatalf("projection did not retain typed blocks for %q: %T", key, projected[key])
			}
			if len(value) != 0 && &blocks[0] != &value[0] {
				test.Fatalf("projection copied content blocks for %q", key)
			}
		case []CIREvent:
			if value == nil {
				if projected[key] != nil {
					test.Fatalf("projection changed nil event slice for %q", key)
				}
				continue
			}
			events, ok := projected[key].([]map[string]any)
			if !ok || len(events) != len(value) {
				test.Fatalf("projection changed event slice for %q: %T", key, projected[key])
			}
			for index, child := range value {
				assertLegacyCanonicalBlocksProjectionShape(test, child, events[index])
			}
		}
	}
}

func assertLegacyCanonicalBlocksProjectionParity(test testing.TB, event CIREvent) ([]byte, bool) {
	test.Helper()
	before, beforeErr := json.Marshal(event)
	defer func() {
		after, afterErr := json.Marshal(event)
		if !bytes.Equal(before, after) || fmt.Sprint(beforeErr) != fmt.Sprint(afterErr) {
			test.Fatal("projection or streaming mutated the source event")
		}
	}()
	oracle, oldErr := canonicalEventJSONObject(event)
	projected, newErr := legacyCanonicalEventJSONObject(event)
	if (oldErr == nil) != (newErr == nil) || fmt.Sprint(oldErr) != fmt.Sprint(newErr) ||
		errors.Is(oldErr, errCanonicalEventShape) != errors.Is(newErr, errCanonicalEventShape) {
		test.Fatalf("projection acceptance differs: old=%v new=%v", oldErr, newErr)
	}
	if oldErr != nil {
		return nil, false
	}
	assertLegacyCanonicalBlocksProjectionShape(test, event, projected)
	want, err := json.Marshal(oracle)
	if err != nil {
		test.Fatal(err)
	}
	encoding, err := newLegacyCanonicalEncoding(context.Background(), projected)
	if err != nil {
		test.Fatal(err)
	}
	got, _ := collectLegacyCanonicalEncoding(test, encoding)
	if !bytes.Equal(got, want) || HashContent(got) != HashContent(want) {
		test.Fatalf("typed block projection changed canonical bytes/hash: got=%q want=%q", got, want)
	}
	chunk, complete, err := encoding.Next(context.Background())
	if err != nil || !complete || len(chunk) != 0 {
		test.Fatal("completed projected encoding changed on retry", complete, err)
	}
	return got, true
}

func TestLegacyCanonicalBlocksProjectionDecodedParity(test *testing.T) {
	accepted, rejected := 0, 0
	for _, candidate := range canonicalInputEventCases(test) {
		test.Run(candidate.name, func(test *testing.T) {
			var event CIREvent
			if json.Unmarshal(candidate.raw, &event) != nil {
				return
			}
			if _, ok := assertLegacyCanonicalBlocksProjectionParity(test, event); ok {
				accepted++
			} else {
				rejected++
			}
		})
	}
	if accepted == 0 || rejected == 0 {
		test.Fatal("decoded corpus did not exercise both projection acceptance and rejection", accepted, rejected)
	}
}

func TestLegacyCanonicalBlocksProjectionMessageAndTypedOutput(test *testing.T) {
	backing := []ContentBlock{{Type: "text", Text: "visible"}, {Type: "hidden", Text: "unused capacity"}}
	for _, candidate := range []struct {
		name   string
		blocks []ContentBlock
	}{
		{name: "nil"},
		{name: "empty", blocks: []ContentBlock{}},
		{name: "zero-fields", blocks: []ContentBlock{{}}},
		{name: "sparse", blocks: []ContentBlock{{Type: "text"}, {Text: "text without type"}}},
		{name: "escaping", blocks: []ContentBlock{{Type: "text<&>\u2028", Text: "\"\\\n\x00<&>\u2028\u2029\ud55c\uad6d🙂\xff"}}},
		{name: "spare-capacity", blocks: backing[:1]},
	} {
		for _, kind := range []EventKind{EventMessage, EventToolResult} {
			for _, nested := range []bool{false, true} {
				test.Run(fmt.Sprintf("%s/%s/nested-%t", candidate.name, kind, nested), func(test *testing.T) {
					event := CIREvent{Kind: kind, Seq: 9007199254740993}
					if kind == EventMessage {
						event.Role, event.Blocks = RoleAssistant, candidate.blocks
					} else {
						event.CallID, event.Output = "call", candidate.blocks
					}
					if nested {
						event = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{
							Kind: EventCompaction, Replacement: []CIREvent{event},
						}}}
					}
					if _, ok := assertLegacyCanonicalBlocksProjectionParity(test, event); !ok {
						test.Fatal("valid typed block event rejected")
					}
				})
			}
		}
	}
}

func TestLegacyCanonicalBlocksProjectionSparsePresence(test *testing.T) {
	for _, candidate := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "message-missing", raw: `{"kind":"message"}`, want: `{"blocks":[],"kind":"message","role":"","seq":0}`},
		{name: "message-null-blocks", raw: `{"kind":"message","blocks":null}`, want: `{"blocks":[],"kind":"message","role":"","seq":0}`},
		{name: "message-empty-blocks", raw: `{"kind":"message","blocks":[]}`, want: `{"blocks":[],"kind":"message","role":"","seq":0}`},
		{name: "message-empty-block", raw: `{"kind":"message","blocks":[{}]}`, want: `{"blocks":[{"text":"","type":""}],"kind":"message","role":"","seq":0}`},
		{name: "explicit-false", raw: `{"kind":"message","agent_message":false}`, want: `{"agent_message":false,"blocks":[],"kind":"message","role":"","seq":0}`},
		{name: "explicit-null", raw: `{"kind":"message","agent_message":null}`, want: `{"agent_message":false,"blocks":[],"kind":"message","role":"","seq":0}`},
		{name: "empty-agent-fields", raw: `{"kind":"message","agent_message":true,"agent_author":"","agent_recipient":"","provider_metadata":{},"locked":{}}`, want: `{"agent_author":"","agent_message":true,"agent_recipient":"","blocks":[],"kind":"message","locked":{"blob":"","provider":"","scheme":""},"provider_metadata":{},"role":"","seq":0}`},
		{name: "metadata-and-locked", raw: `{"kind":"message","agent_message":true,"blocks":[{"type":"text","text":"body"}],"provider_metadata":{"turn_id":"turn","create_time":1E+3},"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`, want: `{"agent_message":true,"blocks":[{"text":"body","type":"text"}],"kind":"message","locked":{"blob":"opaque","provider":"codex","scheme":"encrypted_content"},"provider_metadata":{"create_time":1E+3,"turn_id":"turn"},"role":"","seq":0}`},
		{name: "empty-replacement", raw: `{"kind":"compaction","replacement":[]}`, want: `{"kind":"compaction","replacement":[],"replacement_complete":false,"seq":0}`},
		{name: "locked-reasoning", raw: `{"kind":"reasoning","locked":{},"cross_replayable":true}`, want: `{"cross_replayable":false,"kind":"reasoning","locked":{"blob":"","provider":"","scheme":""},"seq":0}`},
	} {
		test.Run(candidate.name, func(test *testing.T) {
			var event CIREvent
			if err := json.Unmarshal([]byte(candidate.raw), &event); err != nil {
				test.Fatal(err)
			}
			got, ok := assertLegacyCanonicalBlocksProjectionParity(test, event)
			if !ok || !bytes.Equal(got, []byte(candidate.want)) {
				test.Fatalf("sparse field presence changed: got=%q want=%q", got, candidate.want)
			}
			nested := CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{
				Kind: EventCompaction, Replacement: []CIREvent{event},
			}}}
			if _, ok := assertLegacyCanonicalBlocksProjectionParity(test, nested); !ok {
				test.Fatal("nested sparse event rejected")
			}
		})
	}
}

func legacyCanonicalBlocksProjectionReplacementFixture() CIREvent {
	number := json.Number("1787683260.123456789")
	blocks := []ContentBlock{{Type: "text", Text: "shared <&> \ud55c\uad6d🙂"}, {}}
	return CIREvent{Kind: EventCompaction, Seq: 41, ReplacementComplete: true, Replacement: []CIREvent{
		{Kind: EventMessage, Seq: 9, ID: "late-first", Role: RoleAssistant, Blocks: blocks,
			AgentMessage: true, AgentAuthor: "agent", ProviderMetadata: &ProviderMetadata{TurnID: "turn", CreateTime: &number},
			Locked: &LockedBlob{Provider: ProviderKind("codex"), Scheme: "encrypted_content", Blob: "opaque"}},
		{Kind: EventCompaction, Seq: -2, Replacement: []CIREvent{
			{Kind: EventToolResult, Seq: 8, ID: "tie-first", Output: blocks},
			{Kind: EventMessage, Seq: 0, Blocks: nil, fieldPresence: eventFieldAgentMessage},
			{Kind: EventToolResult, Seq: 8, ID: "tie-second", Output: []ContentBlock{}},
		}},
		{Kind: EventMessage, Seq: 9, ID: "late-second", Blocks: blocks},
	}}
}

func TestLegacyCanonicalBlocksProjectionReplacementOrder(test *testing.T) {
	event := legacyCanonicalBlocksProjectionReplacementFixture()
	unsorted, ok := assertLegacyCanonicalBlocksProjectionParity(test, event)
	if !ok {
		test.Fatal("unsorted replacement projection rejected")
	}
	normalized := canonicalEvents([]CIREvent{event})[0]
	oracle, err := canonicalEventJSONObject(normalized)
	if err != nil {
		test.Fatal(err)
	}
	want, err := json.Marshal(oracle)
	if err != nil || bytes.Equal(unsorted, want) {
		test.Fatal("replacement fixture does not distinguish projection from normalization", err)
	}
	before, err := json.Marshal(event)
	if err != nil {
		test.Fatal(err)
	}
	var normalizer LegacyNormalizationDecoder
	var step LegacyNormalizationStep
	if err := normalizer.finishEvent(context.Background(), event, &step); err != nil {
		test.Fatal(err)
	}
	if normalizer.invalid || normalizer.count != 1 || len(step.Events) != 1 || step.Events[0].Sequence != event.Seq {
		test.Fatal("single event normalization changed admission, count or sequence")
	}
	projected, ok := step.Events[0].Encoding.root.(map[string]any)
	if !ok {
		test.Fatal("normalization did not retain an event projection")
	}
	assertLegacyCanonicalBlocksProjectionShape(test, normalized, projected)
	got, err := legacyNormalizationTestBytes(context.Background(), step.Events[0])
	if err != nil || !bytes.Equal(got, want) || HashContent(got) != HashContent(want) {
		test.Fatalf("single event replacement normalization changed bytes/hash: got=%q want=%q err=%v", got, want, err)
	}
	after, err := json.Marshal(event)
	if err != nil || !bytes.Equal(before, after) {
		test.Fatal("replacement normalization mutated the source event", err)
	}
	native, decoded, whole := legacyCanonicalNativePreparationDecode(test, string(before))
	decodedBefore, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(legacyCanonicalNativePreparationOracle(test, decoded), unsorted) {
		test.Fatal("native replacement fixture changed the original event union", err)
	}
	encoding := legacyCanonicalNativePreparationHandoff(test, native, decoded)
	assertLegacyCanonicalNativePreparationInitial(test, encoding, whole)
	nativeProjected, ok := encoding.root.(map[string]any)
	replacement, wrapped := nativeProjected["replacement"].(*legacyCanonicalReplacement)
	if !ok || len(nativeProjected) != len(projected) || nativeProjected["kind"] != event.Kind || nativeProjected["seq"] != event.Seq ||
		nativeProjected["replacement_complete"] != event.ReplacementComplete || !wrapped || replacement == nil || replacement.events == nil ||
		!reflect.DeepEqual(replacement.events, decoded.Replacement) || cap(replacement.events) != cap(decoded.Replacement) ||
		&replacement.events[0] != &decoded.Replacement[0] || !reflect.DeepEqual(replacement.order, legacyCanonicalKeyOrder{}) {
		test.Fatal("native projection did not retain an unvisited shallow replacement wrapper")
	}
	nativeGot, steps := legacyCanonicalNativePreparationDrain(test, encoding, whole)
	if steps == 0 || !bytes.Equal(nativeGot, want) || HashContent(nativeGot) != HashContent(want) {
		test.Fatalf("native replacement preparation changed canonical bytes/hash: got=%q want=%q steps=%d", nativeGot, want, steps)
	}
	decodedAfter, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(decodedBefore, decodedAfter) {
		test.Fatal("native replacement preparation mutated the decoded source event", err)
	}
}

func TestLegacyCanonicalBlocksProjectionRejectsInvalidShapes(test *testing.T) {
	for index, event := range []CIREvent{
		{Kind: EventKind("unknown")},
		{Kind: EventCompaction},
		{Kind: EventCompaction, Replacement: []CIREvent{}, Locked: &LockedBlob{}},
		{Kind: EventMessage, fieldPresence: eventFieldProviderMetadata},
		{Kind: EventMessage, AgentAuthor: "agent"},
		{Kind: EventMessage, fieldPresence: eventFieldLocked},
		{Kind: EventToolResult, Locked: &LockedBlob{}},
		{Kind: EventToolResult, Output: struct{ Text string }{Text: "unsupported"}},
	} {
		for _, nested := range []bool{false, true} {
			test.Run(fmt.Sprintf("%d/nested-%t", index, nested), func(test *testing.T) {
				candidate := event
				if nested {
					candidate = CIREvent{Kind: EventCompaction, Replacement: []CIREvent{{
						Kind: EventCompaction, Replacement: []CIREvent{event},
					}}}
				}
				if _, ok := assertLegacyCanonicalBlocksProjectionParity(test, candidate); ok {
					test.Fatal("invalid union or unsupported typed output was accepted")
				}
			})
		}
	}
}
