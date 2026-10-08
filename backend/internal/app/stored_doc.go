package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

// The application keeps S-ID agreement; adapters prove current owned content.
// Legacy narrow adapters retain complete domain/engine verification. A stronger
// adapter's error is terminal and never falls back to weaker cached evidence.
func (s *Service) verifyStoredSnapshotDoc(ctx context.Context, repo domain.ContentHash, snap domain.Snapshot) error {
	if err := snap.DocIdentity.Validate(); err != nil {
		return err
	}
	if snap.DocIdentity == domain.DocumentIdentityRootV1 {
		if !hasDocumentIdentity(s.DocumentIdentitiesSupported(), snap.DocIdentity) {
			return domain.ErrUnsupportedDocumentIdentity
		}
		ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), s.DocumentIdentitiesSupported())
		if err := requireRootDocumentRepository(ctx, s.meta, repo); err != nil {
			return err
		}
	}
	return s.verifyStoredSnapshotReference(ctx, repo, snap)
}

// The caller owns compatibility/admission and its coherent repository boundary.
// A root reference can only use the stronger current-byte verifier.
func (s *Service) verifyStoredSnapshotReference(ctx context.Context, repo domain.ContentHash, snap domain.Snapshot) error {
	if err := snap.DocumentRef().Validate(); err != nil {
		return err
	}
	if snap.DocIdentity == domain.DocumentIdentityRootV1 && snap.RepoID != repo {
		return domain.ErrIntegrity
	}
	if snap.ID == "" || snap.DocHash == "" || snap.ID != snap.DocHash {
		return domain.ErrIntegrity
	}
	if verifier, ok := s.blobs.(outbound.StoredDocVerifier); ok {
		proof, err := verifier.VerifyStoredDoc(ctx, repo, snap.DocHash)
		if err != nil {
			return err
		}
		if proof.DocumentRef() != snap.DocumentRef() || !proof.Matches(snap) {
			return domain.ErrIntegrity
		}
		return nil
	}
	if snap.DocIdentity != domain.DocumentIdentityLegacy {
		return domain.ErrUnsupportedDocumentIdentity
	}
	doc, err := s.blobs.GetDoc(ctx, repo, snap.DocHash)
	if err != nil {
		return err
	}
	return s.engine.VerifyIntegrity(ctx, snap, doc)
}
