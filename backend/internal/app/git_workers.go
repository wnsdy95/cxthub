package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/gitchangeworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type GitChangeWorkerConfig struct {
	Profile      gitchangeworker.Profile
	Metadata     outbound.MetadataStore
	Blobs        outbound.BlobStore
	Repositories outbound.RepositoryStore
	Reader       outbound.GitEvidenceReader
	Authorizer   outbound.GitSourceAuthorizer
}

type gitChangeScope struct {
	work *repositoryWork
}

func (scope gitChangeScope) WorkerContext(ctx context.Context) context.Context {
	return outbound.WithDocumentIdentityCompatibility(inbound.WithDocumentIdentities(ctx, scope.work.supported), scope.work.supported, scope.work.supported)
}

func (gitChangeScope) WithinWork(ctx context.Context, operation func(context.Context) error) error {
	return admittedWork(ctx, operation)
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

func (scope gitChangeScope) WithinEvidenceWrite(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return repositoryWriteErrorWithWork(evidenceWriteContext(ctx), scope.work, repo, operation)
}

func NewGitChangeWorker(config GitChangeWorkerConfig) (*gitchangeworker.Worker, error) {
	jobs, _ := config.Metadata.(outbound.GitChangeStore)
	if !rootAdapterPresent(jobs) || !rootAdapterPresent(config.Reader) {
		return nil, domain.ErrValidation
	}
	return newGitChangeWorker(config, jobs)
}

func newGitChangeWorker(config GitChangeWorkerConfig, jobs outbound.GitChangeStore) (*gitchangeworker.Worker, error) {
	if config.Profile != gitchangeworker.Compatibility && config.Profile != gitchangeworker.Complete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Metadata) ||
		(config.Reader != nil || config.Profile == gitchangeworker.Complete) && !rootAdapterPresent(config.Reader) ||
		(config.Blobs != nil || config.Profile == gitchangeworker.Complete) && !rootAdapterPresent(config.Blobs) ||
		(config.Repositories != nil || config.Profile == gitchangeworker.Complete) && !rootAdapterPresent(config.Repositories) {
		return nil, domain.ErrValidation
	}
	transactions, _ := config.Metadata.(outbound.RepositoryTransactions)
	if (transactions != nil || config.Profile == gitchangeworker.Complete) && !rootAdapterPresent(transactions) {
		return nil, domain.ErrCatalogUnsupported
	}
	cache, _ := config.Metadata.(outbound.GitScanStore)
	return gitchangeworker.New(gitchangeworker.Config{
		Profile: config.Profile, Scope: gitChangeScope{work: newRepositoryWork(config.Metadata, config.Repositories, ReadDocumentIdentities(config.Metadata, config.Blobs))},
		Jobs: jobs, Metadata: config.Metadata, Reader: config.Reader, Cache: cache, Authorizer: config.Authorizer,
	})
}

func (changes *GitChanges) worker() (*gitchangeworker.Worker, error) {
	return newGitChangeWorker(GitChangeWorkerConfig{
		Profile: gitchangeworker.Compatibility, Metadata: changes.core.meta, Blobs: changes.core.blobs,
		Repositories: changes.core.repositories, Reader: changes.reader, Authorizer: changes.sourceAccess,
	}, changes.store)
}
