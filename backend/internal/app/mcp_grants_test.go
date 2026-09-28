package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestMCPGrantReplayRevokesOnlyItsAuthorization(t *testing.T) {
	runMCPGrantReplay(t, store.NewFSStore(t.TempDir()))
}
func runMCPGrantReplay(t *testing.T, st outbound.RepositoryStore) {
	t.Helper()
	ctx := systemTestContext()
	user := domain.User{ID: domain.NewID("u_"), Username: "grant-" + domain.NewID("")[:10], Email: "grant@example.test"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	s := NewIdentityService(nil, st)
	first, err := s.IssueMCPTokenPair(ctx, user.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	independent, err := s.IssueMCPTokenPair(ctx, user.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.RefreshMCPAccessToken(ctx, first.RefreshToken, "client")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefreshMCPAccessToken(ctx, first.RefreshToken, "client"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("replay accepted", err)
	}
	for _, p := range []domain.OAuthTokenPair{first, second} {
		if _, err = s.ResolveMCPUser(ctx, p.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal("replayed authorization still reads", err)
		}
		if _, err = s.RefreshMCPAccessToken(ctx, p.RefreshToken, "client"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal("replayed authorization still refreshes", err)
		}
	}
	if _, err = s.ResolveMCPUser(ctx, independent.AccessToken); err != nil {
		t.Fatal("separate authorization was revoked", err)
	}
}

func grantUser(t *testing.T, st outbound.RepositoryStore) domain.User {
	t.Helper()
	u := domain.User{ID: domain.NewID("u_"), Username: "grant-" + domain.NewID("")[:10], Email: "grant@example.test"}
	if err := st.UpsertUser(systemTestContext(), u); err != nil {
		t.Fatal(err)
	}
	return u
}
func tokenGrant(t *testing.T, st interface {
	outbound.RepositoryStore
	outbound.OAuthGrants
}, token string) domain.OAuthGrant {
	t.Helper()
	ctx := systemTestContext()
	sess, err := st.GetSession(ctx, domain.HashToken(token))
	if err != nil {
		t.Fatal(err)
	}
	if sess.GrantID == "" {
		t.Fatal("new credential has no grant")
	}
	g, err := st.GetOAuthGrant(ctx, sess.GrantID)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

type mcpGrantStore interface {
	outbound.RepositoryStore
	outbound.OAuthGrants
	outbound.OAuthManagement
}

func TestMCPGrantLifecycleAndLegacy(t *testing.T) {
	runMCPGrantLifecycle(t, store.NewFSStore(t.TempDir()))
}
func runMCPGrantLifecycle(t *testing.T, st mcpGrantStore) {
	ctx := systemTestContext()
	u := grantUser(t, st)
	svc := NewIdentityService(nil, st)
	// Public DCR client IDs are not device labels and must never be truncated.
	client := strings.Repeat("client-", 12)
	first, err := svc.IssueMCPTokenPair(ctx, u.ID, client)
	if err != nil {
		t.Fatal(err)
	}
	g := tokenGrant(t, st, first.AccessToken)
	r := tokenGrant(t, st, first.RefreshToken)
	if g.ID != r.ID {
		t.Fatal("pair has different grants")
	}
	if _, err = svc.RefreshMCPAccessToken(ctx, first.RefreshToken, "foreign"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	second, err := svc.RefreshMCPAccessToken(ctx, first.RefreshToken, client)
	if err != nil {
		t.Fatal(err)
	}
	next := tokenGrant(t, st, second.AccessToken)
	if g.ID != next.ID || !g.CreatedAt.Equal(next.CreatedAt) {
		t.Fatal("refresh changed authorization identity or issuance time")
	}
	// A wrong client with the actual used token must not trigger a revocation.
	if _, err = svc.RefreshMCPAccessToken(ctx, first.RefreshToken, "foreign"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if err = svc.RevokeMCPToken(ctx, first.RefreshToken, "foreign"); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ResolveMCPUser(ctx, second.AccessToken); err != nil {
		t.Fatal(err)
	}
	// Revocation using a consumed refresh token still reaches its authorization.
	if err = svc.RevokeMCPToken(ctx, first.RefreshToken, client); err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.OAuthTokenPair{first, second} {
		if _, err = svc.ResolveMCPUser(ctx, p.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal("access survived revocation", err)
		}
	}
	if err = svc.RevokeMCPToken(ctx, first.RefreshToken, client); err != nil {
		t.Fatal(err)
	}
	audit, err := st.ListAccountAudit(ctx, u.ID)
	if err != nil || len(audit) != 1 || audit[0].GrantID != g.ID || audit[0].Action != "mcp.grant_revoked" {
		t.Fatal("missing or duplicate grant audit", err)
	}
	// A direct CLI token and browser credential stay independent of MCP grants.
	cli, err := svc.CreateCLIToken(ctx, u.ID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ResolveUser(ctx, cli.Token); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.ResolveMCPUser(ctx, cli.Token); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("CLI crossed MCP boundary", err)
	}
	// Upgrade only a presented legacy refresh. No invented association with an
	// older access token, even if the user, client and issuance time match.
	legacyAccess, err := svc.issueSession(ctx, u.ID, "sess_", "mcp_access", client, mcpAccessTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	legacyRefresh, err := svc.issueSession(ctx, u.ID, "refresh_", "mcp_refresh", client, mcpRefreshTokenTTL)
	if err != nil {
		t.Fatal(err)
	}
	upgraded, err := svc.RefreshMCPAccessToken(ctx, legacyRefresh.Token, client)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = svc.RefreshMCPAccessToken(ctx, legacyRefresh.Token, client); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err = svc.ResolveMCPUser(ctx, upgraded.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("legacy replay not detected", err)
	}
	if _, err = svc.ResolveMCPUser(ctx, legacyAccess.Token); err != nil {
		t.Fatal("guessed legacy access relationship", err)
	}
}

func TestMCPGrantRestartAndMissingEvidence(t *testing.T) {
	dir := t.TempDir()
	st := store.NewFSStore(dir)
	ctx := systemTestContext()
	u := grantUser(t, st)
	s := NewIdentityService(nil, st)
	p, err := s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	q, err := s.RefreshMCPAccessToken(ctx, p.RefreshToken, "client")
	if err != nil {
		t.Fatal(err)
	}
	restart := NewIdentityService(nil, store.NewFSStore(dir))
	if _, err = restart.RefreshMCPAccessToken(ctx, p.RefreshToken, "client"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if _, err = restart.ResolveMCPUser(ctx, q.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("restart forgot revocation", err)
	}
	// Incomplete new credentials must fail closed, never become legacy sessions.
	malformed := domain.Session{Token: domain.HashToken("sess_missing_grant"), UserID: u.ID, Kind: "mcp_access", Label: "client", GrantID: "missing", CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}
	if err = st.CreateSession(ctx, malformed); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveMCPUser(ctx, "sess_missing_grant"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("missing grant accepted", err)
	}
	p, err = s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = s.RefreshMCPAccessToken(cancelled, p.RefreshToken, "client"); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
	if _, err = s.RefreshMCPAccessToken(ctx, p.RefreshToken, "client"); err != nil {
		t.Fatal("cancel consumed token", err)
	}
}

// Expired and unrelated capabilities cannot provoke revocation or manufacture
// a new authorization. Missing new-format evidence is never treated as legacy.
func TestMCPGrantExpiredReferenceAndMissingReference(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	ctx := systemTestContext()
	u := grantUser(t, st)
	s := NewIdentityService(nil, st)
	p, err := s.IssueMCPTokenPair(ctx, u.ID, "client")
	if err != nil {
		t.Fatal(err)
	}
	g := tokenGrant(t, st, p.AccessToken)
	expired := "refresh_expired"
	if err = st.CreateOAuthRefreshReference(ctx, domain.OAuthRefreshReference{TokenHash: domain.HashToken(expired), GrantID: g.ID, ExpiresAt: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefreshMCPAccessToken(ctx, expired, "client"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal("expired reference accepted", err)
	}
	if _, err = s.ResolveMCPUser(ctx, p.AccessToken); err != nil {
		t.Fatal("expired reference revoked live grant", err)
	}
	missing := "refresh_missing"
	if err = st.CreateSession(ctx, domain.Session{Token: domain.HashToken(missing), UserID: u.ID, Kind: "mcp_refresh", Label: "client", GrantID: g.ID, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RefreshMCPAccessToken(ctx, missing, "client"); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("missing evidence treated as legacy", err)
	}
	if _, err = s.ResolveMCPUser(ctx, p.AccessToken); err != nil {
		t.Fatal(err)
	}
}
