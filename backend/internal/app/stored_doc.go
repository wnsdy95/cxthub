package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// The application keeps S-ID agreement; adapters prove current owned content.
// Legacy narrow adapters retain complete domain/engine verification. A stronger
// adapter's error is terminal and never falls back to weaker cached evidence.
func (s *Service) verifyStoredSnapshotDoc(ctx context.Context, repo domain.ContentHash, snap domain.Snapshot) error {
	if snap.ID == "" || snap.DocHash == "" || snap.ID != snap.DocHash {
		return domain.ErrIntegrity
	}
	if verifier, ok := s.blobs.(outbound.StoredDocVerifier); ok {
		proof, err := verifier.VerifyStoredDoc(ctx, repo, snap.DocHash)
		if err != nil {
			return err
		}
		if !proof.Matches(snap) {
			return domain.ErrIntegrity
		}
		return nil
	}
	doc, err := s.blobs.GetDoc(ctx, repo, snap.DocHash)
	if err != nil {
		return err
	}
	return s.engine.VerifyIntegrity(ctx, snap, doc)
}
