package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestValidateSessionDocHash(t *testing.T) {
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1", SourceProvider: ProviderClaude, Fidelity: FidelityFull}}
	canonical, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := SessionDoc{Hash: HashContent(canonical), CIR: cir}
	if err := ValidateSessionDocHash(doc); err != nil {
		t.Fatalf("valid doc: %v", err)
	}
	doc.Hash = HashContent([]byte("different"))
	if err := ValidateSessionDocHash(doc); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("mismatched doc error = %v, want ErrIntegrity", err)
	}
}

func TestMemoryDigestCoverageWireHashParity(t *testing.T) {
	h := func(c string) ContentHash { return ContentHash("sha256:" + strings.Repeat(c, 64)) }
	digest := MemoryDigest{
		SnapshotID: h("a"), PreviousMemoryHash: h("f"), Summary: "summary", KeyFacts: []string{"fact"}, OpenTasks: []string{}, Provider: ProviderCodex,
		Fragments: []MemoryFragment{{SourceSnapshot: h("b"), Summary: "fragment"}},
		GraftCoverage: &MemoryGraftCoverage{
			ProjectionVersion: MemoryProjectionVersion, ProjectionComplete: true, LineageFingerprint: h("c"), GraftSeq: 2,
			GraftParents: []ContentHash{h("d")}, PinnedSources: []ContentHash{h("e")},
		},
	}
	wire := `{"snapshot_id":"` + string(h("a")) + `","previous_memory_hash":"` + string(h("f")) + `","summary":"summary","key_facts":["fact"],"open_tasks":[],"provider":"codex","fragments":[{"source_snapshot":"` + string(h("b")) + `","summary":"fragment"}],"graft_coverage":{"projection_version":1,"projection_complete":true,"lineage_fingerprint":"` + string(h("c")) + `","graft_seq":2,"graft_parents":["` + string(h("d")) + `"],"pinned_sources":["` + string(h("e")) + `"]}}`
	got, err := MemoryDigestHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	if want := HashContent([]byte(wire)); got != want {
		t.Fatalf("coverage wire hash = %s, want %s", got, want)
	}
}

func TestMemoryDigestHash(t *testing.T) {
	digest := MemoryDigest{SnapshotID: HashContent([]byte("snapshot")), Summary: "summary", Provider: ProviderCodex}
	left, err := MemoryDigestHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	right, err := MemoryDigestHash(digest)
	if err != nil {
		t.Fatal(err)
	}
	if left != right {
		t.Fatalf("memory hash is not deterministic: %s != %s", left, right)
	}
}

func TestCanonicalBytesPreserveCIRUnionRequiredFields(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"2026-07-12T00:00:00Z","cir_version":"1","cwd":"/repo","fidelity":"full","git_branch":"main","session_origin_id":"session","source_model":"model","source_provider":"codex"},"events":[{"blocks":[],"kind":"message","role":"assistant","seq":0},{"call_id":"call-1","input":{},"kind":"tool_call","seq":1,"tool_name":"shell"},{"call_id":"call-1","kind":"tool_result","output":"","seq":2},{"cross_replayable":false,"kind":"reasoning","seq":3}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("canonical CIR union changed required empty fields:\n got: %s\nwant: %s", got, raw)
	}
}

func TestCanonicalBytesPreserveToolResultBlockArray(t *testing.T) {
	raw := []byte(`{"envelope":{"captured_at":"2026-07-22T00:00:00Z","cir_version":"1","cwd":"/repo","fidelity":"full","git_branch":"main","session_origin_id":"session","source_model":"model","source_provider":"claude"},"events":[{"call_id":"call-1","kind":"tool_result","output":[{"text":"screenshot","type":"text"},{"source":{"data":"AA==","media_type":"image/png","type":"base64"},"type":"image"}],"seq":0}]}`)
	var doc CIRDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	got, err := CanonicalBytes(doc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(raw) {
		t.Fatalf("canonical CIR changed tool_result block array:\n got: %s\nwant: %s", got, raw)
	}
}
