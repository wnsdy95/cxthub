package app

import (
	"context"
	"encoding/json"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func requireRootDocumentRepository(ctx context.Context, meta outbound.MetadataStore, repo domain.ContentHash) error {
	r, err := meta.GetRepo(ctx, repo)
	if err != nil {
		return err
	}
	if err := outbound.CheckDocumentIdentityCompatibility(ctx, r.RequiredDocIdentity); err != nil {
		return err
	}
	if r.RequiredDocIdentity != domain.DocumentIdentityRootV1 {
		return domain.ErrRootPublicationDisabled
	}
	return nil
}

// Send's outer coherent read pins the requirement, published snapshot and all
// owned bytes together. A descriptor request never exposes an unpublished root
// job, trusts a receipt, or falls back to a legacy manifest after root failure.
func (s *Service) publishedRootRepresentation(ctx context.Context, repo, hash domain.ContentHash, snap domain.Snapshot, cirVersions map[string]bool) (domain.DocumentRepresentation, error) {
	if err := snap.DocumentRef().Validate(); err != nil {
		return domain.DocumentRepresentation{}, err
	}
	if snap.RepoID != repo || snap.ID != hash || snap.DocHash != hash || snap.DocIdentity != domain.DocumentIdentityRootV1 {
		return domain.DocumentRepresentation{}, domain.ErrIntegrity
	}
	if err := requireRootDocumentRepository(ctx, s.meta, repo); err != nil {
		return domain.DocumentRepresentation{}, err
	}
	proof, err := s.verifiedRootDoc(ctx, repo, snap)
	if err != nil {
		return domain.DocumentRepresentation{}, err
	}
	manifest, ok := proof.ConversationManifest()
	if !ok {
		return domain.DocumentRepresentation{}, domain.ErrIntegrity
	}
	var envelope struct {
		CIRVersion string `json:"cir_version"`
	}
	if err := json.Unmarshal(manifest.Envelope, &envelope); err != nil {
		return domain.DocumentRepresentation{}, domain.ErrIntegrity
	}
	if err := requireSupportedCIRVersion(envelope.CIRVersion, cirVersions); err != nil {
		return domain.DocumentRepresentation{}, err
	}
	raw, err := domain.CanonicalConversationManifest(manifest)
	if err != nil {
		return domain.DocumentRepresentation{}, err
	}
	return domain.DocumentRepresentation{Hash: hash, Identity: snap.DocIdentity, RootManifest: raw}, ctx.Err()
}
