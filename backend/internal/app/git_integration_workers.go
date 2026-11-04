package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/app/repositoryquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type GitIntegrationProfile string

const (
	GitIntegrationCompatibility GitIntegrationProfile = "compatibility"
	GitIntegrationComplete      GitIntegrationProfile = "complete"
)

type GitIntegrationWorkerConfig struct {
	Profile               GitIntegrationProfile
	Metadata              outbound.MetadataStore
	Blobs                 outbound.BlobStore
	Repositories          outbound.RepositoryStore
	Reader                outbound.GitCommitReader
	Authorizer            outbound.GitSourceAuthorizer
	RequireCompleteMemory bool
}

type GitIntegrationWorkers struct {
	work                  *repositoryWork
	store                 outbound.GitScanStore
	reader                outbound.GitCommitReader
	sourceAccess          outbound.GitSourceAuthorizer
	evidence              outbound.RepositoryReader
	memory                MemoryProjectionSource
	requireCompleteMemory bool
	revisions             inbound.RepositoryStateQueries
	revisionError         error
}

func NewGitIntegrationWorkers(config GitIntegrationWorkerConfig) (*GitIntegrationWorkers, error) {
	if config.Profile != GitIntegrationCompatibility && config.Profile != GitIntegrationComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Metadata) || !rootAdapterPresent(config.Reader) ||
		(config.Blobs != nil || config.Profile == GitIntegrationComplete) && !rootAdapterPresent(config.Blobs) ||
		(config.Repositories != nil || config.Profile == GitIntegrationComplete) && !rootAdapterPresent(config.Repositories) ||
		config.Authorizer != nil && !rootAdapterPresent(config.Authorizer) {
		return nil, domain.ErrValidation
	}
	jobs, _ := config.Metadata.(outbound.GitScanStore)
	heads, _ := config.Metadata.(outbound.GitHeadScanStore)
	trees, _ := config.Metadata.(outbound.GitTreeStore)
	reader, _ := config.Reader.(outbound.GitTreeReader)
	if !rootAdapterPresent(jobs) || !rootAdapterPresent(heads) || !rootAdapterPresent(trees) || !rootAdapterPresent(reader) {
		return nil, domain.ErrValidation
	}
	transactions, _ := config.Metadata.(outbound.RepositoryTransactions)
	if (transactions != nil || config.Profile == GitIntegrationComplete) && !rootAdapterPresent(transactions) {
		return nil, domain.ErrCatalogUnsupported
	}
	work := newRepositoryWork(config.Metadata, config.Repositories, ReadDocumentIdentities(config.Metadata, config.Blobs))
	revisions, revisionError := gitIntegrationRevisionQueries(work)
	if config.Profile == GitIntegrationComplete && revisionError != nil {
		return nil, revisionError
	}
	return &GitIntegrationWorkers{work: work, store: jobs, reader: config.Reader, sourceAccess: config.Authorizer,
		evidence: config.Metadata, memory: serviceProjectionSource{config.Metadata, config.Blobs},
		requireCompleteMemory: config.RequireCompleteMemory, revisions: revisions, revisionError: revisionError}, nil
}

func gitIntegrationRevisionQueries(work *repositoryWork) (inbound.RepositoryStateQueries, error) {
	transactions, _ := work.meta.(documentQuerySnapshots)
	profile := repositoryquery.LegacyCompatibility
	if transactions != nil {
		profile = repositoryquery.PinnedSnapshots
	}
	scope := &repositoryQueryScope{metadata: work.meta, transactions: transactions, supported: work.supported}
	revisions, _ := work.meta.(repositoryquery.RevisionReader)
	history, _ := work.meta.(repositoryquery.HistoryReader)
	return repositoryquery.New(repositoryquery.Config{Profile: profile, Scope: scope, Resources: scope,
		Metadata: work.meta, Revisions: revisions, History: history, AtomicRead: transactions != nil})
}

func (workers *GitIntegrationWorkers) workerDocumentIdentityContext(ctx context.Context) context.Context {
	return (gitChangeScope{work: workers.work}).WorkerContext(ctx)
}

func (workers *GitIntegrationWorkers) workerRepository(ctx context.Context, repo domain.ContentHash) (domain.Repo, error) {
	return (gitChangeScope{work: workers.work}).ReadRepository(ctx, repo)
}

func (workers *GitIntegrationWorkers) graphEvidenceReader() outbound.RepositoryReader {
	return workers.evidence
}

func (workers *GitIntegrationWorkers) newCodeEvidence(ctx context.Context, repo domain.ContentHash) (*codeEvidence, error) {
	return newCodeEvidenceFromReader(ctx, workers.evidence, repo, workers.requireCompleteMemory)
}

func (workers *GitIntegrationWorkers) RepositoryRevision(ctx context.Context, repo domain.ContentHash) (domain.RepositoryRevision, error) {
	if workers.revisionError != nil {
		return domain.RepositoryRevision{}, workers.revisionError
	}
	return workers.revisions.RepositoryRevision(ctx, repo)
}

func (workers *GitIntegrationWorkers) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return listHistoryWithWork(ctx, workers.work, repo)
}

func (workers *GitIntegrationWorkers) verifyContributionCapture(ctx context.Context, repo, capture domain.ContentHash) error {
	return verifyContributionCaptureFromReader(ctx, workers.work.meta, repo, capture)
}

func (workers *GitIntegrationWorkers) contributionPublicationMemorySource(ctx context.Context, repo, capture domain.ContentHash, publication domain.HistoryEvent) (domain.ContentHash, error) {
	return contributionPublicationMemorySourceFromReader(ctx, workers.memory, repo, capture, publication)
}

func (workers *GitIntegrationWorkers) contributionDestination(ctx context.Context, repo domain.ContentHash, name, identity string, history []domain.HistoryEvent) (domain.Ref, error) {
	return contributionDestinationFromReader(ctx, workers.work.meta, repo, name, identity, history)
}
