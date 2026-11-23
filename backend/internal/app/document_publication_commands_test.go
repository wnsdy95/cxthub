package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

var (
	_ inbound.DocumentPublicationCommands = (*DocumentPublicationCommands)(nil)
)

func wave58PublicationDocument(test *testing.T, marker string, events ...domain.CIREvent) domain.SessionDoc {
	test.Helper()
	if len(events) == 0 {
		events = []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: marker}}}}
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: domain.CIRVersionV2, SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull, GitBranch: "main", SessionOriginID: marker},
		Events:   events,
	}}
	document.Hash = domain.HashContent(wave58PublicationBytes(test, document))
	return document
}

func wave58PublicationBytes(test *testing.T, document domain.SessionDoc) []byte {
	test.Helper()
	body, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	return body
}
