package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// sessionArchiveHTTPBackend is a backend that knows one repository and nothing
// else: the routes under test are answered by a service set beside it.
type sessionArchiveHTTPBackend struct {
	Backend
	repo domain.Repo
}

func (backend *sessionArchiveHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}
