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
	verified map[domain.ContentHash]bool
}

func (r *pullDocumentReceiver) HasVerifiedDoc(ctx context.Context, id domain.ContentHash) (bool, error) {
	if r.verified[id] {
		return true, nil
	}
	err := verifyStoredDocument(ctx, r.store, id)
	if errors.Is(err, domain.ErrNotFound) {
		// A missing referenced chunk is corruption of an existing document,
		// not permission to repair it implicitly through normal fetch. Only an
		// absent document descriptor may proceed to download.
		if cerr := ctx.Err(); cerr != nil {
			return false, cerr
		}
		exists, herr := r.store.HasDoc(ctx, id)
		if herr != nil {
			return false, herr
		}
		if exists {
			return false, fmt.Errorf("%w: incomplete stored document %s: %w", domain.ErrHashMismatch, id, err)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	r.verified[id] = true
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
	r.verified[id] = true
	return nil
}
