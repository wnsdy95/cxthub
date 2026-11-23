package documentquery

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyFindGenerationMeta struct {
	failure error
}

type legacyFindGenerationScope struct{}

func (legacyFindGenerationScope) AuthorizeDocumentRead(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	if authorize := inbound.DocumentReadAuthorizer(ctx); authorize != nil {
		return authorize(ctx, repo)
	}
	return ctx, ctx.Err()
}

func (legacyFindGenerationScope) AuthorizeDocumentPreparation(ctx context.Context, _ domain.ContentHash) (context.Context, error) {
	return ctx, domain.ErrForbidden
}

func (scope legacyFindGenerationScope) WithinDocumentRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.WithinDocumentSnapshot(ctx, repo, read)
}

func (scope legacyFindGenerationScope) WithinDocumentSnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	bound, err := scope.AuthorizeDocumentRead(ctx, repo)
	if err != nil {
		return err
	}
	return read(outbound.WithDocReadOnly(bound))
}

func (legacyFindGenerationScope) AcquireInteractive(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, ctx.Err()
}

func (legacyFindGenerationScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	return ctx, func() {}, ctx.Err()
}

func (metadata *legacyFindGenerationMeta) GetSnapshot(context.Context, domain.ContentHash, domain.ContentHash) (domain.Snapshot, error) {
	return domain.Snapshot{}, metadata.failure
}
