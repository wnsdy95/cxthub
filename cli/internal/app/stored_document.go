package app

import (
	"context"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// verifyStoredDocument verifies canonical identity and CIR semantics through a
// capability port when available; narrow stores retain strict full validation.
func verifyStoredDocument(ctx context.Context, store outbound.SessionStore, id domain.ContentHash) error {
	if verifier, ok := store.(outbound.StoredDocumentVerifier); ok {
		return verifier.VerifyStoredDoc(ctx, id)
	}
	doc, err := store.GetDoc(ctx, id)
	if err != nil {
		return err
	}
	if doc.Hash != id {
		return domain.ErrHashMismatch
	}
	return domain.ValidateSessionDocHash(doc)
}
