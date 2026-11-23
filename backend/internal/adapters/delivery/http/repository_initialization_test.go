package http

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type initializationHTTPBackend struct {
	Backend
	repo  domain.Repo
	calls int
	actor string
	err   error
}

func (b *initializationHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return b.repo, nil
}

type initializationHTTPView struct {
	*initializationHTTPBackend
	pending bool
}

func (b initializationHTTPView) BranchPullVersion() int { return 0 }
func (b initializationHTTPView) GetRepositoryInitializationView(_ context.Context, _ domain.ContentHash, branch string) (domain.Repo, bool, error) {
	return b.repo, b.pending && branch != "", nil
}
