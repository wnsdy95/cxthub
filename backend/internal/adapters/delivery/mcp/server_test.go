package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type fixedVerifier struct{ user domain.User }

func (v fixedVerifier) Verify(context.Context, string) (domain.User, error) { return v.user, nil }

type fakeContextBackend struct {
	repos     []domain.Repo
	snapshots map[domain.ContentHash][]domain.Snapshot
	memories  map[domain.ContentHash]domain.MemoryDigest
}

func (f fakeContextBackend) ListRepos(context.Context, string) ([]domain.Repo, error) {
	return f.repos, nil
}
func (f fakeContextBackend) List(_ context.Context, in inbound.ListSnapshotsInput) ([]domain.Snapshot, error) {
	return f.snapshots[in.RepoID], nil
}
func (fakeContextBackend) Search(context.Context, inbound.SearchInput) (inbound.SearchOutput, error) {
	return inbound.SearchOutput{}, nil
}
func (f fakeContextBackend) GetSnapshot(_ context.Context, repoID, id domain.ContentHash) (domain.Snapshot, error) {
	for _, snapshot := range f.snapshots[repoID] {
		if snapshot.ID == id {
			return snapshot, nil
		}
	}
	return domain.Snapshot{}, domain.ErrNotFound
}
func (fakeContextBackend) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	return domain.SessionDoc{}, domain.ErrNotFound
}
func (f fakeContextBackend) GetMemoryObject(_ context.Context, _ domain.ContentHash, hash domain.ContentHash) (domain.MemoryDigest, error) {
	if memory, ok := f.memories[hash]; ok {
		return memory, nil
	}
	return domain.MemoryDigest{}, domain.ErrNotFound
}
func (fakeContextBackend) ListRefs(context.Context, domain.ContentHash) ([]domain.Ref, error) {
	return nil, nil
}

func mcpRequest(t *testing.T, handler http.Handler, method, target string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, body)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w
}

func decodeJSON[T any](t *testing.T, body io.Reader) T {
	t.Helper()
	var value T
	if err := json.NewDecoder(body).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func remoteMCPHeaders(token string) map[string]string {
	headers := map[string]string{
		"Content-Type": "application/json",
		"Accept":       "application/json, text/event-stream",
	}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	return headers
}

func TestRemoteMCPDCRPKCERefreshAndWorkspaceIsolation(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "oauth-user", Email: "oauth@example.test", Name: "OAuth User", Username: "oauth-user"}
	id := app.NewIdentityService(fixedVerifier{user: user}, st)
	_, webSession, err := id.Login(ctx, "idp-token", "test browser")
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := id.CreateWorkspace(ctx, user, "Project")
	if err != nil {
		t.Fatal(err)
	}
	privateOther := domain.Workspace{
		ID: domain.NewID("ws_"), Name: "Secret", OwnerID: "other-user", Slug: "secret",
		OwnerUsername: "other", CreatedAt: time.Now().UTC(),
	}
	if err := st.UpsertUser(ctx, domain.User{ID: "other-user", Email: "other@example.test", Name: "Other", Username: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateWorkspace(ctx, privateOther); err != nil {
		t.Fatal(err)
	}
	repoID := domain.HashContent([]byte("authorized-repository"))
	otherRepoID := domain.HashContent([]byte("hidden-repository"))
	backend := fakeContextBackend{repos: []domain.Repo{
		{ID: repoID, RemoteURL: "https://cxthub.test/oauth-user/project/app", DefaultBranch: "main", WorkspaceID: workspace.ID},
		{ID: otherRepoID, RemoteURL: "https://cxthub.test/other/secret/private", DefaultBranch: "main", WorkspaceID: privateOther.ID},
	}}
	server, err := NewServer(backend, id, st, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}

	// A client may request only authorization_code; refresh support is a server
	// capability and must not be an unnecessary registration prerequisite.
	registerBody := `{"client_name":"Codex App","redirect_uris":["http://127.0.0.1/callback"],"grant_types":["authorization_code"],"response_types":["code"],"token_endpoint_auth_method":"none"}`
	wrongRegistrationType := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/register", strings.NewReader(registerBody), nil)
	if wrongRegistrationType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("registration without JSON media type = %d: %s", wrongRegistrationType.Code, wrongRegistrationType.Body.String())
	}
	ambiguousRegistration := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/register", strings.NewReader(registerBody+`{}`), map[string]string{"Content-Type": "application/json"})
	if ambiguousRegistration.Code != http.StatusBadRequest {
		t.Fatalf("registration with trailing JSON = %d: %s", ambiguousRegistration.Code, ambiguousRegistration.Body.String())
	}
	registered := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/register", strings.NewReader(registerBody), map[string]string{"Content-Type": "application/json"})
	if registered.Code != http.StatusCreated {
		t.Fatalf("register = %d: %s", registered.Code, registered.Body.String())
	}
	client := decodeJSON[struct {
		ClientID string `json:"client_id"`
	}](t, registered.Body)

	verifier := strings.Repeat("v", 64)
	state := "round-trip-state"
	authorizeQuery := url.Values{
		"response_type":         {"code"},
		"client_id":             {client.ClientID},
		"redirect_uri":          {"http://127.0.0.1/callback"},
		"code_challenge":        {pkceChallenge(verifier)},
		"code_challenge_method": {"S256"},
		"resource":              {"https://cxthub.test/mcp"},
		"scope":                 {readScope},
		"state":                 {state},
	}
	authorized := mcpRequest(t, server.Handler(), http.MethodGet, "/oauth/authorize?"+authorizeQuery.Encode(), nil, nil)
	if authorized.Code != http.StatusSeeOther {
		t.Fatalf("authorize = %d: %s", authorized.Code, authorized.Body.String())
	}
	consentURL, err := url.Parse(authorized.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	requestID := consentURL.Query().Get("request")
	if requestID == "" {
		t.Fatalf("consent redirect missing request: %s", consentURL)
	}
	consentDetails := mcpRequest(t, server.Handler(), http.MethodGet, "/api/v1/oauth/requests/"+requestID, nil, map[string]string{
		"Cookie": webSessionCookie(webSession.Token),
	})
	if consentDetails.Code != http.StatusOK || !strings.Contains(consentDetails.Body.String(), `"redirect_uri":"http://127.0.0.1/callback"`) {
		t.Fatalf("consent details = %d: %s", consentDetails.Code, consentDetails.Body.String())
	}

	consentHeaders := map[string]string{
		"Cookie": webSessionCookie(webSession.Token), "Content-Type": "application/json",
		"X-Cxt-CSRF": "1", "Origin": "https://cxthub.test",
	}
	missingOrigin := mcpRequest(t, server.Handler(), http.MethodPost, "/api/v1/oauth/requests/"+requestID, strings.NewReader(`{"approve":true}`), map[string]string{
		"Cookie": webSessionCookie(webSession.Token), "Content-Type": "application/json", "X-Cxt-CSRF": "1",
	})
	if missingOrigin.Code != http.StatusForbidden {
		t.Fatalf("origin-less consent = %d, want 403: %s", missingOrigin.Code, missingOrigin.Body.String())
	}
	ambiguousConsent := mcpRequest(t, server.Handler(), http.MethodPost, "/api/v1/oauth/requests/"+requestID, strings.NewReader(`{"approve":true}{}`), consentHeaders)
	if ambiguousConsent.Code != http.StatusBadRequest {
		t.Fatalf("consent with trailing JSON = %d: %s", ambiguousConsent.Code, ambiguousConsent.Body.String())
	}
	approved := mcpRequest(t, server.Handler(), http.MethodPost, "/api/v1/oauth/requests/"+requestID, strings.NewReader(`{"approve":true}`), consentHeaders)
	if approved.Code != http.StatusOK {
		t.Fatalf("approve = %d: %s", approved.Code, approved.Body.String())
	}
	decision := decodeJSON[struct {
		RedirectURL string `json:"redirect_url"`
	}](t, approved.Body)
	callback, err := url.Parse(decision.RedirectURL)
	if err != nil {
		t.Fatal(err)
	}
	if callback.Query().Get("state") != state || callback.Query().Get("code") == "" {
		t.Fatalf("callback = %s", callback)
	}

	tokenForm := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {client.ClientID},
		"redirect_uri": {"http://127.0.0.1/callback"}, "code": {callback.Query().Get("code")},
		"code_verifier": {verifier}, "resource": {"https://cxthub.test/mcp"},
	}
	wrongTokenType := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()), map[string]string{"Content-Type": "application/json"})
	if wrongTokenType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("token with JSON media type = %d: %s", wrongTokenType.Code, wrongTokenType.Body.String())
	}
	queryOnlyToken := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token?"+tokenForm.Encode(), nil, map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if queryOnlyToken.Code != http.StatusUnauthorized || !strings.Contains(queryOnlyToken.Body.String(), "invalid_client") {
		t.Fatalf("query-only token fields = %d: %s", queryOnlyToken.Code, queryOnlyToken.Body.String())
	}
	tokenResponse := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if tokenResponse.Code != http.StatusOK {
		t.Fatalf("token = %d: %s", tokenResponse.Code, tokenResponse.Body.String())
	}
	tokens := decodeJSON[struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}](t, tokenResponse.Body)
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		t.Fatalf("tokens = %+v", tokens)
	}

	listCall := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"repository_list","arguments":{}}}`
	mcpResponse := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(listCall), map[string]string{
		"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + tokens.AccessToken,
	})
	if mcpResponse.Code != http.StatusOK || !strings.Contains(mcpResponse.Body.String(), "oauth-user/project/app") {
		t.Fatalf("MCP authorized list = %d: %s", mcpResponse.Code, mcpResponse.Body.String())
	}
	if strings.Contains(mcpResponse.Body.String(), "other/secret/private") {
		t.Fatalf("private non-member repository leaked: %s", mcpResponse.Body.String())
	}
	ambiguousMCP := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(listCall+`{}`), map[string]string{
		"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + tokens.AccessToken,
	})
	if ambiguousMCP.Code != http.StatusBadRequest || !strings.Contains(ambiguousMCP.Body.String(), "invalid JSON-RPC request") {
		t.Fatalf("MCP with trailing JSON = %d: %s", ambiguousMCP.Code, ambiguousMCP.Body.String())
	}

	refreshForm := url.Values{
		"grant_type": {"refresh_token"}, "client_id": {client.ClientID},
		"refresh_token": {tokens.RefreshToken}, "resource": {"https://cxthub.test/mcp"},
	}
	refreshed := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(refreshForm.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if refreshed.Code != http.StatusOK || !strings.Contains(refreshed.Body.String(), "access_token") {
		t.Fatalf("refresh = %d: %s", refreshed.Code, refreshed.Body.String())
	}
	refreshedTokens := decodeJSON[struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}](t, refreshed.Body)
	if refreshedTokens.AccessToken == tokens.AccessToken || refreshedTokens.RefreshToken == tokens.RefreshToken {
		t.Fatalf("refresh did not rotate both capabilities: %+v", refreshedTokens)
	}
	refreshReplay := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(refreshForm.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if refreshReplay.Code != http.StatusBadRequest || !strings.Contains(refreshReplay.Body.String(), "invalid_grant") {
		t.Fatalf("refresh replay = %d: %s", refreshReplay.Code, refreshReplay.Body.String())
	}

	revokeAccess := url.Values{"client_id": {client.ClientID}, "token": {refreshedTokens.AccessToken}, "token_type_hint": {"access_token"}}
	revoked := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/revoke", strings.NewReader(revokeAccess.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if revoked.Code != http.StatusOK {
		t.Fatalf("revoke access = %d: %s", revoked.Code, revoked.Body.String())
	}
	revokedCall := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(listCall), map[string]string{
		"Content-Type": "application/json", "Accept": "application/json, text/event-stream", "Authorization": "Bearer " + refreshedTokens.AccessToken,
	})
	if revokedCall.Code != http.StatusUnauthorized {
		t.Fatalf("revoked access token MCP call = %d: %s", revokedCall.Code, revokedCall.Body.String())
	}

	revokeRefresh := url.Values{"client_id": {client.ClientID}, "token": {refreshedTokens.RefreshToken}, "token_type_hint": {"refresh_token"}}
	for attempt := 0; attempt < 2; attempt++ {
		response := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/revoke", strings.NewReader(revokeRefresh.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
		if response.Code != http.StatusOK {
			t.Fatalf("idempotent refresh revoke attempt %d = %d: %s", attempt+1, response.Code, response.Body.String())
		}
	}
	revokedRefreshForm := url.Values{
		"grant_type": {"refresh_token"}, "client_id": {client.ClientID},
		"refresh_token": {refreshedTokens.RefreshToken}, "resource": {"https://cxthub.test/mcp"},
	}
	revokedRefresh := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(revokedRefreshForm.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if revokedRefresh.Code != http.StatusBadRequest || !strings.Contains(revokedRefresh.Body.String(), "invalid_grant") {
		t.Fatalf("revoked refresh = %d: %s", revokedRefresh.Code, revokedRefresh.Body.String())
	}

	replay := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()), map[string]string{"Content-Type": "application/x-www-form-urlencoded"})
	if replay.Code != http.StatusBadRequest || !strings.Contains(replay.Body.String(), "invalid_grant") {
		t.Fatalf("authorization code replay = %d: %s", replay.Code, replay.Body.String())
	}

	unauthorized := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", bytes.NewBufferString(listCall), map[string]string{"Content-Type": "application/json"})
	if unauthorized.Code != http.StatusUnauthorized || !strings.Contains(unauthorized.Header().Get("WWW-Authenticate"), "oauth-protected-resource") {
		t.Fatalf("unauthorized challenge = %d %q", unauthorized.Code, unauthorized.Header().Get("WWW-Authenticate"))
	}
}

func TestOAuthMetadataAdvertisesReadOnlyResourceAndRevocation(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	server, err := NewServer(fakeContextBackend{}, app.NewIdentityService(fixedVerifier{}, st), st, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}
	metadata := mcpRequest(t, server.Handler(), http.MethodGet, "/.well-known/oauth-authorization-server", nil, nil)
	if metadata.Code != http.StatusOK || !strings.Contains(metadata.Body.String(), `"revocation_endpoint":"https://cxthub.test/oauth/revoke"`) ||
		!strings.Contains(metadata.Body.String(), `"mcp:read"`) {
		t.Fatalf("authorization metadata = %d: %s", metadata.Code, metadata.Body.String())
	}
}

func TestTokenExchangeRejectsAuthorizationCodeWithoutMCPReadScope(t *testing.T) {
	ctx := context.Background()
	st := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "legacy-scope-user", Email: "legacy@example.test", Name: "Legacy", Username: "legacy"}
	if err := st.UpsertUser(ctx, user); err != nil {
		t.Fatal(err)
	}
	client := domain.OAuthClient{
		ID: "legacy-scope-client", Name: "Legacy MCP client",
		RedirectURIs: []string{"http://127.0.0.1/callback"}, CreatedAt: time.Now().UTC(),
	}
	if err := st.CreateOAuthClient(ctx, client); err != nil {
		t.Fatal(err)
	}
	verifier := strings.Repeat("v", 64)
	request := domain.OAuthAuthorizationRequest{
		ID: "legacy-scope-request", ClientID: client.ID, RedirectURI: client.RedirectURIs[0],
		CodeChallenge: pkceChallenge(verifier), Resource: "https://cxthub.test/mcp", Scope: "legacy:read",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(5 * time.Minute),
	}
	if err := st.CreateOAuthAuthorizationRequest(ctx, request); err != nil {
		t.Fatal(err)
	}
	rawCode := "legacy-authorization-code"
	if _, err := st.ApproveOAuthAuthorizationRequest(ctx, request.ID, user.ID, domain.HashToken(rawCode), time.Now().UTC().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	identity := app.NewIdentityService(fixedVerifier{user: user}, st)
	server, err := NewServer(fakeContextBackend{}, identity, st, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{
		"grant_type": {"authorization_code"}, "client_id": {client.ID},
		"redirect_uri": {client.RedirectURIs[0]}, "code": {rawCode},
		"code_verifier": {verifier}, "resource": {"https://cxthub.test/mcp"},
	}
	response := mcpRequest(t, server.Handler(), http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "invalid_grant") {
		t.Fatalf("legacy-scope token exchange = %d: %s", response.Code, response.Body.String())
	}
}

func TestStreamableHTTPTransportGuards(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	user := domain.User{ID: "transport-user", Email: "transport@example.test", Name: "Transport", Username: "transport"}
	id := app.NewIdentityService(fixedVerifier{user: user}, st)
	if err := st.UpsertUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	pair, err := id.IssueMCPTokenPair(context.Background(), user.ID, "transport-client")
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(fakeContextBackend{}, id, st, "https://cxthub.test")
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"jsonrpc":"2.0","id":1,"method":"ping"}`

	missingAccept := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(payload), map[string]string{
		"Content-Type": "application/json", "Authorization": "Bearer " + pair.AccessToken,
	})
	if missingAccept.Code != http.StatusNotAcceptable {
		t.Fatalf("missing transport Accept = %d: %s", missingAccept.Code, missingAccept.Body.String())
	}

	badOriginHeaders := remoteMCPHeaders(pair.AccessToken)
	badOriginHeaders["Origin"] = "https://attacker.example"
	badOrigin := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(payload), badOriginHeaders)
	if badOrigin.Code != http.StatusForbidden {
		t.Fatalf("foreign MCP Origin = %d: %s", badOrigin.Code, badOrigin.Body.String())
	}

	badProtocolHeaders := remoteMCPHeaders(pair.AccessToken)
	badProtocolHeaders["MCP-Protocol-Version"] = "2099-01-01"
	badProtocol := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(payload), badProtocolHeaders)
	if badProtocol.Code != http.StatusBadRequest || !strings.Contains(badProtocol.Body.String(), "unsupported MCP-Protocol-Version") {
		t.Fatalf("unsupported protocol = %d: %s", badProtocol.Code, badProtocol.Body.String())
	}

	validHeaders := remoteMCPHeaders(pair.AccessToken)
	validHeaders["Origin"] = "https://cxthub.test"
	validHeaders["MCP-Protocol-Version"] = latestProtocol
	valid := mcpRequest(t, server.Handler(), http.MethodPost, "/mcp", strings.NewReader(payload), validHeaders)
	if valid.Code != http.StatusOK {
		t.Fatalf("valid Streamable HTTP request = %d: %s", valid.Code, valid.Body.String())
	}

	foreignGET := mcpRequest(t, server.Handler(), http.MethodGet, "/mcp", nil, map[string]string{"Origin": "https://attacker.example", "Accept": "text/event-stream"})
	if foreignGET.Code != http.StatusForbidden {
		t.Fatalf("foreign MCP GET Origin = %d: %s", foreignGET.Code, foreignGET.Body.String())
	}
	validGET := mcpRequest(t, server.Handler(), http.MethodGet, "/mcp", nil, map[string]string{"Accept": "text/event-stream"})
	if validGET.Code != http.StatusMethodNotAllowed || validGET.Header().Get("Allow") != "POST" {
		t.Fatalf("stateless MCP GET = %d allow=%q", validGET.Code, validGET.Header().Get("Allow"))
	}
}

func webSessionCookie(token string) string { return sessionCookieName + "=" + token }

func TestRemoteMCPToolsAreReadOnly(t *testing.T) {
	tools := toolDefinitions()
	if len(tools) != 5 {
		t.Fatalf("tool count = %d, want 5 read-only tools", len(tools))
	}
	for _, tool := range tools {
		annotations := tool["annotations"].(map[string]any)
		if annotations["readOnlyHint"] != true || annotations["destructiveHint"] != false || annotations["idempotentHint"] != true {
			t.Fatalf("tool %v annotations = %v", tool["name"], annotations)
		}
	}
}

func TestRequestGateAndProtocolSpecificRateLimitResponses(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := newRequestGate(2, time.Minute)
	if !gate.allow(now) || !gate.allow(now.Add(time.Second)) {
		t.Fatal("gate rejected an allowed request")
	}
	if gate.allow(now.Add(2 * time.Second)) {
		t.Fatal("gate accepted a request above its window limit")
	}
	if !gate.allow(now.Add(time.Minute + time.Second)) {
		t.Fatal("gate did not release capacity after the window")
	}

	s := &Server{}
	assertLimited := func(t *testing.T, handler http.HandlerFunc, wantFragment string) {
		t.Helper()
		first := mcpRequest(t, handler, http.MethodPost, "/", nil, nil)
		if first.Code != http.StatusNoContent {
			t.Fatalf("first response = %d, want 204", first.Code)
		}
		limited := mcpRequest(t, handler, http.MethodPost, "/", nil, nil)
		if limited.Code != http.StatusTooManyRequests || limited.Header().Get("Retry-After") != "60" || !strings.Contains(limited.Body.String(), wantFragment) {
			t.Fatalf("limited response = %d retry=%q body=%s", limited.Code, limited.Header().Get("Retry-After"), limited.Body.String())
		}
	}
	next := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
	assertLimited(t, s.oauthRateLimit(newRequestGate(1, time.Minute), next), `"temporarily_unavailable"`)
	assertLimited(t, s.apiRateLimit(newRequestGate(1, time.Minute), next), `"rate_limited"`)
	assertLimited(t, s.mcpRateLimit(newRequestGate(1, time.Minute), next), `"code":-32002`)
}

func TestRemoteMemoryOutputIsBounded(t *testing.T) {
	repoID := domain.HashContent([]byte("bounded-memory-repo"))
	snapshotID := domain.HashContent([]byte("bounded-memory-snapshot"))
	memoryHash := domain.HashContent([]byte("bounded-memory-object"))
	facts := make([]string, 45)
	for i := range facts {
		facts[i] = fmt.Sprintf("project fact %02d", i)
	}
	backend := fakeContextBackend{
		snapshots: map[domain.ContentHash][]domain.Snapshot{
			repoID: {{ID: snapshotID, RepoID: repoID, DocHash: snapshotID, MemoryHash: memoryHash}},
		},
		memories: map[domain.ContentHash]domain.MemoryDigest{
			memoryHash: {SnapshotID: snapshotID, Summary: "bounded", KeyFacts: facts},
		},
	}
	s := &Server{context: backend}
	got, err := s.toolMemoryLoad(context.Background(), domain.Repo{ID: repoID, DefaultBranch: "main"}, string(snapshotID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, facts[40]) || !strings.Contains(got, "5 additional facts omitted") {
		t.Fatalf("memory output was not bounded:\n%s", got)
	}
}

func TestRemoteRepositoryListOutputIsBoundedAndFilterable(t *testing.T) {
	repositories := make([]domain.Repo, 105)
	for i := range repositories {
		repositories[i] = domain.Repo{
			ID:            domain.HashContent([]byte(fmt.Sprintf("repository-%03d", i))),
			RemoteURL:     fmt.Sprintf("https://cxthub.test/acme/platform/repository-%03d", i),
			DefaultBranch: "main",
		}
	}
	got, err := formatRepositoryList(repositories, "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if rows := strings.Count(got, "\n-"); rows != 50 || !strings.Contains(got, "55 additional repositories omitted") {
		t.Fatalf("bounded repository list rows=%d:\n%s", rows, got)
	}
	filtered, err := formatRepositoryList(repositories, "repository-104", 100)
	if err != nil || !strings.Contains(filtered, "repository-104") || strings.Contains(filtered, "repository-103") {
		t.Fatalf("filtered repository list = %q, err=%v", filtered, err)
	}
	if _, err := formatRepositoryList(repositories, strings.Repeat("q", 129), 10); err == nil {
		t.Fatal("oversized repository query was accepted")
	}
}
