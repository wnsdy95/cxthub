package domain

import (
	"errors"
	"strings"
	"testing"
)

func testSessionDoc(t *testing.T, text string) SessionDoc {
	t.Helper()
	cir := CIRDocument{
		Envelope: Envelope{CIRVersion: "1", SourceProvider: ProviderClaude, Fidelity: FidelityFull},
		Events:   []Event{{Kind: EventMessage, Seq: 0, Role: "user", Blocks: []ContentBlock{{Type: "text", Text: text}}}},
	}
	canonical, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return SessionDoc{Hash: HashContent(canonical), CIR: cir}
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

func TestValidateSessionDocHashRecomputesCanonicalContent(t *testing.T) {
	doc := testSessionDoc(t, "verified")
	if err := ValidateSessionDocHash(doc); err != nil {
		t.Fatalf("valid doc rejected: %v", err)
	}
	doc.CIR.Events[0].Blocks[0].Text = "tampered"
	if err := ValidateSessionDocHash(doc); !errors.Is(err, ErrHashMismatch) {
		t.Fatalf("tampered doc error = %v", err)
	}
}

func TestMemoryDigestHashChangesWithSnapshotIdentity(t *testing.T) {
	a := MemoryDigest{SnapshotID: HashContent([]byte("a")), Summary: "same"}
	b := a
	b.SnapshotID = HashContent([]byte("b"))
	ha, err := MemoryDigestHash(a)
	if err != nil {
		t.Fatal(err)
	}
	hb, err := MemoryDigestHash(b)
	if err != nil {
		t.Fatal(err)
	}
	if ha == hb {
		t.Fatal("memory hash did not bind snapshot_id")
	}
}
