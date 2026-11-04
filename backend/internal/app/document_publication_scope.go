package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentPublicationScope struct {
	work *repositoryWork
}

func (service *Service) DocumentPublicationCommands() (inbound.DocumentPublicationCommands, error) {
	return service.documentPublications(DocumentPublicationComplete)
}

func (service *Service) documentPublications(profile DocumentPublicationProfile) (*DocumentPublicationCommands, error) {
	if service == nil {
		return nil, domain.ErrCatalogUnsupported
	}
	work := service.repositoryWork()
	if profile == DocumentPublicationComplete {
		if err := work.validateCompleteAuthority(); err != nil {
			return nil, err
		}
	}
	identities := service.DocumentIdentitiesSupported()
	sources := documentPublicationSourcesFromStores(service.meta, service.blobs, service.engine, identities)
	verified, _ := service.blobs.(outbound.VerifiedDocStore)
	if sources.stored != nil && !rootAdapterPresent(sources.stored) {
		return nil, domain.ErrValidation
	}
	var sourcePort DocumentPublicationSources = sources
	if profile == DocumentPublicationComplete && sources.stored == nil {
		sourcePort = nil
	}
	return NewDocumentPublicationCommands(DocumentPublicationCommandsConfig{
		Profile: profile, Store: service.meta, Chunks: service.blobs,
		Scope: &documentPublicationScope{work: work}, Sources: sourcePort,
		Verified: verified, LegacyWriter: service.blobs, SupportedIdentities: identities,
	})
}

func (scope *documentPublicationScope) WithinDocumentCommit(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.CommitOutput, error)) (inbound.CommitOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.CommitOutput{}, domain.ErrValidation
	}
	ctx, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		return inbound.CommitOutput{}, err
	}
	defer release()
	ctx = context.WithValue(auditOperation(ctx, "context.objects.stored"), revisionScopeKey{}, "none")
	return repositoryWriteWithWork(ctx, scope.work, repo, write)
}

func (scope *documentPublicationScope) WithinChunkStore(ctx context.Context, repo domain.ContentHash, write func(context.Context) (inbound.StoreChunksOutput, error)) (inbound.StoreChunksOutput, error) {
	if scope == nil || scope.work == nil || write == nil {
		return inbound.StoreChunksOutput{}, domain.ErrValidation
	}
	ctx, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		return inbound.StoreChunksOutput{}, err
	}
	defer release()
	ctx = context.WithValue(auditOperation(ctx, "context.chunks.stored"), revisionScopeKey{}, "none")
	return repositoryWriteWithWork(ctx, scope.work, repo, write)
}

func (scope *documentPublicationScope) GraphChanged(ctx context.Context, repo domain.ContentHash) error {
	if scope == nil || scope.work == nil {
		return domain.ErrValidation
	}
	return scope.work.revisionWrite(context.WithValue(ctx, revisionScopeKey{}, "graph"), repo)
}
