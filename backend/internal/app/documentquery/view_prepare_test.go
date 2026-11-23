package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type viewPreparationReadKey struct{}

type viewPreparationScope struct{}

func (viewPreparationScope) AuthorizeDocumentRead(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, ctx.Err()
}

func (viewPreparationScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	if ctx.Value(viewPreparationReadKey{}) == true {
		return ctx, domain.ErrConflict
	}
	return ctx, ctx.Err()
}

func (viewPreparationScope) WithinDocumentRead(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(context.WithValue(ctx, viewPreparationReadKey{}, true))
}

func (scope viewPreparationScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.WithinDocumentRead(ctx, repo, read)
}
