package domain

import (
	"errors"
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
