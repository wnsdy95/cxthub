package gitengine

import (
	"context"
	"errors"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestIntegrityRejectsRelabeledDocumentScheme(t *testing.T) {
	cir := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1"}}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	hash := domain.HashContent(raw)
	engine := NewEngine(nil)
	for _, schemes := range [][2]domain.DocumentIdentity{
		{domain.DocumentIdentityLegacy, domain.DocumentIdentityLegacy},
		{domain.DocumentIdentityLegacy, domain.DocumentIdentityRootV1},
		{domain.DocumentIdentityRootV1, domain.DocumentIdentityLegacy},
		{domain.DocumentIdentityRootV1, domain.DocumentIdentityRootV1},
	} {
		snap := domain.Snapshot{ID: hash, DocHash: hash, DocIdentity: schemes[0]}
		doc := domain.SessionDoc{Hash: hash, CIR: cir, Identity: schemes[1]}
		err := engine.VerifyIntegrity(context.Background(), snap, doc)
		if schemes[0] == domain.DocumentIdentityLegacy && schemes[1] == domain.DocumentIdentityLegacy {
			if err != nil {
				t.Fatal(err)
			}
		} else if !errors.Is(err, domain.ErrIntegrity) {
			t.Fatalf("schemes=%v: %v", schemes, err)
		}
	}
}
