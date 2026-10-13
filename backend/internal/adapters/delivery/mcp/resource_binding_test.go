package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type resourceBindingStore interface {
	outbound.RepositoryStore
	outbound.OAuthStore
	outbound.OAuthGrants
}

func TestMCPResourceCutover(t *testing.T) {
	directory := t.TempDir()
	runMCPResourceCutover(t, store.NewFSStore(directory), store.NewFSStore(directory))
}

func runMCPResourceCutover(t *testing.T, storage, peer resourceBindingStore) {
	t.Helper()
	ctx := systemTestContext()
	issuer := "https://cxthub.test"
	user := domain.User{ID: domain.NewID("user_"), Username: "audience" + domain.NewID("")[:10], Email: domain.NewID("audience") + "@example.test", Name: "Audience"}
	identity := app.NewIdentityService(fixedVerifier{user: user}, storage)
	_, session, err := identity.Login(ctx, "test-idp-token", "audience test")
	if err != nil {
		t.Fatal(err)
	}
	original, err := NewServerWithOrigins(fakeContextBackend{}, identity, storage, issuer, issuer)
	if err != nil {
		t.Fatal(err)
	}
	cutover, err := NewServerWithOrigins(fakeContextBackend{}, app.NewIdentityService(nil, peer), peer, issuer, "https://mcp.cxthub.test")
	if err != nil {
		t.Fatal(err)
	}
	registered := mcpRequest(t, original.Handler(), http.MethodPost, "/oauth/register", strings.NewReader(`{"client_name":"Audience connector","redirect_uris":["http://127.0.0.1/callback"],"grant_types":["authorization_code","refresh_token"],"response_types":["code"],"token_endpoint_auth_method":"none"}`), map[string]string{"Content-Type": "application/json"})
	if registered.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", registered.Code, registered.Body.String())
	}
	client := decodeJSON[struct {
		ID string `json:"client_id"`
	}](t, registered.Body)
	tokenRequest := func(server *Server, form url.Values) *httptest.ResponseRecorder {
		return mcpRequest(t, server.Handler(), http.MethodPost, issuer+"/oauth/token", strings.NewReader(form.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	}
	authorize := func(server *Server) url.Values {
		verifier := strings.Repeat("v", 64)
		query := url.Values{"response_type": {"code"}, "client_id": {client.ID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_challenge": {pkceChallenge(verifier)}, "code_challenge_method": {"S256"}, "scope": {readScope}, "resource": {server.resource}, "state": {"audience-state"}}
		response := mcpRequest(t, server.Handler(), http.MethodGet, issuer+"/oauth/authorize?"+query.Encode(), nil, nil)
		consent, parseErr := url.Parse(response.Header().Get("Location"))
		if response.Code != http.StatusSeeOther || parseErr != nil || consent.Scheme+"://"+consent.Host != issuer || consent.Path != "/connect/mcp" {
			t.Fatalf("consent redirect: %d %s", response.Code, response.Header().Get("Location"))
		}
		approved := mcpRequest(t, server.Handler(), http.MethodPost, issuer+"/api/v1/oauth/requests/"+consent.Query().Get("request"), strings.NewReader(`{"approve":true}`), map[string]string{"Cookie": webSessionCookie(session.Token), "Content-Type": "application/json", "X-Cxt-CSRF": "1", "Origin": issuer})
		if approved.Code != http.StatusOK {
			t.Fatalf("consent: %d %s", approved.Code, approved.Body.String())
		}
		decision := decodeJSON[struct {
			Redirect string `json:"redirect_url"`
		}](t, approved.Body)
		callback, parseErr := url.Parse(decision.Redirect)
		if parseErr != nil || callback.Query().Get("state") != "audience-state" || callback.Query().Get("code") == "" {
			t.Fatalf("callback: %s %v", decision.Redirect, parseErr)
		}
		return url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {"http://127.0.0.1/callback"}, "code_verifier": {verifier}, "code": {callback.Query().Get("code")}, "resource": {server.resource}}
	}
	assertAccess := func(server *Server, token string, status int) {
		response := mcpRequest(t, server.Handler(), http.MethodPost, server.resource, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`), remoteMCPHeaders(token))
		if response.Code != status {
			t.Fatalf("access at %s: %d want %d: %s", server.resource, response.Code, status, response.Body.String())
		}
		if status == http.StatusUnauthorized && !strings.Contains(response.Header().Get("WWW-Authenticate"), server.resourceOrigin+"/.well-known/oauth-protected-resource/mcp") {
			t.Fatal("wrong audience challenge resource")
		}
	}
	assertDenied := func(response *httptest.ResponseRecorder, code string) {
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), `"error":"`+code+`"`) {
			t.Fatalf("token rejection: %d %s", response.Code, response.Body.String())
		}
	}
	decodeTokens := func(response *httptest.ResponseRecorder) domain.OAuthTokenPair {
		if response.Code != http.StatusOK {
			t.Fatalf("token issuance: %d %s", response.Code, response.Body.String())
		}
		pair := decodeJSON[struct {
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
		}](t, response.Body)
		if pair.AccessToken == "" || pair.RefreshToken == "" {
			t.Fatal("missing token pair")
		}
		return domain.OAuthTokenPair{AccessToken: pair.AccessToken, RefreshToken: pair.RefreshToken}
	}
	refresh := func(server *Server, token, requestedResource string) *httptest.ResponseRecorder {
		return tokenRequest(server, url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {token}, "resource": {requestedResource}})
	}
	assertGrant := func(token, resource string) {
		record, readErr := peer.GetSession(ctx, domain.HashToken(token))
		if readErr != nil {
			t.Fatal(readErr)
		}
		grant, readErr := peer.GetOAuthGrant(ctx, record.GrantID)
		if readErr != nil || grant.Resource != resource || grant.RevokedAt != nil {
			t.Fatalf("persisted grant resource=%q revoked=%v err=%v", grant.Resource, grant.RevokedAt, readErr)
		}
	}

	form := authorize(original)
	assertDenied(tokenRequest(cutover, form), "invalid_target")
	form.Set("resource", cutover.resource)
	assertDenied(tokenRequest(cutover, form), "invalid_grant")
	form.Set("resource", original.resource)
	oldPair := decodeTokens(tokenRequest(original, form))
	assertGrant(oldPair.AccessToken, original.resource)
	assertGrant(oldPair.RefreshToken, original.resource)
	assertAccess(cutover, oldPair.AccessToken, http.StatusUnauthorized)
	assertAccess(original, oldPair.AccessToken, http.StatusOK)
	assertDenied(refresh(cutover, oldPair.RefreshToken, original.resource), "invalid_target")
	assertDenied(refresh(cutover, oldPair.RefreshToken, cutover.resource), "invalid_grant")
	rotated := decodeTokens(refresh(original, oldPair.RefreshToken, original.resource))
	assertGrant(rotated.AccessToken, original.resource)
	assertGrant(rotated.RefreshToken, original.resource)
	assertDenied(refresh(cutover, oldPair.RefreshToken, cutover.resource), "invalid_grant")
	assertAccess(original, rotated.AccessToken, http.StatusOK)
	assertAccess(cutover, rotated.AccessToken, http.StatusUnauthorized)

	reconnected := decodeTokens(tokenRequest(cutover, authorize(cutover)))
	assertGrant(reconnected.AccessToken, cutover.resource)
	assertAccess(cutover, reconnected.AccessToken, http.StatusOK)
	assertAccess(original, reconnected.AccessToken, http.StatusUnauthorized)
	assertDenied(refresh(original, reconnected.RefreshToken, original.resource), "invalid_grant")
	reconnected = decodeTokens(refresh(cutover, reconnected.RefreshToken, cutover.resource))
	assertGrant(reconnected.AccessToken, cutover.resource)
	assertAccess(cutover, reconnected.AccessToken, http.StatusOK)
	assertAccess(original, rotated.AccessToken, http.StatusOK)
	assertDenied(refresh(original, oldPair.RefreshToken, original.resource), "invalid_grant")
	assertAccess(original, rotated.AccessToken, http.StatusUnauthorized)
	assertAccess(cutover, reconnected.AccessToken, http.StatusOK)

	legacy, err := identity.IssueMCPTokenPair(ctx, user.ID, client.ID)
	if err != nil {
		t.Fatal(err)
	}
	legacyAccess, legacyRefresh := domain.NewID("sess_"), domain.NewID("refresh_")
	for _, record := range []domain.Session{
		{Token: domain.HashToken(legacyAccess), UserID: user.ID, Kind: "mcp_access", Label: client.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)},
		{Token: domain.HashToken(legacyRefresh), UserID: user.ID, Kind: "mcp_refresh", Label: client.ID, CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := storage.CreateSession(ctx, record); err != nil {
			t.Fatal(err)
		}
	}
	for _, server := range []*Server{original, cutover} {
		for _, token := range []string{legacy.AccessToken, legacyAccess, session.Token} {
			assertAccess(server, token, http.StatusUnauthorized)
		}
		for _, token := range []string{legacy.RefreshToken, legacyRefresh} {
			assertDenied(refresh(server, token, server.resource), "invalid_grant")
		}
	}
	for _, token := range []string{legacy.AccessToken, legacyAccess} {
		if _, err := identity.ResolveMCPUser(ctx, token); err != nil {
			t.Fatal("HTTP denial changed legacy internal capability", err)
		}
		if _, err := identity.ResolveMCPUser(ctx, token, ""); !errors.Is(err, domain.ErrUnauthorized) {
			t.Fatal("empty expected audience accepted", err)
		}
	}
	for _, token := range []string{legacy.RefreshToken, legacyRefresh} {
		record, readErr := storage.GetSession(ctx, domain.HashToken(token))
		if readErr != nil {
			t.Fatal("audience denial consumed legacy refresh", readErr)
		}
		if token == legacyRefresh && record.GrantID != "" {
			t.Fatal("HTTP denial upgraded resource-less legacy refresh")
		}
	}
	assertGrant(legacy.AccessToken, "")
	if _, err := identity.ResolveUser(context.Background(), session.Token); err != nil {
		t.Fatal("audience enforcement changed web session", err)
	}
}
