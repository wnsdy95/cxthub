package domain

import (
	"bytes"
	"errors"
	"testing"
)

func TestVerifiedSessionDocCannotAliasMutableInput(t *testing.T) {
	doc := SessionDoc{CIR: CIRDocument{Events: []CIREvent{{Kind: EventMessage, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: "original"}}}}}}
	raw, err := CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = HashContent(raw)
	verified, err := VerifySessionDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	doc.CIR.Events[0].Blocks[0].Text = "mutated"
	copy := verified.Bytes()
	copy[0] = '!'
	if !verified.Valid() || verified.Hash() != HashContent(raw) || !bytes.Equal(verified.Bytes(), raw) {
		t.Fatal("caller mutation changed the validated content")
	}
	idx, err := verified.ReadIndex()
	if err != nil || idx.Events[0].Text != "original" {
		t.Fatalf("index changed: %+v %v", idx, err)
	}
	if _, err := VerifySessionDoc(doc); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("changed input accepted: %v", err)
	}
	var zero VerifiedSessionDoc
	if zero.Valid() {
		t.Fatal("zero value claims verification")
	}
	if _, err := zero.ReadIndex(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("zero index: %v", err)
	}
}
