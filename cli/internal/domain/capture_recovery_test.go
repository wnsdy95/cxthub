package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Adding frozen-input metadata must not change v1 fingerprints used by existing
// acknowledgements and protected incomplete capture records.
func TestCaptureAttemptV1WireAndFingerprintStayStable(t *testing.T) {
	type oldOutcome struct {
		Provider    string      `json:"provider"`
		State       string      `json:"state"`
		SessionPath string      `json:"session_path,omitempty"`
		Target      ContentHash `json:"target,omitempty"`
		Error       string      `json:"error,omitempty"`
	}
	old := struct {
		Version     int           `json:"version"`
		Proof       HistoryEvent  `json:"proof"`
		Initial     ContentHash   `json:"initial,omitempty"`
		Outcomes    []oldOutcome  `json:"outcomes"`
		Observation *HistoryEvent `json:"observation,omitempty"`
		Complete    bool          `json:"complete"`
	}{Version: 1, Outcomes: []oldOutcome{{Provider: ProviderCodex, State: "pending"}, {Provider: ProviderClaude, State: "absent", Error: "no active session"}}}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var current CaptureAttempt
	if err := json.Unmarshal(raw, &current); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(current)
	if err != nil || !bytes.Equal(raw, encoded) || current.Fingerprint() != HashContent(raw) {
		t.Fatalf("v1 receipt changed: %s => %s (%v)", raw, encoded, err)
	}
}
