package app

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type pausedLegacySessionStore struct {
	outbound.RepositoryStore
	token     string
	afterRead func()
	once      sync.Once
}

func (storage *pausedLegacySessionStore) GetSession(ctx context.Context, token string) (domain.Session, error) {
	session, err := storage.RepositoryStore.GetSession(ctx, token)
	if err == nil && token == storage.token {
		storage.once.Do(storage.afterRead)
	}
	return session, err
}

func (storage *pausedLegacySessionStore) MigrateSession(ctx context.Context, token string) (domain.Session, error) {
	if _, err := storage.GetSession(ctx, token); err != nil {
		return domain.Session{}, err
	}
	return storage.RepositoryStore.MigrateSession(ctx, token)
}

func checkLegacySessionLogoutRace(test *testing.T, storage, peer outbound.RepositoryStore) {
	test.Helper()
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("migration-user-"), Username: domain.NewID("migration-")}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	token := domain.NewID("sess_")
	if err := storage.CreateSession(ctx, domain.Session{Token: token, UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		test.Fatal(err)
	}
	revoker := NewIdentityService(nil, peer)
	paused := &pausedLegacySessionStore{RepositoryStore: storage, token: token, afterRead: func() {
		if err := revoker.Logout(ctx, token); err != nil {
			test.Fatal(err)
		}
	}}
	_, resolveErr := NewIdentityService(nil, paused).ResolveSession(ctx, token)
	if _, err := peer.GetSession(ctx, domain.HashToken(token)); !errors.Is(err, domain.ErrNotFound) {
		test.Errorf("legacy read followed by completed logout resurrected hash: %v", err)
	}
	if _, err := revoker.ResolveSession(ctx, token); !errors.Is(err, domain.ErrUnauthorized) {
		test.Errorf("revoked capability authorizes a subsequent request: %v", err)
	}
	if !errors.Is(resolveErr, domain.ErrUnauthorized) {
		test.Errorf("migration accepted a legacy row deleted before transition: %v", resolveErr)
	}
}

func TestLegacySessionLogoutRace(test *testing.T) {
	directory := test.TempDir()
	checkLegacySessionLogoutRace(test, store.NewFSStore(directory), store.NewFSStore(directory))
}

type recordingSessionRevoker struct {
	outbound.RepositoryStore
	deleted []string
}

func (storage *recordingSessionRevoker) DeleteSession(ctx context.Context, token string) error {
	storage.deleted = append(storage.deleted, token)
	return storage.RepositoryStore.DeleteSession(ctx, token)
}

func checkLegacySessionLogoutSingleRevocation(test *testing.T, storage outbound.RepositoryStore) {
	test.Helper()
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("migration-user-"), Username: domain.NewID("migration-")}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	token := domain.NewID("sess_")
	for _, key := range []string{token, domain.HashToken(token)} {
		if err := storage.CreateSession(ctx, domain.Session{Token: key, UserID: user.ID, Kind: "web", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
			test.Fatal(err)
		}
	}
	recorder := &recordingSessionRevoker{RepositoryStore: storage}
	if err := NewIdentityService(nil, recorder).Logout(ctx, token); err != nil {
		test.Fatal(err)
	}
	if !reflect.DeepEqual(recorder.deleted, []string{token}) {
		test.Fatal("logout did not issue exactly one raw-token revocation")
	}
	for _, key := range []string{token, domain.HashToken(token)} {
		if _, err := storage.GetSession(ctx, key); !errors.Is(err, domain.ErrNotFound) {
			test.Fatalf("logout left a session alias: %v", err)
		}
	}
}

func TestLegacySessionLogoutSingleRevocation(test *testing.T) {
	checkLegacySessionLogoutSingleRevocation(test, store.NewFSStore(test.TempDir()))
}

type migratingSessionRevoker struct {
	outbound.RepositoryStore
	token string
}

func (storage migratingSessionRevoker) DeleteSession(ctx context.Context, token string) error {
	if token == storage.token {
		if _, err := storage.RepositoryStore.MigrateSession(ctx, storage.token); err != nil {
			return err
		}
	}
	return storage.RepositoryStore.DeleteSession(ctx, token)
}

func (storage migratingSessionRevoker) ListSessionsForUser(ctx context.Context, userID string) ([]domain.Session, error) {
	sessions, err := storage.RepositoryStore.ListSessionsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	if _, err := storage.RepositoryStore.MigrateSession(ctx, storage.token); err != nil {
		return nil, err
	}
	return sessions, nil
}

func checkLegacySessionRevocationOrders(test *testing.T, storage outbound.RepositoryStore) {
	test.Helper()
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("migration-user-"), Username: domain.NewID("migration-")}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	for _, mode := range []string{"logout", "web-inventory", "cli-inventory"} {
		test.Run(mode, func(test *testing.T) {
			prefix := "sess_"
			if mode == "cli-inventory" {
				prefix = "sess_cli_"
			}
			token := domain.NewID(prefix)
			if err := storage.CreateSession(ctx, domain.Session{Token: token, UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
				test.Fatal(err)
			}
			identity := NewIdentityService(nil, migratingSessionRevoker{RepositoryStore: storage, token: token})
			var err error
			switch mode {
			case "logout":
				err = identity.Logout(ctx, token)
			case "web-inventory":
				err = identity.RevokeWebSession(ctx, user.ID, domain.TokenHint(token))
			case "cli-inventory":
				err = identity.RevokeCLIToken(ctx, user.ID, domain.TokenHint(token))
			}
			if err != nil {
				test.Fatal(err)
			}
			for _, key := range []string{token, domain.HashToken(token)} {
				if _, err := storage.GetSession(ctx, key); !errors.Is(err, domain.ErrNotFound) {
					test.Fatalf("revocation left a session record: %v", err)
				}
			}
		})
	}
}

func TestLegacySessionRevocationOrders(test *testing.T) {
	checkLegacySessionRevocationOrders(test, store.NewFSStore(test.TempDir()))
}

func checkLegacySessionMigrationIdentity(test *testing.T, storage outbound.RepositoryStore, credentialIDRequired bool) {
	test.Helper()
	ctx := context.Background()
	user := domain.User{ID: domain.NewID("migration-user-"), Username: domain.NewID("migration-")}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	identity := NewIdentityService(nil, storage)
	for _, fixture := range []struct {
		name     string
		prefix   string
		kind     string
		wantKind string
		grant    bool
	}{
		{name: "untyped-web", prefix: "sess_", wantKind: "web"},
		{name: "untyped-cli", prefix: "sess_cli_", wantKind: "cli"},
		{name: "explicit-web", prefix: "sess_cli_", kind: "web", wantKind: "web"},
		{name: "explicit-cli", prefix: "sess_", kind: "cli", wantKind: "cli"},
		{name: "legacy-mcp-access", prefix: "sess_", kind: "mcp_access", wantKind: "mcp_access"},
		{name: "bound-mcp-access", prefix: "sess_", kind: "mcp_access", wantKind: "mcp_access", grant: true},
		{name: "bound-mcp-refresh", prefix: "sess_", kind: "mcp_refresh", wantKind: "mcp_refresh", grant: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			token := domain.NewID(fixture.prefix)
			now := time.Now().UTC().Truncate(time.Microsecond)
			session := domain.Session{Token: token, UserID: user.ID, Kind: fixture.kind, Label: "migration-client", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour)}
			if fixture.grant {
				grant := domain.OAuthGrant{ID: domain.NewID("grant_"), UserID: user.ID, ClientID: session.Label, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
				if err := identity.withIdentity(ctx, func(ctx context.Context) error {
					return storage.(outbound.OAuthGrants).CreateOAuthGrant(ctx, grant)
				}); err != nil {
					test.Fatal(err)
				}
				session.GrantID = grant.ID
			}
			if err := storage.CreateSession(ctx, session); err != nil {
				test.Fatal(err)
			}
			before, err := storage.GetSession(ctx, token)
			if err != nil {
				test.Fatal(err)
			}
			if credentialIDRequired && before.CredentialID == "" {
				test.Fatal("credential fixture has no stable selector")
			}
			migrated, resolved, err := identity.resolveSessionRecord(ctx, token)
			if err != nil || resolved.ID != user.ID {
				test.Fatalf("migration failed: %v", err)
			}
			want := before
			want.Token, want.Hint, want.Kind = domain.HashToken(token), domain.TokenHint(token), fixture.wantKind
			if !reflect.DeepEqual(migrated, want) {
				test.Fatal("migration changed session identity, grant, kind, label or timestamps")
			}
			persisted, err := storage.GetSession(ctx, want.Token)
			if err != nil || !reflect.DeepEqual(persisted, want) {
				test.Fatalf("persisted migration identity mismatch: %v", err)
			}
			if _, err := storage.GetSession(ctx, token); !errors.Is(err, domain.ErrNotFound) {
				test.Fatalf("plaintext remains: %v", err)
			}
			for _, resolve := range []func(context.Context, string) (domain.User, error){identity.ResolveSession, identity.ResolveUser} {
				_, err := resolve(ctx, token)
				if fixture.wantKind == "mcp_access" || fixture.wantKind == "mcp_refresh" {
					if !errors.Is(err, domain.ErrUnauthorized) {
						test.Fatalf("MCP capability crossed generic bearer gate: %v", err)
					}
				} else if err != nil {
					test.Fatal(err)
				}
			}
			_, err = identity.ResolveMCPUser(ctx, token)
			if fixture.wantKind == "mcp_access" {
				if err != nil {
					test.Fatal(err)
				}
			} else if !errors.Is(err, domain.ErrUnauthorized) {
				test.Fatalf("non-access capability crossed MCP gate: %v", err)
			}
			if fixture.grant && fixture.wantKind == "mcp_access" {
				if err := identity.RevokeMCPToken(ctx, token, session.Label); err != nil {
					test.Fatal(err)
				}
				if _, err := identity.ResolveMCPUser(ctx, token); !errors.Is(err, domain.ErrUnauthorized) {
					test.Fatalf("migration detached capability from revoked grant: %v", err)
				}
			}
		})
	}
}

func TestLegacySessionMigrationIdentity(test *testing.T) {
	checkLegacySessionMigrationIdentity(test, store.NewFSStore(test.TempDir()), false)
}

type failedSessionMigrationStore struct {
	outbound.RepositoryStore
	readErr    error
	migrations int
}

func (storage *failedSessionMigrationStore) GetSession(ctx context.Context, token string) (domain.Session, error) {
	if storage.readErr != nil {
		return domain.Session{}, storage.readErr
	}
	return storage.RepositoryStore.GetSession(ctx, token)
}

func (storage *failedSessionMigrationStore) MigrateSession(context.Context, string) (domain.Session, error) {
	storage.migrations++
	return domain.Session{}, errors.New("migration unavailable")
}

func TestLegacySessionMigrationFailsClosed(test *testing.T) {
	ctx := context.Background()
	storage := store.NewFSStore(test.TempDir())
	user := domain.User{ID: "migration-user", Username: "migration-user"}
	if err := storage.UpsertUser(ctx, user); err != nil {
		test.Fatal(err)
	}
	token := domain.NewID("sess_")
	if err := storage.CreateSession(ctx, domain.Session{Token: token, UserID: user.ID, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		test.Fatal(err)
	}
	for _, readErr := range []error{nil, errors.New("read unavailable")} {
		failed := &failedSessionMigrationStore{RepositoryStore: storage, readErr: readErr}
		if _, err := NewIdentityService(nil, failed).ResolveSession(ctx, token); !errors.Is(err, domain.ErrUnauthorized) {
			test.Fatalf("failed persistence accepted stale legacy session: %v", err)
		}
		if (readErr == nil && failed.migrations != 1) || (readErr != nil && failed.migrations != 0) {
			test.Fatal("migration did not require a definite hash miss")
		}
	}
}
