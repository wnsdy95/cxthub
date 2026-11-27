package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type gitChangeScope struct {
	work *repositoryWork
}

func (scope gitChangeScope) WorkerContext(ctx context.Context) context.Context {
	return outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(ctx, scope.work.supported), scope.work.supported, scope.work.supported)
}

func (scope gitChangeScope) ReadRepository(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.work.supported)
	return repositoryReadWithWork(ctx, scope.work, func(bound context.Context) (domain.Repo, error) {
		if err := scope.work.checkDocumentIdentity(bound, repo, true); err != nil {
			return domain.Repo{}, err
		}
		return scope.work.meta.GetRepo(bound, repo)
	})
}
