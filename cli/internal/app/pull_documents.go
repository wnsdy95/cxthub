package app

import (
	"context"
	"errors"
	"fmt"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

type pullDocumentReceiver struct {
	store    outbound.SessionStore
	verified map[domain.DocumentRef]bool
}

func (r *pullDocumentReceiver) HasVerifiedDoc(ctx context.Context, ref domain.DocumentRef) (bool, error) {
	if r.verified[ref] {
		return true, nil
	}
	err := verifyStoredDocumentReference(ctx, r.store, ref)
	if errors.Is(err, domain.ErrNotFound) {
		// A missing referenced chunk is corruption of an existing document,
		// not permission to repair it implicitly through normal fetch. Only an
		// absent document descriptor may proceed to download.
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
		exists, herr := r.store.HasDoc(ctx, ref.Hash)
		if herr != nil {
			return false, herr
		}
		if exists {
			return false, fmt.Errorf("%w: incomplete stored document %s: %w", domain.ErrHashMismatch, ref.Hash, err)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if r.verified == nil {
		r.verified = make(map[domain.DocumentRef]bool)
	}
	r.verified[ref] = true
	return true, nil
}

func (r *pullDocumentReceiver) ReceiveDoc(ctx context.Context, doc domain.SessionDoc) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
		return err
	}
	id, err := r.store.PutDoc(ctx, doc)
	if err != nil {
		return err
	}
	if id != doc.Hash {
		return domain.ErrHashMismatch
	}
	if r.verified == nil {
		r.verified = make(map[domain.DocumentRef]bool)
	}
	r.verified[doc.DocumentRef()] = true
	return nil
}

func (r *pullDocumentReceiver) ReceiveRoot(ctx context.Context, rep domain.DocumentRepresentation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := rep.ConversationManifest(); err != nil {
		return err
	}
	installer, ok := r.store.(outbound.RootDocumentStore)
	if !ok {
		return domain.ErrUnsupportedDocumentIdentity
	}
	if err := installer.PutConversationManifest(ctx, rep); err != nil {
		return err
	}
	if r.verified == nil {
		r.verified = make(map[domain.DocumentRef]bool)
	}
	r.verified[rep.DocumentRef()] = true
	return nil
}
