package domain

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

type legacyNativePagedParityResult struct {
	canonical []byte
	emitted   int
	children  int
	steps     int
	reads     int
}

func legacyNativePagedParityOracle(test *testing.T, raw []byte) (CIRDocument, []byte, error) {
	test.Helper()
	var document struct {
		Envelope CIREnvelope       `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		test.Fatal("paged parity fixture is not a typed JSON document", err)
	}
	typed := CIRDocument{Envelope: document.Envelope, Events: make([]CIREvent, len(document.Events))}
	for ordinal, event := range document.Events {
		if err := typed.Events[ordinal].UnmarshalJSON(event); err != nil {
			test.Fatal("independent CIREvent.UnmarshalJSON rejected the parity fixture", ordinal, err)
		}
	}
	canonical, err := CanonicalBytes(typed)
	if err == nil {
		generic, genericErr := canonicalJSON(CIRDocument{Envelope: typed.Envelope, Events: canonicalEvents(typed.Events)})
		if genericErr != nil || !bytes.Equal(canonical, generic) || sha256.Sum256(canonical) != sha256.Sum256(generic) {
			test.Fatal("existing typed canonical byte and generic JSON oracles disagree", genericErr)
		}
	}
	return typed, canonical, err
}

func legacyNativePagedParityValue(test *testing.T, event CIREvent, replacement *legacyNativeEventValues, blocks *legacyNativeBlockValues, expected CIREvent) int {
	test.Helper()
	children := expected.Replacement
	typedBlocks := expected.Blocks
	expected.Replacement, expected.Blocks = nil, nil
	if event.Replacement != nil || event.Blocks != nil || !reflect.DeepEqual(event, expected) {
		test.Fatalf("public paged value differs from independent typed fields/presence: id=%q got=%#v want=%#v", expected.ID, event, expected)
	}
	if (blocks == nil) != (typedBlocks == nil) || blocks.length() != len(typedBlocks) {
		test.Fatal("public paged value lost typed block null/array presence or logical length", expected.ID)
	}
	for ordinal, expectedBlock := range typedBlocks {
		block := blocks.at(ordinal)
		if block == nil || block.block != expectedBlock {
			test.Fatal("public paged value changed independent typed block content or order", expected.ID, ordinal)
		}
	}
	if (replacement == nil) != (children == nil) {
		test.Fatal("public paged value lost typed replacement null/array presence", expected.ID)
	}
	if replacement == nil {
		return 0
	}
	if replacement.length() != len(children) {
		test.Fatal("public paged value lost typed replacement children", expected.ID, replacement.length(), len(children))
	}
	count := len(children)
	for ordinal, child := range children {
		value := replacement.at(ordinal)
		if value == nil {
			test.Fatal("public owner omitted a replacement value", expected.ID, ordinal)
		}
		count += legacyNativePagedParityValue(test, value.event, value.replacement, value.blocks, child)
	}
	return count
}

func legacyNativePagedParitySourceReads(test *testing.T, input *legacySourceProofBuffer, partBytes int, raw []byte) {
	test.Helper()
	if !bytes.Equal(input.body, raw) || sha256.Sum256(input.body) != sha256.Sum256(raw) {
		test.Fatal("public native/owner/encoding path changed original source bytes or SHA")
	}
	covered := make([]bool, (len(raw)+partBytes-1)/partBytes)
	var zeroEOF, oneEOF bool
	for _, read := range input.reads {
		if read[0] == len(raw) && (read[1] == 0 || read[1] == 1) {
			zeroEOF = zeroEOF || read[1] == 0
			oneEOF = oneEOF || read[1] == 1
			continue
		}
		if read[0] < 0 || read[0] >= len(raw) || read[0]%partBytes != 0 || read[1] != min(partBytes, len(raw)-read[0]) {
			test.Fatal("paged parity used a different authenticated source profile", read, partBytes)
		}
		covered[read[0]/partBytes] = true
	}
	for ordinal, present := range covered {
		if !present {
			test.Fatal("paged normalization did not observe an original source part", ordinal, partBytes)
		}
	}
	if !zeroEOF || !oneEOF {
		test.Fatal("paged normalization finished without the original source EOF probes", zeroEOF, oneEOF)
	}
}

func legacyNativePagedParityRun(test *testing.T, raw []byte, partBytes int, accepted bool) legacyNativePagedParityResult {
	test.Helper()
	typed, want, wantErr := legacyNativePagedParityOracle(test, raw)
	if (wantErr == nil) != accepted {
		test.Fatal("paged parity fixture changed its independent typed acceptance", accepted, wantErr)
	}
	input := &legacySourceProofBuffer{body: bytes.Clone(raw)}
	source := LegacyByteSource{Bytes: len(raw), PartBytes: partBytes, Read: input.Read}
	if partBytes == 0 {
		partBytes = ConversationManifestChunkBytes
	}
	for offset := 0; offset < len(raw); offset += partBytes {
		body := raw[offset:min(offset+partBytes, len(raw))]
		source.Parts = append(source.Parts, ConversationManifestChunk{Hash: HashContent(body), Bytes: int64(len(body))})
	}
	normalizer, err := NewLegacyNormalizationDecoder(context.Background(), source)
	if err != nil || normalizer == nil || len(input.reads) != 0 {
		test.Fatal("public normalization constructor failed or read eagerly", err, len(input.reads))
	}
	var result legacyNativePagedParityResult
	var events []struct {
		sequence int
		body     []byte
	}
	for stepIndex := 0; stepIndex < 4096; stepIndex++ {
		step, err := normalizer.Step(context.Background())
		result.steps++
		if native := normalizer.nativeCache; native != nil && !native.object.pagedEvents {
			test.Fatal("public normalization selected the old typed internal replacement consumer")
		}
		if err != nil {
			if accepted || !errors.Is(err, ErrIntegrity) || errors.Is(err, ErrPreparedDocRangeObject) ||
				step.Complete || step.Envelope != nil || len(step.Events) != 0 || normalizer.phase == legacyNormalizationComplete {
				test.Fatal("paged normalization changed typed rejection or exposed completion", err, wantErr)
			}
			legacyNativePagedParitySourceReads(test, input, partBytes, raw)
			result.reads = len(input.reads)
			return result
		}
		if step.Reset {
			if len(events) != 0 {
				test.Fatal("single-array parity fixture unexpectedly reset completed events")
			}
		}
		for _, event := range step.Events {
			if result.emitted >= len(typed.Events) || event.Encoding == nil {
				test.Fatal("public paged normalization emitted an extra or unencoded event", result.emitted)
			}
			expected := typed.Events[result.emitted]
			encoding := event.Encoding
			owner := encoding.nativeOwner
			if owner == nil || encoding.preparation == nil || encoding.inspectWork != 0 || event.Sequence != expected.Seq {
				test.Fatal("public byte decoder bypassed native owner/pending canonical preparation or changed seq", result.emitted, event.Sequence, expected.Seq)
			}
			result.children += legacyNativePagedParityValue(test, owner.event, owner.replacement, owner.blocks, expected)
			if expected.Kind == EventCompaction && expected.Replacement != nil {
				fields, ok := encoding.root.(map[string]any)
				if !ok {
					test.Fatal("public paged owner lost the existing union projection")
				}
				replacement, ok := fields["replacement"].(*legacyCanonicalReplacement)
				if !ok || replacement == nil || replacement.events != nil || replacement.native != owner.replacement ||
					replacement.length() != len(expected.Replacement) {
					test.Fatal("public canonical replacement detached from its paged owner or rebuilt the old typed array")
				}
			}
			wantEvent, err := canonicalJSON(canonicalEvents([]CIREvent{expected})[0])
			if err != nil {
				test.Fatal("public paged projection admitted a typed-invalid event union", err)
			}
			reads := len(input.reads)
			body, _ := collectLegacyCanonicalEncoding(test, encoding)
			if len(input.reads) != reads || !bytes.Equal(body, wantEvent) ||
				sha256.Sum256(body) != sha256.Sum256(wantEvent) || HashContent(body) != HashContent(wantEvent) {
				test.Fatalf("public paged event changed typed canonical bytes/SHA or reread source: ordinal=%d got=%d bytes want=%d bytes", result.emitted, len(body), len(wantEvent))
			}
			events = append(events, struct {
				sequence int
				body     []byte
			}{sequence: event.Sequence, body: body})
			result.emitted++
		}
		if !step.Complete {
			if step.Envelope != nil || step.Count != 0 {
				test.Fatal("unfinished paged normalization exposed a final envelope/count")
			}
			continue
		}
		if !accepted || step.Count != len(typed.Events) || result.emitted != step.Count {
			test.Fatal("paged normalization completed a rejected or incomplete typed document", step.Count, result.emitted, wantErr)
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].sequence < events[right].sequence })
		bodies := make([][]byte, len(events))
		for ordinal, event := range events {
			bodies[ordinal] = event.body
		}
		result.canonical = AssembleCanonicalDoc(step.Envelope, bodies)
		if !bytes.Equal(result.canonical, want) || sha256.Sum256(result.canonical) != sha256.Sum256(want) ||
			HashContent(result.canonical) != HashContent(want) {
			test.Fatal("completed public paged document changed typed canonical bytes, sequence order, envelope, or SHA")
		}
		legacyNativePagedParitySourceReads(test, input, partBytes, raw)
		result.reads = len(input.reads)
		repeated, err := normalizer.Step(context.Background())
		if err != nil || !repeated.Complete || repeated.Count != step.Count || !bytes.Equal(repeated.Envelope, step.Envelope) ||
			len(repeated.Events) != 0 || len(input.reads) != result.reads {
			test.Fatal("completed paged normalization changed on repeat or reread source", err)
		}
		return result
	}
	test.Fatal("public paged parity exhausted its finite normalization allowance")
	return result
}

func legacyNativePagedParityProfiles(test *testing.T, raw []byte, accepted bool, children int) {
	test.Helper()
	var baseline legacyNativePagedParityResult
	for _, profile := range []struct {
		name      string
		partBytes int
	}{
		{name: "source-64KiB", partBytes: LegacyNormalizationPartBytes},
		{name: "source-default-512KiB"},
	} {
		test.Run(profile.name, func(test *testing.T) {
			result := legacyNativePagedParityRun(test, raw, profile.partBytes, accepted)
			if result.children != children {
				test.Fatal("profile did not independently observe all typed paged children", result.children, children)
			}
			if profile.partBytes != 0 {
				baseline = result
			} else if !bytes.Equal(result.canonical, baseline.canonical) || result.emitted != baseline.emitted {
				test.Fatal("64 KiB and default 512 KiB public paged paths disagree")
			}
		})
	}
}

func TestLegacyNativePagedParityPublicSmallAndTypedInternalDefault(test *testing.T) {
	native, err := newLegacyNormalizationNativeEvent()
	if err != nil || native == nil || native.object.pagedEvents {
		test.Fatal("direct native constructor no longer preserves its old typed internal consumer", err)
	}
	raw := []byte("{\"events\":[{\"kind\":\"turn\",\"seq\":9,\"role\":\"user\"},{\"kind\":\"compaction\",\"seq\":-2,\"replacement\":[{\"kind\":\"message\",\"seq\":3,\"role\":\"assistant\",\"blocks\":[{\"type\":\"text\",\"text\":\"raw <&> \ud55c🙂\\n\"}]},{\"kind\":\"turn\",\"seq\":-4,\"role\":\"developer\"}],\"replacement_complete\":true},{\"kind\":\"turn\",\"seq\":9,\"role\":\"system\"}],\"envelope\":{\"cir_version\":\"2\",\"source_provider\":\"codex\",\"fidelity\":\"full\"}}")
	legacyNativePagedParityProfiles(test, raw, true, 2)
}

func TestLegacyNativePagedParityPublic8192Replacement(test *testing.T) {
	const count = 8192
	var raw strings.Builder
	raw.WriteString(`{"envelope":{"cir_version":"2","source_model":"paged-byte-oracle","fidelity":"full"},"events":[{"kind":"compaction","seq":17,"replacement":[`)
	for ordinal := 0; ordinal < count; ordinal++ {
		if ordinal != 0 {
			raw.WriteByte(',')
		}
		fmt.Fprintf(&raw, "{\"kind\":\"message\",\"id\":\"child-%04d\",\"seq\":%d,\"role\":\"assistant\",\"blocks\":[{\"type\":\"text\",\"text\":\"original \ud55c🙂 <&>\\n\"}]}", ordinal, (count-ordinal)%257-128)
	}
	raw.WriteString(`],"replacement_complete":false}]}`)
	if raw.Len() <= ConversationManifestChunkBytes || raw.Len() > 2<<20 {
		test.Fatal("8192-child byte fixture lost its original multi-part/2 MiB boundaries", raw.Len())
	}
	legacyNativePagedParityProfiles(test, []byte(raw.String()), true, count)
}

func TestLegacyNativePagedParityPublicNestedUnionFidelity(test *testing.T) {
	const children = "{\"kind\":\"message\",\"seq\":8,\"agent_message\":true,\"agent_author\":\"\",\"agent_recipient\":\"worker\",\"role\":\"assistant\",\"blocks\":[{\"type\":\"text\",\"text\":\"quote \\\" slash \\\\ \\ud800 \\u2028\\u2029 <&> \ud55c🙂\"}],\"locked\":{\"provider\":\"codex\",\"scheme\":\"encrypted_content\",\"blob\":\"ENC<&>\"},\"provider_metadata\":{\"turn_id\":\"turn\",\"create_time\":1787683260.123456789}}," +
		`{"kind":"tool_call","seq":-3,"call_id":"call","tool_name":"read","provider_tool_name":"Read","input":{"z":-0,"n":9007199254740993,"a":[null,true,{"z":"<&>","a":1e-7}]},"status":"done"},` +
		"{\"kind\":\"tool_result\",\"seq\":-3,\"call_id\":\"call\",\"output\":[{\"z\":1e21,\"a\":false},\"\ud55c🙂\"],\"is_error\":true}," +
		`{"kind":"reasoning","seq":2,"locked":{"provider":"claude","scheme":"signature","blob":"SIG"},"redacted_summary":"summary","cross_replayable":true},` +
		`{"kind":"compaction","seq":1,"replacement":[{"kind":"message","seq":0,"blocks":null,"agent_message":false},{"kind":"turn","seq":-1,"role":"developer","provider_metadata":{}}],"replacement_complete":true},` +
		`{"kind":"compaction","seq":0,"replacement":null,"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}},` +
		`{"kind":"compaction","seq":-1,"replacement":[],"replacement_complete":false}`
	raw := []byte(`{"envelope":{"cir_version":"2","source_provider":"codex","source_models":["first","second"],"fidelity":"full"},"events":[{"kind":"compaction","seq":4,"replacement":[` + children + `],"replacement_complete":true}]}`)
	legacyNativePagedParityProfiles(test, raw, true, 9)
}

func TestLegacyNativePagedParityPublicVersionAndUnionGates(test *testing.T) {
	for _, fixture := range []struct {
		name     string
		version  string
		event    string
		accepted bool
		children int
	}{
		{name: "legacy-v1-turn", version: "1", event: `{"kind":"turn","seq":7,"role":"user"}`, accepted: true},
		{name: "v2-empty-paged-boundary", version: "2", event: `{"kind":"compaction","replacement":[]}`, accepted: true},
		{name: "v1-rejects-empty-paged-boundary", version: "1", event: `{"kind":"compaction","replacement":[]}`},
		{name: "implicit-v1-rejects-paged-boundary", event: `{"kind":"compaction","replacement":[{"kind":"turn","seq":-1}]}`, children: 1},
		{name: "unsupported-version", version: "99", event: `{"kind":"compaction","replacement":[]}`},
		{name: "v1-rejects-exact-present-false", version: "1", event: `{"kind":"message","agent_message":false,"blocks":[]}`},
		{name: "replacement-and-locked-rejected", version: "2", event: `{"kind":"compaction","replacement":[],"locked":{"provider":"codex","scheme":"encrypted_content","blob":"opaque"}}`},
		{name: "wrong-union-exact-replacement", version: "2", event: `{"kind":"turn","replacement":[]}`},
		{name: "wrong-union-folded-replacement", version: "2", event: `{"kind":"turn","REPLACEMENT":[]}`},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			raw := []byte(`{"envelope":{"cir_version":"` + fixture.version + `"},"events":[` + fixture.event + `]}`)
			legacyNativePagedParityProfiles(test, raw, fixture.accepted, fixture.children)
		})
	}
}
