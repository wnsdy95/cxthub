package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type sessionArchiveHTTPBackend struct {
	Backend
	repo     domain.Repo
	calls    int
	actor    string
	snapshot domain.ContentHash
	archived bool
	err      error
}

func (backend *sessionArchiveHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *sessionArchiveHTTPBackend) SetSessionArchived(ctx context.Context, repo, snapshot domain.ContentHash, archived bool) error {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.snapshot, backend.archived = snapshot, archived
	return backend.err
}
