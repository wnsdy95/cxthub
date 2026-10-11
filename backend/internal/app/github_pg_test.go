//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGGitHubSyncContract(t *testing.T) {
	core, st, _ := collaborationPG(t)
	f := makeTeamFixture(t, st)
	remote := &githubFake{proof: outbound.GitHubAuthorization{Identity: domain.GitHubIdentity{ExternalID: time.Now().UnixMicro(), Login: "operator"}, Installation: domain.GitHubInstallation{ID: time.Now().UnixMicro(), AccountID: 456, Kind: "Organization", Login: "acme"}}}
	g := NewGitHubConnections(f.identity, core, st, remote)
	runGitHubSyncContract(t, g, f, remote)
	// Every write shares the identity transaction; aborted mappings never leak.
	ctx := context.Background()
	sentinel := errors.New("rollback")
	err := st.WithinIdentity(ctx, func(tx context.Context) error {
		c, e := st.GetGitHubConnection(tx, f.organization.NamespaceID)
		if e != nil {
			return e
		}
		c.Enabled = true
		if e = st.PutGitHubConnection(tx, c); e != nil {
			return e
		}
		if e = st.ReplaceGitHubTeamMembers(tx, c.NamespaceID, []domain.GitHubTeamMember{}); e != nil {
			return e
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	c, err := st.GetGitHubConnection(ctx, f.organization.NamespaceID)
	if err != nil || c.Enabled {
		t.Fatalf("rollback leaked: %+v %v", c, err)
	}
}

type githubObservationRollbackPG struct {
	*store.PostgresStore
	rejectRevision bool
}

func (storage *githubObservationRollbackPG) AdvanceEvidenceRevision(ctx context.Context, repo domain.ContentHash) error {
	if err := storage.PostgresStore.AdvanceEvidenceRevision(ctx, repo); err != nil {
		return err
	}
	if storage.rejectRevision {
		return domain.ErrIntegrity
	}
	return nil
}

func TestPGGitHubPRObservationRollbackAndConflicts(t *testing.T) {
	for _, action := range []string{"synchronize", "closed"} {
		t.Run(action, func(t *testing.T) {
			core, storage, _ := collaborationPG(t)
			fixture := makeTeamFixture(t, storage)
			ctx := context.Background()
			installationID := time.Now().UnixMicro()
			repo := domain.Repo{ID: hh(fmt.Sprintf("%s-%d", t.Name(), installationID)), RepositoryID: fixture.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
			if _, err := storage.PutRepo(ctx, repo); err != nil {
				t.Fatal(err)
			}
			connection := domain.GitHubConnection{NamespaceID: fixture.organization.NamespaceID, Installation: domain.GitHubInstallation{ID: installationID, AccountID: 456, Kind: "Organization", Login: "acme"}, Enabled: true, Generation: 1, Status: "connected", CheckedAt: time.Now().UTC(), Repositories: []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}, Bindings: []domain.GitHubBinding{{RepositoryID: fixture.repository.ID, ContextRepoID: repo.ID, ExternalID: 91, GitOrigin: repo.GitRemoteURL}}}
			if err := storage.WithinIdentity(ctx, func(tx context.Context) error {
				return storage.PutGitHubConnection(tx, connection)
			}); err != nil {
				t.Fatal(err)
			}
			failing := &githubObservationRollbackPG{PostgresStore: storage, rejectRevision: true}
			core.meta = failing
			worker := NewGitHubConnections(fixture.identity, core, storage, &githubFake{})
			body := githubPRObservationBody(action, strings.Repeat("a", 40), strings.Repeat("b", 40))
			body = []byte(strings.Replace(string(body), `"id":789`, fmt.Sprintf(`"id":%d`, installationID), 1))
			deliveryID := "pg-observation-" + string(repo.ID)
			if err := worker.Receive(ctx, deliveryID, "pull_request", body); err != nil {
				t.Fatal(err)
			}
			before, err := storage.RepositoryRevision(ctx, repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := worker.deliver(ctx, githubHash(deliveryID)); err != nil {
				t.Fatal(err)
			}
			database, err := pgx.Connect(ctx, os.Getenv("CXT_TEST_DSN"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close(ctx)
			assertCount := func(table string, expected int) {
				t.Helper()
				var count int
				if err := database.QueryRow(ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", repo.ID).Scan(&count); err != nil || count != expected {
					t.Fatalf("%s count = %d, expected %d: %v", table, count, expected, err)
				}
			}
			for _, table := range []string{"git_ref_observations", "git_scan_jobs", "pr_promotion_jobs"} {
				assertCount(table, 0)
			}
			after, err := storage.RepositoryRevision(ctx, repo.ID)
			if err != nil || before != after {
				t.Fatalf("revision escaped rollback: %+v -> %+v: %v", before, after, err)
			}
			delivery, err := storage.GetGitHubDelivery(ctx, githubHash(deliveryID))
			if err != nil || delivery.Done || string(delivery.Body) != string(body) {
				t.Fatalf("rollback acknowledged webhook: %+v %v", delivery, err)
			}
			failing.rejectRevision = false
			if err := storage.WithinIdentity(ctx, func(tx context.Context) error {
				delivery.NextAttempt = time.Now().UTC()
				return storage.PutGitHubDelivery(tx, delivery)
			}); err != nil {
				t.Fatal(err)
			}
			restarted := NewGitHubConnections(fixture.identity, core, storage, &githubFake{})
			if err := restarted.deliver(ctx, githubHash(deliveryID)); err != nil {
				t.Fatal(err)
			}
			assertCount("git_ref_observations", 1)
			assertCount("git_scan_jobs", 2)
			if action == "closed" {
				assertCount("pr_promotion_jobs", 1)
			} else {
				assertCount("pr_promotion_jobs", 0)
			}
			after, err = storage.RepositoryRevision(ctx, repo.ID)
			if err != nil || after.Evidence != before.Evidence+1 || after.Graph != before.Graph {
				t.Fatalf("incorrect committed revision: %+v -> %+v: %v", before, after, err)
			}
			delivery, err = storage.GetGitHubDelivery(ctx, githubHash(deliveryID))
			if err != nil || !delivery.Done || len(delivery.Body) != 0 {
				t.Fatalf("committed evidence not acknowledged: %+v %v", delivery, err)
			}
			var raw []byte
			if err := database.QueryRow(ctx, `SELECT payload FROM git_ref_observations WHERE repo_id=$1`, repo.ID).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			var observation domain.GitRefObservation
			if err := json.Unmarshal(raw, &observation); err != nil || observation.Validate() != nil || observation.PullRequest.ID != 701 || observation.PullRequest.InstallationID != installationID || observation.Source != "github-pull-request" {
				t.Fatalf("lost normalized metadata: %+v %v", observation, err)
			}
			if err := storage.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
				return storage.RecordGitRefObservation(tx, observation, time.Now().UTC())
			}); err != nil {
				t.Fatalf("exact duplicate rejected: %v", err)
			}
			for _, mutate := range []func(*domain.GitRefObservation){
				func(changed *domain.GitRefObservation) { changed.After = strings.Repeat("d", 40) },
				func(changed *domain.GitRefObservation) { changed.PullRequest.ID++ },
			} {
				changed := observation
				metadata := *observation.PullRequest
				changed.PullRequest = &metadata
				mutate(&changed)
				if err := changed.Validate(); err != nil {
					t.Fatal(err)
				}
				if err := storage.WithinRepository(ctx, repo.ID, func(tx context.Context) error {
					return storage.RecordGitRefObservation(tx, changed, time.Now().UTC())
				}); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("conflicting duplicate accepted: %v", err)
				}
			}
			assertCount("git_ref_observations", 1)
			assertCount("git_scan_jobs", 2)
			var unchanged bool
			if err := database.QueryRow(ctx, `SELECT payload=$2::jsonb FROM git_ref_observations WHERE repo_id=$1`, repo.ID, raw).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("conflict replaced accepted evidence: %v", err)
			}
		})
	}
}
