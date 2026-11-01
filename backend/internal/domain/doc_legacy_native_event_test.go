package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func legacyNativeEventDocument(version, events string) []byte {
	return []byte(fmt.Sprintf(`{"envelope":{"cir_version":%q},"events":[%s]}`, version, events))
}

func legacyNativeEventEscapedKey(name string) string {
	var key strings.Builder
	key.WriteByte('"')
	for _, character := range name {
		fmt.Fprintf(&key, `\u%04x`, character)
	}
	key.WriteByte('"')
	return key.String()
}

func legacyNativeEventCanonicalCheck(test *testing.T, raw []byte, accepted bool) ([]byte, error) {
	test.Helper()
	want, wantErr := legacyNormalizationOracle(raw)
	if (wantErr == nil) != accepted {
		test.Fatal("fixture lost its original typed acceptance", accepted, wantErr)
	}
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	got, err := normalizeLegacyFixture(context.Background(), input.input())
	if !errors.Is(err, wantErr) || (err == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
		test.Fatalf("event member decoding changed original acceptance or canonical bytes: got=%q error=%v want=%q error=%v", got, err, want, wantErr)
	}
	if err != nil && errors.Is(err, ErrPreparedDocRangeObject) {
		test.Fatal("authenticated native rejection became a private-source failure", err)
	}
	return want, wantErr
}

func legacyNativeEventCheck(test *testing.T, raw []byte, accepted bool) {
	test.Helper()
	want, wantErr := legacyNativeEventCanonicalCheck(test, raw, accepted)
	canonical, original, oracleErr := legacyNormalizationPreparationOracle(raw)
	if !errors.Is(oracleErr, wantErr) || !bytes.Equal(canonical, want) {
		test.Fatal("existing independent preparation oracle disagrees", oracleErr, wantErr)
	}
	work, storage := legacyNormalizationPreparationStart(test, raw, true)
	_, preparationErr := legacyNormalizationPreparationDrain(test, context.Background(), work)
	if !errors.Is(preparationErr, wantErr) || (preparationErr == nil) != accepted {
		test.Fatal("preparation changed native acceptance", preparationErr, wantErr)
	}
	if preparationErr != nil {
		if errors.Is(preparationErr, ErrPreparedDocRangeObject) {
			test.Fatal("native rejection changed preparation error category", preparationErr)
		}
		return
	}
	legacyNormalizationPreparationCheck(test, work, storage, canonical, original)
}

func legacyNativeEventZero(test *testing.T, step LegacyNormalizationStep) {
	test.Helper()
	if step.Reset || step.Events != nil || step.Complete || step.Envelope != nil || step.Count != 0 {
		test.Fatal("failed native step exposed provisional output", step.Reset, len(step.Events), step.Complete, len(step.Envelope), step.Count)
	}
}

func legacyNativeEventDrain(test *testing.T, ctx context.Context, normalizer *LegacyNormalizationDecoder, input *legacySourceProofBuffer, bounded bool) (int, error) {
	test.Helper()
	nativeSteps, seen := 0, 0
	for stepIndex := 0; stepIndex < 4096; stepIndex++ {
		wasNative, before := normalizer.native != nil, len(input.reads)
		stepCtx, cancel := context.WithCancel(ctx)
		step, err := normalizer.Step(stepCtx)
		cancel()
		framed := wasNative || normalizer.native != nil
		if framed {
			nativeSteps++
		}
		loads := 0
		for _, read := range input.reads[before:] {
			if read[0] == len(input.body) && (read[1] == 0 || read[1] == 1) {
				continue
			}
			if read[0] < 0 || read[0] >= len(input.body) || read[0]%ConversationManifestChunkBytes != 0 ||
				read[1] != min(ConversationManifestChunkBytes, len(input.body)-read[0]) {
				test.Fatal("native framing bypassed authenticated full-part reads", stepIndex, read)
			}
			loads++
		}
		if bounded && framed && loads > 2 {
			test.Fatal("one framed native WIP step reread a whole ignored event instead of at most two payload parts", stepIndex, loads, input.reads[before:])
		}
		retained, retainedErr := normalizer.RetainedBytes()
		if retainedErr != nil || retained <= 0 {
			test.Fatal("native WIP retained-byte accounting failed", retained, retainedErr)
		}
		if bounded && retained >= 64<<10 {
			test.Fatal("small known fields retained a giant ignored event member, key, whitespace, or authenticated page", retained)
		}
		if err != nil {
			legacyNativeEventZero(test, step)
			return nativeSteps, err
		}
		if len(step.Events) > LegacyNormalizationStepEvents {
			test.Fatal("native framing exceeded the existing event batch", len(step.Events))
		}
		if step.Reset {
			seen = 0
		}
		seen += len(step.Events)
		if step.Complete {
			if seen != step.Count || !json.Valid(step.Envelope) {
				test.Fatal("native WIP lost reset, event, or final-envelope output", seen, step.Count)
			}
			return nativeSteps, nil
		}
	}
	test.Fatal("native event framing exceeded the fixed 4096-step fixture limit")
	return nativeSteps, ErrIntegrity
}

func TestLegacyNativeEventReflectedTagsAndFoldedNativeErrors(test *testing.T) {
	eventType := reflect.TypeOf(CIREvent{})
	for fieldIndex := 0; fieldIndex < eventType.NumField(); fieldIndex++ {
		field := eventType.Field(fieldIndex)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if field.PkgPath != "" || name == "" || name == "-" {
			continue
		}
		invalid := `false`
		fieldType := field.Type
		for fieldType.Kind() == reflect.Pointer {
			fieldType = fieldType.Elem()
		}
		if fieldType.Kind() == reflect.Bool {
			invalid = `{}`
		} else if field.Type.Kind() == reflect.Interface {
			invalid = `1e999`
		}
		keys := []struct {
			label string
			key   string
		}{
			{label: "exact", key: fmt.Sprintf("%q", name)},
			{label: "uppercase", key: fmt.Sprintf("%q", strings.ToUpper(name))},
			{label: "escaped-exact", key: legacyNativeEventEscapedKey(name)},
		}
		if strings.Contains(name, "s") {
			keys = append(keys, struct {
				label string
				key   string
			}{label: "unicode-long-s", key: fmt.Sprintf("%q", strings.ReplaceAll(name, "s", "ſ"))})
		}
		if strings.Contains(name, "k") {
			keys = append(keys, struct {
				label string
				key   string
			}{label: "unicode-kelvin", key: fmt.Sprintf("%q", strings.ReplaceAll(name, "k", "K"))})
		}
		for _, key := range keys {
			test.Run(name+"/"+key.label, func(test *testing.T) {
				raw := []byte(`{"events":[{"kind":"turn",` + key.key + `:` + invalid + `}],"events":[],"envelope":{"cir_version":"2"}}`)
				legacyNativeEventCheck(test, raw, false)
			})
		}
		if !strings.EqualFold(field.Name, name) {
			test.Run(name+"/go-name-is-not-json-tag", func(test *testing.T) {
				raw := legacyNativeEventDocument("1", fmt.Sprintf(`{"kind":"turn",%q:1e999}`, field.Name))
				legacyNativeEventCheck(test, raw, true)
			})
		}
	}
}

func TestLegacyNativeEventOriginalFieldMerging(test *testing.T) {
	for _, fixture := range []struct {
		name  string
		event string
	}{
		{name: "scalar-null-keeps-native-value", event: `{"kind":"unknown","KIND":"turn","kind":null,"seq":8,"SEQ":null,"role":"old","ROLE":"user","id":"retained","ID":null,"ts":"retained-time","TS":null}`},
		{name: "escaped-and-unicode-folded-tags", event: "{\"\\u006bind\":\"message\",\"ſeq\":4,\"ROLE\":\"assistant\",\"blocKſ\":[{\"TYPE\":\"text\",\"te\\u0078t\":\"\ud55c🙂\"}],\"compact_ſummary\":true,\"compact_summary\":null}"},
		{name: "metadata-object-merges", event: `{"kind":"turn","provider_metadata":{"turn_id":"retained","create_time":1787683260.123456789},"PROVIDER_METADATA":{"turn_id":"changed"},"provider_metadata":{"unknown":1e999}}`},
		{name: "metadata-pointer-null-reset", event: `{"kind":"turn","provider_metadata":{"turn_id":"stale","create_time":1E+3},"provider_metadata":null,"PROVIDER_METADATA":{"turn_id":"fresh"}}`},
		{name: "metadata-number-pointer-and-null", event: `{"kind":"turn","provider_metadata":{"turn_id":"retained","create_time":1E+3},"provider_metadata":{"create_time":null},"provider_metadata":{"create_time":2.500}}`},
		{name: "metadata-number-not-float64", event: `{"kind":"turn","provider_metadata":{"turn_id":"precise","create_time":1e999},"provider_metadata":{"turn_id":null}}`},
		{name: "blocks-reuse-and-hidden-tail", event: `{"kind":"message","blocks":[{"type":"text","text":"old"},{"type":"text","text":"hidden-tail"}],"BLOCKS":[{"text":"changed"}],"bloc\u006bs":[null,{}]}`},
		{name: "blocks-empty-discards-backing-history", event: `{"kind":"message","blocks":[{"type":"text","text":"stale"}],"blocks":[],"blocks":[{}]}`},
		{name: "blocks-null-discards-backing-history", event: `{"kind":"message","blocks":[{"type":"text","text":"stale"}],"blocks":null,"blocks":[{},null]}`},
		{name: "input-merges-outer-map-not-nested-values", event: `{"kind":"tool_call","input":{"keep":1,"replace":{"old":1},"same":1,"same":2},"INPUT":{"replace":{"new":2},"same":null}}`},
		{name: "input-null-allocates-fresh-map", event: `{"kind":"tool_call","input":{"stale":1},"input":null,"inpu\u0074":{"fresh":2}}`},
		{name: "output-interface-object-replaced", event: `{"kind":"tool_result","output":{"stale":1,"replace":{"old":1}},"OUTPUT":{"replace":{"new":2}}}`},
		{name: "output-interface-duplicate-map-key", event: `{"kind":"tool_result","output":{"keep":1,"replace":{"old":1},"replace":{"new":2}}}`},
		{name: "output-interface-type-and-null-reset", event: `{"kind":"tool_result","output":{"old":1},"output":[1,2],"output":null,"output":"fresh","is_error":true,"IS_ERROR":null}`},
		{name: "native-float64-input-conversion", event: `{"kind":"tool_call","input":{"rounded":9007199254740993,"negative_zero":-0,"underflow":1e-999,"maximum":1.7976931348623157e308,"nested":[0.1,9007199254740995]}}`},
		{name: "native-float64-output-conversion", event: `{"kind":"tool_result","output":[9007199254740993,-0,1e-999,{"fraction":1.234567890123456789}]}`},
		{name: "unknown-overflow-is-not-an-interface-decode", event: `{"kind":"message","ignored":1e999,"unknown":{"input":1e999,"output":1e999,"agent_message":false},"blocks":[]}`},
		{name: "nested-replacement-custom-decoder-resets-elements", event: `{"kind":"compaction","replacement":[{"kind":"message","id":"stale","agent_message":true,"agent_author":"stale","blocks":[{"type":"text","text":"stale"}]}],"REPLACEMENT":[{"kind":"turn","seq":2}],"replacement_complete":true,"replacement_complete":null}`},
		{name: "replacement-hidden-tail-custom-reset", event: `{"kind":"compaction","replacement":[{"kind":"turn","id":"old-first","role":"user"},{"kind":"message","id":"old-tail","agent_message":true,"agent_author":"old-tail"}],"replacement":[{"kind":"turn","seq":7}],"replacement":[{"kind":"turn","seq":2},{"kind":"turn","seq":1}]}`},
		{name: "replacement-null-element-can-be-overwritten", event: `{"kind":"compaction","replacement":[null],"replacement":[{"kind":"turn"}]}`},
		{name: "replacement-null-then-empty", event: `{"kind":"compaction","replacement":[{"kind":"turn"}],"replacement":null,"replacement":[],"replacement_complete":false}`},
		{name: "replacement-recursive-sort-and-presence", event: `{"kind":"compaction","replacement_complete":true,"replacement":[{"kind":"message","seq":8,"agent_message":false},{"kind":"compaction","seq":1,"replacement":[{"kind":"turn","id":"tie-first","seq":2},{"kind":"turn","seq":-1},{"kind":"turn","id":"tie-second","seq":2}],"replacement_complete":false}]}`},
		{name: "locked-pointer-object-merges", event: `{"kind":"reasoning","locked":{"provider":"codex","scheme":"encrypted_content","blob":"old"},"locKed":{"blob":"new"},"cross_replayable":false,"CROSS_REPLAYABLE":null}`},
		{name: "locked-pointer-null-reset", event: `{"kind":"reasoning","locked":{"provider":"codex","scheme":"old","blob":"old"},"LOCKED":null,"locked":{"provider":"claude","scheme":"signature","blob":"new"}}`},
		{name: "agent-exact-and-folded-duplicate-null", event: `{"kind":"message","agent_message":true,"AGENT_MESSAGE":null,"agent_author":"retained","AGENT_AUTHOR":null,"agent_recipient":"all","agent_recipient":null}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeEventCheck(test, legacyNativeEventDocument("2", fixture.event), true)
		})
	}
}

func TestLegacyNativeEventExactPresenceAndFoldedRecognition(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		kind    string
		value   string
		exactV2 bool
	}{
		{name: "provider_metadata", kind: "turn", value: `null`},
		{name: "agent_message", kind: "message", value: `false`, exactV2: true},
		{name: "agent_author", kind: "message", value: `""`},
		{name: "agent_recipient", kind: "message", value: `""`},
		{name: "replacement", kind: "turn", value: `null`},
		{name: "replacement_complete", kind: "turn", value: `false`},
		{name: "locked", kind: "reasoning", value: `null`},
	} {
		for _, version := range []string{"1", "2"} {
			for _, spelling := range []struct {
				label string
				key   string
				exact bool
			}{
				{label: "exact", key: fmt.Sprintf("%q", fixture.name), exact: true},
				{label: "escaped-exact", key: legacyNativeEventEscapedKey(fixture.name), exact: true},
				{label: "folded", key: fmt.Sprintf("%q", strings.ToUpper(fixture.name))},
				{label: "escaped-folded", key: legacyNativeEventEscapedKey(strings.ToUpper(fixture.name))},
			} {
				test.Run(fixture.name+"/v"+version+"/"+spelling.label, func(test *testing.T) {
					event := fmt.Sprintf(`{"kind":%q,%s:%s}`, fixture.kind, spelling.key, fixture.value)
					accepted := !spelling.exact || version == "2" && fixture.exactV2
					legacyNativeEventCheck(test, legacyNativeEventDocument(version, event), accepted)
				})
			}
		}
	}
	for _, fixture := range []struct {
		name     string
		version  string
		event    string
		accepted bool
	}{
		{name: "folded-true-still-requires-v2", version: "1", event: `{"kind":"message","AGENT_MESSAGE":true}`},
		{name: "folded-metadata-object-still-requires-v2", version: "1", event: `{"kind":"turn","PROVIDER_METADATA":{}}`},
		{name: "folded-reasoning-locked-is-v1", version: "1", event: `{"kind":"reasoning","loc\u212aed":{"provider":"codex","scheme":"s","blob":"b"}}`, accepted: true},
		{name: "folded-message-locked-needs-agent", version: "2", event: `{"kind":"message","LOCKED":{"provider":"codex","scheme":"s","blob":"b"}}`},
		{name: "exact-false-survives-folded-null", version: "1", event: `{"kind":"message","agent_message":false,"AGENT_MESSAGE":null}`},
		{name: "exact-empty-author-survives-folded-null", version: "2", event: `{"kind":"message","agent_author":"","AGENT_AUTHOR":null}`},
		{name: "folded-then-exact-null-adds-presence", version: "1", event: `{"kind":"message","AGENT_MESSAGE":false,"\u0061gent_message":null}`},
		{name: "embedded-nul-and-surrogate-are-unknown", version: "1", event: `{"kind":"turn","\u0000agent_message":false,"agent_message\u0000":true,"\ud800gent_message":false,"agent_meſſage":false}`, accepted: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeEventCheck(test, legacyNativeEventDocument(fixture.version, fixture.event), fixture.accepted)
		})
	}
}

func TestLegacyNativeEventStickyNativeErrorsAndResettableUnionErrors(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		raw      string
		accepted bool
	}{
		{name: "seq-error-before-duplicate-and-array-reset", raw: `{"events":[{"kind":"turn","seq":"bad","seq":2}],"events":[]}`},
		{name: "later-event-error-before-null-reset", raw: `{"events":[{"kind":"turn"},{"kind":"turn","seq":"bad"}],"events":null}`},
		{name: "block-error-before-field-and-array-reset", raw: `{"events":[{"kind":"message","blocks":[{"text":7}],"blocks":[]}],"events":[]}`},
		{name: "metadata-error-before-pointer-and-array-reset", raw: `{"events":[{"kind":"turn","provider_metadata":{"turn_id":7},"provider_metadata":null}],"events":[]}`},
		{name: "metadata-number-error-before-array-reset", raw: `{"events":[{"kind":"turn","provider_metadata":{"create_time":"not-a-number"}}],"events":[]}`},
		{name: "input-overflow-before-member-field-and-array-reset", raw: `{"events":[{"kind":"tool_call","input":{"number":1e999,"number":0},"input":null}],"events":[]}`},
		{name: "output-overflow-before-member-field-and-array-reset", raw: `{"events":[{"kind":"tool_result","output":{"number":1e999,"number":0},"output":null}],"events":[]}`},
		{name: "output-direct-overflow-before-duplicate", raw: `{"events":[{"kind":"tool_result","output":1e999,"output":0}],"events":[]}`},
		{name: "input-array-error-before-duplicate", raw: `{"events":[{"kind":"tool_call","input":[],"input":{}}],"events":[]}`},
		{name: "nested-native-error-survives-replacement-reset", raw: `{"events":[{"kind":"compaction","replacement":[{"kind":"turn","seq":"bad"}],"replacement":[]}],"events":[]}`},
		{name: "nested-native-error-survives-whole-array-reset", raw: `{"events":[{"kind":"compaction","replacement":[{"kind":"tool_result","output":[1e999]}]}],"events":null}`},
		{name: "event-null-final-is-invalid", raw: `{"events":[null]}`},
		{name: "event-array-final-is-native-error", raw: `{"events":[[]],"events":[]}`},
		{name: "event-scalar-final-is-native-error", raw: `{"events":[true],"events":[]}`},
		{name: "union-kind-can-change-before-finalization", raw: `{"events":[{"kind":"unknown","kind":"turn"}]}`, accepted: true},
		{name: "unknown-kind-array-reset", raw: `{"events":[{"kind":"unknown"}],"events":[{"kind":"turn"}]}`, accepted: true},
		{name: "null-event-array-reset", raw: `{"events":[null],"events":[]}`, accepted: true},
		{name: "union-metadata-null-reset", raw: `{"envelope":{"cir_version":"2"},"events":[{"kind":"turn","provider_metadata":null}],"events":null}`, accepted: true},
		{name: "union-replacement-null-reset", raw: `{"events":[{"kind":"turn","replacement":null}],"events":[]}`, accepted: true},
		{name: "v2-presence-array-reset-under-v1", raw: `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_message":false}],"events":[]}`, accepted: true},
		{name: "nested-union-error-array-reset", raw: `{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[{}]}],"events":[{"kind":"turn"}]}`, accepted: true},
		{name: "nested-union-error-replacement-reset", raw: `{"envelope":{"cir_version":"2"},"events":[{"kind":"compaction","replacement":[{}],"replacement":[]}]}`, accepted: true},
		{name: "whole-array-custom-element-reset", raw: `{"envelope":{"cir_version":"1"},"events":[{"kind":"message","agent_message":true,"agent_author":"stale"}],"events":[{"kind":"turn"}]}`, accepted: true},
		{name: "whole-array-hidden-tail-custom-reset", raw: `{"events":[{"kind":"turn","id":"old-first"},{"kind":"message","id":"old-tail","agent_message":true}],"events":[{"kind":"turn"}],"events":[{"kind":"turn","seq":2},{"kind":"turn","seq":1}]}`, accepted: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeEventCheck(test, []byte(fixture.raw), fixture.accepted)
		})
	}
}

func TestLegacyNativeEventGiantIgnoredMembersAndKeysHaveBoundedWIPReads(test *testing.T) {
	long := strings.Repeat("x", 5*ConversationManifestChunkBytes+73)
	known := `"kind":"message","seq":7,"role":"user","blocks":[{"type":"text","text":"visible"}]`
	for _, fixture := range []struct {
		name   string
		member string
		first  bool
	}{
		{name: "string-with-escapes", member: `"unknown":"` + long + `\u0061\"\\\/\b\f\n\r\t\ud83d\ude42"`, first: true},
		{name: "number-never-converted-to-float64", member: `"unknown":` + strings.Repeat("7", len(long)) + `e+999999`},
		{name: "nested-object-with-fake-known-members", member: `"unknown":{"events":[{"seq":"bad","locked":null,"padding":"` + long + `"}],"provider_metadata":null,"agent_message":false,"output":1e999}`},
		{name: "long-key-known-prefix", member: `"blocks` + long + `":1e999`},
		{name: "long-key-known-escaped-suffix", member: `"` + long + `\u0062locks":null`},
		{name: "long-escaped-key", member: `"` + strings.Repeat(`\u0078`, len(long)/6+1) + `":false`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			event := `{` + known + `,` + fixture.member + `,"SEQ":8,"id":"after-ignored"}`
			if fixture.first {
				event = `{` + fixture.member + `,` + known + `,"SEQ":8,"id":"after-ignored"}`
			}
			raw := legacyNativeEventDocument("2", event)
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			normalizer, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
			if err != nil || len(input.reads) != 0 {
				test.Fatal("native constructor eagerly read source payloads", err)
			}
			nativeSteps, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, true)
			if err != nil || nativeSteps < 4 {
				test.Fatal("giant ignored EVENT member never advanced cooperative semantic WIP", nativeSteps, err)
			}
			legacyNativeEventCheck(test, raw, true)
		})
	}
}

func TestLegacyNativeEventGiantWhitespaceAtMemberBoundaries(test *testing.T) {
	space := strings.Repeat(" \t\r\n", (5*ConversationManifestChunkBytes)/4+19)
	for _, fixture := range []struct {
		name   string
		before string
		after  string
	}{
		{name: "before-first-member", before: `{`, after: `"kind":"turn","seq":1}`},
		{name: "before-next-member", before: `{"kind":"turn",`, after: `"seq":1}`},
		{name: "before-known-colon", before: `{"kind":"turn","seq"`, after: `:1}`},
		{name: "before-known-value", before: `{"kind":"turn","seq":`, after: `1}`},
		{name: "after-known-value", before: `{"kind":"turn","seq":1`, after: `,"role":"user"}`},
		{name: "before-unknown-colon", before: `{"kind":"turn","unknown"`, after: `:1e999}`},
		{name: "before-unknown-value", before: `{"kind":"turn","unknown":`, after: `1e999}`},
		{name: "before-event-close", before: `{"kind":"turn","seq":1`, after: `}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := legacyNativeEventDocument("2", fixture.before+space+fixture.after)
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			normalizer, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
			if err != nil {
				test.Fatal(err)
			}
			nativeSteps, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, true)
			if err != nil || nativeSteps < 4 {
				test.Fatal("giant EVENT whitespace was consumed outside cooperative native WIP", nativeSteps, err)
			}
			legacyNativeEventCheck(test, raw, true)
		})
	}
}

func TestLegacyNativeEventFramingKeepsOriginalDepthAndSyntax(test *testing.T) {
	for _, depth := range []int{9996, 9997, 9998} {
		test.Run(fmt.Sprintf("event-inclusive-depth-%d", depth+3), func(test *testing.T) {
			raw := legacyNativeEventDocument("2", `{"kind":"turn","unknown":`+strings.Repeat("[", depth)+`0`+strings.Repeat("]", depth)+`}`)
			legacyNativeEventCheck(test, raw, depth+3 <= 10000)
		})
	}
	for ordinal, event := range []string{
		`{"kind":"turn",}`,
		`{"kind":"turn" "seq":1}`,
		`{"kind":"turn","unknown":01}`,
		`{"kind":"turn","unknown":1e+}`,
		`{"kind":"turn","unknown":{"nested":true,}}`,
		`{"kind":"turn","unknown":"\q"}`,
		`{"kind":"turn","\u12xz":null}`,
		`{"kind":"turn","unknown":[1,]}`,
	} {
		test.Run(fmt.Sprintf("malformed-event-%d", ordinal), func(test *testing.T) {
			legacyNativeEventCheck(test, legacyNativeEventDocument("2", event), false)
		})
	}
}

func TestLegacyNativeEventGiantSelectedNativeDecodeRemainsOpen(test *testing.T) {
	long := strings.Repeat("x", 5*ConversationManifestChunkBytes+73)
	for _, fixture := range []struct {
		name    string
		event   string
		bounded bool
	}{
		{name: "blocks", event: `{"kind":"message","blocks":[{"type":"text","text":"` + long + `"}]}`, bounded: true},
		{name: "input", event: `{"kind":"tool_call","input":{"selected":"` + long + `"}}`},
		{name: "output", event: `{"kind":"tool_result","output":"` + long + `"}`, bounded: true},
		{name: "replacement", event: `{"kind":"compaction","replacement":[{"kind":"message","blocks":[{"type":"text","text":"` + long + `"}]}]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			if fixture.bounded {
				test.Log("Selected string/block decoding has separate bounded-read regressions; this original fixture retains typed/canonical parity coverage")
			} else {
				test.Log("OPEN: selected giant known-field native decode is not covered by the two-payload WIP read or small-retention assertion")
			}
			raw := legacyNativeEventDocument("2", fixture.event)
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			normalizer, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
			if err != nil {
				test.Fatal(err)
			}
			if _, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, false); err != nil {
				test.Fatal("selected known native reduction changed acceptance", err)
			}
			legacyNativeEventCanonicalCheck(test, raw, true)
		})
	}
}

func TestLegacyNativeEventPrivateFailuresOutrankNativeAndUnionRejection(test *testing.T) {
	long := strings.Repeat("x", 5*ConversationManifestChunkBytes+73)
	for _, event := range []string{
		`{"kind":"turn","seq":"bad","unknown":"` + long + `"}`,
		`{"kind":"turn","provider_metadata":null,"unknown":"` + long + `"}`,
		`{"kind":"turn","unknown":"` + long + `\q"}`,
	} {
		for _, failure := range []string{"part-hash", "part-private-eof", "end-zero", "end-one"} {
			test.Run(fmt.Sprintf("%d-%s", len(event), failure), func(test *testing.T) {
				input := &legacySourceProofBuffer{body: legacyNativeEventDocument("2", event)}
				source := input.input()
				last := (len(input.body) - 1) / ConversationManifestChunkBytes * ConversationManifestChunkBytes
				if failure == "part-hash" {
					input.body[len(input.body)-4] ^= 1
				} else {
					source.Read = func(ctx context.Context, offset, length int) ([]byte, error) {
						body, err := input.Read(ctx, offset, length)
						if failure == "part-private-eof" && offset == last ||
							failure == "end-zero" && offset == len(input.body) && length == 0 ||
							failure == "end-one" && offset == len(input.body) && length == 1 {
							return nil, errors.Join(ErrPreparedDocRangeObject, io.EOF)
						}
						return body, err
					}
				}
				normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
				if err != nil {
					test.Fatal(err)
				}
				if _, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, true); !errors.Is(err, ErrPreparedDocRangeObject) {
					test.Fatal("document rejection hid private-source or EOF failure", err)
				}
				reads := len(input.reads)
				step, err := normalizer.Step(context.Background())
				legacyNativeEventZero(test, step)
				if !errors.Is(err, ErrPreparedDocRangeObject) || len(input.reads) != reads {
					test.Fatal("terminal private native-source failure was not sticky", err)
				}
			})
		}
	}
}

func TestLegacyNativeEventWIPCancellationAndEOFPrecedence(test *testing.T) {
	raw := legacyNativeEventDocument("2", `{"kind":"turn","unknown":"`+strings.Repeat("x", 5*ConversationManifestChunkBytes+73)+`","seq":1}`)
	test.Run("pre-canceled-native-wip-remains-resumable", func(test *testing.T) {
		input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
		normalizer, err := NewLegacyNormalizationDecoder(context.Background(), input.input())
		if err != nil {
			test.Fatal(err)
		}
		for stepIndex := 0; stepIndex < 64 && normalizer.native == nil; stepIndex++ {
			if step, err := normalizer.Step(context.Background()); err != nil || step.Complete || len(step.Events) != 0 {
				test.Fatal("native WIP did not become independently observable", err)
			}
		}
		if normalizer.native == nil {
			test.Fatal("giant event never entered semantic native WIP")
		}
		before, retainedErr := normalizer.RetainedBytes()
		reads := len(input.reads)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		step, err := normalizer.Step(ctx)
		legacyNativeEventZero(test, step)
		after, afterErr := normalizer.RetainedBytes()
		if !errors.Is(err, context.Canceled) || len(input.reads) != reads || retainedErr != nil || afterErr != nil || before != after || normalizer.native == nil {
			test.Fatal("pre-canceled native WIP read, discarded, or changed retained state", err, retainedErr, afterErr)
		}
		if _, err := legacyNativeEventDrain(test, context.Background(), normalizer, input, true); err != nil {
			test.Fatal("pre-cancellation poisoned resumable native WIP", err)
		}
		legacyNativeEventCheck(test, raw, true)
	})
	for _, when := range []string{"native-payload", "end-zero", "end-one"} {
		test.Run(when, func(test *testing.T) {
			input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
			source := input.input()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var normalizer *LegacyNormalizationDecoder
			triggered := false
			source.Read = func(readCtx context.Context, offset, length int) ([]byte, error) {
				body, err := input.Read(readCtx, offset, length)
				if when == "native-payload" && normalizer != nil && normalizer.native != nil && offset < len(input.body) && length > 0 ||
					when == "end-zero" && offset == len(input.body) && length == 0 ||
					when == "end-one" && offset == len(input.body) && length == 1 {
					triggered = true
					cancel()
					return body, errors.Join(ErrPreparedDocRangeObject, err, io.EOF)
				}
				return body, err
			}
			var err error
			normalizer, err = NewLegacyNormalizationDecoder(ctx, source)
			if err != nil {
				test.Fatal(err)
			}
			if _, err := legacyNativeEventDrain(test, ctx, normalizer, input, true); !triggered || !errors.Is(err, context.Canceled) {
				test.Fatal("native payload or EOF private failure outranked cancellation", triggered, err)
			}
		})
	}
}
