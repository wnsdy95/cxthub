//go:build postgres

package app

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
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
