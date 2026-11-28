//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type githubSyncPGFixture struct {
	ctx        context.Context
	core       *Service
	storage    *store.PostgresStore
	peer       *store.PostgresStore
	observer   *pgx.Conn
	team       teamFixture
	remote     *githubFake
	repo       domain.Repo
	connection domain.GitHubConnection
}

func newGitHubSyncPGFixture(test *testing.T) *githubSyncPGFixture {
	test.Helper()
	core, storage, _ := collaborationPG(test)
	ctx, cancel := context.WithTimeout(wave40PGContext(context.Background(), storage), 30*time.Second)
	test.Cleanup(cancel)
	team := makeTeamFixture(test, storage)
	repo := domain.Repo{ID: hh(test.Name() + domain.NewID("")), RepositoryID: team.repository.ID, DefaultBranch: "main", GitRemoteURL: "https://github.com/acme/api.git"}
	if _, err := storage.PutRepo(ctx, repo); err != nil {
		test.Fatal(err)
	}
	remote := &githubFake{proof: outbound.GitHubAuthorization{Identity: domain.GitHubIdentity{ExternalID: 123, Login: "operator"}, Installation: domain.GitHubInstallation{ID: time.Now().UnixMicro(), AccountID: 456, Kind: "Organization", Login: "acme"}}, repos: []domain.GitHubRepository{{ID: 91, AccountID: 456, FullName: "acme/api"}}}
	connection := domain.GitHubConnection{NamespaceID: team.organization.NamespaceID, Installation: remote.proof.Installation, Enabled: true, Generation: 1, Status: "connected", CheckedAt: time.Now().UTC(), Repositories: remote.repos, Bindings: []domain.GitHubBinding{{RepositoryID: team.repository.ID, ContextRepoID: repo.ID, ExternalID: 91, GitOrigin: repo.GitRemoteURL}}}
	if err := storage.WithinIdentity(ctx, func(bound context.Context) error {
		return storage.PutGitHubConnection(bound, connection)
	}); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	config, err := pgxpool.ParseConfig(collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	observer, err := pgx.ConnectConfig(ctx, config.ConnConfig.Copy())
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := observer.Close(cleanup); err != nil {
			test.Error(err)
		}
	})
	return &githubSyncPGFixture{ctx: ctx, core: core, storage: storage, peer: peer, observer: observer, team: team, remote: remote, repo: repo, connection: connection}
}

func (fixture *githubSyncPGFixture) config(storage *store.PostgresStore, remote outbound.GitHubApp) GitHubWorkerConfig {
	return GitHubWorkerConfig{Profile: GitHubWorkerComplete, Store: storage, Remote: remote, Metadata: storage, Blobs: storage, Repositories: storage, Organizations: storage, Teams: storage, IdentityTransactions: storage}
}

func newGitHubSyncPGOwner(test *testing.T, config GitHubWorkerConfig) *GitHubWorker {
	test.Helper()
	owner, err := NewGitHubWorker(config)
	if err != nil {
		test.Fatal("complete public GitHub owner", err)
	}
	return owner
}

func (fixture *githubSyncPGFixture) updateConnection(test *testing.T, change func(*domain.GitHubConnection)) domain.GitHubConnection {
	test.Helper()
	var connection domain.GitHubConnection
	if err := fixture.peer.WithinIdentity(fixture.ctx, func(bound context.Context) error {
		var err error
		connection, err = fixture.peer.GetGitHubConnection(bound, fixture.connection.NamespaceID)
		if err != nil {
			return err
		}
		change(&connection)
		return fixture.peer.PutGitHubConnection(bound, connection)
	}); err != nil {
		test.Fatal(err)
	}
	return connection
}

func (fixture *githubSyncPGFixture) receive(test *testing.T, action string) domain.GitHubDelivery {
	test.Helper()
	body := githubPRObservationBody(action, strings.Repeat("a", 40), strings.Repeat("b", 40))
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		test.Fatal(err)
	}
	installation, err := json.Marshal(map[string]int64{"id": fixture.connection.Installation.ID})
	if err != nil {
		test.Fatal(err)
	}
	payload["installation"] = installation
	body, err = json.Marshal(payload)
	if err != nil {
		test.Fatal(err)
	}
	id := "public-owner-" + string(fixture.repo.ID)
	commands := NewGitHubConnections(fixture.team.identity, fixture.core, fixture.storage, fixture.remote)
	if err := commands.Receive(fixture.ctx, id, "pull_request", body); err != nil {
		test.Fatal(err)
	}
	delivery, err := fixture.peer.GetGitHubDelivery(fixture.ctx, githubHash(id))
	if err != nil || delivery.Done || delivery.Attempts != 0 {
		test.Fatal("durable webhook fixture", delivery, err)
	}
	return delivery
}

func (fixture *githubSyncPGFixture) count(test *testing.T, table string, expected int) {
	test.Helper()
	var count int
	if err := fixture.observer.QueryRow(fixture.ctx, "SELECT count(*) FROM "+table+" WHERE repo_id=$1", fixture.repo.ID).Scan(&count); err != nil || count != expected {
		test.Fatal("independent durable count", table, count, expected, err)
	}
}

func TestPGGitHubWorkerReconciliationCurrentAuthorityAndLease(test *testing.T) {
	wave40IsolatedPG(test)
	for _, scenario := range []string{"current", "organization_revoked", "team_deleted", "generation_changed", "lease_replaced"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubSyncPGFixture(test)
			if err := fixture.team.identity.SetTeamRepository(fixture.ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.team.ID, fixture.team.repository.ID, domain.RolePuller); err != nil {
				test.Fatal(err)
			}
			memberID := fixture.connection.Installation.ID
			if err := fixture.storage.WithinIdentity(fixture.ctx, func(bound context.Context) error {
				return fixture.storage.PutGitHubIdentity(bound, domain.GitHubIdentity{UserID: fixture.team.member.ID, ExternalID: memberID, Login: "member"})
			}); err != nil {
				test.Fatal(err)
			}
			fixture.updateConnection(test, func(connection *domain.GitHubConnection) {
				connection.Mappings = []domain.GitHubTeamMapping{{TeamID: fixture.team.team.ID, ExternalID: 99, SyncMembers: true}}
			})
			fixture.remote.members = []int64{memberID, memberID + 1}
			called := false
			var replacement domain.GitHubConnection
			fixture.remote.onMembers = func() {
				called = true
				deadline, bounded := fixture.ctx.Deadline()
				if !bounded || time.Until(deadline) <= 0 {
					test.Fatal("unbounded provider operation")
				}
				if err := fixture.peer.WithinIdentity(fixture.ctx, func(bound context.Context) error {
					current, err := fixture.peer.GetGitHubConnection(bound, fixture.connection.NamespaceID)
					if err != nil || current.NextSync.Before(time.Now().Add(110*time.Second)) || current.NextSync.After(time.Now().Add(2*time.Minute)) {
						return errors.Join(domain.ErrIntegrity, err)
					}
					return nil
				}); err != nil {
					test.Fatal("provider held identity lock or lost two-minute reservation", err)
				}
				switch scenario {
				case "organization_revoked":
					commands := NewIdentityService(nil, fixture.peer)
					if err := commands.OffboardOrganizationMember(fixture.ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.member.ID, "retain"); err != nil {
						test.Fatal(err)
					}
				case "team_deleted":
					commands := NewIdentityService(nil, fixture.peer)
					if err := commands.DeleteTeam(fixture.ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.team.ID); err != nil {
						test.Fatal(err)
					}
				case "generation_changed", "lease_replaced":
					replacement = fixture.updateConnection(test, func(connection *domain.GitHubConnection) {
						if scenario == "generation_changed" {
							connection.Generation++
						} else {
							connection.NextSync = time.Now().UTC().Add(3 * time.Minute)
						}
					})
				}
			}
			owner := newGitHubSyncPGOwner(test, fixture.config(fixture.storage, fixture.remote))
			err := owner.reconcile(fixture.ctx, fixture.connection.NamespaceID)
			fenced := scenario == "generation_changed" || scenario == "lease_replaced"
			if !called || (fenced && !errors.Is(err, domain.ErrConflict)) || (!fenced && err != nil) {
				test.Fatal("actual reconciliation lost current authority/lease fence", called, err)
			}
			members, err := fixture.peer.ListTeamMembers(fixture.ctx, fixture.team.team.ID)
			if err != nil || (scenario == "current" && (len(members) != 1 || members[0].Source != "github" || members[0].UserID != fixture.team.member.ID)) || (scenario != "current" && len(members) != 0) {
				test.Fatal("stale or unverified synchronized membership", members, err)
			}
			current, err := fixture.peer.GetGitHubConnection(fixture.ctx, fixture.connection.NamespaceID)
			if err != nil {
				test.Fatal(err)
			}
			if fenced && !reflect.DeepEqual(current, replacement) {
				test.Fatal("stale reconcile overwrote winning reservation", current, replacement)
			}
			if scenario == "current" {
				role, allowed := fixture.team.identity.RoleOf(fixture.ctx, fixture.team.repository.ID, fixture.team.member.ID)
				var expires time.Time
				err := fixture.observer.QueryRow(fixture.ctx, `SELECT expires_at FROM github_team_members WHERE namespace_id=$1 AND user_id=$2`, current.NamespaceID, fixture.team.member.ID).Scan(&expires)
				if !allowed || role != domain.RolePuller || current.UnresolvedMembers != 1 || current.Status != "connected" || current.NextSync.Before(time.Now().Add(4*time.Minute)) || err != nil || expires.Before(time.Now().Add(9*time.Minute)) || expires.After(time.Now().Add(10*time.Minute)) {
					test.Fatal("current grant lost role, provenance, expiry or repair schedule", role, current, expires, err)
				}
			}
		})
	}
}

type githubSyncPGQueue struct {
	*store.PostgresStore
	outsideList func(context.Context) error
}

func (storage *githubSyncPGQueue) ListGitHubConnections(ctx context.Context) ([]domain.GitHubConnection, error) {
	connections, err := storage.PostgresStore.ListGitHubConnections(ctx)
	if err == nil && !storage.InTransaction(ctx) && storage.outsideList != nil {
		operation := storage.outsideList
		storage.outsideList = nil
		err = operation(ctx)
	}
	return connections, err
}

func TestPGGitHubWorkerDeliveryLeaseAndBodyFences(test *testing.T) {
	wave40IsolatedPG(test)
	for _, scenario := range []string{"body_integrity", "lease_reclaimed"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubSyncPGFixture(test)
			delivery := fixture.receive(test, "synchronize")
			queue := &githubSyncPGQueue{PostgresStore: fixture.storage}
			config := fixture.config(fixture.storage, fixture.remote)
			config.Store = queue
			owner := newGitHubSyncPGOwner(test, config)
			var replacement domain.GitHubDelivery
			if scenario == "body_integrity" {
				delivery.Body = append(delivery.Body, ' ')
				if err := fixture.peer.WithinIdentity(fixture.ctx, func(bound context.Context) error {
					return fixture.peer.PutGitHubDelivery(bound, delivery)
				}); err != nil {
					test.Fatal(err)
				}
			} else {
				queue.outsideList = func(ctx context.Context) error {
					return fixture.peer.WithinIdentity(ctx, func(bound context.Context) error {
						current, err := fixture.peer.GetGitHubDelivery(bound, delivery.ID)
						if err != nil || current.Attempts != 1 || current.Lease == "" || current.LeaseUntil.Before(time.Now().Add(110*time.Second)) || current.LeaseUntil.After(time.Now().Add(2*time.Minute)) {
							return errors.Join(domain.ErrIntegrity, err)
						}
						current.Lease, current.LeaseUntil = "winning-public-owner", time.Now().UTC().Add(2*time.Minute)
						replacement = current
						return fixture.peer.PutGitHubDelivery(bound, current)
					})
				}
			}
			expected := domain.ErrIntegrity
			if scenario == "lease_reclaimed" {
				expected = domain.ErrConflict
			}
			if err := owner.deliver(fixture.ctx, delivery.ID); !errors.Is(err, expected) {
				test.Fatal("actual delivery accepted stale lease/body", err)
			}
			current, err := fixture.peer.GetGitHubDelivery(fixture.ctx, delivery.ID)
			if err != nil || current.Done || !slices.Equal(current.Body, delivery.Body) || (scenario == "body_integrity" && !reflect.DeepEqual(current, delivery)) || (scenario == "lease_reclaimed" && !reflect.DeepEqual(current, replacement)) {
				test.Fatal("rejected delivery overwrote durable winner", current, err)
			}
		})
	}
}

func TestPGGitHubWorkerCurrentDocumentPolicyAtClaimAndFinish(test *testing.T) {
	wave40IsolatedPG(test)
	for _, scenario := range []string{"claim_connection", "finish_connection", "claim_delivery", "finish_delivery"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubSyncPGFixture(test)
			queue := &githubSyncPGQueue{PostgresStore: fixture.storage}
			config := fixture.config(fixture.storage, fixture.remote)
			config.Store = queue
			config.Blobs = struct{ outbound.BlobStore }{fixture.storage}
			if !slices.Equal(ReadDocumentIdentities(config.Metadata, config.Blobs), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) {
				test.Fatal("policy fixture unexpectedly advertises root readers")
			}
			owner := newGitHubSyncPGOwner(test, config)
			upgrade := func() error {
				return fixture.peer.RequireDocumentIdentity(fixture.ctx, fixture.repo.ID, domain.DocumentIdentityRootV1)
			}
			var delivery domain.GitHubDelivery
			isDelivery := strings.HasSuffix(scenario, "delivery")
			if isDelivery {
				delivery = fixture.receive(test, "closed")
			} else {
				fixture.updateConnection(test, func(connection *domain.GitHubConnection) {
					connection.Mappings = []domain.GitHubTeamMapping{{TeamID: fixture.team.team.ID, ExternalID: 99, SyncMembers: true}}
				})
			}
			upgraded := false
			if strings.HasPrefix(scenario, "claim") {
				if err := upgrade(); err != nil {
					test.Fatal(err)
				}
			} else if isDelivery {
				queue.outsideList = func(context.Context) error { upgraded = true; return upgrade() }
			} else {
				fixture.remote.onMembers = func() {
					upgraded = true
					if err := upgrade(); err != nil {
						test.Fatal(err)
					}
				}
			}
			before, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil {
				test.Fatal(err)
			}
			if isDelivery {
				err = owner.deliver(fixture.ctx, delivery.ID)
			} else {
				err = owner.reconcile(fixture.ctx, fixture.connection.NamespaceID)
			}
			if !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) || (strings.HasPrefix(scenario, "finish") && !upgraded) {
				test.Fatal("public owner missed current source policy boundary", upgraded, err)
			}
			after, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil || before != after {
				test.Fatal("source policy rejection published state", before, after, err)
			}
			if isDelivery {
				current, err := fixture.peer.GetGitHubDelivery(fixture.ctx, delivery.ID)
				expectedAttempts := 0
				if upgraded {
					expectedAttempts = 1
				}
				if err != nil || current.Done || current.Attempts != expectedAttempts || !slices.Equal(current.Body, delivery.Body) || (upgraded && current.LeaseUntil.Before(time.Now().Add(110*time.Second))) {
					test.Fatal("policy fence consumed or acknowledged webhook", current, err)
				}
			} else {
				current, err := fixture.peer.GetGitHubConnection(fixture.ctx, fixture.connection.NamespaceID)
				if err != nil || !current.CheckedAt.Equal(fixture.connection.CheckedAt) || (upgraded && current.NextSync.Before(time.Now().Add(110*time.Second))) || (!upgraded && !current.NextSync.IsZero()) {
					test.Fatal("policy fence finalized stale connection", current, err)
				}
			}
		})
	}
}
