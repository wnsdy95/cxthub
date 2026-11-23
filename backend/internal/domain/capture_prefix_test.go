package domain

import (
	"testing"
)

func prefixFixture(t testing.TB, texts ...string) (SessionDoc, VerifiedSessionDoc) {
	t.Helper()
	cir := CIRDocument{Envelope: CIREnvelope{CIRVersion: "1", SourceProvider: ProviderCodex, SessionOriginID: "native-prefix"}}
	for i, text := range texts {
		cir.Events = append(cir.Events, CIREvent{Kind: EventMessage, Seq: i, Role: RoleUser, Blocks: []ContentBlock{{Type: "text", Text: text}}})
	}
	raw, err := CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	doc := SessionDoc{Hash: HashContent(raw), CIR: cir}
	v, err := VerifySessionDoc(doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc, v
}
