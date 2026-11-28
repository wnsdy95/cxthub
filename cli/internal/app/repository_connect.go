package app

import (
	"context"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub-cli/internal/ports/outbound"
)

// RepositoryConnectService registers the repository of a folder with its
// server and returns the server's record of it. The server reads the URL's
// path to bind the repository, so the record says whether it is bound.
type RepositoryConnectService struct {
	remote outbound.RepoRegistry
	gitCtx outbound.GitContext
}

// NewRepositoryConnectService creates the service. gitCtx may be nil when
// only ConnectRepository is used.
func NewRepositoryConnectService(remote outbound.RepoRegistry, gitCtx outbound.GitContext) *RepositoryConnectService {
	return &RepositoryConnectService{remote: remote, gitCtx: gitCtx}
}

// Connect registers the repository the folder belongs to.
func (s *RepositoryConnectService) Connect(ctx context.Context, cwd string) (inbound.ConnectOutput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, cwd)
	if err != nil {
		return inbound.ConnectOutput{}, err
	}
	return s.ConnectRepository(ctx, repo)
}

// ConnectRepository registers a repository the caller already resolved,
// without reading the folder's configuration again.
func (s *RepositoryConnectService) ConnectRepository(ctx context.Context, repo domain.Repo) (inbound.ConnectOutput, error) {
	if domain.ValidateContentHash(repo.ID) != nil || repo.RemoteURL == "" {
		return inbound.ConnectOutput{}, domain.ErrHashMismatch
	}
	registered, err := s.remote.RegisterRepo(ctx, repo)
	if err != nil {
		return inbound.ConnectOutput{}, err
	}
	return inbound.ConnectOutput{Repo: registered}, nil
}

var _ inbound.RepositoryConnector = (*RepositoryConnectService)(nil)
