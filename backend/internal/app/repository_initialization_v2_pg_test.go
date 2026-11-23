//go:build postgres

package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Instrument only this fixture store; all validation still uses real owned PG
// documents. Mutations run after the coherent read captured its exact proof.
type initializationCapturePG struct {
	*store.PostgresStore
	afterCapture func() error
	verifyHook   func(context.Context) error
}

func (s *initializationCapturePG) VerifyStoredDoc(ctx context.Context, id, hash domain.ContentHash) (domain.VerifiedDocReference, error) {
	if !s.InReadOnlyTransaction(ctx) {
		return domain.VerifiedDocReference{}, errors.New("body validation under writer lock")
	}
	if s.verifyHook != nil {
		if err := s.verifyHook(ctx); err != nil {
			return domain.VerifiedDocReference{}, err
		}
	}
	return s.PostgresStore.VerifyStoredDoc(ctx, id, hash)
}
