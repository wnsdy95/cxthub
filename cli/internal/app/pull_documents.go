package app

import (
	"context"
	"errors"

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
	doc, err := r.store.GetDoc(ctx, id)
	if errors.Is(err, domain.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if doc.Hash != id {
		return false, domain.ErrHashMismatch
	}
	if err := domain.ValidateSessionDocHash(doc); err != nil {
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
