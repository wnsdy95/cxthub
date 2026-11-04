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
	"testing"
	"unsafe"
)

func legacyNativeBlockPagedParityValue(test *testing.T, event CIREvent, blocks *legacyNativeBlockValues, replacement *legacyNativeEventValues, expected CIREvent, visible bool, projected map[*legacyNativeBlockValues]bool) {
	test.Helper()
	expectedBlocks, expectedChildren := expected.Blocks, expected.Replacement
	expected.Blocks, expected.Replacement = nil, nil
	if event.Blocks != nil || event.Replacement != nil || !reflect.DeepEqual(event, expected) {
		test.Fatalf("public block owner changed typed fields/presence or materialized arrays: id=%q", expected.ID)
	}
	if (blocks == nil) != (expectedBlocks == nil) {
		test.Fatal("public block owner changed typed null/array presence", expected.ID)
	}
	if blocks != nil {
		if blocks.length() != len(expectedBlocks) || !blocks.records.valid() {
			test.Fatal("public block owner lost its actual paged values", expected.ID, blocks.length(), len(expectedBlocks))
		}
		if len(expectedBlocks) == 0 && blocks.records != (legacyCanonicalPages[legacyNativeBlockValue]{}) {
			test.Fatal("empty public blocks retained single/inline/page storage", expected.ID)
		}
		if len(expectedBlocks) == 1 && (blocks.records.single == nil || blocks.records.inline != nil ||
			blocks.records.root != nil || blocks.at(0) != blocks.records.single) {
			test.Fatal("one public block did not retain exactly its actual single value slot", expected.ID)
		}
		if len(expectedBlocks) >= 2 && len(expectedBlocks) <= 8 &&
			(blocks.records.single != nil || blocks.records.inline == nil || blocks.records.root != nil ||
				blocks.at(0) != &blocks.records.inline[0]) {
			test.Fatal("two through eight public blocks bypassed the existing inline page", expected.ID)
		}
		if len(expectedBlocks) == 9 && (blocks.records.single != nil || blocks.records.inline != nil ||
			blocks.records.root == nil || blocks.records.root.values == nil || blocks.records.nodes != 1 ||
			blocks.records.pages != 1 || blocks.records.depth != 0 || blocks.at(8) != &blocks.records.root.values[8]) {
			test.Fatal("nine public blocks did not retain exactly one actual 64-value page", expected.ID)
		}
		if len(expectedBlocks) == 8192 && (blocks.records.single != nil || blocks.records.inline != nil || blocks.records.root == nil || blocks.records.pages <= 1) {
			test.Fatal("8192 public blocks did not reach actual fixed pages", expected.ID)
		}
		pageHeap := legacyCanonicalIndependentPageHeap(test, &blocks.records)
		var backing docRangeHeaderHeapEstimate
		backing.allocation(1, int64(unsafe.Sizeof(*blocks)))
		backing.include(pageHeap)
		owned, ownedErr := blocks.records.ownedBytes()
		actualBacking := blocks.backing()
		if backing.err != nil || ownedErr != nil || owned != pageHeap.bytes || actualBacking.err != nil || actualBacking.bytes != backing.bytes {
			test.Fatal("public block backing differs from actual single/inline/page allocation sizes", expected.ID, owned, pageHeap, actualBacking, backing)
		}
		for ordinal, expectedBlock := range expectedBlocks {
			value := blocks.at(ordinal)
			if value == nil || value.block != expectedBlock {
				test.Fatal("public block page differs from independent typed content", expected.ID, ordinal)
			}
		}
		projected[blocks] = visible && expected.Kind == EventMessage
	}
	if (replacement == nil) != (expectedChildren == nil) {
		test.Fatal("public block owner changed replacement presence", expected.ID)
	}
	if replacement == nil {
		return
	}
	if replacement.length() != len(expectedChildren) {
		test.Fatal("public block owner lost nested events", expected.ID)
	}
	for ordinal, child := range expectedChildren {
		value := replacement.at(ordinal)
		if value == nil {
			test.Fatal("public block owner omitted a nested value", expected.ID, ordinal)
		}
		legacyNativeBlockPagedParityValue(test, value.event, value.blocks, value.replacement, child, visible && expected.Kind == EventCompaction, projected)
	}
}

func legacyNativeBlockPagedParityProjection(test *testing.T, encoding *LegacyCanonicalEncoding, projected map[*legacyNativeBlockValues]bool) {
	test.Helper()
	for steps := 0; encoding.preparation != nil && steps < 1<<16; steps++ {
		chunk, complete, err := encoding.Next(context.Background())
		if err != nil || complete || len(chunk) != 0 {
			test.Fatal("public block projection failed before canonical emission", complete, len(chunk), err)
		}
	}
	if encoding.preparation != nil {
		test.Fatal("public block projection did not finish its finite preparation")
	}
	seen := make(map[*legacyNativeBlockValues]bool)
	for mapIndex := 0; mapIndex < encoding.mapKeys.length(); mapIndex++ {
		keys := encoding.mapKeys.at(mapIndex)
		if keys == nil {
			test.Fatal("public block projection lost prepared map fields", mapIndex)
		}
		for fieldIndex := 0; fieldIndex < keys.fields.length(); fieldIndex++ {
			field := keys.fields.at(fieldIndex)
			if field == nil {
				test.Fatal("public block projection lost a prepared field", mapIndex, fieldIndex)
			}
			blocks, paged := field.value.(*legacyNativeBlockValues)
			if !paged {
				continue
			}
			if field.key != "blocks" || !projected[blocks] || seen[blocks] {
				test.Fatal("canonical projection detached, duplicated, or revived union-dropped block pages", field.key)
			}
			seen[blocks] = true
		}
	}
	for blocks, emitted := range projected {
		if seen[blocks] != emitted {
			test.Fatal("canonical projection did not preserve actual owner page identity and union visibility", blocks.length(), emitted)
		}
	}
}

func legacyNativeBlockPagedParityRun(test *testing.T, raw []byte, accepted bool) {
	test.Helper()
	want, wantErr := legacyNormalizationOracle(raw)
	if (wantErr == nil) != accepted {
		test.Fatal("public block parity fixture changed independent typed acceptance", accepted, wantErr)
	}
	var typed CIRDocument
	if err := json.Unmarshal(raw, &typed); err != nil {
		test.Fatal("public block parity fixture is not a typed JSON document", err)
	}
	normalizer, input := legacyNativeScalarReviewStart(test, bytes.Clone(raw))
	var events []struct {
		sequence int
		body     []byte
	}
	for stepIndex := 0; stepIndex < 4096; stepIndex++ {
		step, err := legacyNativeScalarReviewStep(test, context.Background(), normalizer, input)
		if err != nil {
			if accepted || !errors.Is(err, wantErr) || errors.Is(err, ErrPreparedDocRangeObject) || step.Complete {
				test.Fatal("public block parity changed typed rejection or exposed completion", err, wantErr)
			}
			legacyNativePagedParitySourceReads(test, input, ConversationManifestChunkBytes, raw)
			return
		}
		if step.Reset && len(events) != 0 {
			test.Fatal("single-array public block fixture unexpectedly reset emitted events")
		}
		for _, event := range step.Events {
			ordinal := len(events)
			if ordinal >= len(typed.Events) || event.Encoding == nil {
				test.Fatal("public block parity emitted an extra or unencoded event", ordinal)
			}
			expected := typed.Events[ordinal]
			encoding := event.Encoding
			owner := encoding.nativeOwner
			if owner == nil || encoding.preparation == nil || event.Sequence != expected.Seq {
				test.Fatal("public normalizer bypassed native block ownership/preparation or changed sequence", ordinal)
			}
			projected := make(map[*legacyNativeBlockValues]bool)
			legacyNativeBlockPagedParityValue(test, owner.event, owner.blocks, owner.replacement, expected, true, projected)
			wantEvent, err := canonicalJSON(canonicalEvents([]CIREvent{expected})[0])
			if err != nil {
				test.Fatal("public block parity emitted a typed-invalid event union", ordinal, err)
			}
			reads := len(input.reads)
			legacyNativeBlockPagedParityProjection(test, encoding, projected)
			body, _ := collectLegacyCanonicalEncoding(test, encoding)
			if len(input.reads) != reads || !bytes.Equal(body, wantEvent) || sha256.Sum256(body) != sha256.Sum256(wantEvent) ||
				HashContent(body) != HashContent(wantEvent) {
				test.Fatal("public paged blocks changed exact event bytes/SHA or reread source", ordinal, len(body), len(wantEvent))
			}
			events = append(events, struct {
				sequence int
				body     []byte
			}{sequence: event.Sequence, body: body})
		}
		if !step.Complete {
			if step.Envelope != nil || step.Count != 0 {
				test.Fatal("unfinished public block parity exposed a completed envelope/count")
			}
			continue
		}
		if !accepted || step.Count != len(typed.Events) || len(events) != step.Count {
			test.Fatal("public block parity completed a rejected or incomplete document", step.Count, len(events), wantErr)
		}
		sort.SliceStable(events, func(left, right int) bool { return events[left].sequence < events[right].sequence })
		bodies := make([][]byte, len(events))
		for ordinal, event := range events {
			bodies[ordinal] = event.body
		}
		canonical := AssembleCanonicalDoc(step.Envelope, bodies)
		if !bytes.Equal(canonical, want) || sha256.Sum256(canonical) != sha256.Sum256(want) || HashContent(canonical) != HashContent(want) {
			test.Fatal("public paged blocks changed typed document bytes/SHA, envelope, or stable sequence order", len(canonical), len(want))
		}
		legacyNativePagedParitySourceReads(test, input, ConversationManifestChunkBytes, raw)
		return
	}
	test.Fatal("public block parity exhausted its finite normalization allowance")
}

func TestLegacyNativeBlockPagedParityPublic8And8192FlatNested(test *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, count := range []int{8, 8192} {
			test.Run(fmt.Sprintf("nested=%t/blocks=%d", nested, count), func(test *testing.T) {
				raw := legacyCanonicalBlocksPublicDocument(test, count, nested)
				if len(raw) <= ConversationManifestChunkBytes || len(raw) > 2<<20 {
					test.Fatal("public block fixture lost its original multipart/2 MiB boundaries", len(raw))
				}
				legacyNativeBlockPagedParityRun(test, raw, true)
			})
		}
	}
}

func TestLegacyNativeBlockPagedParityPublicSingletonAndPromotionBoundaries(test *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, count := range []int{1, 2, 9} {
			test.Run(fmt.Sprintf("nested=%t/blocks=%d", nested, count), func(test *testing.T) {
				raw := legacyCanonicalBlocksPublicDocument(test, count, nested)
				if len(raw) <= ConversationManifestChunkBytes || len(raw) > 2<<20 {
					test.Fatal("public small-list fixture lost its original multipart/2 MiB boundaries", len(raw))
				}
				legacyNativeBlockPagedParityRun(test, raw, true)
			})
		}
	}
}

func TestLegacyNativeBlockPagedParityPublicEmptyNullUnionAndVersions(test *testing.T) {
	const blocks = "[{\"type\":\"text\",\"text\":\"head <&> \ud55c🙂\\n\"},{\"type\":\"opaque\",\"text\":\"quote \\\" slash \\\\\"},{\"text\":\"\\u0000\\t\\r\\b\\f\"},{\"type\":\"text\"},{\"type\":\"\",\"text\":\"\"},{\"type\":\"text\",\"text\":\"\\ud800\"},{\"type\":\"text\",\"text\":\"\\u2028\\u2029\"},{\"type\":\"text\",\"text\":\"tail\"}]"
	for _, fixture := range []struct {
		name     string
		version  string
		events   string
		accepted bool
	}{
		{name: "missing-blocks", version: "2", events: `{"kind":"message","seq":7,"role":"user"}`, accepted: true},
		{name: "null-blocks", version: "2", events: `{"kind":"message","seq":7,"role":"user","blocks":null}`, accepted: true},
		{name: "empty-blocks", version: "2", events: `{"kind":"message","seq":7,"role":"user","blocks":[]}`, accepted: true},
		{name: "nested-empty-null", version: "2", events: `{"kind":"compaction","replacement":[{"kind":"message","seq":4,"blocks":[]},{"kind":"compaction","replacement":[{"kind":"message","seq":-1,"blocks":null},{"kind":"message","seq":-1}]}]}`, accepted: true},
		{name: "implicit-v1-message", events: `{"kind":"message","blocks":` + blocks + `}`, accepted: true},
		{name: "v1-message", version: "1", events: `{"kind":"message","blocks":` + blocks + `}`, accepted: true},
		{name: "v2-message", version: "2", events: `{"kind":"message","blocks":` + blocks + `}`, accepted: true},
		{name: "v2-present-false", version: "2", events: `{"kind":"message","agent_message":false,"blocks":` + blocks + `}`, accepted: true},
		{name: "v1-rejects-present-false", version: "1", events: `{"kind":"message","agent_message":false,"blocks":` + blocks + `}`},
		{name: "unsupported-version", version: "99", events: `{"kind":"message","blocks":` + blocks + `}`},
		{name: "v1-rejects-nested-blocks", version: "1", events: `{"kind":"compaction","replacement":[{"kind":"message","blocks":` + blocks + `}]}`},
		{name: "wrong-union-agent-presence", version: "2", events: `{"kind":"turn","agent_message":false,"blocks":` + blocks + `}`},
		{
			name: "non-message-blocks-stay-dropped", version: "2", accepted: true,
			events: `{"kind":"turn","seq":3,"role":"user","blocks":` + blocks + `},` +
				`{"kind":"tool_call","seq":-1,"input":{"z":1,"a":"<&>"},"blocks":` + blocks + `},` +
				"{\"kind\":\"tool_result\",\"seq\":-1,\"output\":[true,\"\ud55c🙂\"],\"blocks\":" + blocks + `},` +
				`{"kind":"reasoning","seq":0,"redacted_summary":"summary","blocks":` + blocks + `},` +
				`{"kind":"compaction","seq":2,"blocks":` + blocks + `,"replacement":[{"kind":"message","seq":1,"blocks":` + blocks + `},{"kind":"compaction","seq":-1,"blocks":` + blocks + `,"replacement":[{"kind":"message","seq":0,"blocks":[]}]}]}`,
		},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			legacyNativeBlockPagedParityRun(test, legacyNativeEventDocument(fixture.version, fixture.events), fixture.accepted)
		})
	}
}
