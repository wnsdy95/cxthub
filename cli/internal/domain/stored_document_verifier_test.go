package domain

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"
)

func storedVerifierCanonical(t testing.TB, version string, events ...Event) []byte {
	t.Helper()
	raw, err := CanonicalBytes(CIRDocument{Envelope: Envelope{CIRVersion: version}, Events: events})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func storedVerifierEnvelope(t testing.TB, version string) string {
	t.Helper()
	var doc struct{ Envelope json.RawMessage }
	if err := json.Unmarshal(storedVerifierCanonical(t, version), &doc); err != nil {
		t.Fatal(err)
	}
	return string(doc.Envelope)
}

type storedVerifierProofState struct {
	events     map[storedEventKey]int
	order      []storedEventKey
	next       int
	firstBytes int
}

func storedVerifierProofSnapshot(v *StoredDocumentVerifier) storedVerifierProofState {
	v.mu.Lock()
	defer v.mu.Unlock()
	return storedVerifierProofState{maps.Clone(v.events), slices.Clone(v.order), v.next, v.firstBytes}
}
func storedVerifierProofEqual(a, b storedVerifierProofState) bool {
	return maps.Equal(a.events, b.events) && slices.Equal(a.order, b.order) && a.next == b.next && a.firstBytes == b.firstBytes
}
