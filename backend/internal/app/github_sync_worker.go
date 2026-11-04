package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type GitHubWorkerProfile string

const (
	GitHubWorkerCompatibility GitHubWorkerProfile = "compatibility"
	GitHubWorkerComplete      GitHubWorkerProfile = "complete"
)

type GitHubWorkerConfig struct {
	Profile               GitHubWorkerProfile
	Store                 outbound.GitHubStore
	Remote                outbound.GitHubApp
	Metadata              outbound.MetadataStore
	Blobs                 outbound.BlobStore
	Repositories          outbound.RepositoryStore
	Organizations         outbound.OrganizationStore
	Teams                 outbound.TeamStore
	IdentityTransactions  outbound.IdentityTransactions
	Evidence              outbound.RepositoryReader
	Memory                outbound.MemoryObjectReader
	RequireCompleteMemory bool
}

type GitHubWorker struct {
	store         outbound.GitHubStore
	remote        outbound.GitHubApp
	metadata      outbound.MetadataStore
	repositories  outbound.RepositoryStore
	organizations outbound.OrganizationStore
	teams         outbound.TeamStore
	transactions  outbound.IdentityTransactions
	work          *repositoryWork
	publications  *prPublicationCommands
}

func NewGitHubWorker(config GitHubWorkerConfig) (*GitHubWorker, error) {
	if config.Profile != GitHubWorkerCompatibility && config.Profile != GitHubWorkerComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Metadata) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Remote, config.Blobs, config.Repositories, config.Organizations, config.Teams, config.IdentityTransactions, config.Evidence, config.Memory} {
		if dependency != nil && !rootAdapterPresent(dependency) {
			return nil, domain.ErrValidation
		}
	}
	if config.Profile == GitHubWorkerComplete {
		if err := validateGitHubWorkerDependencies(config); err != nil {
			return nil, err
		}
	}
	work := newRepositoryWork(config.Metadata, config.Repositories, ReadDocumentIdentities(config.Metadata, config.Blobs))
	revisions, revisionError := gitIntegrationRevisionQueries(work)
	if config.Profile == GitHubWorkerComplete && revisionError != nil {
		return nil, revisionError
	}
	return assembleGitHubWorker(config, work, revisions, revisionError), nil
}

func validateGitHubWorkerDependencies(config GitHubWorkerConfig) error {
	transactions, _ := config.Metadata.(outbound.RepositoryTransactions)
	snapshots, _ := config.Metadata.(documentQuerySnapshots)
	presence, _ := config.Metadata.(outbound.RepositoryTransactionPresence)
	state, _ := config.Metadata.(outbound.RepositoryTransactionState)
	revisions, _ := config.Metadata.(outbound.RepositoryRevisions)
	history, _ := config.Metadata.(outbound.HistoryStore)
	jobs, _ := config.Metadata.(outbound.PRJobStore)
	scans, _ := config.Metadata.(outbound.GitScanStore)
	access, _ := config.Repositories.(outbound.RepositoryAccessLocker)
	reader := config.Evidence
	if reader == nil {
		reader = config.Metadata
	}
	evidence, _ := reader.(outbound.GraphEvidenceReader)
	for _, dependency := range []any{config.Remote, config.Blobs, config.Repositories, config.Organizations, config.Teams, config.IdentityTransactions, transactions, snapshots, presence, state, revisions, history, jobs, scans, access, evidence} {
		if !rootAdapterPresent(dependency) {
			return domain.ErrCatalogUnsupported
		}
	}
	if _, supported := config.Remote.(outbound.GitHubPRHistory); supported {
		history, _ := config.Metadata.(outbound.PRHistoryStore)
		contributions, _ := config.Metadata.(outbound.PRContributionStore)
		if !rootAdapterPresent(history) || !rootAdapterPresent(contributions) {
			return domain.ErrCatalogUnsupported
		}
	}
	return nil
}

func assembleGitHubWorker(config GitHubWorkerConfig, work *repositoryWork, revisions inbound.RepositoryStateQueries, revisionError error) *GitHubWorker {
	evidence := config.Evidence
	if evidence == nil {
		evidence = config.Metadata
	}
	memory := config.Memory
	if memory == nil {
		memory = serviceProjectionSource{config.Metadata, config.Blobs}
	}
	return &GitHubWorker{store: config.Store, remote: config.Remote, metadata: config.Metadata,
		repositories: config.Repositories, organizations: config.Organizations, teams: config.Teams,
		transactions: config.IdentityTransactions, work: work,
		publications: &prPublicationCommands{work: work, evidence: evidence, memory: memory,
			requireCompleteMemory: config.RequireCompleteMemory, revisions: revisions, revisionError: revisionError}}
}

func (worker *GitHubWorker) withIdentity(ctx context.Context, execute func(context.Context) error) error {
	return withIdentityTransactions(ctx, worker.transactions, execute)
}

func (worker *GitHubWorker) workerDocumentIdentityContext(ctx context.Context) context.Context {
	return (gitChangeScope{work: worker.work}).WorkerContext(ctx)
}

func (worker *GitHubWorker) ListHistory(ctx context.Context, repo domain.ContentHash) ([]domain.HistoryEvent, error) {
	return listHistoryWithWork(ctx, worker.work, repo)
}

func (worker *GitHubWorker) organizationRoleOf(ctx context.Context, organization, user string) (domain.OrganizationRole, bool) {
	return organizationRoleFromReader(ctx, worker.organizations, organization, user)
}

func (worker *GitHubWorker) checkTeamDocumentIdentities(ctx context.Context, team string) error {
	return checkTeamDocumentIdentitiesFromStores(ctx, worker.repositories, worker.teams, team)
}
