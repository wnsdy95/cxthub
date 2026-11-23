package domain

import (
	"bytes"
	"testing"
)

func TestVerifiedDocReferenceBindsIdentityAndOwnsNoMutableInput(t *testing.T) {
	raw := []byte(`{"envelope":{"cir_version":"1"},"events":[]}`)
	// Non-canonical legacy JSON needs the same canonical identity, not raw SHA.
	doc := SessionDoc{CIR: CIRDocument{Envelope: CIREnvelope{CIRVersion: "1"}, Events: []CIREvent{}}}
	canonical, err := CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = HashContent(canonical)
	p, err := VerifyStoredDocBytes(doc.Hash, raw)
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		raw[i] = 'x'
	}
	if !p.Matches(Snapshot{ID: doc.Hash, DocHash: doc.Hash}) {
		t.Fatal("caller changed proof")
	}
	other := HashContent([]byte("other"))
	if p.Matches(Snapshot{ID: other, DocHash: doc.Hash}) || p.Matches(Snapshot{ID: doc.Hash, DocHash: other}) || (VerifiedDocReference{}).Valid() {
		t.Fatal("unbound or zero proof accepted")
	}
	// Unknown data that would be dropped by typed decoding must not be accepted
	// under a hash calculated directly over its bytes.
	forged := bytes.Replace(canonical, []byte(`"events":[]`), []byte(`"events":[],"unknown":"field"`), 1)
	if _, err := VerifyStoredDocBytes(HashContent(forged), forged); err == nil {
		t.Fatal("raw hash bypassed canonical schema")
	}
}
