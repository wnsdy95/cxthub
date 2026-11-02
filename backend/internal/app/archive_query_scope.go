package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type archiveQueryScope struct {
	repository *repositoryQueryScope
	document   *documentQueryScope
	presence   outbound.RepositoryTransactionPresence
}

func (scope *archiveQueryScope) ReadRepositories(ctx context.Context, read func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ctx = outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.repository.supported)
	for _, identity := range inbound.DocumentIdentities(ctx) {
		if err := identity.Validate(); err != nil {
			return err
		}
	}
	if scope.repository.transactions != nil {
		return scope.repository.transactions.WithinReadSnapshot(ctx, read)
	}
	return read(ctx)
}

func (scope *archiveQueryScope) ReadRepository(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	queries := contextQueryScope{repository: scope.repository, presence: scope.presence}
	return queries.ReadContext(ctx, repo, read)
}

func (scope *archiveQueryScope) ReadDocument(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.document.WithinDocumentRead(ctx, repo, read)
}
