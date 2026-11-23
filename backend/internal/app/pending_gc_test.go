package app

import (
	"context"
	"errors"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type unreadablePendingGCDoc struct {
	outbound.BlobStore
	hash domain.ContentHash
}

// Protected history must be rejected before loading either transcript.
type countPendingGCReads struct {
	outbound.BlobStore
	calls int
}

func (s *countPendingGCReads) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	s.calls++
	return s.BlobStore.GetDoc(ctx, repo, hash)
}

func (s *unreadablePendingGCDoc) GetDoc(ctx context.Context, repo, hash domain.ContentHash) (domain.SessionDoc, error) {
	if hash == s.hash {
		return domain.SessionDoc{}, errors.New("document unreadable")
	}
	return s.BlobStore.GetDoc(ctx, repo, hash)
}
