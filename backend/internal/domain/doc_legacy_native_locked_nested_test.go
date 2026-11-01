package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func legacyNativeLockedNestedEvents() []struct {
	name   string
	fields string
} {
	return []struct {
		name   string
		fields string
	}{
		{name: "reasoning", fields: `"kind":"reasoning","cross_replayable":false,"redacted_summary":"visible reasoning"`},
		{name: "agent-message", fields: `"kind":"message","role":"assistant","blocks":[{"type":"text","text":"visible agent message"}],"agent_message":true,"agent_author":"worker","agent_recipient":"parent"`},
		{name: "compaction", fields: `"kind":"compaction"`},
	}
}

func legacyNativeLockedNestedString(test *testing.T, prefix string) string {
	test.Helper()
	var scalar strings.Builder
	for _, boundary := range []struct {
		offset int
		token  string
		split  int
	}{
		{offset: LegacyCanonicalEncodingBytes, token: `\u0061`, split: 3},
		{offset: ConversationManifestChunkBytes, token: `\uD83D\uDE42`, split: 8},
		{offset: 2 * ConversationManifestChunkBytes, token: "🙂", split: 2},
		{offset: 3 * ConversationManifestChunkBytes, token: `\uD800x`, split: 4},
		{offset: 4 * ConversationManifestChunkBytes, token: `\"`, split: 1},
		{offset: 5 * ConversationManifestChunkBytes, token: `\uD800\uD801\uDC00`, split: 8},
	} {
		padding := boundary.offset - len(prefix) - 1 - scalar.Len() - boundary.split
		if padding < 0 {
			test.Fatal("locked scalar boundary overlaps an earlier token", boundary.offset)
		}
		scalar.WriteString(strings.Repeat("x", padding))
		scalar.WriteString(boundary.token)
	}
	scalar.WriteString(`\"\\\/\b\f\n\r\t\u0000\u2028\u2029\uDC00`)
	scalar.WriteString("한<&>")
	const ending = `\uD800`
	padding := 5*ConversationManifestChunkBytes + 73 - scalar.Len() - len(ending)
	if padding < 0 {
		test.Fatal("locked scalar exceeded its fixed giant payload", scalar.Len())
	}
	scalar.WriteString(strings.Repeat("x", padding))
	scalar.WriteString(ending)
	return `"` + scalar.String() + `"`
}

func legacyNativeLockedNestedWorkCheck(test *testing.T, raw []byte, expected CIREvent) {
	test.Helper()
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil {
		test.Fatal("native locked event constructor failed", err)
	}
	native.reset(0, len(raw))
	native.object.prefix = true
	buffer := &legacySourceProofBuffer{body: raw}
	source := legacyNormalizationPreparationInput(test, buffer, true)
	ctx := context.Background()
	var frame legacyJSONValueScanner
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	maximumWork, maximumFields := 0, 0
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		input := &legacyNormalizationInput{ctx: ctx, source: source}
		actual, complete, err := native.step(ctx, input, &frame)
		if input.workBytes > ConversationManifestChunkBytes || input.loads > 2 || input.unbounded ||
			input.fields > LegacyNormalizationStepEvents*len(native.object.metadata.fields) {
			test.Fatal("nested locked decoding exceeded its shared work, part, or field quantum", stepIndex, input.workBytes, input.loads, input.fields, input.unbounded)
		}
		maximumWork, maximumFields = max(maximumWork, input.workBytes), max(maximumFields, input.fields)
		if input.failure != nil || err != nil {
			test.Fatal("native locked decoding rejected an accepted typed event", stepIndex, input.failure, err)
		}
		if complete {
			if !reflect.DeepEqual(actual, expected) {
				test.Fatal("nested locked decoding changed native event values or exact presence")
			}
			test.Logf("native steps=%d max_work_bytes=%d max_fields=%d", stepIndex+1, maximumWork, maximumFields)
			return
		}
	}
	test.Fatal("native locked decoding exhausted its original per-part progress allowance")
}

func legacyNativeLockedNestedCheck(test *testing.T, event string, expected LockedBlob) {
	test.Helper()
	var native CIREvent
	if err := json.Unmarshal([]byte(event), &native); err != nil || native.Locked == nil || *native.Locked != expected {
		test.Fatal("locked fixture lost encoding/json merge, null, or key-matching semantics", err)
	}
	wantEvent, err := canonicalEventJSONObject(native)
	if err != nil {
		test.Fatal("locked fixture is not a valid CIR event", err)
	}
	raw := legacyNativeEventDocument("2", event+`,{"kind":"turn","seq":1,"role":"user","id":"first"},{"kind":"turn","seq":7,"role":"assistant","id":"tie-last"}`)
	want, wantErr := legacyNormalizationOracle(raw)
	if wantErr != nil {
		test.Fatal("locked fixture lost its original CIR-v2 canonical acceptance", wantErr)
	}
	normalizer, input := legacyNativeScalarReviewStart(test, raw)
	type materializedEvent struct {
		sequence int
		body     []byte
	}
	var events []materializedEvent
	parts := (len(raw) + ConversationManifestChunkBytes - 1) / ConversationManifestChunkBytes
	var physical int64
	maximumLoads, maximumSourceWork, maximumAdvance := 0, 0, 0
	for stepIndex := 0; stepIndex < 64+32*parts; stepIndex++ {
		position, reads := normalizer.position, len(input.reads)
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil {
			test.Fatal("full normalizer rejected an accepted locked fixture", stepIndex, err)
		}
		advance := normalizer.position - position
		if advance < 0 || advance > legacyNormalizationFrameQuantum {
			test.Fatal("nested locked parsing exceeded the original framing progress quantum", stepIndex, position, normalizer.position)
		}
		loads, sourceWork := 0, 0
		for _, read := range input.reads[reads:] {
			physical += int64(read[1])
			if read[0] != len(raw) {
				loads++
				sourceWork += read[1]
			}
		}
		if loads > 2 || sourceWork > 2*ConversationManifestChunkBytes || physical > 8*int64(len(raw))+2 {
			test.Fatal("nested locked parsing exceeded the original current-source read bounds", stepIndex, loads, sourceWork, physical, len(raw))
		}
		maximumLoads = max(maximumLoads, loads)
		maximumSourceWork = max(maximumSourceWork, sourceWork)
		maximumAdvance = max(maximumAdvance, advance)
		if step.Reset {
			events = nil
		}
		for _, emitted := range step.Events {
			if emitted.Sequence == 7 && len(events) == 0 {
				fields, ok := emitted.Encoding.root.(map[string]any)
				if !ok || !reflect.DeepEqual(legacyCanonicalBlocksProjectionForTest(fields), wantEvent) {
					test.Fatal("native locked handoff changed event fields, visible content, or locked values")
				}
			}
			body, err := legacyNormalizationTestBytes(context.Background(), emitted)
			if err != nil {
				test.Fatal("locked event encoding retained a canceled step context", err)
			}
			events = append(events, materializedEvent{sequence: emitted.Sequence, body: body})
		}
		if !step.Complete {
			continue
		}
		if len(events) != 3 || step.Count != len(events) || !json.Valid(step.Envelope) {
			test.Fatal("nested locked stream lost surrounding events, final count, or envelope", len(events), step.Count)
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].sequence < events[right].sequence })
		bodies := make([][]byte, len(events))
		for ordinal, emitted := range events {
			bodies[ordinal] = emitted.body
		}
		got := AssembleCanonicalDoc(step.Envelope, [][]byte{bytes.Join(bodies, []byte(","))})
		if !bytes.Equal(got, want) {
			test.Fatal("nested locked parsing changed original native canonical bytes", len(got), len(want))
		}
		legacyNativeLockedNestedWorkCheck(test, []byte(event), native)
		test.Logf("full steps=%d parts=%d max_current_source_loads=%d max_current_source_bytes=%d max_progress=%d physical_bytes=%d source_bytes=%d", stepIndex+1, parts, maximumLoads, maximumSourceWork, maximumAdvance, physical, len(raw))
		return
	}
	test.Fatal("full locked normalization exhausted its original per-part progress allowance")
}

func TestLegacyNativeLockedNestedGiantStringsMatchTypedOracle(test *testing.T) {
	for _, shape := range legacyNativeLockedNestedEvents() {
		for _, field := range []string{"blob", "scheme"} {
			for _, keys := range []struct {
				name   string
				locked string
				blob   string
				scheme string
			}{
				{name: "exact", locked: `"locked"`, blob: `"blob"`, scheme: `"scheme"`},
				{name: "escaped", locked: `"loc\u006bed"`, blob: `"blo\u0062"`, scheme: `"\u0073cheme"`},
				{name: "casefold", locked: `"LOCKED"`, blob: `"BLOB"`, scheme: `"ſCHEME"`},
			} {
				test.Run(shape.name+"/"+field+"/"+keys.name, func(test *testing.T) {
					prefix := `{"envelope":{"cir_version":"2"},"events":[{"id":"locked","seq":7,` + shape.fields + `,` + keys.locked + `:{"provider":"codex",`
					selected, other := keys.blob, keys.scheme+`:"encrypted_content",`
					if field == "scheme" {
						selected, other = keys.scheme, keys.blob+`:"opaque",`
					}
					prefix += other + selected + `:`
					giant := legacyNativeLockedNestedString(test, prefix)
					var decoded string
					if err := json.Unmarshal([]byte(giant), &decoded); err != nil || len(giant)-2 != 5*ConversationManifestChunkBytes+73 {
						test.Fatal("giant locked string lost its native acceptance or exact size", err, len(giant)-2)
					}
					expected := LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "opaque"}
					if field == "blob" {
						expected.Blob = decoded
					} else {
						expected.Scheme = decoded
					}
					event := strings.TrimPrefix(prefix, `{"envelope":{"cir_version":"2"},"events":[`) + giant + `},"ts":"2026-10-25T00:00:00Z"}`
					legacyNativeLockedNestedCheck(test, event, expected)
				})
			}
		}
	}
}

func TestLegacyNativeLockedNestedDuplicateMergeAndNullMatchTypedOracle(test *testing.T) {
	blob := strings.Repeat("b", 2*ConversationManifestChunkBytes+73) + "한🙂<&>\n"
	scheme := strings.Repeat("s", 2*ConversationManifestChunkBytes+91) + "\u2028\u2029\"\\"
	blobJSON, err := json.Marshal(blob)
	if err != nil {
		test.Fatal(err)
	}
	schemeJSON, err := json.Marshal(scheme)
	if err != nil {
		test.Fatal(err)
	}
	for _, shape := range legacyNativeLockedNestedEvents() {
		for _, fixture := range []struct {
			name     string
			fields   string
			expected LockedBlob
		}{
			{name: "duplicate-object-keeps-giant-blob", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":` + string(blobJSON) + `},"LOCKED":{"scheme":"signature"}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: "signature", Blob: blob}},
			{name: "duplicate-object-keeps-giant-scheme", fields: `"locked":{"provider":"codex","scheme":` + string(schemeJSON) + `,"blob":"old"},"locKed":{"blob":"fresh"}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: scheme, Blob: "fresh"}},
			{name: "scalar-nulls-and-empty-object-retain-giants", fields: `"locked":{"provider":"codex","scheme":` + string(schemeJSON) + `,"blob":` + string(blobJSON) + `},"loc\u006bed":{"provider":null,"ſcheme":null,"BLOB":null},"LOCKED":{}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: scheme, Blob: blob}},
			{name: "short-blob-overwrite-then-null", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":` + string(blobJSON) + `,"BLOB":"fresh\nvalue","\u0062lob":null}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "fresh\nvalue"}},
			{name: "short-scheme-overwrite-then-null", fields: `"locked":{"provider":"codex","blob":"opaque","scheme":` + string(schemeJSON) + `,"ſcheme":"signature","SCHEME":null}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: "signature", Blob: "opaque"}},
			{name: "escaped-giants-merge-into-existing-object", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":"old"},"LOCKED":{"blo\u0062":` + string(blobJSON) + `},"locKed":{"\u0073cheme":` + string(schemeJSON) + `}`, expected: LockedBlob{Provider: ProviderCodex, Scheme: scheme, Blob: blob}},
			{name: "outer-null-resets-giant-blob-before-new-object", fields: `"locked":{"provider":"codex","scheme":"encrypted_content","blob":` + string(blobJSON) + `},"locKed":null,"loc\u006bed":{"provider":"claude","scheme":"signature","blob":null}`, expected: LockedBlob{Provider: ProviderClaude, Scheme: "signature"}},
			{name: "outer-null-resets-giant-scheme-before-new-object", fields: `"locked":{"provider":"codex","scheme":` + string(schemeJSON) + `,"blob":"stale"},"LOCKED":null,"locked":{"provider":"claude","scheme":"signature","blob":null}`, expected: LockedBlob{Provider: ProviderClaude, Scheme: "signature"}},
		} {
			test.Run(shape.name+"/"+fixture.name, func(test *testing.T) {
				event := `{"id":"locked","seq":7,` + shape.fields + `,` + fixture.fields + `,"ts":"2026-10-25T00:00:00Z"}`
				legacyNativeLockedNestedCheck(test, event, fixture.expected)
			})
		}
	}
}
