package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/memoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type memorySnapshotScope struct {
	resources  *preparedMemoryQueryScope
	repository *repositoryQueryScope
}

func (scope *memorySnapshotScope) WithMemoryReadResources(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.resources.WithMemoryReadResources(ctx, repo, read)
}

func (scope *memorySnapshotScope) AuthorizeSnapshot(ctx context.Context, repo domain.ContentHash) (context.Context, error) {
	return scope.resources.AuthorizeSnapshot(ctx, repo)
}

func (scope *memorySnapshotScope) WithinMemorySnapshot(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	if scope.repository.transactions != nil && inbound.DocumentReadAuthorizer(ctx) != nil && inbound.DocumentSnapshotReadAuthorizer(ctx) == nil {
		return domain.ErrCatalogUnsupported
	}
	return scope.repository.WithinRepositoryRead(ctx, repo, read)
}

func (service *Service) memorySnapshotReadScope() *memorySnapshotScope {
	transactions, _ := service.meta.(documentQuerySnapshots)
	return &memorySnapshotScope{resources: service.memoryResourceScope(), repository: &repositoryQueryScope{
		metadata: service.meta, transactions: transactions, supported: service.DocumentIdentitiesSupported(),
	}}
}

func (service *Service) MemorySnapshotQueries() (*memoryquery.SnapshotQueries, error) {
	return memoryquery.NewSnapshots(memoryquery.SnapshotConfig{Scope: service.memorySnapshotReadScope(), Metadata: service.meta,
		Objects: service.blobs, Projection: service.memoryProjectionReader()})
}
