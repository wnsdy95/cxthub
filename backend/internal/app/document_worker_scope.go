package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentWorkerScope struct {
	documents *documentQueryScope
}

var _ documentworker.Scope = (*documentWorkerScope)(nil)

func newDocumentWorkerScope(metadata outbound.MetadataStore, blobs outbound.BlobStore, profile documentquery.Profile) *documentWorkerScope {
	transactions, _ := metadata.(documentQuerySnapshots)
	presence, _ := metadata.(outbound.RepositoryTransactionPresence)
	return &documentWorkerScope{documents: &documentQueryScope{
		metadata: metadata, transactions: transactions, presence: presence,
		supported:            append([]domain.DocumentIdentity(nil), ReadDocumentIdentities(metadata, blobs)...),
		requireAuthorization: profile == documentquery.VerifiedRoots,
	}}
}

func (scope *documentWorkerScope) WorkerContext(ctx context.Context) context.Context {
	ctx = inbound.WithDocumentIdentities(inbound.WithSystemActor(ctx), scope.documents.supported)
	return outbound.WithDocumentIdentityCompatibility(ctx, inbound.DocumentIdentities(ctx), scope.documents.supported)
}

func (scope *documentWorkerScope) WithinWork(ctx context.Context, work func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if outbound.DocReadOnly(ctx) {
		return domain.ErrForbidden
	}
	return admittedWork(ctx, work)
}

func (scope *documentWorkerScope) WithinRepositoryRead(ctx context.Context, repo domain.ContentHash, read func(context.Context) error) error {
	return scope.documents.WithinDocumentSnapshot(ctx, repo, func(bound context.Context) error {
		current, cancel := context.WithCancel(bound)
		defer cancel()
		return read(current)
	})
}

func (scope *documentWorkerScope) CanYield(ctx context.Context) bool {
	owner, _ := ctx.Value(resourceYieldOwnerKey{}).(*resourceYieldOwner)
	return owner != nil && scope.documents.presence != nil && !scope.documents.presence.InTransaction(ctx)
}

func (scope *documentWorkerScope) Yield(ctx context.Context, resources outbound.RootPublicationResources) error {
	if !scope.CanYield(ctx) {
		return nil
	}
	return yieldPreparedRoot(ctx, resources)
}
