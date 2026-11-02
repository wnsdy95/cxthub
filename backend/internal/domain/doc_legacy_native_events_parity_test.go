package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

type legacyNativeEventsParityResult struct {
	fixture      *legacyNativeCanonicalOwnerFixture
	event        CIREvent
	expected     CIREvent
	quanta       int
	nestedCursor bool
}

func legacyNativeEventsParityDecode(test *testing.T, raw string) legacyNativeEventsParityResult {
	test.Helper()
	result := legacyNativeEventsParityResult{fixture: legacyNativeCanonicalOwnerStart(test, raw)}
	if err := json.Unmarshal([]byte(raw), &result.expected); err != nil {
		test.Fatal("typed CIREvent oracle rejected the normal replacement fixture", err)
	}
	if result.expected.Kind != EventCompaction {
		test.Fatal("replacement parity fixture lost its compaction root")
	}
	index := legacyEventNativeFields.match("replacement")
	for quantum := 0; quantum < 256; quantum++ {
		event, complete, err := result.fixture.step(test, context.Background(), 0)
		if err != nil {
			test.Fatal("actual native replacement decoding failed", quantum, err)
		}
		if !bytes.Equal(result.fixture.buffer.body, []byte(raw)) {
			test.Fatal("native replacement decoding mutated authenticated source bytes", quantum)
		}
		object := result.fixture.native.object
		if index >= 0 && index < len(object.events) && object.events[index] != nil {
			list := object.events[index]
			if list.cursor != nil && list.cursor != list {
				if list.cursor.parent == nil {
					test.Fatal("partial nested cursor lost its actual parent", quantum)
				}
				result.nestedCursor = true
			}
		}
		if !complete {
			if result.fixture.native.canonicalReady {
				test.Fatal("partial replacement became canonical ready", quantum)
			}
			continue
		}
		if !result.fixture.native.canonicalReady || !reflect.DeepEqual(event, result.expected) {
			test.Fatalf("native replacement differs from typed CIREvent values or exact presence: got=%#v want=%#v", event, result.expected)
		}
		if event.Replacement != nil && (index < 0 || index >= len(object.events) || object.events[index] == nil) {
			test.Fatal("replacement parity bypassed the actual native event-list driver")
		}
		result.event, result.quanta = event, quantum+1
		return result
	}
	test.Fatal("normal replacement parity exhausted its finite native decode allowance")
	return result
}

func legacyNativeEventsParityCanonical(test *testing.T, result legacyNativeEventsParityResult) {
	test.Helper()
	normalized := canonicalEvents([]CIREvent{result.expected})[0]
	want := legacyCanonicalNativePreparationOracle(test, normalized)
	original, err := canonicalJSON(normalized)
	if err != nil || !bytes.Equal(original, want) || HashContent(original) != HashContent(want) {
		test.Fatal("existing typed canonical byte/SHA oracles disagree", err)
	}
	source := bytes.Clone(result.fixture.buffer.body)
	snapshot := legacyCanonicalReplacementOwnershipSnapshot(test, result.event)
	reads := len(result.fixture.buffer.reads)
	owner, err := result.fixture.native.takeCanonicalOwner(context.Background())
	if err != nil || owner == nil {
		test.Fatal("actual native replacement could not transfer its canonical owner", err)
	}
	defer owner.release()
	if !reflect.DeepEqual(owner.event, result.expected) || cap(owner.event.Replacement) != cap(result.event.Replacement) ||
		len(result.event.Replacement) != 0 && &owner.event.Replacement[0] != &result.event.Replacement[0] {
		test.Fatal("owner transfer changed typed values, presence, or the measured native backing")
	}
	if result.fixture.native.canonicalReady || !reflect.DeepEqual(result.fixture.native.decoded, wire{}) || result.fixture.native.object.events != nil {
		test.Fatal("owner transfer retained native replacement values or helper references")
	}
	encoding, err := newLegacyCanonicalEncodingNative(context.Background(), owner)
	if err != nil || encoding == nil {
		test.Fatal("actual replacement owner could not enter canonical preparation", err)
	}
	defer encoding.release()
	if encoding.nativeOwner != owner || encoding.preparation == nil || encoding.inspectWork != 0 {
		test.Fatal("replacement parity bypassed pending native-owner preparation")
	}
	actual, steps := legacyCanonicalNativePreparationDrain(test, encoding, 0)
	if steps == 0 || !bytes.Equal(actual, want) || HashContent(actual) != HashContent(want) || HashContent(actual) != HashContent(original) {
		test.Fatalf("native replacement changed existing canonical bytes or SHA: got=%s want=%s", actual, want)
	}
	if len(result.fixture.buffer.reads) != reads || !bytes.Equal(result.fixture.buffer.body, source) ||
		!reflect.DeepEqual(result.event, result.expected) ||
		!reflect.DeepEqual(snapshot, legacyCanonicalReplacementOwnershipSnapshot(test, result.event)) {
		test.Fatal("owner preparation reread source or changed source values, presence, capacities, or hidden tails")
	}
}

func TestLegacyNativeEventsParityNilEmptyAndDuplicateReplacement(test *testing.T) {
	const locked = `,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"state"}`
	for _, fixture := range []struct {
		name    string
		members string
	}{
		{name: "absent-locked-state", members: locked},
		{name: "null-locked-state", members: `,"replacement":null` + locked},
		{name: "empty-boundary", members: `,"replacement":[],"replacement_complete":false`},
		{name: "child-then-null", members: `,"replacement":[{"kind":"turn","id":"old"}],"replacement":null` + locked},
		{name: "child-then-empty", members: `,"replacement":[{"kind":"turn","id":"old"}],"replacement":[]`},
		{name: "null-then-empty", members: `,"replacement":null,"replacement":[]`},
		{name: "empty-null-child", members: `,"replacement":[],"replacement":null,"replacement":[{"kind":"turn","seq":3}]`},
		{name: "null-elements-overwritten", members: `,"replacement":[null,null],"replacement":[{"kind":"message","blocks":[],"agent_message":false}]`},
		{name: "folded-escaped-duplicate-arrays", members: `,"replacement":[{"kind":"message","agent_message":true,"agent_author":"old"},{"kind":"turn","id":"hidden"}],"REPLACEMENT":[{"kind":"turn"}],"repla\u0063ement":[{"kind":"tool_call","input":{}}],"replacement_complete":true,"REPLACEMENT_COMPLETE":false`},
		{name: "folded-null-escaped-empty", members: `,"REPLACEMENT":[{"kind":"turn","id":"old"}],"RePlAcEmEnT":null,"repla\u0063ement":[]`},
		{name: "empty-discards-history", members: `,"replacement":[{"kind":"message","agent_message":true,"agent_author":"old","blocks":[{"type":"text","text":"old"}]}],"replacement":[],"replacement":[{"kind":"turn"}]`},
		{name: "null-discards-history", members: `,"replacement":[{"kind":"tool_call","input":{"old":1}}],"replacement":null,"replacement":[{"kind":"tool_call","input":{}}]`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			result := legacyNativeEventsParityDecode(test, `{"kind":"compaction","seq":17`+fixture.members+`}`)
			if (result.event.Replacement == nil) != (result.expected.Replacement == nil) {
				test.Fatal("typed replacement nil/empty distinction changed")
			}
			legacyNativeEventsParityCanonical(test, result)
		})
	}
}

func TestLegacyNativeEventsParityFreshChildWireAndHiddenTail(test *testing.T) {
	arrays := []string{
		`[{"kind":"message","id":"old-message","ts":"old-time","seq":9,"role":"assistant","blocks":[{"type":"text","text":"old-block"}],"agent_message":true,"agent_author":"old-author","agent_recipient":"old-recipient","provider_metadata":{"turn_id":"old-turn","create_time":123.450e-8},"locked":{"provider":"codex","scheme":"encrypted_content","blob":"old-agent"}},` +
			`{"kind":"tool_call","id":"old-call","seq":8,"call_id":"call","tool_name":"read","provider_tool_name":"Read","input":{"old":[1,{"text":"retained-tail"}]},"status":"done"},` +
			`{"kind":"tool_result","id":"old-result","seq":7,"call_id":"call","output":{"old":[true,"tail-output"]},"is_error":true},` +
			`{"kind":"reasoning","id":"old-reasoning","seq":6,"locked":{"provider":"claude","scheme":"signature","blob":"tail-signature"},"redacted_summary":"tail-summary","cross_replayable":true},` +
			`{"kind":"compaction","id":"old-nested","seq":5,"replacement":[{"kind":"message","blocks":[{"type":"text","text":"tail-child"}]}],"replacement_complete":true},` +
			`{"kind":"turn","id":"old-turn","seq":4,"role":"developer","provider_metadata":{"turn_id":"tail-turn"}}]`,
		`[{"kind":"message","agent_message":false,"blocks":null}]`,
		`[{"kind":"message","blocks":[]},{"kind":"tool_call","input":{}},{"kind":"tool_result","output":null,"is_error":false},{"kind":"reasoning"},{"kind":"compaction","replacement":[]},{"kind":"turn"}]`,
		`[{"kind":"turn"}]`,
	}
	for last := range arrays {
		test.Run(fmt.Sprintf("replacement-prefix-%d", last+1), func(test *testing.T) {
			raw := `{"kind":"compaction","replacement":` + strings.Join(arrays[:last+1], `,"replacement":`) + `,"replacement_complete":true}`
			result := legacyNativeEventsParityDecode(test, raw)
			if cap(result.event.Replacement) < 6 || cap(result.expected.Replacement) < 6 {
				test.Fatal("duplicate replacement discarded backing containing six decoded children")
			}
			actualBacking := result.event.Replacement[:cap(result.event.Replacement)]
			expectedBacking := result.expected.Replacement[:cap(result.expected.Replacement)]
			if !reflect.DeepEqual(actualBacking[len(result.event.Replacement):6], expectedBacking[len(result.expected.Replacement):6]) {
				test.Fatal("short replacement changed the typed oracle's known hidden children")
			}
			legacyNativeEventsParityCanonical(test, result)
		})
	}
}

func TestLegacyNativeEventsParityNullElementsResetChildWire(test *testing.T) {
	for _, replacement := range []string{
		`[null]`,
		`[{"kind":"message","agent_message":true,"agent_author":"old","blocks":[{"type":"text","text":"old"}]}],"replacement":[null]`,
		`[{"kind":"turn","provider_metadata":{"turn_id":"old"}},{"kind":"tool_result","output":{"old":true}}],"replacement":[null,null]`,
		`[{"kind":"compaction","replacement":[{"kind":"message","agent_message":false},null]}]`,
	} {
		test.Run(fmt.Sprintf("replacement-%d-bytes", len(replacement)), func(test *testing.T) {
			raw := `{"kind":"compaction","replacement":` + replacement + `}`
			result := legacyNativeEventsParityDecode(test, raw)
			snapshot := legacyCanonicalReplacementOwnershipSnapshot(test, result.event)
			reads := len(result.fixture.buffer.reads)
			owner, err := result.fixture.native.takeCanonicalOwner(context.Background())
			if err != nil || owner == nil {
				test.Fatal("normal typed null elements could not transfer to their owner", err)
			}
			defer owner.release()
			event, _, err := owner.claim(context.Background())
			if err != nil || !reflect.DeepEqual(event, result.expected) ||
				cap(event.Replacement) != cap(result.event.Replacement) || &event.Replacement[0] != &result.event.Replacement[0] ||
				!reflect.DeepEqual(snapshot, legacyCanonicalReplacementOwnershipSnapshot(test, event)) {
				test.Fatal("null child owner claim reused stale wire fields or changed measured backing", err)
			}
			if len(result.fixture.buffer.reads) != reads || !bytes.Equal(result.fixture.buffer.body, []byte(raw)) {
				test.Fatal("normal typed null-element transfer reread or mutated source")
			}
		})
	}
}

func TestLegacyNativeEventsParityCasefoldEscapesAndPresence(test *testing.T) {
	for _, fixture := range []struct {
		name          string
		key           string
		child         string
		childPresence eventFieldPresence
		rootPresence  eventFieldPresence
	}{
		{
			name: "exact-zero-fields", key: `"replacement"`,
			child:         `{"kind":"message","blocks":[],"agent_message":false,"provider_metadata":{}}`,
			childPresence: eventFieldAgentMessage | eventFieldProviderMetadata, rootPresence: eventFieldReplacement,
		},
		{
			name: "folded-values-without-exact-presence", key: `"REPLACEMENT"`,
			child: `{"KIND":"message","BLOCKS":[],"AGENT_MESSAGE":false,"PROVIDER_METADATA":{}}`,
		},
		{
			name: "escaped-exact-presence", key: legacyNativeEventEscapedKey("replacement"),
			child:         `{"\u006bind":"message","agent_\u006dessage":true,"agent_\u0061uthor":"","agent_recipien\u0074":"","provider_\u006detadata":{},"blocks":[]}`,
			childPresence: eventFieldAgentMessage | eventFieldAgentAuthor | eventFieldAgentRecipient | eventFieldProviderMetadata,
			rootPresence:  eventFieldReplacement,
		},
		{
			name: "exact-and-folded-duplicates-retain-presence", key: `"replacement"`,
			child:         `{"kind":"message","agent_message":false,"AGENT_MESSAGE":true,"agent_author":"","AGENT_AUTHOR":null,"agent_recipient":"","AGENT_RECIPIENT":null,"blocks":[{"type":"text","text":"old"}],"BLOCKS":[]}`,
			childPresence: eventFieldAgentMessage | eventFieldAgentAuthor | eventFieldAgentRecipient, rootPresence: eventFieldReplacement,
		},
		{
			name: "unicode-folded-tags-and-escaped-values", key: `"RePlAcEmEnT"`,
			child: "{\"KIND\":\"message\",\"ſeq\":-3,\"ROLE\":\"assistant\",\"agent_meſſage\":false,\"blocKſ\":[{\"TYPE\":\"text\",\"te\\u0078t\":\"\ud55c\\ud83d\\ude42\\n\\u0000\\u2028\\u2029<&>\\\\\\\"\\/\\ud800\"}],\"compact_ſummary\":true}",
		},
		{
			name: "lookalikes-do-not-create-presence", key: `"replacement"`,
			child:        `{"kind":"message","blocks":[],"agent_message ":true,"agent_message\u0000":false,"ProviderMetadata":{"ignored":1e999},"Replacement_":null,"replacement ":[null],"unknown":{"agent_message":false,"replacement":[]}}`,
			rootPresence: eventFieldReplacement,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := `{"kind":"compaction",` + fixture.key + `:[` + fixture.child + `]}`
			result := legacyNativeEventsParityDecode(test, raw)
			if result.event.fieldPresence != fixture.rootPresence || len(result.event.Replacement) != 1 ||
				result.event.Replacement[0].fieldPresence != fixture.childPresence {
				test.Fatal("replacement or child recognition changed exact decoded-key presence", result.event.fieldPresence, fixture.rootPresence)
			}
			legacyNativeEventsParityCanonical(test, result)
		})
	}
}

func TestLegacyNativeEventsParityNestedCursorResumesBeforeFollowingSiblings(test *testing.T) {
	text := strings.Repeat("x", LegacyCanonicalEncodingBytes+17)
	child := `{"kind":"message","role":"assistant","agent_message":false,"blocks":[{"type":"text","text":"` + text + `"}]}`
	children := strings.Repeat(child+",", 8) + child
	raw := `{"kind":"compaction","seq":17,"replacement":[{"kind":"compaction","id":"outer","replacement":[` +
		`{"kind":"compaction","id":"inner","replacement":[` + children + `],"replacement_complete":true},` +
		`{"kind":"turn","id":"nested-after","seq":-1}],"replacement_complete":true},` +
		`{"kind":"turn","id":"root-after","seq":-2}],"replacement_complete":true}`
	if len(raw) <= ConversationManifestChunkBytes || len(raw) > 2<<20 {
		test.Fatal("nested parity fixture lost its original source-page and input-ceiling boundaries", len(raw))
	}
	result := legacyNativeEventsParityDecode(test, raw)
	if result.quanta <= 1 || !result.nestedCursor || len(result.event.Replacement) != 2 ||
		result.event.Replacement[1].ID != "root-after" || result.event.Replacement[0].Replacement[1].ID != "nested-after" {
		test.Fatal("nested replacement did not resume its actual parent/cursor before following siblings", result.quanta, result.nestedCursor)
	}
	legacyNativeEventsParityCanonical(test, result)
}
