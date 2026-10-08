package store

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type captureByteReader interface {
	readDocBytes(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, bool, error)
}

func compareStoredCaptures(ctx context.Context, s captureByteReader, cache *docProofCache, repo, old, next domain.ContentHash, provider domain.ProviderKind, session string) (bool, error) {
	oldBody, _, err := s.readDocBytes(ctx, repo, old)
	if errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		return false, nil // Root-involved pairs are retained until reference-aware compaction.
	}
	if err != nil {
		return false, err
	}
	oldProof, err := cache.verify(ctx, repo, old, oldBody)
	if err != nil {
		return false, err
	}
	nextBody, _, err := s.readDocBytes(ctx, repo, next)
	if errors.Is(err, domain.ErrUnsupportedDocumentIdentity) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	nextProof, err := cache.verify(ctx, repo, next, nextBody)
	if err != nil {
		return false, err
	}
	return domain.CaptureSupersedes(ctx, oldProof, oldBody, nextProof, nextBody, provider, session)
}
