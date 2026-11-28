package app

import (
	"context"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type GitHubWorkerProfile string

const (
	GitHubWorkerCompatibility GitHubWorkerProfile = "compatibility"
	GitHubWorkerComplete      GitHubWorkerProfile = "complete"
)

type GitHubWorkerConfig struct {
	Profile              GitHubWorkerProfile
	Store                outbound.GitHubStore
	Remote               outbound.GitHubApp
	Metadata             outbound.MetadataStore
	Blobs                outbound.BlobStore
	Repositories         outbound.RepositoryStore
	Organizations        outbound.OrganizationStore
	Teams                outbound.TeamStore
	IdentityTransactions outbound.IdentityTransactions
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
}

func NewGitHubWorker(config GitHubWorkerConfig) (*GitHubWorker, error) {
	if config.Profile != GitHubWorkerCompatibility && config.Profile != GitHubWorkerComplete {
		return nil, domain.ErrValidation
	}
	if !rootAdapterPresent(config.Store) || !rootAdapterPresent(config.Metadata) {
		return nil, domain.ErrValidation
	}
	for _, dependency := range []any{config.Remote, config.Blobs, config.Repositories, config.Organizations, config.Teams, config.IdentityTransactions} {
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
	return assembleGitHubWorker(config, work), nil
}

// validateGitHubWorkerDependencies checks what the worker writes through: the
// repository transaction in which a push is carried into the branch ledger,
// and the identity stores it reconciles.
func validateGitHubWorkerDependencies(config GitHubWorkerConfig) error {
	transactions, _ := config.Metadata.(outbound.RepositoryTransactions)
	access, _ := config.Repositories.(outbound.RepositoryAccessLocker)
	for _, dependency := range []any{config.Remote, config.Blobs, config.Repositories, config.Organizations, config.Teams, config.IdentityTransactions, transactions, access} {
		if !rootAdapterPresent(dependency) {
			return domain.ErrCatalogUnsupported
		}
	}
	return nil
}

func assembleGitHubWorker(config GitHubWorkerConfig, work *repositoryWork) *GitHubWorker {
	return &GitHubWorker{store: config.Store, remote: config.Remote, metadata: config.Metadata,
		repositories: config.Repositories, organizations: config.Organizations, teams: config.Teams,
		transactions: config.IdentityTransactions, work: work,
	}
}

func (worker *GitHubWorker) withIdentity(ctx context.Context, execute func(context.Context) error) error {
	return withIdentityTransactions(ctx, worker.transactions, execute)
}

func (worker *GitHubWorker) workerDocumentIdentityContext(ctx context.Context) context.Context {
	return (gitChangeScope{work: worker.work}).WorkerContext(ctx)
}

func (worker *GitHubWorker) organizationRoleOf(ctx context.Context, organization, user string) (domain.OrganizationRole, bool) {
	return organizationRoleFromReader(ctx, worker.organizations, organization, user)
}

func (worker *GitHubWorker) checkTeamDocumentIdentities(ctx context.Context, team string) error {
	return checkTeamDocumentIdentitiesFromStores(ctx, worker.repositories, worker.teams, team)
}
