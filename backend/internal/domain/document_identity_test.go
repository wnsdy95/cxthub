package domain

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestDocumentIdentityLegacyWireAndPublicationFence(t *testing.T) {
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: []CIREvent{{Kind: EventMessage, Seq: 1, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "synthetic identity fixture"}}}}}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	legacy := SessionDoc{Hash: HashContent(raw), CIR: cir}
	before, _ := json.Marshal(struct {
		Hash ContentHash `json:"hash"`
		CIR  CIRDocument `json:"cir"`
	}{legacy.Hash, cir})
	after, err := json.Marshal(legacy)
	if err != nil || string(before) != string(after) {
		t.Fatalf("legacy wire changed: %s, %v", after, err)
	}
	proof, err := VerifySessionDoc(legacy)
	if err != nil || !proof.Valid() {
		t.Fatal(proof, err)
	}
	snapshot := Snapshot{ID: legacy.Hash, DocHash: legacy.Hash}
	if !proof.Reference().Matches(snapshot) {
		t.Fatal("legacy proof lost its snapshot")
	}
	snapshot.DocIdentity = DocumentIdentityRootV1
	if proof.Reference().Matches(snapshot) {
		t.Fatal("legacy proof authorized a different identity scheme")
	}
	manifest, _, err := ConversationManifestForCIR(cir)
	if err != nil {
		t.Fatal(err)
	}
	root, err := ConversationManifestHash(manifest)
	if err != nil || root == legacy.Hash {
		t.Fatal("identity domains collided", err)
	}
	doc := SessionDoc{Hash: root, CIR: cir, Identity: DocumentIdentityRootV1}
	if err := VerifySessionDocIdentity(context.Background(), doc); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifySessionDoc(doc); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
		t.Fatal("unready publication path accepted root", err)
	}
	doc.Identity = DocumentIdentityLegacy
	if err := VerifySessionDocIdentity(context.Background(), doc); !errors.Is(err, ErrIntegrity) {
		t.Fatal("root relabeled as legacy", err)
	}
	doc.Hash = legacy.Hash
	doc.Identity = DocumentIdentityRootV1
	if err := VerifySessionDocIdentity(context.Background(), doc); !errors.Is(err, ErrIntegrity) {
		t.Fatal("legacy relabeled as root", err)
	}
}

func TestDocumentIdentityRejectsUnknownAndNull(t *testing.T) {
	for _, raw := range []string{`null`, `3`, `"unknown"`, `"sha256"`} {
		got := DocumentIdentityRootV1
		if err := json.Unmarshal([]byte(raw), &got); !errors.Is(err, ErrUnsupportedDocumentIdentity) || got != DocumentIdentityRootV1 {
			t.Fatalf("identity %s: value=%q err=%v", raw, got, err)
		}
	}
	if _, err := json.Marshal(DocumentIdentity("unknown")); !errors.Is(err, ErrUnsupportedDocumentIdentity) {
		t.Fatal(err)
	}
	for _, identity := range []DocumentIdentity{DocumentIdentityLegacy, DocumentIdentityRootV1} {
		raw, err := json.Marshal(identity)
		if err != nil {
			t.Fatal(err)
		}
		var got DocumentIdentity
		if err := json.Unmarshal(raw, &got); err != nil || got != identity {
			t.Fatal(got, err)
		}
	}
}
