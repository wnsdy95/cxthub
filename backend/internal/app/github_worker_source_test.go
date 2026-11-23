package app

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave48GitHubSourceStore struct {
	*store.FSStore
	repo            *domain.Repo
	repository      *domain.Repository
	connection      *domain.GitHubConnection
	repoError       error
	repositoryError error
	connectionError error
	checkContext    func(context.Context)
}

func (storage *wave48GitHubSourceStore) GetRepo(ctx context.Context, repoID domain.ContentHash) (domain.Repo, error) {
	if storage.checkContext != nil {
		storage.checkContext(ctx)
	}
	if storage.repoError != nil {
		return domain.Repo{}, storage.repoError
	}
	if storage.repo != nil {
		return *storage.repo, nil
	}
	return storage.FSStore.GetRepo(ctx, repoID)
}

func (storage *wave48GitHubSourceStore) GetRepository(ctx context.Context, repositoryID string) (domain.Repository, error) {
	if storage.checkContext != nil {
		storage.checkContext(ctx)
	}
	if storage.repositoryError != nil {
		return domain.Repository{}, storage.repositoryError
	}
	if storage.repository != nil {
		return *storage.repository, nil
	}
	return storage.FSStore.GetRepository(ctx, repositoryID)
}

func (storage *wave48GitHubSourceStore) GetGitHubConnection(ctx context.Context, namespaceID string) (domain.GitHubConnection, error) {
	if storage.checkContext != nil {
		storage.checkContext(ctx)
	}
	if storage.connectionError != nil {
		return domain.GitHubConnection{}, storage.connectionError
	}
	if storage.connection != nil {
		return *storage.connection, nil
	}
	return storage.FSStore.GetGitHubConnection(ctx, namespaceID)
}

func wave48GitHubSourceFixture(test *testing.T) (*GitHubWorker, *wave48GitHubSourceStore, domain.Repo, domain.Repository, domain.GitHubConnection) {
	test.Helper()
	_, original, repo := githubPRObservationFixture(test)
	ctx := context.Background()
	local, err := original.GetRepository(ctx, repo.RepositoryID)
	if err != nil {
		test.Fatal(err)
	}
	connection, err := original.GetGitHubConnection(ctx, local.OwnerNamespaceID)
	if err != nil {
		test.Fatal(err)
	}
	storage := &wave48GitHubSourceStore{FSStore: original}
	worker, err := NewGitHubWorker(GitHubWorkerConfig{Profile: GitHubWorkerCompatibility, Store: storage, Metadata: storage, Repositories: storage})
	if err != nil {
		test.Fatal(err)
	}
	return worker, storage, repo, local, connection
}

func TestWave48GitHubWorkerEvidenceInstallationPreservesRenameScopeAndFallback(test *testing.T) {
	worker, storage, repo, _, connection := wave48GitHubSourceFixture(test)
	ctx := context.Background()
	connection.Repositories[0].FullName = "acme/Renamed"
	if err := storage.PutGitHubConnection(ctx, connection); err != nil {
		test.Fatal(err)
	}
	scoped := outbound.WithGitRepository(ctx, repo.ID)
	for _, path := range []string{"/repos/acme/api/git/commits/AbC", "/REPOS/ACME/API/git/commits/AbC"} {
		installation, rewritten, err := worker.EvidenceInstallation(scoped, path)
		if err != nil || installation != 789 || rewritten != "/repos/acme/Renamed/git/commits/AbC" {
			test.Fatal("immutable binding rename or suffix changed", installation, rewritten, err)
		}
	}
	stored, err := storage.GetRepo(ctx, repo.ID)
	if err != nil || !reflect.DeepEqual(stored, repo) {
		test.Fatal("evidence resolution rewrote stored Git provenance", stored, err)
	}
	for _, path := range []string{"/repos/another/private/git/commits/AbC", "/repos/acme/api-other/git/commits/AbC", "/repos/acme/api"} {
		if installation, rewritten, err := worker.EvidenceInstallation(scoped, path); !errors.Is(err, domain.ErrForbidden) || installation != 0 || rewritten != "" {
			test.Fatal("foreign repository prefix accepted", path, installation, rewritten, err)
		}
	}
	if _, _, err := worker.EvidenceInstallation(ctx, "/repos/acme/api/git/commits/AbC"); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("unscoped evidence access accepted", err)
	}
	storage.connectionError = domain.ErrNotFound
	path := "/repos/operator/private/git/commits/AbC"
	if installation, rewritten, err := worker.EvidenceInstallation(scoped, path); err != nil || installation != 0 || rewritten != path {
		test.Fatal("operator fallback changed the requested path", installation, rewritten, err)
	}
	storage.connectionError = nil
	connection.Enabled = false
	storage.connection = &connection
	if _, _, err := worker.EvidenceInstallation(scoped, "/repos/acme/api/git/commits/AbC"); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("disabled binding fell back to operator credentials", err)
	}
}
