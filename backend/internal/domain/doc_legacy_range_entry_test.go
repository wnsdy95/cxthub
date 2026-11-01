package domain

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func legacyRangeEntryPlan(test testing.TB, events []CIREvent) (LegacyByteBackfill, [][]byte) {
	test.Helper()
	proof, _ := legacyDocByteBackfillDocumentFixture(test, CIRDocument{
		Envelope: CIREnvelope{CIRVersion: "2", SourceProvider: ProviderCodex, Fidelity: FidelityFull}, Events: events,
	})
	plan, err := NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		test.Fatal(err)
	}
	plan, err = plan.Spool(context.Background(), &legacyTestStream{})
	if err != nil {
		test.Fatal(err)
	}
	stream, err := proof.EventStreamRange(context.Background(), 0, int(plan.spec.StreamBytes))
	if err != nil {
		test.Fatal(err)
	}
	var bodies []json.RawMessage
	if err := json.Unmarshal(append(append([]byte{'['}, stream...), ']'), &bodies); err != nil {
		test.Fatal(err)
	}
	result := make([][]byte, len(bodies))
	for ordinal := range bodies {
		result[ordinal] = bodies[ordinal]
	}
	return plan, result
}

func TestLegacyRangeEntryMatchesOriginalMetadataDecoder(test *testing.T) {
	events := []CIREvent{
		{Kind: EventMessage, Seq: -1, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "\ufeff [cxt seed] Branch-switch context: main → work"}}},
		{Kind: EventMessage, Seq: 0, Role: RoleAssistant, Blocks: []ContentBlock{{Type: "text", Text: strings.Repeat("x", 1<<20)}}},
		{Kind: EventReasoning, Seq: 0, RedactedSummary: " \ufeff\t\n", Locked: &LockedBlob{}},
		{Kind: EventReasoning, Seq: 1, RedactedSummary: strings.Repeat("\u2003", 70<<10) + "visible"},
		{Kind: EventToolCall, Seq: 2, ToolName: strings.Repeat("\ud55c🙂\"\\", 65), Input: map[string]any{"ignored": strings.Repeat("x", 1<<20)}},
		{Kind: EventToolResult, Seq: 3, Output: map[string]any{"nested": []any{strings.Repeat("x", 1<<20), nil, true}}},
		{Kind: EventCompaction, Seq: 4, Replacement: []CIREvent{{Kind: EventMessage, Role: RoleUser, Seq: 0}}, ReplacementComplete: true},
		{Kind: EventMessage, Seq: 5, Role: RoleUser, CompactSummary: true},
		{Kind: EventTurn, Seq: 6, Role: RoleDeveloper},
		{Kind: EventMessage, Seq: 7, Role: Role(strings.Repeat("\ud55c🙂\n\"\\", 65)), Blocks: []ContentBlock{}},
		{Kind: EventMessage, Seq: 8, Role: RoleAssistant, AgentMessage: true, AgentAuthor: "worker"},
		{Kind: EventReasoning, Seq: 9, RedactedSummary: ""},
		{Kind: EventMessage, Seq: 10, Role: RoleSystem, Blocks: []ContentBlock{{Type: "text", Text: " \u2003[cxt] This session was resumed from a branch context seed. tail"}}},
		{Kind: EventMessage, Seq: 11, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: ""}, {Type: "text", Text: "not seed"}}},
		{Kind: EventCompaction, Seq: 12, Replacement: []CIREvent{}, ReplacementComplete: true},
	}
	for _, candidate := range canonicalEventCases() {
		event, err := decodeCanonicalEvent(candidate.raw)
		if err != nil {
			continue
		}
		event.Seq = len(events)
		document := CIRDocument{Envelope: CIREnvelope{CIRVersion: "2"}, Events: []CIREvent{event}}
		_, encodingError := CanonicalBytes(document)
		if encodingError == nil && ValidateCIRVersion(document) == nil {
			events = append(events, event)
		}
	}
	plan, bodies := legacyRangeEntryPlan(test, events)
	if len(bodies) < 40 {
		test.Fatal("canonical metadata corpus lost its valid field cases", len(bodies))
	}
	offset := 0
	for ordinal, body := range bodies {
		want, wantError := docRangeEntryWithSequence(ordinal, offset, body, 0, false)
		got, gotError := plan.rangeEntry(context.Background(), ordinal, offset, body)
		if wantError != nil || gotError != nil || got != want {
			test.Fatalf("entry %d differs: got=%+v (%v) want=%+v (%v)", ordinal, got, gotError, want, wantError)
		}
		offset += len(body) + 1
		clear(body)
		if got != want {
			test.Fatalf("entry %d retained borrowed event bytes", ordinal)
		}
	}
}

func TestLegacyRangeEntryRequiresSourceAuthorityAndChecksCancellation(test *testing.T) {
	plan, bodies := legacyRangeEntryPlan(test, []CIREvent{{Kind: EventToolResult, Seq: 1, Output: strings.Repeat("x", 2<<20)}})
	var serialized LegacyByteBackfill
	raw, err := json.Marshal(plan)
	if err != nil || json.Unmarshal(raw, &serialized) != nil {
		test.Fatal("plan serialization setup", err)
	}
	withoutSource := plan
	withoutSource.source = nil
	for _, untrusted := range []LegacyByteBackfill{{}, serialized, withoutSource} {
		if got, err := untrusted.rangeEntry(context.Background(), 0, 0, bodies[0]); !errors.Is(err, ErrIntegrity) || got != (DocRangeEntry{}) {
			test.Fatal("unproven source produced metadata", got, err)
		}
	}
	bound, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	ctx := canonicalAdmissionContext{Context: bound, check: func() {
		checks++
		if checks == 12 {
			cancel()
		}
	}}
	got, err := plan.rangeEntry(ctx, 0, 0, bodies[0])
	if !errors.Is(err, context.Canceled) || got != (DocRangeEntry{}) || checks != 12 {
		test.Fatal("giant skipped value did not observe in-traversal cancellation", got, err, checks)
	}
}

func TestDocRangeMetadataDecoderPreservesAnonymousTypeErrors(test *testing.T) {
	for _, raw := range []string{`{"kind":4}`, `{"tool_name":false}`, `{"compact_summary":"yes"}`} {
		var metadata struct {
			Seq             docRangeJSONView `json:"seq"`
			Kind            EventKind        `json:"kind"`
			Role            string           `json:"role"`
			ToolName        string           `json:"tool_name"`
			CompactSummary  bool             `json:"compact_summary"`
			Blocks          docRangeJSONView `json:"blocks"`
			RedactedSummary docRangeJSONView `json:"redacted_summary"`
		}
		want := json.Unmarshal([]byte(raw), &metadata)
		_, got := docRangeEntryWithSequence(0, 0, []byte(raw), 0, false)
		if want == nil || got == nil || !reflect.DeepEqual(got, want) || got.Error() != want.Error() {
			test.Fatalf("metadata error changed: got=%#v want=%#v", got, want)
		}
	}
}
