package http

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docViewAuthorizationBackend struct {
	Backend
	read func(context.Context) error
}

func (backend *docViewAuthorizationBackend) ReadDocViewEvents(ctx context.Context, _, _ domain.ContentHash, _, _ int, _ string) (domain.DocViewPage, error) {
	return domain.DocViewPage{Next: -1}, backend.read(ctx)
}

func (backend *docViewAuthorizationBackend) ReadDocViewFragment(ctx context.Context, _, _ domain.ContentHash, _, _, _ int) (domain.DocViewFragment, error) {
	return domain.DocViewFragment{Next: -1}, backend.read(ctx)
}
