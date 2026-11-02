//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type registrationScopePutHoldPG struct {
	*store.PostgresStore
	ready         chan struct{}
	release       <-chan struct{}
	once          sync.Once
	authorized    domain.Repo
	actor         string
	system        bool
	transactional bool
}

func (storage *registrationScopePutHoldPG) PutRepo(ctx context.Context, repo domain.Repo) (domain.Repo, error) {
	storage.once.Do(func() {
		storage.authorized = repo
		storage.actor, storage.system = inbound.RepositoryActor(ctx)
		storage.transactional = storage.PostgresStore.InTransaction(ctx) && !storage.PostgresStore.InReadOnlyTransaction(ctx)
		close(storage.ready)
	})
	select {
	case <-storage.release:
		return storage.PostgresStore.PutRepo(ctx, repo)
	case <-ctx.Done():
		return domain.Repo{}, ctx.Err()
	}
}

func registrationScopeBlocked(test *testing.T, ctx context.Context, observer *pgxpool.Pool, waiter, holder string, done <-chan error) {
	test.Helper()
	authorityScopeBlocked(test, ctx, observer, waiter, holder, "repositories", done)
	var rowBlocked bool
	err := observer.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM pg_stat_activity waiting
		JOIN pg_locks locks ON locks.pid=waiting.pid
		JOIN pg_stat_activity holding ON holding.pid=ANY(pg_blocking_pids(waiting.pid))
		WHERE waiting.application_name=$1 AND holding.application_name=$2
		AND waiting.wait_event_type='Lock' AND waiting.wait_event IN ('transactionid','tuple')
		AND locks.locktype=waiting.wait_event AND NOT locks.granted
		AND (locks.locktype<>'tuple' OR locks.relation='repositories'::regclass)
		AND position('repositories' in waiting.query)>0
		AND waiting.query ~* 'FOR[[:space:]]+(SHARE|UPDATE)'
	)`, waiter, holder).Scan(&rowBlocked)
	if err != nil {
		test.Fatal(err)
	}
	if !rowBlocked {
		test.Fatal("operation did not wait on the other transaction's repositories policy row")
	}
}

type registrationScopeEffects struct {
	repos           int64
	revisions       int64
	refs            int64
	reflog          int64
	snapshots       int64
	settings        int64
	secrets         int64
	initializations int64
	notifications   int64
}

func registrationScopeImage(test *testing.T, ctx context.Context, observer *pgxpool.Pool, repo domain.ContentHash, repository string) registrationScopeEffects {
	test.Helper()
	var image registrationScopeEffects
	err := observer.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM repos WHERE id=$1 OR repository_id=$2),
		(SELECT count(*) FROM repository_revisions WHERE repo_id=$1),
		(SELECT count(*) FROM refs WHERE repo_id=$1),
		(SELECT count(*) FROM reflog WHERE repo_id=$1),
		(SELECT count(*) FROM snapshots WHERE repo_id=$1),
		(SELECT count(*) FROM repo_settings WHERE repo_id=$1),
		(SELECT count(*) FROM repo_secrets WHERE repo_id=$1),
		(SELECT count(*) FROM repository_initializations WHERE repo_id=$1),
		(SELECT count(*) FROM notification_outbox WHERE repository_id=$2)`, string(repo), repository).
		Scan(&image.repos, &image.revisions, &image.refs, &image.reflog, &image.snapshots, &image.settings, &image.secrets, &image.initializations, &image.notifications)
	if err != nil {
		test.Fatal(err)
	}
	return image
}

func registrationScopePolicy(test *testing.T, ctx context.Context, service *Service, storage *store.PostgresStore, fixture teamFixture, registration domain.Repo, mutation string, revoked bool) {
	test.Helper()
	repository, err := storage.GetRepository(ctx, fixture.repository.ID)
	if err != nil {
		test.Fatal(err)
	}
	expected := fixture.repository
	expected.Archived = revoked && mutation == "archive"
	if !reflect.DeepEqual(repository, expected) {
		test.Fatalf("repository URL/ownership/archive state changed unexpectedly: got=%+v want=%+v", repository, expected)
	}
	resolved, err := service.repositoryForURL(ctx, registration.RemoteURL)
	if err != nil || resolved != fixture.repository.ID {
		test.Fatalf("registration URL no longer resolves to its actual organization repository: %q %v", resolved, err)
	}
	expectedMember := !revoked || mutation != "remove"
	member, err := storage.IsMember(ctx, fixture.repository.ID, fixture.member.ID)
	if err != nil || member != expectedMember {
		test.Fatalf("direct membership=%v, want %v: %v", member, expectedMember, err)
	}
	role, allowed := fixture.identity.RoleOf(ctx, fixture.repository.ID, fixture.member.ID)
	if allowed != expectedMember || expectedMember && role != domain.RoleMember {
		test.Fatalf("effective registration role=%q allowed=%v, want member=%v", role, allowed, expectedMember)
	}
}

func registrationScopeAudits(test *testing.T, fixture teamFixture, before, after []domain.OrganizationAuditEvent, mutation string, registered bool) {
	test.Helper()
	previous := make(map[string]domain.OrganizationAuditEvent, len(before))
	for _, event := range before {
		previous[event.ID] = event
	}
	mutationAction := "repository.member.removed"
	if mutation == "archive" {
		mutationAction = "repository.access_settings.updated"
	}
	var registrationCount, mutationCount int
	for _, event := range after {
		if prior, exists := previous[event.ID]; exists {
			if !reflect.DeepEqual(prior, event) {
				test.Fatal("registration or revocation changed an existing audit event")
			}
			delete(previous, event.ID)
			continue
		}
		if event.OrganizationID != fixture.organization.ID || event.TargetType != "repository" || event.TargetID != fixture.repository.ID {
			test.Fatalf("unexpected registration/revocation audit target: %+v", event)
		}
		switch {
		case event.ActorID == fixture.owner.ID && event.Action == mutationAction:
			mutationCount++
		case registered && event.ActorID == fixture.member.ID && event.Action == "context.updated":
			registrationCount++
		default:
			test.Fatalf("unexpected registration/revocation audit effect: %+v", event)
		}
	}
	expectedRegistrations := 0
	if registered {
		expectedRegistrations = 1
	}
	if len(previous) != 0 || mutationCount != 1 || registrationCount != expectedRegistrations {
		test.Fatalf("audit effects: missing=%d revocations=%d registrations=%d, want 0/1/%d", len(previous), mutationCount, registrationCount, expectedRegistrations)
	}
}

func TestPGRegistrationScopeFirstRegistrationOrdersWithRevocation(test *testing.T) {
	for _, mutation := range []string{"remove", "archive"} {
		for _, order := range []string{"revoke-first", "register-first"} {
			test.Run(mutation+"/"+order, func(test *testing.T) {
				service, storage, _ := collaborationPG(test)
				ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
				defer cancel()
				fixture := makeTeamFixture(test, storage)
				repository, err := storage.GetRepository(ctx, fixture.repository.ID)
				if err != nil {
					test.Fatal(err)
				}
				fixture.repository = repository
				if err := storage.AddMember(ctx, domain.Membership{RepositoryID: fixture.repository.ID, UserID: fixture.member.ID, Role: domain.RoleMember}); err != nil {
					test.Fatal(err)
				}
				remote := "https://example.test/" + fixture.organization.Slug + "/" + fixture.repository.Slug
				registration := domain.Repo{
					ID:            domain.HashContent([]byte(normalizeGitURL(remote))),
					RemoteURL:     remote,
					DefaultBranch: "main",
					GitRemoteURL:  "https://github.com/acme/registration.git",
				}
				if fixture.repository.OwnerUsername != fixture.organization.Slug {
					test.Fatal("fixture URL does not use the actual organization owner")
				}
				registrationScopePolicy(test, ctx, service, storage, fixture, registration, mutation, false)
				if _, err := storage.GetRepo(ctx, registration.ID); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("first registration already has a metadata repo: %v", err)
				}
				if _, err := storage.GetBoundRepo(ctx, fixture.repository.ID); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("first registration already has a repository binding: %v", err)
				}
				observer := authorityScopeObserver(test, ctx)
				writer, writerName := authorityScopePeer(test, ctx)
				authority, authorityName := authorityScopePeer(test, ctx)
				before := registrationScopeImage(test, ctx, observer, registration.ID, fixture.repository.ID)
				if before != (registrationScopeEffects{}) {
					test.Fatalf("unregistered repository already has durable effects: %+v", before)
				}
				audits, err := storage.ListOrganizationAudit(ctx, fixture.organization.ID, 100)
				if err != nil {
					test.Fatal(err)
				}
				release := make(chan struct{})
				var releaseOnce sync.Once
				unblock := func() { releaseOnce.Do(func() { close(release) }) }
				var operations []<-chan error
				launch := func(operation func() error) <-chan error {
					done := make(chan error, 1)
					operations = append(operations, done)
					go func() {
						defer close(done)
						done <- operation()
					}()
					return done
				}
				test.Cleanup(func() {
					cancel()
					unblock()
					cleanup, stop := context.WithTimeout(systemTestContext(), 5*time.Second)
					defer stop()
					for _, done := range operations {
						select {
						case <-done:
						case <-cleanup.Done():
							test.Error("registration/revocation did not stop within cleanup deadline")
							return
						}
					}
				})
				var registered domain.Repo
				register := func(writer *Service) error {
					result, err := writer.EnsureRepo(ctx, fixture.member.ID, registration)
					registered = result
					return err
				}
				var first, second <-chan error
				if order == "revoke-first" {
					paused := &authorityScopeHoldPG{PostgresStore: authority, ready: make(chan struct{}), release: release}
					first = launch(func() error { return authorityScopeMutation(ctx, NewIdentityService(nil, paused), fixture, mutation) })
					authorityScopeAwait(test, ctx, paused.ready, first)
					second = launch(func() error { return register(NewService(writer, writer, nil, gitengine.NewEngine(writer), writer)) })
					registrationScopeBlocked(test, ctx, observer, writerName, authorityName, second)
				} else {
					paused := &registrationScopePutHoldPG{PostgresStore: writer, ready: make(chan struct{}), release: release}
					first = launch(func() error { return register(NewService(paused, writer, nil, gitengine.NewEngine(writer), writer)) })
					authorityScopeAwait(test, ctx, paused.ready, first)
					expected := registration
					expected.RepositoryID = fixture.repository.ID
					if !reflect.DeepEqual(paused.authorized, expected) || paused.actor != fixture.member.ID || paused.system || !paused.transactional {
						test.Fatalf("EnsureRepo did not reach PutRepo with its authorized URL binding and actor inside the base transaction: %+v actor=%q system=%v transactional=%v", paused.authorized, paused.actor, paused.system, paused.transactional)
					}
					second = launch(func() error {
						return authorityScopeMutation(ctx, NewIdentityService(nil, authority), fixture, mutation)
					})
					registrationScopeBlocked(test, ctx, observer, authorityName, writerName, second)
				}
				registrationScopePolicy(test, ctx, service, storage, fixture, registration, mutation, false)
				if current := registrationScopeImage(test, ctx, observer, registration.ID, fixture.repository.ID); current != before {
					test.Fatalf("held transaction exposed partial registration effects: %+v", current)
				}
				if _, err := storage.GetRepo(ctx, registration.ID); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("metadata registration became visible before release: %v", err)
				}
				if current, err := storage.ListOrganizationAudit(ctx, fixture.organization.ID, 100); err != nil || !reflect.DeepEqual(current, audits) {
					test.Fatalf("held registration/revocation exposed an audit effect: %v", err)
				}
				unblock()
				if err := authorityScopeDone(test, ctx, first); err != nil {
					test.Fatal(err)
				}
				secondErr := authorityScopeDone(test, ctx, second)
				allowed := order == "register-first"
				if allowed && secondErr != nil || !allowed && !errors.Is(secondErr, domain.ErrForbidden) {
					test.Fatalf("second operation violated %s ordering: %v", order, secondErr)
				}
				registrationScopePolicy(test, ctx, service, storage, fixture, registration, mutation, true)
				expectedEffects := before
				if allowed {
					expectedEffects.repos = 1
					if registered.ID != registration.ID || registered.RepositoryID != fixture.repository.ID || registered.RemoteURL != remote || registered.GitRemoteURL != registration.GitRemoteURL || registered.DefaultBranch != registration.DefaultBranch {
						test.Fatalf("authorized first registration lost its actual URL/binding/origin: %+v", registered)
					}
					current, err := storage.GetRepo(ctx, registration.ID)
					if err != nil || !reflect.DeepEqual(current, registered) {
						test.Fatalf("authorized registration did not commit exactly its returned metadata: %+v %v", current, err)
					}
					bound, err := storage.GetBoundRepo(ctx, fixture.repository.ID)
					if err != nil || !reflect.DeepEqual(bound, registered) {
						test.Fatalf("authorized registration did not commit its repository binding: %+v %v", bound, err)
					}
				} else {
					if !reflect.DeepEqual(registered, domain.Repo{}) {
						test.Fatalf("denied first registration returned partial metadata: %+v", registered)
					}
					if _, err := storage.GetRepo(ctx, registration.ID); !errors.Is(err, domain.ErrNotFound) {
						test.Fatalf("denied first registration created a metadata repo: %v", err)
					}
					if _, err := storage.GetBoundRepo(ctx, fixture.repository.ID); !errors.Is(err, domain.ErrNotFound) {
						test.Fatalf("denied first registration created a repository binding: %v", err)
					}
				}
				if after := registrationScopeImage(test, ctx, observer, registration.ID, fixture.repository.ID); after != expectedEffects {
					test.Fatalf("registration left unexpected durable effects: got=%+v want=%+v", after, expectedEffects)
				}
				afterAudits, err := storage.ListOrganizationAudit(ctx, fixture.organization.ID, 100)
				if err != nil {
					test.Fatal(err)
				}
				registrationScopeAudits(test, fixture, audits, afterAudits, mutation, allowed)
				if _, err := service.EnsureRepo(ctx, fixture.member.ID, registration); !errors.Is(err, domain.ErrForbidden) {
					test.Fatalf("later registration retained revoked authority: %v", err)
				}
				if current := registrationScopeImage(test, ctx, observer, registration.ID, fixture.repository.ID); current != expectedEffects {
					test.Fatalf("later denied registration produced partial durable effects: %+v", current)
				}
				if allowed {
					if current, err := storage.GetRepo(ctx, registration.ID); err != nil || !reflect.DeepEqual(current, registered) {
						test.Fatalf("later denied registration changed previously committed metadata: %+v %v", current, err)
					}
				}
				if current, err := storage.ListOrganizationAudit(ctx, fixture.organization.ID, 100); err != nil || !reflect.DeepEqual(current, afterAudits) {
					test.Fatalf("later denied registration produced an audit effect: %v", err)
				}
			})
		}
	}
}
