package app

import (
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func seedLegacyFindDocument(test *testing.T, storage *store.FSStore, repo domain.ContentHash, events []domain.CIREvent) domain.SessionDoc {
	test.Helper()
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex}, Events: events}}
	for index := range doc.CIR.Events {
		doc.CIR.Events[index].Seq = index
	}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		test.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := storage.PutDoc(systemTestContext(), repo, doc); err != nil {
		test.Fatal(err)
	}
	return doc
}
