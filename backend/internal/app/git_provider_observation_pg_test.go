//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func providerObservationPGFixture(t *testing.T) (*GitHubConnections, *store.PostgresStore, domain.Repo) {
	t.Helper()
	ctx := context.Background()
	storage, err := store.NewPostgresStore(ctx, a11IsolatedPGDSN(t, ctx))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../schemas/db/migrations"); err != nil {
		t.Fatal(err)
	}
	fixture := makeTeamFixture(t, storage)
	repo := domain.Repo{ID: hh(t.Name()), RepositoryID: fixture.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		t.Fatal(err)
	}
	connection := domain.GitHubConnection{NamespaceID: fixture.organization.NamespaceID, Installation: domain.GitHubInstallation{ID: 789, AccountID: 456, Kind: "Organization", Login: "acme"}, Enabled: true, Generation: 1, Status: "connected", CheckedAt: time.Now().UTC(), Repositories: []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}, Bindings: []domain.GitHubBinding{{RepositoryID: fixture.repository.ID, ContextRepoID: repo.ID, ExternalID: 91, GitOrigin: repo.GitRemoteURL}}}
	if err := storage.WithinIdentity(ctx, func(tx context.Context) error { return storage.PutGitHubConnection(tx, connection) }); err != nil {
		t.Fatal(err)
	}
	core := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	return NewGitHubConnections(fixture.identity, core, storage, &githubFake{}), storage, repo
}

func TestPGGitProviderObservationReconcileBindingFence(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(fmt.Sprint(revoke), func(t *testing.T) {
			worker, storage, repo := providerObservationPGFixture(t)
			runProviderObservationReconcile(t, worker, storage, repo, revoke)
		})
	}
}

func TestPGGitProviderObservationPublicationRollback(t *testing.T) {
	for _, source := range []string{"github-push", "reconciliation"} {
		t.Run(source, func(t *testing.T) {
			worker, storage, repo := providerObservationPGFixture(t)
			ctx := context.Background()
			failing := &githubObservationRollbackPG{PostgresStore: storage, rejectRevision: true}
			worker.core.meta = failing
			before, err := storage.RepositoryRevision(ctx, repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			expected := providerObservationExpected(repo, source, "")
			var scans *GitScans
			if source == "github-push" {
				expected.Delivery = "rollback-push"
				expected = expected.WithID()
				body := []byte(fmt.Sprintf(`{"installation":{"id":789},"repository":{"id":91},"ref":"refs/heads/main","after":"%s"}`, expected.After))
				err := worker.applyDelivery(ctx, domain.GitHubDelivery{ID: expected.Delivery, Kind: "push", Body: body})
				if !errors.Is(err, domain.ErrIntegrity) {
					t.Fatalf("revision failure not returned: %v", err)
				}
			} else {
				reader := &scanReader{headPages: map[int][]outbound.GitHead{1: {{Ref: "refs/heads/main", Commit: strings.Repeat("a", 40)}}}}
				scans, err = NewGitScans(worker.core, reader)
				if err != nil {
					t.Fatal(err)
				}
				scans.SetSourceAuthorizer(worker)
				if err := scans.Reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				checkpoint, err := storage.GetGitHeadScan(ctx, repo.ID, repo.GitRemoteURL)
				if err != nil || checkpoint.State != "running" || checkpoint.Page != 1 || checkpoint.LeaseUntil.IsZero() {
					t.Fatalf("checkpoint escaped rollback: %+v %v", checkpoint, err)
				}
			}
			if _, err := storage.GetPRRefObservation(ctx, repo.ID, repo.GitRemoteURL, expected.ID); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("observation escaped rollback: %v", err)
			}
			receipts, err := storage.ListGitHeadReceipts(ctx, repo.ID, repo.GitRemoteURL, 0, 10)
			if err != nil || len(receipts) != 0 {
				t.Fatalf("qualified read receipt escaped rollback: %+v %v", receipts, err)
			}
			jobs, err := storage.ListGitScans(ctx, repo.ID, "", 10)
			if err != nil || len(jobs) != 0 {
				t.Fatalf("endpoint jobs escaped rollback: %+v %v", jobs, err)
			}
			after, err := storage.RepositoryRevision(ctx, repo.ID)
			if err != nil || before != after {
				t.Fatalf("revision escaped rollback: %+v -> %+v %v", before, after, err)
			}
		})
	}
}
