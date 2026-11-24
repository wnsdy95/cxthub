package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func githubPRObservationFixture(t *testing.T) (*GitHubConnections, *store.FSStore, domain.Repo) {
	t.Helper()
	worker, storage, fixture, remote := githubFixture(t)
	ctx := context.Background()
	worker.core = NewService(storage, storage, storage)
	remote.repos = []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}
	state := startGitHub(t, worker, fixture.owner.ID, fixture.organization.NamespaceID)
	if _, err := worker.Complete(ctx, fixture.owner.ID, state, "code"); err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcile(ctx, fixture.organization.NamespaceID); err != nil {
		t.Fatal(err)
	}
	repo := domain.Repo{ID: hh(t.Name()), RepositoryID: fixture.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	connection, err := storage.GetGitHubConnection(ctx, fixture.organization.NamespaceID)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.Edit(ctx, fixture.owner.ID, fixture.organization.NamespaceID, connection.Generation, "bind", domain.GitHubBinding{RepositoryID: fixture.repository.ID, ContextRepoID: repo.ID, ExternalID: 91}, domain.GitHubTeamMapping{}); err != nil {
		t.Fatal(err)
	}
	return worker, storage, repo
}

func githubPRObservationBody(action, before, after string) []byte {
	return []byte(fmt.Sprintf(`{"installation":{"id":789},"repository":{"id":91},"action":%q,"number":7,"before":%q,"after":%q,"pull_request":{"id":701,"merged":%t,"merge_commit_sha":%q,"head":{"ref":"feature","sha":%q,"repo":{"id":91}},"base":{"ref":"main","repo":{"id":91}}}}`, action, before, after, action == "closed", strings.Repeat("c", 40), after))
}
