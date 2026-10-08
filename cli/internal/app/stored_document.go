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

// Explicit dual verification never converts an identity-unaware success into
// root evidence. Stores without the reference port remain legacy-only.
func verifyStoredDocumentReference(ctx context.Context, store outbound.SessionStore, ref domain.DocumentRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if ref.Identity == domain.DocumentIdentityLegacy {
		return verifyStoredDocument(ctx, store, ref.Hash)
	}
	if verifier, ok := store.(outbound.StoredDocumentReferenceVerifier); ok {
		return verifier.VerifyStoredDocReference(ctx, ref)
	}
	return domain.ErrUnsupportedDocumentIdentity
}

// readDocumentReference materializes the exact requested identity using
// the explicit reader's complete current-byte verification contract. Do not
// rebuild a verified root from the returned CIR merely to check its tag.
func readDocumentReference(ctx context.Context, store interface {
	GetDoc(context.Context, domain.ContentHash) (domain.SessionDoc, error)
}, ref domain.DocumentRef) (domain.SessionDoc, error) {
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	if err := ref.Validate(); err != nil {
		return domain.SessionDoc{}, err
	}
	var doc domain.SessionDoc
	var err error
	if reader, ok := store.(outbound.DocumentReferenceReader); ok {
		doc, err = reader.GetDocReference(ctx, ref)
	} else {
		if ref.Identity != domain.DocumentIdentityLegacy {
			return domain.SessionDoc{}, domain.ErrUnsupportedDocumentIdentity
		}
		doc, err = store.GetDoc(ctx, ref.Hash)
		if err == nil {
			err = domain.ValidateSessionDocHash(doc)
		}
	}
	if err != nil {
		return domain.SessionDoc{}, err
	}
	if doc.DocumentRef() != ref {
		return domain.SessionDoc{}, domain.ErrHashMismatch
	}
	if err := ctx.Err(); err != nil {
		return domain.SessionDoc{}, err
	}
	return doc, nil
}
