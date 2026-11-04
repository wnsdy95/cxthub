package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave48GitHubSourceContextKey struct{}

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

func TestWave48GitHubWorkerAuthorizationUsesCurrentBindingFence(test *testing.T) {
	lookupFailure := errors.New("source lookup failed")
	for _, scenario := range []struct {
		name string
		want error
	}{
		{name: "unchanged"},
		{name: "rename"},
		{name: "disabled", want: domain.ErrForbidden},
		{name: "suspended", want: domain.ErrForbidden},
		{name: "generation", want: domain.ErrForbidden},
		{name: "expired", want: domain.ErrForbidden},
		{name: "pending", want: domain.ErrForbidden},
		{name: "binding-removed", want: domain.ErrForbidden},
		{name: "external-repository-removed", want: domain.ErrForbidden},
		{name: "archived", want: domain.ErrForbidden},
		{name: "namespace-changed", want: domain.ErrForbidden},
		{name: "origin-changed", want: domain.ErrConflict},
		{name: "repository-binding-changed", want: domain.ErrConflict},
		{name: "connection-missing", want: domain.ErrNotFound},
		{name: "connection-error", want: lookupFailure},
		{name: "metadata-error", want: lookupFailure},
		{name: "repository-error", want: lookupFailure},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			worker, storage, repo, local, connection := wave48GitHubSourceFixture(test)
			ctx := context.Background()
			identity, observationFence, err := worker.AuthorizeGitObservation(ctx, repo.ID)
			if err != nil || identity == nil || *identity != (domain.GitProviderObservation{InstallationID: 789, RepositoryID: 91}) || observationFence == nil {
				test.Fatal("direct owner lost provider qualification", identity, err)
			}
			readFence, err := worker.AuthorizeGitRead(ctx, repo.ID)
			if err != nil || readFence == nil {
				test.Fatal("direct owner lost the read fence", err)
			}
			switch scenario.name {
			case "rename":
				connection.Repositories[0].FullName = "acme/renamed"
			case "disabled":
				connection.Enabled = false
			case "suspended":
				connection.Installation.Suspended = true
			case "generation":
				connection.Generation++
			case "expired":
				connection.CheckedAt = time.Now().Add(-11 * time.Minute)
			case "pending":
				connection.Status = "pending"
			case "binding-removed":
				connection.Bindings = nil
			case "external-repository-removed":
				connection.Repositories = nil
			case "archived":
				local.Archived = true
			case "namespace-changed":
				local.OwnerNamespaceID = "another-namespace"
			case "origin-changed":
				repo.GitRemoteURL = "https://github.com/acme/another.git"
			case "repository-binding-changed":
				repo.RepositoryID = "another-repository"
			case "connection-missing":
				storage.connectionError = domain.ErrNotFound
			case "connection-error":
				storage.connectionError = lookupFailure
			case "metadata-error":
				storage.repoError = lookupFailure
			case "repository-error":
				storage.repositoryError = lookupFailure
			}
			storage.repo, storage.repository, storage.connection = &repo, &local, &connection
			publicationContext := context.WithValue(ctx, wave48GitHubSourceContextKey{}, scenario.name)
			storage.checkContext = func(actual context.Context) {
				if actual != publicationContext {
					test.Fatal("source fence replaced the publication context")
				}
			}
			for _, fence := range []GitReadFence{observationFence, readFence} {
				if err := fence(publicationContext); !errors.Is(err, scenario.want) {
					test.Fatal("current binding fence changed", err, scenario.want)
				}
			}
		})
	}
}

func TestWave48GitHubWorkerOperatorFallbackFenceRechecksSource(test *testing.T) {
	lookupFailure := errors.New("operator source lookup failed")
	for _, scenario := range []struct {
		name string
		want error
	}{
		{name: "unchanged"},
		{name: "origin-changed"},
		{name: "new-connection", want: domain.ErrConflict},
		{name: "repository-binding-changed", want: domain.ErrConflict},
		{name: "namespace-changed", want: domain.ErrConflict},
		{name: "metadata-error", want: lookupFailure},
		{name: "repository-error", want: lookupFailure},
		{name: "connection-error", want: lookupFailure},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			worker, storage, repo, local, connection := wave48GitHubSourceFixture(test)
			ctx := context.Background()
			storage.connectionError = domain.ErrNotFound
			identity, observationFence, err := worker.AuthorizeGitObservation(ctx, repo.ID)
			if err != nil || identity != nil || observationFence == nil {
				test.Fatal("operator source became provider-qualified", identity, err)
			}
			readFence, err := worker.AuthorizeGitRead(ctx, repo.ID)
			if err != nil || readFence == nil {
				test.Fatal("operator source lost its read fence", err)
			}
			switch scenario.name {
			case "origin-changed":
				repo.GitRemoteURL = "https://github.com/acme/another.git"
			case "new-connection":
				storage.connectionError = nil
			case "repository-binding-changed":
				repo.RepositoryID = "another-repository"
			case "namespace-changed":
				local.OwnerNamespaceID = "another-namespace"
			case "metadata-error":
				storage.repoError = lookupFailure
			case "repository-error":
				storage.repositoryError = lookupFailure
			case "connection-error":
				storage.connectionError = lookupFailure
			}
			storage.repo, storage.repository, storage.connection = &repo, &local, &connection
			publicationContext := context.WithValue(ctx, wave48GitHubSourceContextKey{}, scenario.name)
			storage.checkContext = func(actual context.Context) {
				if actual != publicationContext {
					test.Fatal("operator source fence replaced the publication context")
				}
			}
			for _, fence := range []GitReadFence{observationFence, readFence} {
				if err := fence(publicationContext); !errors.Is(err, scenario.want) {
					test.Fatal("operator fallback source contract changed", err, scenario.want)
				}
			}
		})
	}
}

func TestWave48GitHubWorkerSourceLookupErrorsPropagate(test *testing.T) {
	lookupFailure := errors.New("source unavailable")
	for _, stage := range []string{"metadata", "repository", "connection"} {
		test.Run(stage, func(test *testing.T) {
			worker, storage, repo, _, _ := wave48GitHubSourceFixture(test)
			switch stage {
			case "metadata":
				storage.repoError = lookupFailure
			case "repository":
				storage.repositoryError = lookupFailure
			case "connection":
				storage.connectionError = lookupFailure
			}
			ctx := context.Background()
			if installation, path, err := worker.EvidenceInstallation(outbound.WithGitRepository(ctx, repo.ID), "/repos/acme/api/git/commits/abc"); !errors.Is(err, lookupFailure) || installation != 0 || path != "" {
				test.Fatal("evidence lookup error became fallback credentials", installation, path, err)
			}
			if identity, fence, err := worker.AuthorizeGitObservation(ctx, repo.ID); !errors.Is(err, lookupFailure) || identity != nil || fence != nil {
				test.Fatal("observation authorization swallowed a source lookup error", identity, err)
			}
			if fence, err := worker.AuthorizeGitRead(ctx, repo.ID); !errors.Is(err, lookupFailure) || fence != nil {
				test.Fatal("read authorization swallowed a source lookup error", err)
			}
		})
	}
}
