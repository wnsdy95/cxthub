//go:build postgres

package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMCPApplicationRevocation(t *testing.T) {
	_, st, _ := collaborationPG(t)
	runMCPApplicationRevocation(t, st)
}
func TestPGMCPRefreshRacesApplicationRevocation(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewIdentityService(nil, peer)
	pair, err := f.identity.IssueMCPTokenPair(ctx, f.owner.ID, "test-client")
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() { <-start; done <- other.RevokeMCPApplication(ctx, f.owner.ID, "test-client") }()
	close(start)
	refreshed, err := f.identity.RefreshMCPAccessToken(ctx, pair.RefreshToken, "test-client")
	if err != nil && !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, p := range []domain.OAuthTokenPair{pair, refreshed} {
		if p.AccessToken == "" {
			continue
		}
		if _, err := f.identity.ResolveMCPUser(ctx, p.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("access crossed revocation: %v", err)
		}
		if _, err := f.identity.RefreshMCPAccessToken(ctx, p.RefreshToken, "test-client"); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("refresh crossed revocation: %v", err)
		}
	}
}

func TestPGMCPCodeExchangeRacesApplicationRevocation(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	peer, err := store.NewPostgresStore(ctx, collaborationDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	other := NewIdentityService(nil, peer)
	request, hash := approvedMCPCode(t, st, f.owner.ID)
	start := make(chan struct{})
	done := make(chan error, 1)
	go func() { <-start; done <- other.RevokeMCPApplication(ctx, f.owner.ID, request.ClientID) }()
	close(start)
	pair, err := f.identity.ExchangeMCPAuthorizationCode(ctx, hash, request.ClientID, request.RedirectURI, request.CodeChallenge, request.Resource)
	if err != nil && !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if pair.AccessToken != "" {
		if _, err := f.identity.ResolveMCPUser(ctx, pair.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("code exchange crossed revocation: %v", err)
		}
		if _, err := f.identity.RefreshMCPAccessToken(ctx, pair.RefreshToken, request.ClientID); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatalf("code exchange refresh crossed revocation: %v", err)
		}
	}
	if _, err := f.identity.ExchangeMCPAuthorizationCode(ctx, hash, request.ClientID, request.RedirectURI, request.CodeChallenge, request.Resource); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("revoked code remained exchangeable: %v", err)
	}
}

func approvedMCPCode(t *testing.T, st *store.PostgresStore, user string) (domain.OAuthAuthorizationRequest, string) {
	t.Helper()
	ctx := systemTestContext()
	client := domain.OAuthClient{ID: domain.NewID("client_"), Name: "Concurrent connector", RedirectURIs: []string{"http://127.0.0.1/callback"}, CreatedAt: time.Now().UTC()}
	if err := st.CreateOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	r := domain.OAuthAuthorizationRequest{ID: domain.NewID("req_"), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeChallenge: "challenge", Resource: "https://example.test/mcp", Scope: "mcp:read", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := st.CreateOAuthAuthorizationRequest(ctx, r); err != nil {
		t.Fatal(err)
	}
	hash := domain.HashToken(domain.NewID("code_"))
	if _, err := st.ApproveOAuthAuthorizationRequest(ctx, r.ID, user, hash, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	return r, hash
}

type failedAccountAuditStore struct{ *store.PostgresStore }

func (s failedAccountAuditStore) AppendAccountAudit(context.Context, domain.AccountAuditEvent) error {
	return errors.New("injected account audit failure")
}

func TestPGMCPAuthorizationAndRevocationRequireAudit(t *testing.T) {
	_, st, _ := collaborationPG(t)
	ctx := systemTestContext()
	f := makeTeamFixture(t, st)
	broken := NewIdentityService(nil, failedAccountAuditStore{st})
	r, hash := approvedMCPCode(t, st, f.owner.ID)
	if _, err := broken.ExchangeMCPAuthorizationCode(ctx, hash, r.ClientID, r.RedirectURI, r.CodeChallenge, r.Resource); err == nil {
		t.Fatal("authorization ignored audit failure")
	}
	if sessions, err := st.ListSessionsForUser(ctx, f.owner.ID); err != nil || len(sessions) != 0 {
		t.Fatalf("tokens escaped rollback: %+v %v", sessions, err)
	}
	pair, err := f.identity.ExchangeMCPAuthorizationCode(ctx, hash, r.ClientID, r.RedirectURI, r.CodeChallenge, r.Resource)
	if err != nil {
		t.Fatal("failed authorization consumed consent", err)
	}
	if err := broken.RevokeMCPApplication(ctx, f.owner.ID, r.ClientID); err == nil {
		t.Fatal("revocation ignored audit failure")
	}
	if _, err := f.identity.ResolveMCPUser(ctx, pair.AccessToken); err != nil {
		t.Fatal("revocation escaped rollback", err)
	}
	if _, err := f.identity.RefreshMCPAccessToken(ctx, pair.RefreshToken, r.ClientID); err != nil {
		t.Fatal("refresh removal escaped rollback", err)
	}
}
