package app

import (
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type mcpManagementStore interface {
	outbound.RepositoryStore
	outbound.OAuthStore
	outbound.OAuthManagement
}

func TestMCPApplicationRevocation(t *testing.T) {
	runMCPApplicationRevocation(t, store.NewFSStore(t.TempDir()))
}
func runMCPApplicationRevocation(t *testing.T, st mcpManagementStore) {
	ctx := systemTestContext()
	s := NewIdentityService(nil, st)
	user := domain.User{ID: domain.NewID("u_"), Email: "mcp@example.test", Username: "mcp-" + domain.NewID("")[:10]}
	other := domain.User{ID: domain.NewID("u_"), Email: "other@example.test", Username: "mcp-" + domain.NewID("")[:10]}
	for _, u := range []domain.User{user, other} {
		if err := st.UpsertUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	client := domain.OAuthClient{ID: domain.NewID("client_"), Name: "Test connector", RedirectURIs: []string{"http://127.0.0.1/callback"}, CreatedAt: time.Now().UTC()}
	if err := st.CreateOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	pair, err := s.IssueMCPTokenPair(ctx, user.ID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	otherPair, err := s.IssueMCPTokenPair(ctx, other.ID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	apps, err := s.ListMCPApplications(ctx, user.ID)
	if err != nil || len(apps) != 1 || apps[0].Name != client.Name {
		t.Fatalf("inventory: %+v %v", apps, err)
	}
	request := domain.OAuthAuthorizationRequest{ID: domain.NewID("req_"), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeChallenge: "challenge", Resource: "https://example.test/mcp", Scope: "mcp:read", CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Minute)}
	if err := st.CreateOAuthAuthorizationRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	codeHash := domain.HashToken("pending-code")
	if _, err := st.ApproveOAuthAuthorizationRequest(ctx, request.ID, user.ID, codeHash, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeMCPApplication(ctx, user.ID, client.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveMCPUser(ctx, pair.AccessToken); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("access survived: %v", err)
	}
	if _, err := s.RefreshMCPAccessToken(ctx, pair.RefreshToken, client.ID); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("refresh survived: %v", err)
	}
	if _, err := s.ExchangeMCPAuthorizationCode(ctx, codeHash, client.ID, request.RedirectURI, request.CodeChallenge, request.Resource); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("unused consent survived: %v", err)
	}
	if _, err := s.ResolveMCPUser(ctx, otherPair.AccessToken); err != nil {
		t.Fatal("other user's app revoked", err)
	}
	apps, err = s.ListMCPApplications(ctx, user.ID)
	if err != nil || len(apps) != 0 {
		t.Fatalf("revoked inventory: %+v %v", apps, err)
	}
	audit, err := s.AccountAudit(ctx, user.ID)
	if err != nil || len(audit) != 1 || audit[0].Action != "mcp.revoked" {
		t.Fatalf("audit: %+v %v", audit, err)
	}
	// A new explicit consent after revocation may authorize the application again.
	request.ID = domain.NewID("req_")
	if err = st.CreateOAuthAuthorizationRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	codeHash = domain.HashToken("new-code")
	if _, err = st.ApproveOAuthAuthorizationRequest(ctx, request.ID, user.ID, codeHash, time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExchangeMCPAuthorizationCode(ctx, codeHash, client.ID, request.RedirectURI, request.CodeChallenge, request.Resource); err != nil {
		t.Fatal("new consent denied", err)
	}
	audit, err = s.AccountAudit(ctx, user.ID)
	if err != nil || len(audit) != 2 || audit[0].Action != "mcp.authorized" {
		t.Fatalf("new consent audit: %+v %v", audit, err)
	}
}
