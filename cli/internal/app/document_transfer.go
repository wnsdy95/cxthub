package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

func (s *SyncRepoService) preflightSnapshotReferences(ctx context.Context, repo string, snaps []domain.Snapshot) error {
	seen := map[domain.ContentHash]domain.DocumentIdentity{}
	var refs []domain.DocumentRef
	for _, snap := range snaps {
		ref := snap.DocumentRef()
		if err := ref.Validate(); err != nil {
			return err
		}
		if old, ok := seen[ref.Hash]; ok && old != ref.Identity {
			return domain.ErrHashMismatch
		}
		seen[ref.Hash] = ref.Identity
		if ref.Identity != domain.DocumentIdentityLegacy {
			refs = append(refs, ref)
		}
	}
	if len(refs) == 0 {
		return ctx.Err()
	}
	peer, ok := s.remote.(outbound.DocumentPublicationPreflight)
	if !ok {
		return domain.ErrUnsupportedDocumentIdentity
	}
	return peer.PreflightDocumentReferences(ctx, repo, refs)
}

// Check existing root inventory before ancillary registration/settings/history
// writes, including when document negotiation will later report zero wants.
func (s *SyncRepoService) preflightLocalRoots(ctx context.Context, repo string) error {
	snaps, err := s.store.ListSnapshots(ctx, repo, "")
	if err != nil {
		return err
	}
	return s.preflightSnapshotReferences(ctx, repo, snaps)
}
