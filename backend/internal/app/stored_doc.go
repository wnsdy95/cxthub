package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// The application keeps S-ID agreement; adapters prove current owned content.
// Legacy narrow adapters retain complete domain/engine verification. A stronger
// adapter's error is terminal and never falls back to weaker cached evidence.
func (s *Service) verifyStoredSnapshotDoc(ctx context.Context, repo domain.ContentHash, snap domain.Snapshot) error {
	return verifyStoredSnapshotDocument(ctx, s.meta, s.blobs, s.engine, s.DocumentIdentitiesSupported(), repo, snap)
}

func (s *Service) verifyStoredSnapshotReference(ctx context.Context, repo domain.ContentHash, snap domain.Snapshot) error {
	return verifyStoredSnapshotReferenceFromSources(ctx, s.blobs, s.engine, repo, snap)
}
