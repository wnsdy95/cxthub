package store

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestFSOAuthAuthorizationCodeIsBoundAndConsumedOnce(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	now := time.Now().UTC()
	client := domain.OAuthClient{
		ID: "mcp_client_123", Name: "Codex", RedirectURIs: []string{"http://127.0.0.1/callback"}, CreatedAt: now,
	}
	if err := st.CreateOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	req := domain.OAuthAuthorizationRequest{
		ID: "oauth_req_123", ClientID: client.ID, RedirectURI: client.RedirectURIs[0], State: "state",
		CodeChallenge: "challenge", Resource: "https://cxthub.com/mcp", Scope: "mcp:read",
		CreatedAt: now, ExpiresAt: now.Add(10 * time.Minute),
	}
	if err := st.CreateOAuthAuthorizationRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	code, err := st.ApproveOAuthAuthorizationRequest(ctx, req.ID, "user-1", "tkh_code", time.Now().UTC().Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if code.UserID != "user-1" || code.ClientID != client.ID {
		t.Fatalf("approved code = %+v", code)
	}
	if _, err := st.GetOAuthAuthorizationRequest(ctx, req.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("approved request remains: %v", err)
	}
	if _, err := st.ConsumeOAuthAuthorizationCode(ctx, code.CodeHash, client.ID, code.RedirectURI, "wrong"); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("wrong PKCE error = %v, want unauthorized", err)
	}
	if _, err := st.ConsumeOAuthAuthorizationCode(ctx, code.CodeHash, client.ID, code.RedirectURI, code.CodeChallenge); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ConsumeOAuthAuthorizationCode(ctx, code.CodeHash, client.ID, code.RedirectURI, code.CodeChallenge); !errors.Is(err, domain.ErrUnauthorized) {
		t.Fatalf("replayed code error = %v, want unauthorized", err)
	}
}

func TestFSOAuthDenyConsumesConsentRequest(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	now := time.Now().UTC()
	client := domain.OAuthClient{ID: "mcp_client_456", Name: "Claude", RedirectURIs: []string{"https://claude.ai/callback"}, CreatedAt: now}
	if err := st.CreateOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	req := domain.OAuthAuthorizationRequest{
		ID: "oauth_req_456", ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeChallenge: "challenge",
		Resource: "https://cxthub.com/mcp", Scope: "mcp:read", CreatedAt: now, ExpiresAt: now.Add(time.Minute),
	}
	if err := st.CreateOAuthAuthorizationRequest(ctx, req); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DenyOAuthAuthorizationRequest(ctx, req.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DenyOAuthAuthorizationRequest(ctx, req.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("second deny error = %v, want not found", err)
	}
}

func TestFSOAuthExpiredRecordsAreRemovedLazily(t *testing.T) {
	ctx := context.Background()
	st := NewFSStore(t.TempDir())
	now := time.Now().UTC()
	client := domain.OAuthClient{ID: "mcp_client_expiry", Name: "Codex", RedirectURIs: []string{"http://127.0.0.1/callback"}, CreatedAt: now}
	if err := st.CreateOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	expired := domain.OAuthAuthorizationRequest{
		ID: "oauth_req_expired", ClientID: client.ID, RedirectURI: client.RedirectURIs[0], CodeChallenge: "challenge",
		Resource: "https://cxthub.com/mcp", Scope: "mcp:read", CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
	}
	if err := st.CreateOAuthAuthorizationRequest(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetOAuthAuthorizationRequest(ctx, expired.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired request lookup = %v, want not found", err)
	}
	if _, err := os.Stat(oauthRecordPath(st.oauthRequestsDir(), expired.ID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired request file remains: %v", err)
	}

	expiredCode := domain.OAuthAuthorizationCode{
		CodeHash: "tkh_" + strings.Repeat("a", 64), ClientID: client.ID, RedirectURI: client.RedirectURIs[0], UserID: "user-expiry",
		CodeChallenge: "challenge", Resource: "https://cxthub.com/mcp", Scope: "mcp:read",
		CreatedAt: now.Add(-2 * time.Minute), ExpiresAt: now.Add(-time.Minute),
	}
	codePath := oauthRecordPath(st.oauthCodesDir(), expiredCode.CodeHash)
	if err := writeExclusiveJSON(codePath, expiredCode); err != nil {
		t.Fatal(err)
	}
	if err := pruneExpiredOAuthCodes(st.oauthCodesDir(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(codePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired code file remains: %v", err)
	}
}
