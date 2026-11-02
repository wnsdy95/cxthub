//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGSnapshotCredentialsDoNotMutate(test *testing.T) {
	_, storage, repo := collaborationPG(test)
	ctx := context.Background()
	identity := NewIdentityService(auth.NewDevVerifier(), storage)
	token := "dev:" + domain.NewID("snapshot") + "@example.test"
	user, err := identity.Authenticate(ctx, token)
	if err != nil {
		test.Fatal(err)
	}
	legacy := domain.Session{Token: domain.NewID("sess_"), UserID: user.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)}
	expired := legacy
	expired.Token, expired.ExpiresAt = domain.NewID("sess_"), time.Now().Add(-time.Second)
	for _, session := range []domain.Session{legacy, expired} {
		if err := storage.CreateSession(ctx, session); err != nil {
			test.Fatal(err)
		}
	}
	const resource = "https://mcp.example.test/mcp"
	pair, err := identity.IssueMCPTokenPair(ctx, user.ID, "snapshot-client", resource)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := identity.ResolveUserForSnapshot(ctx, token); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("snapshot credential accepted an unbound context", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		_, err := identity.ResolveUserForSnapshot(bound, token)
		if !errors.Is(err, domain.ErrConflict) {
			test.Fatal("snapshot credential accepted a write transaction", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	foreign := NewIdentityService(auth.NewDevVerifier(), peer)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		for _, credential := range []string{token, legacy.Token} {
			resolved, err := identity.ResolveUserForSnapshot(bound, credential)
			if err != nil || resolved.ID != user.ID {
				test.Fatal("existing credential rejected", err)
			}
		}
		if _, err := identity.ResolveUserForSnapshot(bound, expired.Token); !errors.Is(err, domain.ErrUnauthorized) {
			test.Fatal("expired token accepted", err)
		}
		for _, credential := range []string{pair.AccessToken, pair.RefreshToken, "dev:unknown-snapshot-user@example.test"} {
			if _, err := identity.ResolveUserForSnapshot(bound, credential); !errors.Is(err, domain.ErrUnauthorized) {
				test.Fatal("invalid REST credential accepted", err)
			}
		}
		if _, err := foreign.ResolveUserForSnapshot(bound, token); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("foreign store escaped snapshot", err)
		}
		if resolved, err := identity.ResolveMCPUserForSnapshot(bound, pair.AccessToken, resource); err != nil || resolved.ID != user.ID {
			test.Fatal("MCP access denied", err)
		}
		if _, err := identity.ResolveMCPUserForSnapshot(bound, pair.AccessToken, "https://other.example.test/mcp"); !errors.Is(err, domain.ErrUnauthorized) {
			test.Fatal("MCP audience ignored", err)
		}
		for _, credential := range []string{token, legacy.Token, pair.RefreshToken} {
			if _, err := identity.ResolveMCPUserForSnapshot(bound, credential, resource); !errors.Is(err, domain.ErrUnauthorized) {
				test.Fatal("invalid MCP credential accepted", err)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	for _, session := range []domain.Session{legacy, expired} {
		if _, err := storage.GetSession(ctx, session.Token); err != nil {
			test.Fatal("snapshot deleted a legacy/expired session", err)
		}
		if _, err := storage.GetSession(ctx, domain.HashToken(session.Token)); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("snapshot migrated a session", err)
		}
	}
	if _, err := storage.GetUser(ctx, "dev:unknown-snapshot-user@example.test"); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("snapshot provisioned user", err)
	}
	if _, err := identity.ResolveUser(ctx, legacy.Token); err != nil {
		test.Fatal("admission migration failed", err)
	}
	if _, err := storage.GetSession(ctx, legacy.Token); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("admission did not migrate legacy session", err)
	}
	if err := identity.RevokeMCPToken(ctx, pair.AccessToken, "snapshot-client"); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		_, err := identity.ResolveMCPUserForSnapshot(bound, pair.AccessToken, resource)
		if !errors.Is(err, domain.ErrUnauthorized) {
			test.Fatal("revoked MCP grant accepted", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

type identityOnlySnapshotStore struct {
	outbound.RepositoryStore
	outbound.IdentityTransactions
	outbound.RepositoryTransactionState
	outbound.RepositoryTransactionPresence
}

type unknownIdentityTransactionStore struct {
	outbound.RepositoryStore
	outbound.IdentityTransactions
}

func TestPGSnapshotReadPermitLifetimeAndBorrowedTransactions(test *testing.T) {
	_, storage, _ := collaborationPG(test)
	ctx := context.Background()
	identity := NewIdentityService(auth.NewDevVerifier(), storage)
	user, err := identity.Authenticate(ctx, "dev:"+domain.NewID("permit")+"@example.test")
	if err != nil {
		test.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, user, "permit")
	if err != nil {
		test.Fatal(err)
	}
	request, cancel := context.WithCancel(ctx)
	permit, err := identity.AdmitRepositoryRead(request, repository.ID, user.ID)
	if err != nil {
		test.Fatal(err)
	}
	defer cancel()
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if err := permit.AuthorizeSnapshot(bound, repository.ID, user.ID); err != nil {
			return err
		}
		if err := permit.AuthorizeSnapshot(bound, repository.ID, "other-user"); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("permit changed actor", err)
		}
		if err := permit.AuthorizeSnapshot(bound, domain.NewID("ws_"), user.ID); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("permit changed repository", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	cancel()
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if err := permit.AuthorizeSnapshot(bound, repository.ID, user.ID); !errors.Is(err, context.Canceled) {
			test.Fatal("permit outlived request", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	narrow := NewIdentityService(nil, &identityOnlySnapshotStore{storage, storage, storage, storage})
	unknown := NewIdentityService(nil, &unknownIdentityTransactionStore{storage, storage})
	if escaped, err := unknown.AdmitRepositoryRead(ctx, repository.ID, user.ID); !errors.Is(err, domain.ErrConflict) || escaped != nil {
		test.Fatal("uninspectable transaction issued a permit", err)
	}
	for _, service := range []*IdentityService{identity, narrow} {
		if err := storage.WithinIdentity(ctx, func(bound context.Context) error {
			escaped, err := service.AdmitRepositoryRead(bound, repository.ID, user.ID)
			if !errors.Is(err, domain.ErrConflict) || escaped != nil {
				test.Fatal("borrowed identity transaction issued permit", err)
			}
			return domain.ErrConflict
		}); !errors.Is(err, domain.ErrConflict) {
			test.Fatal(err)
		}
	}
}

func TestPGSnapshotAuthorizationAgreesWithRevocationSnapshot(test *testing.T) {
	_, storage, _ := collaborationPG(test)
	ctx := context.Background()
	identity := NewIdentityService(auth.NewDevVerifier(), storage)
	owner, err := identity.Authenticate(ctx, "dev:"+domain.NewID("owner")+"@example.test")
	if err != nil {
		test.Fatal(err)
	}
	reader, session, err := identity.Login(ctx, "dev:"+domain.NewID("reader")+"@example.test", "snapshot")
	if err != nil {
		test.Fatal(err)
	}
	repository, err := identity.CreateRepository(ctx, owner, "snapshot-revoke")
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.AddMember(ctx, domain.Membership{RepositoryID: repository.ID, UserID: reader.ID, Role: domain.RoleViewer}); err != nil {
		test.Fatal(err)
	}
	permit, err := identity.AdmitRepositoryRead(ctx, repository.ID, reader.ID)
	if err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	revoker := NewIdentityService(nil, peer)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := identity.ResolveUserForSnapshot(bound, session.Token); err != nil {
			return err
		}
		if err := permit.AuthorizeSnapshot(bound, repository.ID, reader.ID); err != nil {
			return err
		}
		if err := revoker.RemoveMember(ctx, owner.ID, repository.ID, reader.ID); err != nil {
			return err
		}
		if err := revoker.Logout(ctx, session.Token); err != nil {
			return err
		}
		if _, err := identity.ResolveUserForSnapshot(bound, session.Token); err != nil {
			return err
		}
		return permit.AuthorizeSnapshot(bound, repository.ID, reader.ID)
	}); err != nil {
		test.Fatal("one snapshot observed inconsistent credential/permission state", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := identity.ResolveUserForSnapshot(bound, session.Token); !errors.Is(err, domain.ErrUnauthorized) {
			test.Fatal("new snapshot missed credential revocation", err)
		}
		if err := permit.AuthorizeSnapshot(bound, repository.ID, reader.ID); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("new snapshot missed membership revocation", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}
