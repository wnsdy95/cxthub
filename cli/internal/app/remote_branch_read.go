package app

import (
	"context"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

// ReadRemoteBranch reads the current shared pointer without synchronizing local
// objects or moving refs. Callers must separately check local graph coverage.
func (s *SyncRepoService) ReadRemoteBranch(ctx context.Context, in inbound.SyncInput, branch string) (domain.Ref, error) {
	repo, err := s.repoID(ctx, in)
	if err != nil {
		return domain.Ref{}, err
	}
	manifest, err := s.remote.RemoteManifest(ctx, repo)
	if err != nil {
		return domain.Ref{}, err
	}
	for _, ref := range manifest.Refs {
		if ref.Kind != domain.RefBranch || ref.Name != branch || ref.Target == "" {
			continue
		}
		if ref.RepoID != repo || domain.ValidateRef(ref) != nil {
			return domain.Ref{}, domain.ErrHashMismatch
		}
		return ref, nil
	}
	return domain.Ref{}, domain.ErrNotFound
}
