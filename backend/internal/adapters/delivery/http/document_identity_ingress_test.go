package http

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func identityIngressDoc(t *testing.T, text string) domain.SessionDoc {
	t.Helper()
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}},
	}
	raw, err := domain.CanonicalBytes(cir)
	if err != nil {
		t.Fatal(err)
	}
	return domain.SessionDoc{Hash: domain.HashContent(raw), CIR: cir}
}
