package app

import (
	"context"
	"path/filepath"

	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// InitRepoService sets cxt up in a Git repository: it resolves which
// repository the folder is and writes the mark that cxt may act there.
type InitRepoService struct {
	gitCtx outbound.GitContext
	marker outbound.SetupMarker
}

// NewInitRepoService creates the service.
func NewInitRepoService(gitCtx outbound.GitContext, marker outbound.SetupMarker) *InitRepoService {
	return &InitRepoService{gitCtx: gitCtx, marker: marker}
}

// Init writes the mark when it is absent. Running it again changes nothing.
func (s *InitRepoService) Init(ctx context.Context, in inbound.InitInput) (inbound.InitOutput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.InitOutput{}, err
	}
	root, err := s.marker.MarkSetUp(ctx, in.Cwd)
	if err != nil {
		return inbound.InitOutput{}, err
	}
	return inbound.InitOutput{RepoID: repo.ID, LocalStorePath: filepath.Join(root, ".cxt")}, nil
}

var _ inbound.InitRepo = (*InitRepoService)(nil)
