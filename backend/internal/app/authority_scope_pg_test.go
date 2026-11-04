//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func authorityScopePeer(test *testing.T, ctx context.Context) (*store.PostgresStore, string) {
	test.Helper()
	name := "authority-scope-" + domain.NewID("")
	dsn := collaborationDSN(test)
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		parsed, err := url.Parse(dsn)
		if err != nil {
			test.Fatal(err)
		}
		query := parsed.Query()
		query.Set("application_name", name)
		parsed.RawQuery = query.Encode()
		dsn = parsed.String()
	} else {
		dsn += " application_name=" + name
	}
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	return storage, name
}

func authorityScopeObserver(test *testing.T, ctx context.Context) *pgxpool.Pool {
	test.Helper()
	observer, err := pgxpool.New(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(observer.Close)
	return observer
}

func authorityScopeAwait(test *testing.T, ctx context.Context, ready <-chan struct{}, done <-chan error) {
	test.Helper()
	select {
	case <-ready:
	case err := <-done:
		test.Fatalf("operation ended before its transaction was held: %v", err)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
}

func authorityScopeDone(test *testing.T, ctx context.Context, done <-chan error) error {
	test.Helper()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		test.Fatal(ctx.Err())
		return ctx.Err()
	}
}

func authorityScopeBlocked(test *testing.T, ctx context.Context, observer *pgxpool.Pool, waiter, holder, statement string, done <-chan error) {
	test.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := observer.QueryRow(ctx, `WITH RECURSIVE blockers(pid) AS (
			SELECT unnest(pg_blocking_pids(waiting.pid)) FROM pg_stat_activity waiting
			WHERE waiting.application_name=$1 AND waiting.wait_event_type='Lock'
			AND position($3 in waiting.query)>0
			UNION
			SELECT unnest(pg_blocking_pids(blockers.pid)) FROM blockers
		) SELECT EXISTS (
			SELECT 1 FROM blockers JOIN pg_stat_activity holding ON holding.pid=blockers.pid
			WHERE holding.application_name=$2
		)`, waiter, holder, statement).Scan(&blocked)
		if err != nil {
			test.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case err := <-done:
			test.Fatalf("operation crossed the held SQL boundary: %v", err)
		case <-ctx.Done():
			test.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}

type authorityScopeHoldPG struct {
	*store.PostgresStore
	ready   chan struct{}
	release <-chan struct{}
}

func (storage *authorityScopeHoldPG) WithinIdentity(context.Context, func(context.Context) error) error {
	return errors.New("repository authority mutation used the broad identity transaction")
}

func (storage *authorityScopeHoldPG) WithinRepositoryMetadata(ctx context.Context, repository string, operation func(context.Context) error) error {
	return storage.PostgresStore.WithinRepositoryMetadata(ctx, repository, func(bound context.Context) error {
		if err := operation(bound); err != nil {
			return err
		}
		close(storage.ready)
		select {
		case <-storage.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

type authorityScopeWriteHoldPG struct {
	*store.PostgresStore
	ready   chan struct{}
	release <-chan struct{}
}

func (storage *authorityScopeWriteHoldPG) WithinRepository(ctx context.Context, repo domain.ContentHash, operation func(context.Context) error) error {
	return storage.PostgresStore.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := operation(bound); err != nil {
			return err
		}
		close(storage.ready)
		select {
		case <-storage.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
}

func authorityScopeMutation(ctx context.Context, identity *IdentityService, fixture teamFixture, mutation string) error {
	switch mutation {
	case "role":
		return identity.UpdateMemberRole(ctx, fixture.owner.ID, fixture.repository.ID, fixture.member.ID, domain.RoleViewer)
	case "remove":
		return identity.RemoveMember(ctx, fixture.owner.ID, fixture.repository.ID, fixture.member.ID)
	case "archive":
		archived := true
		_, err := identity.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, RepositoryPatch{Archived: &archived})
		return err
	case "settings-policy":
		policy := "owner"
		_, err := identity.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, RepositoryPatch{SettingsPolicy: &policy})
		return err
	case "secrets-policy":
		policy := "owner"
		_, err := identity.UpdateRepositorySettings(ctx, fixture.owner.ID, fixture.repository.ID, RepositoryPatch{SecretsPolicy: &policy})
		return err
	default:
		return fmt.Errorf("unknown authority mutation: %s", mutation)
	}
}

type authorityScopeFixture struct {
	team    teamFixture
	repo    domain.ContentHash
	base    domain.ContentHash
	tip     domain.ContentHash
	secrets inbound.SaveSecretsInput
}

func authorityScopeSeed(test *testing.T, ctx context.Context, storage *store.PostgresStore, observer *pgxpool.Pool, repo domain.ContentHash) authorityScopeFixture {
	test.Helper()
	fixture := authorityScopeFixture{team: makeTeamFixture(test, storage), repo: repo}
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: fixture.team.repository.ID, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: fixture.team.repository.ID, UserID: fixture.team.member.ID, Role: domain.RoleMaintainer}); err != nil {
		test.Fatal(err)
	}
	webhook := "https://example.test/authority-scope"
	if _, err := fixture.team.identity.UpdateRepositorySettings(ctx, fixture.team.owner.ID, fixture.team.repository.ID, RepositoryPatch{WebhookURL: &webhook}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(systemTestContext(), 5*time.Second)
		defer cancel()
		if _, err := observer.Exec(cleanup, `DELETE FROM notification_outbox WHERE repository_id=$1`, fixture.team.repository.ID); err != nil {
			test.Error(err)
		}
	})
	fixture.base = collaborationSnapshot(test, storage, repo, "authority-base")
	fixture.tip = collaborationSnapshot(test, storage, repo, "authority-tip", fixture.base)
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{Kind: domain.RefBranch, Name: "main", Target: fixture.base}, ""); err != nil {
		test.Fatal(err)
	}
	service := NewService(storage, storage, nil, gitengine.NewEngine(storage), storage)
	owner := inbound.WithRepositoryActor(ctx, fixture.team.owner.ID)
	if err := service.PutSettings(owner, repo, authorityScopeSettings("b2xk")); err != nil {
		test.Fatal(err)
	}
	fixture.secrets = inbound.SaveSecretsInput{RepoID: repo, ActorID: fixture.team.owner.ID, Envelope: secretsTestEnvelope("aaaaaaaaaaaa"), Edit: domain.SecretsEdit{ExpectedRevision: "absent"}}
	ack, err := service.SaveSecrets(ctx, fixture.secrets)
	if err != nil {
		test.Fatal(err)
	}
	fixture.secrets.ActorID = fixture.team.member.ID
	fixture.secrets.Edit.ExpectedRevision = ack.Revision
	return fixture
}

func authorityScopeSettings(content string) domain.SettingsBundle {
	return domain.SettingsBundle{Kind: "claude", Files: []domain.SettingsFile{{Path: "CLAUDE.md", ContentB64: content}}}
}

func authorityScopeWrite(ctx context.Context, service *Service, fixture authorityScopeFixture, kind string) error {
	actor := inbound.WithRepositoryActor(ctx, fixture.team.member.ID)
	switch kind {
	case "ref":
		_, err := service.UpdateRef(actor, inbound.UpdateRefInput{RepoID: fixture.repo, Ref: domain.Ref{Kind: domain.RefBranch, Name: "main", Target: fixture.tip}, ExpectedTarget: fixture.base})
		return err
	case "settings":
		return service.PutSettings(actor, fixture.repo, authorityScopeSettings("bmV3"))
	case "secrets":
		_, err := service.SaveSecrets(actor, fixture.secrets)
		return err
	default:
		return fmt.Errorf("unknown public write: %s", kind)
	}
}

func authorityScopeImage(test *testing.T, ctx context.Context, storage *store.PostgresStore, fixture authorityScopeFixture) []any {
	test.Helper()
	ref, err := storage.GetRef(ctx, fixture.repo, domain.RefBranch, "main")
	if err != nil {
		test.Fatal(err)
	}
	settings, err := storage.GetSettingsBundle(ctx, fixture.repo, "claude")
	if err != nil {
		test.Fatal(err)
	}
	secrets, err := storage.GetSecretsEnvelope(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	revision, err := storage.RepositoryRevision(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	reflog, err := storage.ReadReflog(ctx, fixture.repo)
	if err != nil {
		test.Fatal(err)
	}
	notifications, err := storage.ListNotifications(ctx, fixture.team.repository.ID)
	if err != nil {
		test.Fatal(err)
	}
	return []any{ref, settings, secrets, revision, reflog, notifications}
}

func TestPGAuthorityScopeUnrelatedCustomerProgress(test *testing.T) {
	for _, mutation := range []string{"role", "remove", "archive"} {
		test.Run(mutation, func(test *testing.T) {
			_, storage, repo := collaborationPG(test)
			ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
			defer cancel()
			observer := authorityScopeObserver(test, ctx)
			fixture := authorityScopeSeed(test, ctx, storage, observer, repo)
			otherRepo := hh(test.Name() + domain.NewID(""))
			other := authorityScopeSeed(test, ctx, storage, observer, otherRepo)
			holder, holderName := authorityScopePeer(test, ctx)
			peer, peerName := authorityScopePeer(test, ctx)
			release := make(chan struct{})
			paused := &authorityScopeHoldPG{PostgresStore: holder, ready: make(chan struct{}), release: release}
			done := make(chan error, 1)
			go func() { done <- authorityScopeMutation(ctx, NewIdentityService(nil, paused), fixture.team, mutation) }()
			authorityScopeAwait(test, ctx, paused.ready, done)
			var shared, exclusive bool
			if err := observer.QueryRow(ctx, `SELECT
				COALESCE(bool_or(locks.mode='ShareLock'),false), COALESCE(bool_or(locks.mode='ExclusiveLock'),false)
				FROM pg_locks locks JOIN pg_stat_activity activity ON activity.pid=locks.pid
				WHERE activity.application_name=$1 AND locks.locktype='advisory' AND locks.granted`, holderName).Scan(&shared, &exclusive); err != nil {
				test.Fatal(err)
			}
			if !shared || exclusive {
				test.Fatalf("authority mutation did not retain only the shared identity lock: shared=%v exclusive=%v", shared, exclusive)
			}
			blocked := make(chan error, 1)
			go func() {
				blocked <- authorityScopeWrite(ctx, NewService(peer, peer, nil, gitengine.NewEngine(peer), peer), fixture, "settings")
			}()
			authorityScopeBlocked(test, ctx, observer, peerName, holderName, "repositories", blocked)
			progress := make(chan error, 1)
			go func() {
				identity := NewIdentityService(nil, storage)
				if err := identity.UpdateMemberRole(ctx, other.team.owner.ID, other.team.repository.ID, other.team.member.ID, domain.RoleMember); err != nil {
					progress <- err
					return
				}
				progress <- authorityScopeWrite(ctx, NewService(storage, storage, nil, gitengine.NewEngine(storage), storage), other, "ref")
			}()
			if err := authorityScopeDone(test, ctx, progress); err != nil {
				test.Fatalf("another customer's authority and ref writes could not commit: %v", err)
			}
			close(release)
			if err := authorityScopeDone(test, ctx, done); err != nil {
				test.Fatal(err)
			}
			if err := authorityScopeDone(test, ctx, blocked); !errors.Is(err, domain.ErrForbidden) {
				test.Fatalf("blocked write retained revoked authority: %v", err)
			}
		})
	}
}

func TestPGAuthorityScopePublicWriteOrdering(test *testing.T) {
	for _, kind := range []string{"ref", "settings", "secrets"} {
		for _, mutation := range []string{"role", "remove", "archive", "settings-policy", "secrets-policy"} {
			if mutation == "settings-policy" && kind != "settings" || mutation == "secrets-policy" && kind != "secrets" {
				continue
			}
			for _, order := range []string{"revoke-first", "write-first"} {
				test.Run(kind+"/"+mutation+"/"+order, func(test *testing.T) {
					_, storage, repo := collaborationPG(test)
					ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
					defer cancel()
					observer := authorityScopeObserver(test, ctx)
					fixture := authorityScopeSeed(test, ctx, storage, observer, repo)
					writer, writerName := authorityScopePeer(test, ctx)
					authority, authorityName := authorityScopePeer(test, ctx)
					before := authorityScopeImage(test, ctx, storage, fixture)
					audits, err := storage.ListOrganizationAudit(ctx, fixture.team.organization.ID, 100)
					if err != nil {
						test.Fatal(err)
					}
					release := make(chan struct{})
					first, second := make(chan error, 1), make(chan error, 1)
					if order == "revoke-first" {
						paused := &authorityScopeHoldPG{PostgresStore: authority, ready: make(chan struct{}), release: release}
						go func() { first <- authorityScopeMutation(ctx, NewIdentityService(nil, paused), fixture.team, mutation) }()
						authorityScopeAwait(test, ctx, paused.ready, first)
						go func() {
							second <- authorityScopeWrite(ctx, NewService(writer, writer, nil, gitengine.NewEngine(writer), writer), fixture, kind)
						}()
						authorityScopeBlocked(test, ctx, observer, writerName, authorityName, "repositories", second)
					} else {
						paused := &authorityScopeWriteHoldPG{PostgresStore: writer, ready: make(chan struct{}), release: release}
						go func() {
							first <- authorityScopeWrite(ctx, NewService(paused, paused, nil, gitengine.NewEngine(paused), paused), fixture, kind)
						}()
						authorityScopeAwait(test, ctx, paused.ready, first)
						go func() {
							second <- authorityScopeMutation(ctx, NewIdentityService(nil, authority), fixture.team, mutation)
						}()
						authorityScopeBlocked(test, ctx, observer, authorityName, writerName, "repositories", second)
					}
					if current := authorityScopeImage(test, ctx, storage, fixture); !reflect.DeepEqual(current, before) {
						test.Fatal("uncommitted write escaped its held transaction")
					}
					if current, err := storage.ListOrganizationAudit(ctx, fixture.team.organization.ID, 100); err != nil || !reflect.DeepEqual(current, audits) {
						test.Fatalf("uncommitted audit escaped its held transaction: %v", err)
					}
					close(release)
					if err := authorityScopeDone(test, ctx, first); err != nil {
						test.Fatal(err)
					}
					secondErr := authorityScopeDone(test, ctx, second)
					if order == "revoke-first" {
						if !errors.Is(secondErr, domain.ErrForbidden) {
							test.Fatalf("revoked actor's public write returned %v", secondErr)
						}
						if after := authorityScopeImage(test, ctx, storage, fixture); !reflect.DeepEqual(after, before) {
							test.Fatal("denied write changed ref, settings, secrets, revision, reflog or outbox")
						}
					} else {
						if secondErr != nil {
							test.Fatal(secondErr)
						}
						after := authorityScopeImage(test, ctx, storage, fixture)
						if after[3].(domain.RepositoryRevision).Graph != before[3].(domain.RepositoryRevision).Graph+1 {
							test.Fatal("authorized write did not commit exactly one graph revision")
						}
						addedNotifications := 1
						if kind == "settings" {
							addedNotifications = 0
						}
						if len(after[5].([]domain.NotificationJob)) != len(before[5].([]domain.NotificationJob))+addedNotifications {
							test.Fatal("authorized write did not commit its expected notification outbox effects")
						}
						switch kind {
						case "ref":
							if after[0].(domain.Ref).Target != fixture.tip {
								test.Fatal("authorized ref write was lost")
							}
						case "settings":
							if after[1].(domain.SettingsBundle).Files[0].ContentB64 != "bmV3" {
								test.Fatal("authorized settings write was lost")
							}
						case "secrets":
							if domain.SecretsRevision(after[2].([]byte)) == fixture.secrets.Edit.ExpectedRevision {
								test.Fatal("authorized secrets write was lost")
							}
						}
						if err := authorityScopeWrite(ctx, NewService(writer, writer, nil, gitengine.NewEngine(writer), writer), fixture, kind); !errors.Is(err, domain.ErrForbidden) {
							test.Fatalf("a later write retained revoked authority: %v", err)
						}
						if current := authorityScopeImage(test, ctx, storage, fixture); !reflect.DeepEqual(current, after) {
							test.Fatal("late denial produced partial durable effects")
						}
					}
					afterAudits, err := storage.ListOrganizationAudit(ctx, fixture.team.organization.ID, 100)
					if err != nil {
						test.Fatal(err)
					}
					added := 1
					if order == "write-first" {
						added++
					}
					if len(afterAudits) != len(audits)+added {
						test.Fatalf("authority/write audit count=%d, want %d", len(afterAudits), len(audits)+added)
					}
				})
			}
		}
	}
}

func TestPGAuthorityScopeNarrowMutationPrecedesDocumentIdentityOptIn(test *testing.T) {
	_, storage, repo := collaborationPG(test)
	ctx, cancel := context.WithTimeout(systemTestContext(), 10*time.Second)
	defer cancel()
	fixture := makeTeamFixture(test, storage)
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, RepositoryID: fixture.repository.ID}); err != nil {
		test.Fatal(err)
	}
	observer := authorityScopeObserver(test, ctx)
	holder, holderName := authorityScopePeer(test, ctx)
	peer, peerName := authorityScopePeer(test, ctx)
	release := make(chan struct{})
	paused := &authorityScopeHoldPG{PostgresStore: holder, ready: make(chan struct{}), release: release}
	mutation, optIn := make(chan error, 1), make(chan error, 1)
	go func() { mutation <- authorityScopeMutation(ctx, NewIdentityService(nil, paused), fixture, "archive") }()
	authorityScopeAwait(test, ctx, paused.ready, mutation)
	go func() { optIn <- peer.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1) }()
	authorityScopeBlocked(test, ctx, observer, peerName, holderName, "repositories", optIn)
	close(release)
	if err := authorityScopeDone(test, ctx, mutation); err != nil {
		test.Fatal(err)
	}
	if err := authorityScopeDone(test, ctx, optIn); err != nil {
		test.Fatal(err)
	}
	current, err := storage.GetRepository(ctx, fixture.repository.ID)
	if err != nil || !current.Archived {
		test.Fatalf("mutation ordered before opt-in was lost: %+v %v", current, err)
	}
	if err := authorityScopeMutation(ctx, NewIdentityService(nil, peer), fixture, "archive"); !errors.Is(err, domain.ErrDocumentIdentityUpgradeRequired) {
		test.Fatalf("legacy authority mutation crossed completed opt-in: %v", err)
	}
}
