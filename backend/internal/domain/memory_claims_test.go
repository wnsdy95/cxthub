package domain

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestTypedMemoryWirePreservesSourceClaims(t *testing.T) {
	raw, err := os.ReadFile("../../../schemas/testdata/memory-claims-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var digest MemoryDigest
	if err := json.Unmarshal(raw, &digest); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, bytes.TrimSpace(raw)) {
		t.Fatalf("memory claims lost during round trip:\n%s", encoded)
	}
}

func TestMemoryDigestWithoutClaimsKeepsItsEncoding(t *testing.T) {
	legacy := MemoryDigest{SnapshotID: HashContent([]byte("legacy")), Summary: "stable", Provider: ProviderCodex}
	raw, _ := json.Marshal(legacy)
	want := `{"snapshot_id":"` + string(legacy.SnapshotID) + `","summary":"stable","key_facts":null,"open_tasks":null,"provider":"codex"}`
	if string(raw) != want {
		t.Fatalf("legacy encoding changed: %s", raw)
	}
}
