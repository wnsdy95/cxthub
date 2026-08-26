package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEffectiveContextUsesLatestBoundaryAndPreservesArchive(t *testing.T) {
	message := func(seq int, text string) Event {
		return Event{Kind: EventMessage, Seq: seq, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: text}}}
	}
	doc := CIRDocument{Events: []Event{
		message(0, "archive-a"),
		{Kind: EventCompaction, Seq: 1, Replacement: []Event{message(0, "replacement-one")}, ReplacementComplete: true},
		message(2, "archive-b"),
		{Kind: EventCompaction, Seq: 3, Replacement: []Event{message(0, "replacement-two")}, ReplacementComplete: true},
		message(4, "after-latest"),
	}}

	active := doc.EffectiveContext()
	if !doc.HasCompleteCompactionBoundary() {
		t.Fatal("latest complete boundary was not reported as authoritative")
	}
	if len(doc.Events) != 5 || doc.Events[0].Blocks[0].Text != "archive-a" {
		t.Fatal("effective projection mutated archival events")
	}
	if len(active.Events) != 2 || active.Events[0].Blocks[0].Text != "replacement-two" || active.Events[1].Blocks[0].Text != "after-latest" {
		t.Fatalf("effective context = %+v", active.Events)
	}
}

func TestEffectiveContextFallsBackToArchiveWhenLatestBoundaryIsIncomplete(t *testing.T) {
	message := func(seq int, text string) Event {
		return Event{Kind: EventMessage, Seq: seq, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: text}}}
	}
	doc := CIRDocument{Events: []Event{
		message(0, "archive-a"),
		{Kind: EventCompaction, Seq: 1, Replacement: []Event{message(0, "known-old")}, ReplacementComplete: true},
		message(2, "archive-b"),
		{Kind: EventCompaction, Seq: 3, Replacement: []Event{message(0, "partial-new")}, ReplacementComplete: false},
		message(4, "archive-c"),
	}}

	active := doc.EffectiveContext()
	if doc.HasCompleteCompactionBoundary() {
		t.Fatal("incomplete latest boundary was reported as authoritative")
	}
	if len(active.Events) != len(doc.Events) || active.Events[0].Blocks[0].Text != "archive-a" {
		t.Fatalf("incomplete latest boundary did not fall back to archive: %+v", active.Events)
	}
}

func TestCompactionEmptyReplacementSurvivesWireAndCanonicalSorting(t *testing.T) {
	doc := CIRDocument{
		Envelope: Envelope{CIRVersion: CIRVersionV2},
		Events: []Event{{
			Kind: EventCompaction, Seq: 2,
			ReplacementComplete: true,
			Replacement: []Event{
				{Kind: EventMessage, Seq: 9, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: "later"}}},
				{Kind: EventMessage, Seq: 3, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: "earlier"}}},
			},
		}},
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(canonical), "earlier") > strings.Index(string(canonical), "later") {
		t.Fatalf("nested replacement was not canonically sorted: %s", canonical)
	}

	empty := Event{Kind: EventCompaction, Replacement: []Event{}, ReplacementComplete: true}
	raw, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"replacement":[]`) {
		t.Fatalf("empty boundary replacement omitted: %s", raw)
	}
	if !strings.Contains(string(raw), `"replacement_complete":true`) {
		t.Fatalf("boundary completeness omitted: %s", raw)
	}
	var restored Event
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Replacement == nil {
		t.Fatal("empty replacement became nil and lost boundary identity")
	}

	incomplete := Event{Kind: EventCompaction, Replacement: []Event{}, ReplacementComplete: false}
	raw, err = json.Marshal(incomplete)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"replacement_complete":false`) {
		t.Fatalf("incomplete boundary silently omitted fail-safe bit: %s", raw)
	}
}

func TestAgentMessageLockedStateCanonicalWire(t *testing.T) {
	doc := CIRDocument{Envelope: Envelope{CIRVersion: CIRVersionV2}, Events: []Event{{
		Kind: EventMessage, Seq: 0, Role: "assistant",
		Blocks:       []ContentBlock{{Type: "text", Text: "review result"}},
		AgentMessage: true, AgentAuthor: "/root/reviewer", AgentRecipient: "/root",
		Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "ENC"},
	}}}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"agent_message":true`, `"agent_author":"/root/reviewer"`, `"agent_recipient":"/root"`, `"blob":"ENC"`} {
		if !strings.Contains(string(canonical), want) {
			t.Fatalf("agent message canonical bytes missing %s: %s", want, canonical)
		}
	}
	var restored CIRDocument
	if err := json.Unmarshal(canonical, &restored); err != nil {
		t.Fatal(err)
	}
	if len(restored.Events) != 1 || !restored.Events[0].AgentMessage || restored.Events[0].Locked == nil || restored.Events[0].Locked.Blob != "ENC" {
		t.Fatalf("agent message round trip = %+v", restored.Events)
	}
}

func TestProviderMetadataCreateTimeCanonicalPrecision(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"blocks":[],"kind":"message","provider_metadata":{"create_time":1787683260.123456789,"turn_id":"turn-1"},"role":"user","seq":0}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Events[0].ProviderMetadata == nil || doc.Events[0].ProviderMetadata.CreateTime == nil ||
		doc.Events[0].ProviderMetadata.CreateTime.String() != "1787683260.123456789" {
		t.Fatalf("CLI rounded provider create_time: %+v", doc.Events[0].ProviderMetadata)
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatalf("CLI provider metadata canonical mismatch:\n got: %s\nwant: %s", canonical, raw)
	}
}

func TestCIRV1RejectsV2EventShape(t *testing.T) {
	doc := CIRDocument{Envelope: Envelope{CIRVersion: CIRVersionV1}, Events: []Event{{
		Kind: EventCompaction, Replacement: []Event{}, ReplacementComplete: true,
	}}}
	if _, err := CanonicalBytes(doc); !strings.Contains(fmt.Sprint(err), "require cir_version") {
		t.Fatalf("v1 compaction error = %v", err)
	}
}

func TestCIRV1RejectsProviderMetadata(t *testing.T) {
	doc := CIRDocument{Envelope: Envelope{CIRVersion: CIRVersionV1}, Events: []Event{{
		Kind: EventMessage, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: "hello"}},
		ProviderMetadata: &ProviderMetadata{TurnID: "turn-1"},
	}}}
	if _, err := CanonicalBytes(doc); !strings.Contains(fmt.Sprint(err), "require cir_version") {
		t.Fatalf("v1 provider metadata error = %v", err)
	}
}

func TestCIRV1RejectsPresentZeroValuedV2Fields(t *testing.T) {
	base := `{"envelope":{"captured_at":"","cir_version":"1","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[%s]}`
	cases := map[string]string{
		"agent false":     `{"agent_message":false,"blocks":[],"kind":"message","role":"user","seq":0}`,
		"empty author":    `{"agent_author":"","blocks":[],"kind":"message","role":"assistant","seq":0}`,
		"empty recipient": `{"agent_recipient":"","blocks":[],"kind":"message","role":"assistant","seq":0}`,
	}
	for name, event := range cases {
		t.Run(name, func(t *testing.T) {
			var doc CIRDocument
			if err := json.Unmarshal([]byte(fmt.Sprintf(base, event)), &doc); err != nil {
				t.Fatal(err)
			}
			if _, err := CanonicalBytes(doc); !strings.Contains(fmt.Sprint(err), "require cir_version") {
				t.Fatalf("present v2 field under v1 error = %v", err)
			}
		})
	}
}

func TestMessageRejectsCompactionFields(t *testing.T) {
	var doc CIRDocument
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"blocks":[],"kind":"message","replacement":[],"replacement_complete":true,"role":"user","seq":0}]}`)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalBytes(doc); !strings.Contains(fmt.Sprint(err), "replacement fields require compaction") {
		t.Fatalf("message replacement error = %v", err)
	}
}

func TestCompactionBoundaryAndLockedStateAreMutuallyExclusive(t *testing.T) {
	_, err := json.Marshal(Event{
		Kind: EventCompaction, Replacement: []Event{}, ReplacementComplete: true,
		Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "ENC"},
	})
	if err == nil {
		t.Fatal("ambiguous compaction boundary plus locked state was accepted")
	}
}
