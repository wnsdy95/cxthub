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
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
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
	return GitHubWorkerConfig{Profile: GitHubWorkerComplete, Store: storage, Remote: remote, Metadata: storage, Blobs: storage, Repositories: storage, Organizations: storage, Teams: storage, IdentityTransactions: storage, Evidence: storage, Memory: storage, RequireCompleteMemory: true}
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

func TestPGGitHubWorkerAPIConfigSamePortsFactoryAndEnqueue(test *testing.T) {
	fixture := newGitHubSyncPGFixture(test)
	connections := NewGitHubConnections(fixture.team.identity, fixture.core, fixture.storage, fixture.remote)
	config := connections.GitHubWorkerConfig(GitHubWorkerComplete)
	if config.Store != fixture.storage || config.Metadata != fixture.storage || config.Blobs != fixture.storage || config.Repositories != fixture.storage || config.Organizations != fixture.storage || config.Teams != fixture.storage || config.IdentityTransactions != fixture.storage || config.Remote != fixture.remote || config.RequireCompleteMemory != fixture.core.requireCompleteMemory {
		test.Fatal("API configuration replaced current GitHub dependencies")
	}
	owner := newGitHubSyncPGOwner(test, config)
	before, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	request := domain.PullRequestMerge{Number: 73, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	if err := owner.enqueuePR(fixture.ctx, fixture.connection, fixture.connection.Bindings[0], request); err != nil {
		test.Fatal("configured public owner could not durably enqueue", err)
	}
	job, err := fixture.storage.GetPRJob(fixture.ctx, fixture.repo.ID, domain.PRPromotionID(fixture.repo.ID, request.Number))
	if err != nil || job.PR != request || job.State != "waiting" {
		test.Fatal("configured owner lost original PR job", job, err)
	}
	after, err := fixture.storage.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil || after != before {
		test.Fatal("enqueue changed graph before publication", before, after, err)
	}
	fixture.count(test, "git_scan_jobs", 2)
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
					if err := commands.RemoveOrganizationMember(fixture.ctx, fixture.team.owner.ID, fixture.team.organization.ID, fixture.team.member.ID); err != nil {
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

func TestPGGitHubWorkerDeliveryAtomicRollbackAndRestart(test *testing.T) {
	wave40IsolatedPG(test)
	for _, action := range []string{"synchronize", "closed"} {
		test.Run(action, func(test *testing.T) {
			fixture := newGitHubSyncPGFixture(test)
			githubSyncPGRootSnapshot(test, fixture)
			if err := fixture.storage.RequireDocumentIdentity(fixture.ctx, fixture.repo.ID, domain.DocumentIdentityRootV1); err != nil {
				test.Fatal(err)
			}
			delivery := fixture.receive(test, action)
			failing := &githubObservationRollbackPG{PostgresStore: fixture.storage, rejectRevision: true}
			config := fixture.config(fixture.storage, fixture.remote)
			config.Metadata = failing
			owner := newGitHubSyncPGOwner(test, config)
			before, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil {
				test.Fatal(err)
			}
			if err := owner.deliver(fixture.ctx, delivery.ID); err != nil {
				test.Fatal(err)
			}
			for _, table := range []string{"git_ref_observations", "git_scan_jobs", "pr_promotion_jobs"} {
				fixture.count(test, table, 0)
			}
			after, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil || after != before {
				test.Fatal("repository revision escaped real rollback", before, after, err)
			}
			retrying, err := fixture.peer.GetGitHubDelivery(fixture.ctx, delivery.ID)
			if err != nil || retrying.Done || retrying.Attempts != 1 || len(retrying.Lease) == 0 || !retrying.LeaseUntil.IsZero() || retrying.NextAttempt.Before(time.Now().Add(50*time.Second)) || !slices.Equal(retrying.Body, delivery.Body) {
				test.Fatal("rollback consumed webhook or lost retry", retrying, err)
			}
			retrying.NextAttempt = time.Now().UTC().Add(-time.Second)
			if err := fixture.peer.WithinIdentity(fixture.ctx, func(bound context.Context) error {
				return fixture.peer.PutGitHubDelivery(bound, retrying)
			}); err != nil {
				test.Fatal(err)
			}
			restarted := newGitHubSyncPGOwner(test, fixture.config(fixture.peer, fixture.remote))
			for range 2 {
				if err := restarted.deliver(fixture.ctx, delivery.ID); err != nil {
					test.Fatal("fresh public owner could not resume durable delivery", err)
				}
			}
			fixture.count(test, "git_ref_observations", 1)
			fixture.count(test, "git_scan_jobs", 2)
			expectedJobs := 0
			if action == "closed" {
				expectedJobs = 1
			}
			fixture.count(test, "pr_promotion_jobs", expectedJobs)
			after, err = fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil || after.Graph != before.Graph || after.Evidence != before.Evidence+1 {
				test.Fatal("delivery changed graph revision or duplicated evidence", before, after, err)
			}
			done, err := fixture.storage.GetGitHubDelivery(fixture.ctx, delivery.ID)
			if err != nil || !done.Done || len(done.Body) != 0 || done.BodyHash != delivery.BodyHash || done.Attempts != 2 || !done.LeaseUntil.IsZero() {
				test.Fatal("committed delivery lost idempotency or completion", done, err)
			}
			var raw []byte
			if err := fixture.observer.QueryRow(fixture.ctx, `SELECT payload FROM git_ref_observations WHERE repo_id=$1`, fixture.repo.ID).Scan(&raw); err != nil {
				test.Fatal(err)
			}
			var observation domain.GitRefObservation
			if err := json.Unmarshal(raw, &observation); err != nil || observation.Validate() != nil || observation.Source != "github-pull-request" || observation.Delivery != delivery.ID || observation.PullRequest == nil || observation.PullRequest.InstallationID != fixture.connection.Installation.ID || observation.PullRequest.ID != 701 || observation.PullRequest.Action != action {
				test.Fatal("actual public owner lost provider/source proof", observation, err)
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
			expectedObservations := 0
			if scenario == "lease_reclaimed" {
				expectedObservations = 1
			}
			fixture.count(test, "git_ref_observations", expectedObservations)
		})
	}
}

func TestPGGitHubWorkerBindingFences(test *testing.T) {
	wave40IsolatedPG(test)
	for _, scenario := range []string{"generation", "expired_check", "archived", "namespace", "origin"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubSyncPGFixture(test)
			owner := newGitHubSyncPGOwner(test, fixture.config(fixture.storage, fixture.remote))
			connection := fixture.connection
			expected := domain.ErrForbidden
			switch scenario {
			case "generation":
				fixture.updateConnection(test, func(current *domain.GitHubConnection) { current.Generation++ })
			case "expired_check":
				fixture.updateConnection(test, func(current *domain.GitHubConnection) { current.CheckedAt = time.Now().UTC().Add(-11 * time.Minute) })
			case "archived", "namespace":
				record := fixture.team.repository
				if scenario == "archived" {
					record.Archived = true
				} else {
					other, err := fixture.team.identity.CreateOrganization(fixture.ctx, fixture.team.owner, "Other", "other-"+domain.NewID("")[:10])
					if err != nil {
						test.Fatal(err)
					}
					record.OwnerNamespaceID = other.NamespaceID
				}
				if err := fixture.peer.WithinIdentity(fixture.ctx, func(bound context.Context) error { return fixture.peer.CreateRepository(bound, record) }); err != nil {
					test.Fatal(err)
				}
			case "origin":
				if _, err := fixture.observer.Exec(fixture.ctx, `UPDATE repos SET git_remote_url=$2 WHERE id=$1`, fixture.repo.ID, "https://github.com/acme/replaced.git"); err != nil {
					test.Fatal(err)
				}
				expected = domain.ErrConflict
			}
			before, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil {
				test.Fatal(err)
			}
			request := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("b", 40), MergeSHA: strings.Repeat("c", 40)}
			if err := owner.enqueuePR(fixture.ctx, connection, connection.Bindings[0], request); !errors.Is(err, expected) {
				test.Fatal("stale binding queued PR", scenario, err)
			}
			fixture.count(test, "pr_promotion_jobs", 0)
			fixture.count(test, "git_scan_jobs", 0)
			after, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil || before != after {
				test.Fatal("rejected binding advanced publication revision", before, after, err)
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
			fixture.count(test, "git_ref_observations", 0)
			fixture.count(test, "git_scan_jobs", 0)
			fixture.count(test, "pr_promotion_jobs", 0)
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

type githubSyncPGHistory struct {
	*githubSyncPGFixture
	remote      *githubHistoryRemote
	ordinary    domain.HistoryEvent
	main        domain.Ref
	base        domain.ContentHash
	memory      domain.ContentHash
	publication domain.HistoryEvent
}

func githubSyncPGRootSnapshot(test *testing.T, fixture *githubSyncPGFixture, parents ...domain.ContentHash) domain.ContentHash {
	test.Helper()
	representation, bodies := a11RootRepresentation(test, "original root source before remote rebase")
	if _, _, err := fixture.storage.PutChunks(fixture.ctx, fixture.repo.ID, bodies); err != nil {
		test.Fatal(err)
	}
	if err := fixture.core.ConfigureConversationRootPublication(true); err != nil {
		test.Fatal("root source fixture configuration", err)
	}
	identity := domain.DocumentIdentityRootV1
	if _, err := fixture.core.PatchRepoProfile(fixture.ctx, fixture.repo.ID, inbound.RepoProfilePatch{RequiredDocIdentity: &identity}); err != nil {
		test.Fatal("root source fixture repository opt-in", err)
	}
	accepted, err := fixture.core.SubmitDocFinalization(fixture.ctx, fixture.repo.ID, representation)
	if err != nil {
		test.Fatal("root source fixture admission", err)
	}
	if err := fixture.core.ProcessDocFinalizations(fixture.ctx, 1); err != nil {
		test.Fatal("root source fixture finalization", err)
	}
	completed, err := fixture.storage.GetDocJob(fixture.ctx, fixture.repo.ID, accepted.ID)
	if err != nil || completed.State != "completed" {
		test.Fatal("root source fixture not durably published", completed, err)
	}
	if err := fixture.storage.PutSnapshot(fixture.ctx, domain.Snapshot{ID: representation.Hash, RepoID: fixture.repo.ID, DocHash: representation.Hash, DocIdentity: domain.DocumentIdentityRootV1, Parents: parents, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	return representation.Hash
}

func newGitHubSyncPGHistory(test *testing.T) *githubSyncPGHistory {
	test.Helper()
	fixture := newGitHubSyncPGFixture(test)
	base := collaborationSnapshot(test, fixture.storage, fixture.repo.ID, "base")
	source := githubSyncPGRootSnapshot(test, fixture, base)
	latest := collaborationSnapshot(test, fixture.storage, fixture.repo.ID, "continued current main", base)
	main := domain.Ref{RepoID: fixture.repo.ID, Kind: domain.RefBranch, Name: "main", BranchID: domain.LegacyContextBranchID(string(fixture.repo.ID), "main"), Target: latest}
	if err := fixture.storage.CompareAndSwapRef(fixture.ctx, fixture.repo.ID, main, ""); err != nil {
		test.Fatal(err)
	}
	ordinary := domain.HistoryEvent{ID: strings.Repeat("1", 32), RepoID: string(fixture.repo.ID), BranchID: "feature", Branch: "feature", Kind: "birth", Source: base, Target: source, GitAfter: strings.Repeat("a", 40), CreatedAt: time.Unix(100, 0).UTC()}
	if err := fixture.core.RecordHistory(fixture.ctx, ordinary); err != nil {
		test.Fatal(err)
	}
	digest := domain.MemoryDigest{SnapshotID: source, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: source, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "PINNED CONTRIBUTION MEMORY"}}}}}
	memory, err := fixture.storage.PutMemory(fixture.ctx, fixture.repo.ID, digest)
	if err != nil {
		test.Fatal(err)
	}
	publication := prPublication(ordinary)
	publication.MemoryHash, publication.MemorySource = memory, source
	if err := fixture.core.RecordHistory(fixture.ctx, publication); err != nil {
		test.Fatal(err)
	}
	digest.Fragments[0].Claims[0].Text = "UNPUBLISHED MUTABLE SOURCE"
	mutable, err := fixture.storage.PutMemory(fixture.ctx, fixture.repo.ID, digest)
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.storage.CompareAndSwapSnapshotMemory(fixture.ctx, fixture.repo.ID, source, "", mutable); err != nil {
		test.Fatal(err)
	}
	position := domain.HistoryEvent{ID: strings.Repeat("8", 32), RepoID: string(fixture.repo.ID), BranchID: main.BranchID, Branch: "main", Kind: "position", Source: latest, Target: latest, GitAfter: strings.Repeat("e", 40), CreatedAt: time.Unix(400, 0).UTC()}
	if err := fixture.core.RecordHistory(fixture.ctx, position); err != nil {
		test.Fatal(err)
	}
	gitReader := &scanReader{deltas: map[string]domain.GitCommitDelta{}}
	previous := ""
	for _, character := range []string{"1", "d", "e"} {
		commit := strings.Repeat(character, 40)
		parents := []string{}
		if previous != "" {
			parents = append(parents, previous)
		}
		gitReader.deltas[commit] = domain.GitCommitDelta{Commit: commit, Parent: previous, Parents: parents, Complete: true, Changes: []domain.GitPathChange{}}
		previous = commit
	}
	original := ordinary.GitAfter
	gitReader.deltas[original] = domain.GitCommitDelta{Commit: original, Parents: []string{}, Complete: true, Changes: []domain.GitPathChange{}}
	scanStorage := &collaborationGitEvidencePG{PostgresStore: fixture.storage, repo: fixture.repo.ID}
	scanCore := NewService(scanStorage, fixture.storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(fixture.storage), fixture.storage)
	scans, err := NewGitScans(scanCore, gitReader)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := scans.ObservePush(fixture.ctx, fixture.repo.GitRemoteURL, "refs/heads/main", "", position.GitAfter, false, "public-owner-main"); err != nil {
		test.Fatal(err)
	}
	for range 4 {
		if err := scans.Process(fixture.ctx, 100); err != nil {
			test.Fatal(err)
		}
	}
	for _, character := range []string{"1", "d", "e"} {
		if _, err := fixture.peer.GetGitCommitTree(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, strings.Repeat(character, 40)); err != nil {
			test.Fatal("fixture Git ancestry not durably scanned", err)
		}
	}
	request := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("c", 40), MergeSHA: strings.Repeat("d", 40)}
	owner := newGitHubSyncPGOwner(test, fixture.config(fixture.storage, fixture.remote))
	before, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	if err := owner.enqueuePR(fixture.ctx, fixture.connection, fixture.connection.Bindings[0], request); err != nil {
		test.Fatal("public owner did not durably accept PR", err)
	}
	after, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil || before != after {
		test.Fatal("PR enqueue changed publication generation", before, after, err)
	}
	page := domain.GitHubPRHistoryPage{RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: request.HeadSHA, MergeSHA: request.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(), Rewrites: []domain.GitHubHeadRewrite{{ID: "event-1", Before: ordinary.GitAfter, After: strings.Repeat("b", 40), CreatedAt: time.Unix(200, 0).UTC()}}}
	remote := &githubHistoryRemote{GitHubApp: fixture.remote, installationID: fixture.connection.Installation.ID, pages: map[string]domain.GitHubPRHistoryPage{"": page}}
	if err := fixture.storage.RequireDocumentIdentity(fixture.ctx, fixture.repo.ID, domain.DocumentIdentityRootV1); err != nil {
		test.Fatal(err)
	}
	return &githubSyncPGHistory{githubSyncPGFixture: fixture, remote: remote, ordinary: ordinary, main: main, base: base, memory: memory, publication: publication}
}

func (fixture *githubSyncPGHistory) work(test *testing.T, storage *store.PostgresStore) githubPRHistoryWork {
	test.Helper()
	connection, err := storage.GetGitHubConnection(fixture.ctx, fixture.connection.NamespaceID)
	if err != nil {
		test.Fatal(err)
	}
	job, err := storage.GetPRJob(fixture.ctx, fixture.repo.ID, domain.PRPromotionID(fixture.repo.ID, 7))
	if err != nil {
		test.Fatal(err)
	}
	checkpoint, err := storage.GetPRHistoryCheckpoint(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 91, 7)
	if errors.Is(err, domain.ErrNotFound) {
		checkpoint = domain.PRHistoryCheckpoint{RepoID: fixture.repo.ID, GitOrigin: fixture.repo.GitRemoteURL, InstallationID: connection.Installation.ID, ProviderRepositoryID: 91, PR: job.PR}
	} else if err != nil {
		test.Fatal(err)
	}
	return githubPRHistoryWork{connection: connection, binding: connection.Bindings[0], remote: connection.Repositories[0], checkpoint: checkpoint}
}

type githubSyncPGMemory struct {
	*store.PostgresStore
	reads     []domain.ContentHash
	afterRead func() error
	corrupt   bool
}

func (storage *githubSyncPGMemory) GetMemory(ctx context.Context, repo, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if !storage.InTransaction(ctx) || !storage.InReadOnlyTransaction(ctx) {
		return domain.MemoryDigest{}, domain.ErrIntegrity
	}
	storage.reads = append(storage.reads, hash)
	digest, err := storage.PostgresStore.GetMemory(ctx, repo, hash)
	if err == nil && storage.afterRead != nil {
		operation := storage.afterRead
		storage.afterRead = nil
		err = operation()
	}
	if storage.corrupt {
		digest.SnapshotID = hh("wrong-current-memory-source")
	}
	return digest, err
}

func (fixture *githubSyncPGHistory) assertUnpublished(test *testing.T, expectedVersion int64) {
	test.Helper()
	checkpoint, err := fixture.peer.GetPRHistoryCheckpoint(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 91, 7)
	if err != nil || checkpoint.Version != expectedVersion || checkpoint.PullRequestID != 0 || checkpoint.Cursor != "" || checkpoint.Complete || checkpoint.NextAttempt.Before(time.Now().Add(110*time.Second)) {
		test.Fatal("failed page advanced beyond reservation", checkpoint, err)
	}
	proofs, err := fixture.peer.ListPRParticipations(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 0 {
		test.Fatal("participation escaped page rollback", proofs, err)
	}
	contributions, err := fixture.peer.ListPRContributions(fixture.ctx, fixture.repo.ID)
	if err != nil || len(contributions) != 0 {
		test.Fatal("contribution escaped page rollback", contributions, err)
	}
	memories, err := fixture.peer.ListPRContributionMemories(fixture.ctx, fixture.repo.ID)
	if err != nil || len(memories) != 0 {
		test.Fatal("immutable memory pin escaped page rollback", memories, err)
	}
	observations, err := fixture.peer.ListPRRefObservations(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 91, 7, "", 101)
	if err != nil || len(observations) != 0 {
		test.Fatal("provider observation escaped page rollback", observations, err)
	}
	job, err := fixture.peer.GetPRJob(fixture.ctx, fixture.repo.ID, domain.PRPromotionID(fixture.repo.ID, 7))
	if err != nil || job.State != "waiting" {
		test.Fatal("failed page completed durable PR job", job, err)
	}
}

func TestPGGitHubWorkerHistoryAtomicPublicationMemoryAndRestart(test *testing.T) {
	wave40IsolatedPG(test)
	fixture := newGitHubSyncPGHistory(test)
	failing := &githubObservationRollbackPG{PostgresStore: fixture.storage, rejectRevision: true}
	memory := &githubSyncPGMemory{PostgresStore: fixture.storage}
	config := fixture.config(fixture.storage, fixture.remote)
	config.Metadata, config.Memory = failing, memory
	owner := newGitHubSyncPGOwner(test, config)
	work := fixture.work(test, fixture.storage)
	before, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	historyBefore, err := fixture.peer.ListHistoryEvents(fixture.ctx, fixture.repo.ID)
	if err != nil {
		test.Fatal(err)
	}
	snapshotsBefore, err := fixture.peer.ListSnapshots(fixture.ctx, fixture.repo.ID, "")
	if err != nil {
		test.Fatal(err)
	}
	if err := owner.reconcilePRHistory(fixture.ctx, fixture.remote, failing, work); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("public owner did not reach actual atomic publication rollback", err)
	}
	fixture.assertUnpublished(test, 1)
	if !slices.Contains(memory.reads, fixture.memory) {
		test.Fatal("publication bypassed explicit current immutable memory reader", memory.reads)
	}
	after, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil || before != after {
		test.Fatal("page revision escaped rollback", before, after, err)
	}
	restartedMemory := &githubSyncPGMemory{PostgresStore: fixture.peer}
	config = fixture.config(fixture.peer, fixture.remote)
	config.Memory = restartedMemory
	restarted := newGitHubSyncPGOwner(test, config)
	work = fixture.work(test, fixture.peer)
	fixture.remote.during = func() {
		if err := fixture.storage.WithinIdentity(fixture.ctx, func(context.Context) error { return nil }); err != nil {
			test.Fatal("provider read retained identity lock", err)
		}
	}
	if err := restarted.reconcilePRHistory(fixture.ctx, fixture.remote, fixture.peer, work); err != nil {
		test.Fatal("fresh public owner could not recover atomic PR history page", err)
	}
	if err := owner.reconcilePRHistory(fixture.ctx, fixture.remote, failing, work); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale checkpoint remained publishable after restart", err)
	}
	checkpoint, err := fixture.storage.GetPRHistoryCheckpoint(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 91, 7)
	if err != nil || checkpoint.Version != 3 || !checkpoint.Complete || checkpoint.PullRequestID != 701 || checkpoint.Cursor != "" || checkpoint.HistoryCheckedAt.IsZero() || len(fixture.remote.calls) != 2 {
		test.Fatal("recovery lost provider checkpoint/version", checkpoint, fixture.remote.calls, err)
	}
	proofs, err := fixture.storage.ListPRParticipations(fixture.ctx, fixture.repo.ID, fixture.repo.GitRemoteURL, 7)
	if err != nil || len(proofs) != 1 || proofs[0].Capture != fixture.ordinary.Target || proofs[0].OriginalHead != fixture.ordinary.GitAfter || proofs[0].PublicationID != fixture.publication.ID {
		test.Fatal("public owner did not preserve original capture proof", proofs, err)
	}
	contributions, err := fixture.storage.ListPRContributions(fixture.ctx, fixture.repo.ID)
	if err != nil || len(contributions) != 1 || contributions[0].Capture != fixture.ordinary.Target || contributions[0].DestinationBranchID != fixture.main.BranchID {
		test.Fatal("public owner omitted durable semantic contribution", contributions, err)
	}
	pins, err := fixture.storage.ListPRContributionMemories(fixture.ctx, fixture.repo.ID)
	if err != nil || len(pins) != 1 || pins[0].MemoryHash != fixture.memory || pins[0].MemorySource != fixture.ordinary.Target || pins[0].PublicationID != fixture.publication.ID || !slices.Contains(restartedMemory.reads, fixture.memory) {
		test.Fatal("restart replaced immutable publication pin with mutable memory", pins, restartedMemory.reads, err)
	}
	job, err := fixture.storage.GetPRJob(fixture.ctx, fixture.repo.ID, domain.PRPromotionID(fixture.repo.ID, 7))
	if err != nil || job.State != "completed" || !job.LeaseUntil.IsZero() {
		test.Fatal("PR contribution job did not complete atomically", job, err)
	}
	after, err = fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
	if err != nil || after.Graph != before.Graph || after.Evidence != before.Evidence+1 {
		test.Fatal("atomic publication changed wrong generation", before, after, err)
	}
	queries := NewService(fixture.peer, fixture.peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(fixture.peer), fixture.peer)
	contextView, err := queries.QueryContext(fixture.ctx, fixture.repo.ID, domain.ContextSelection{Scope: "current", Branch: "main", Position: "main"})
	if err != nil || contextView.Inclusion == nil || contextView.Inclusion.SnapshotID != fixture.main.Target || contextView.Inclusion.CodeCommit != strings.Repeat("e", 40) || !slices.Contains(contextView.Inclusion.SnapshotIDs, fixture.ordinary.Target) || len(contextView.Inclusion.Merges) != 1 || contextView.Inclusion.Merges[0].State != "included" {
		test.Fatal("actual published contribution absent from selected context", contextView.Inclusion, err)
	}
	projection, err := queries.QueryBranchMemory(fixture.ctx, fixture.repo.ID, "main", fixture.main.Target, "")
	if err != nil || !projection.Found || !reflect.DeepEqual(projection.Inclusion, contextView.Inclusion) {
		test.Fatal("published memory disagrees with actual context selection", projection, err)
	}
	items, err := effectiveMemoryItems(projection.Digest, "claims")
	if err != nil || len(items) != 1 || items[0].Text != "PINNED CONTRIBUTION MEMORY" || items[0].SourceSnapshot != fixture.ordinary.Target {
		test.Fatal("current memory reader substituted unpublished pointer", items, err)
	}
	main, err := fixture.peer.GetRef(fixture.ctx, fixture.repo.ID, domain.RefBranch, "main")
	if err != nil || main != fixture.main {
		test.Fatal("history reconciliation rewound current main", main, err)
	}
	historyAfter, err := fixture.peer.ListHistoryEvents(fixture.ctx, fixture.repo.ID)
	if err != nil || !reflect.DeepEqual(historyBefore, historyAfter) {
		test.Fatal("history reconciliation manufactured source publications", historyAfter, err)
	}
	snapshotsAfter, err := fixture.peer.ListSnapshots(fixture.ctx, fixture.repo.ID, "")
	if err != nil || !reflect.DeepEqual(snapshotsBefore, snapshotsAfter) {
		test.Fatal("contribution changed source snapshots or mutable memory", err)
	}
	rewound := fixture.main
	rewound.Target = fixture.base
	if err := fixture.peer.CompareAndSwapRef(fixture.ctx, fixture.repo.ID, rewound, fixture.main.Target); err != nil {
		test.Fatal(err)
	}
	position := domain.HistoryEvent{ID: strings.Repeat("9", 32), RepoID: string(fixture.repo.ID), BranchID: rewound.BranchID, Branch: "main", Kind: "position", Source: fixture.base, Target: fixture.base, GitAfter: strings.Repeat("1", 40), CreatedAt: time.Unix(600, 0).UTC()}
	if err := queries.RecordHistory(fixture.ctx, position); err != nil {
		test.Fatal(err)
	}
	if err := restarted.reconcilePRHistory(fixture.ctx, fixture.remote, fixture.peer, fixture.work(test, fixture.peer)); err != nil {
		test.Fatal(err)
	}
	main, err = fixture.peer.GetRef(fixture.ctx, fixture.repo.ID, domain.RefBranch, "main")
	if err != nil || main != rewound || len(fixture.remote.calls) != 2 {
		test.Fatal("completed-page replay undid deliberate rewind or reread provider", main, fixture.remote.calls, err)
	}
	projection, err = queries.QueryBranchMemory(fixture.ctx, fixture.repo.ID, "main", rewound.Target, "")
	if err != nil || projection.Found || projection.Inclusion == nil || slices.Contains(projection.Inclusion.SnapshotIDs, fixture.ordinary.Target) {
		test.Fatal("rewound context still includes future contribution memory", projection, err)
	}
}

func TestPGGitHubWorkerHistoryCurrentSourceAndRevisionFences(test *testing.T) {
	wave40IsolatedPG(test)
	for _, scenario := range []string{"connection_revoked", "provider_identity", "memory_integrity", "revision_repin"} {
		test.Run(scenario, func(test *testing.T) {
			fixture := newGitHubSyncPGHistory(test)
			memory := &githubSyncPGMemory{PostgresStore: fixture.storage}
			config := fixture.config(fixture.storage, fixture.remote)
			config.Memory = memory
			owner := newGitHubSyncPGOwner(test, config)
			before, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			if err != nil {
				test.Fatal(err)
			}
			expected := domain.ErrIntegrity
			switch scenario {
			case "connection_revoked":
				expected = domain.ErrForbidden
				fixture.remote.during = func() {
					fixture.updateConnection(test, func(connection *domain.GitHubConnection) { connection.Enabled = false; connection.Generation++ })
				}
			case "provider_identity":
				page := fixture.remote.pages[""]
				page.RepositoryID++
				fixture.remote.pages[""] = page
			case "memory_integrity":
				memory.corrupt = true
			case "revision_repin":
				expected = domain.ErrConflict
				memory.afterRead = func() error {
					return fixture.peer.WithinRepository(fixture.ctx, fixture.repo.ID, func(bound context.Context) error {
						return fixture.peer.AdvanceEvidenceRevision(bound, fixture.repo.ID)
					})
				}
			}
			if err := owner.reconcilePRHistory(fixture.ctx, fixture.remote, fixture.storage, fixture.work(test, fixture.storage)); !errors.Is(err, expected) {
				test.Fatal("history owner published stale authority/source/revision", scenario, err)
			}
			fixture.assertUnpublished(test, 1)
			after, err := fixture.peer.RepositoryRevision(fixture.ctx, fixture.repo.ID)
			expectedRevision := before
			if scenario == "revision_repin" {
				expectedRevision.Evidence++
			}
			if err != nil || after != expectedRevision {
				test.Fatal("rejected history page advanced generation", before, after, err)
			}
			if (scenario == "memory_integrity" || scenario == "revision_repin") && !slices.Contains(memory.reads, fixture.memory) {
				test.Fatal("source fence did not execute explicit publication memory reader", memory.reads)
			}
		})
	}
}
