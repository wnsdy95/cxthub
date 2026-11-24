package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
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
