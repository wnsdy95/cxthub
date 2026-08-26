package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestCanonicalCompactionReplacementPreservesBoundaryShape(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"kind":"compaction","replacement":[{"blocks":[{"text":"later","type":"text"}],"kind":"message","role":"user","seq":9},{"blocks":[{"text":"earlier","type":"text"}],"kind":"message","role":"user","seq":3}],"replacement_complete":true,"seq":2}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Events[0].Replacement == nil {
		t.Fatal("backend lost compaction boundary replacement")
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(string(canonical), "earlier") > strings.Index(string(canonical), "later") {
		t.Fatalf("backend did not sort nested replacement: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"replacement"`) {
		t.Fatalf("backend canonical bytes dropped replacement: %s", canonical)
	}
	if !strings.Contains(string(canonical), `"replacement_complete":true`) {
		t.Fatalf("backend canonical bytes dropped replacement completeness: %s", canonical)
	}
}

func TestCanonicalCompactionPreservesIncompleteFailSafe(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"kind":"compaction","replacement":[],"replacement_complete":false,"seq":0}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatalf("backend changed incomplete boundary:\n got: %s\nwant: %s", canonical, raw)
	}
}

func TestCanonicalAgentMessagePreservesLockedState(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"agent_author":"/root/reviewer","agent_message":true,"agent_recipient":"/root","blocks":[{"text":"review result","type":"text"}],"kind":"message","locked":{"blob":"ENC","provider":"codex","scheme":"encrypted_content"},"role":"assistant","seq":0}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Events) != 1 || !doc.Events[0].AgentMessage || doc.Events[0].Locked == nil {
		t.Fatalf("backend agent event decode = %+v", doc.Events)
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatalf("backend agent canonical mismatch:\n got: %s\nwant: %s", canonical, raw)
	}
}

func TestCanonicalProviderMetadataRoundTrip(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"blocks":[{"text":"hello","type":"text"}],"id":"msg-1","kind":"message","provider_metadata":{"create_time":1787683260.123456789,"turn_id":"turn-1"},"role":"user","seq":0}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Events) != 1 || doc.Events[0].ProviderMetadata == nil || doc.Events[0].ProviderMetadata.TurnID != "turn-1" {
		t.Fatalf("backend provider metadata decode = %+v", doc.Events)
	}
	if doc.Events[0].ProviderMetadata.CreateTime == nil || doc.Events[0].ProviderMetadata.CreateTime.String() != "1787683260.123456789" {
		t.Fatalf("backend rounded provider create_time: %+v", doc.Events[0].ProviderMetadata)
	}
	canonical, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(canonical) != string(raw) {
		t.Fatalf("backend provider metadata canonical mismatch:\n got: %s\nwant: %s", canonical, raw)
	}
}

func TestBackendCIRV1RejectsPresentZeroValuedV2Fields(t *testing.T) {
	base := `{"envelope":{"captured_at":"","cir_version":"1","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[%s]}`
	cases := map[string]string{
		"agent false":  `{"agent_message":false,"blocks":[],"kind":"message","role":"user","seq":0}`,
		"empty author": `{"agent_author":"","blocks":[],"kind":"message","role":"assistant","seq":0}`,
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

func TestBackendMessageRejectsCompactionFields(t *testing.T) {
	var doc CIRDocument
	raw := []byte(`{"envelope":{"captured_at":"","cir_version":"2","cwd":"","fidelity":"","git_branch":"","session_origin_id":"","source_model":"","source_provider":""},"events":[{"blocks":[],"kind":"message","replacement":[],"replacement_complete":true,"role":"user","seq":0}]}`)
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalBytes(doc); !strings.Contains(fmt.Sprint(err), "replacement fields require compaction") {
		t.Fatalf("message replacement error = %v", err)
	}
}

func TestCanonicalCompactionRejectsBoundaryWithLockedState(t *testing.T) {
	doc := CIRDocument{Events: []CIREvent{{
		Kind: EventCompaction, Replacement: []CIREvent{}, ReplacementComplete: true,
		Locked: &LockedBlob{Provider: ProviderCodex, Scheme: "encrypted_content", Blob: "ENC"},
	}}}
	if _, err := CanonicalBytes(doc); err == nil {
		t.Fatal("backend accepted ambiguous compaction boundary plus locked state")
	}
}
