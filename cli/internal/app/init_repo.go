package app

import (
	"context"
	"path/filepath"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/cli/internal/ports/outbound"
)

// InitRepoService implements the InitRepo inbound port as per the _RECONCILIATION section.
//
// Dependency outbound ports: GitContext(remote/cwd → RepoID), SessionStore(.cxt/ store creation).
//
// Init sequence:
//  1. If in.RemoteURL is empty, use GitContext.CurrentRepo(Cwd) to auto-detect origin (fallback to cwd if not found).
//  2. RepoID = ContentHash(normalize(remote_url_or_cwd)) to determine.
//  3. SessionStore initializes the .cxt/ layout at the repo root (_RECONCILIATION C.3) (objects/, refs/heads, refs/tags, HEAD, manifest.json, config).
//  4. Return InitOutput{RepoID, LocalStorePath}.
type InitRepoService struct {
	gitCtx outbound.GitContext
	store  outbound.SessionStore
}

// NewInitRepoService creates and injects dependencies for InitRepoService.
func NewInitRepoService(gitCtx outbound.GitContext, store outbound.SessionStore) *InitRepoService {
	return &InitRepoService{gitCtx: gitCtx, store: store}
}

// Init registers the current repo and initializes the local .cxt/ store.
// repoID is determined by GitContext.CurrentRepo (ensures same key as save). If RemoteURL is specified,
// it is recorded as Repo.RemoteURL, and the identifier key follows the CurrentRepo result for consistency.
func (s *InitRepoService) Init(ctx context.Context, in inbound.InitInput) (inbound.InitOutput, error) {
	repo, err := s.gitCtx.CurrentRepo(ctx, in.Cwd)
	if err != nil {
		return inbound.InitOutput{}, err
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	// .cxt/ initialization: creates store directory using HEAD symbolic ref.
	if err := s.store.PutRef(ctx, domain.Ref{Kind: domain.RefHEAD, Name: "HEAD", RepoID: repo.ID, Symbolic: branch}); err != nil {
		return inbound.InitOutput{}, err
	}
	return inbound.InitOutput{RepoID: repo.ID, LocalStorePath: filepath.Join(in.Cwd, ".cxt")}, nil
}

// Ensure InitRepoService implements inbound.InitRepo.
var _ inbound.InitRepo = (*InitRepoService)(nil)
