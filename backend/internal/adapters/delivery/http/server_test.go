package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type githubWebhookBackend struct {
	Backend
	repositories int
	calls        []githubWebhookPush
}

type githubWebhookPush struct {
	origin, ref, before, after string
	forced                     bool
	delivery                   string
}

func (backend *githubWebhookBackend) ObserveRemotePush(_ context.Context, origin, ref, before, after string, forced bool, delivery string) (int, error) {
	backend.calls = append(backend.calls, githubWebhookPush{origin, ref, before, after, forced, delivery})
	return backend.repositories, nil
}

type policyDownIdentity struct {
	*app.IdentityService
	user domain.User
}

func (p policyDownIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	return p.user, nil
}

func (p policyDownIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return domain.Repository{}, domain.ErrNotFound
}

func (p policyDownIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return domain.RoleMaintainer, true
}

// newTestServer returns an httptest server with a real Handler using the FSStore backend.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	delivery := NewServer(svc, idSvc)
	setServiceDocumentEventQueries(t, delivery, svc)
	return httptest.NewServer(delivery.Handler())
}

func doJSON(t *testing.T, method, url string, body any, out any) int {
	return doJSONAs(t, "dev:test@t.io:Test", method, url, body, out)
}

func doJSONAs(t *testing.T, token, method, url string, body any, out any) int {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("req: %v", err)
	}
	// 5th role gate: Write path requires authentication — mimics a request logged in with a dev token.
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode < 300 {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func repoIDForRemoteURLForTest(raw string) domain.ContentHash {
	normalized := strings.TrimSpace(raw)
	normalized = strings.TrimPrefix(normalized, "https://")
	normalized = strings.TrimPrefix(normalized, "http://")
	normalized = strings.TrimSuffix(normalized, ".git")
	normalized = strings.TrimRight(normalized, "/")
	return domain.HashContent([]byte(strings.ToLower(normalized)))
}

func githubWebhookRequest(t *testing.T, handler http.Handler, secret, event string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", event)
	if secret != "" {
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write(body)
		req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// The plain webhook is told about pushes: a push to a branch is handed on, so
// that the branch ledger learns it and the remote is read again. A pull
// request event is not acted on: the push that comes with a merge is.
func TestGitHubWebhookSecurityAndPushes(t *testing.T) {
	const secret = "test-webhook-secret"
	head := strings.Repeat("a", 40)
	push := []byte(`{"ref":"refs/heads/topic","before":"` + strings.Repeat("0", 40) + `","after":"` + head + `","forced":false,
		"repository":{"html_url":"https://github.com/acme/project","clone_url":"https://github.com/acme/project.git"}}`)
	merged := []byte(`{"action":"closed","number":7,"pull_request":{"merged":true},"repository":{"full_name":"acme/project"}}`)

	t.Run("disabled when secret is unset", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", "")
		backend := &githubWebhookBackend{}
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), "", "push", push)
		if rec.Code != http.StatusNotFound || len(backend.calls) != 0 {
			t.Fatalf("status = %d, calls = %#v", rec.Code, backend.calls)
		}
	})

	t.Run("rejects bad signature", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", bytes.NewReader(push))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-Hub-Signature-256", "sha256=bad")
		rec := httptest.NewRecorder()
		NewServer(backend, nil).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || len(backend.calls) != 0 {
			t.Fatalf("status = %d, calls = %#v", rec.Code, backend.calls)
		}
	})

	for _, event := range []struct {
		name string
		body []byte
	}{{"ping", []byte(`{"zen":"safe"}`)}, {"pull_request", merged}} {
		t.Run("ignores "+event.name, func(t *testing.T) {
			t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
			backend := &githubWebhookBackend{}
			rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, event.name, event.body)
			var response map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if rec.Code != http.StatusOK || response["status"] != "ignored" || len(backend.calls) != 0 {
				t.Fatalf("status = %d, response = %#v, calls = %#v", rec.Code, response, backend.calls)
			}
		})
	}

	t.Run("ignores a push that is not to a branch", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{}
		tag := bytes.Replace(push, []byte("refs/heads/topic"), []byte("refs/tags/v1"), 1)
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, "push", tag)
		if rec.Code != http.StatusOK || len(backend.calls) != 0 {
			t.Fatalf("status = %d, calls = %#v", rec.Code, backend.calls)
		}
	})

	t.Run("hands a push to a branch on", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{repositories: 1}
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, "push", push)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		// A branch that did not exist before has no commit to come from.
		want := githubWebhookPush{origin: "https://github.com/acme/project.git", ref: "refs/heads/topic", after: head}
		if len(backend.calls) != 1 || backend.calls[0] != want {
			t.Fatalf("calls = %#v, want %#v", backend.calls, want)
		}
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if response["status"] != "accepted" || response["queued"] != float64(1) {
			t.Fatalf("response = %#v, want status accepted and queued 1", response)
		}
	})
}

func TestHealthIsPublicAndMinimal(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/api/v1/health", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	// Cookie exists, safe GET should not be a CSRF target.
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "review-only"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("health request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var out map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("health body: %v", err)
	}
	if len(out) != 1 || out["status"] != "ok" {
		t.Fatalf("health body = %#v, want only status=ok", out)
	}
	for name, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"Cache-Control":          "no-store",
		"Referrer-Policy":        "no-referrer",
		"X-Frame-Options":        "DENY",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("health %s = %q, want %q", name, got, want)
		}
	}
}

// TestGitOriginMismatchRejected: attempting to connect to the same cxthub URL (same RepoID) from a different folder should be rejected with a 409 git_origin_mismatch (onboarding safety measure).
// The first connector confirms the origin, and ssh/https format differences are normalized to be treated as the same.
func TestGitOriginMismatchRejected(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me); code != 200 || me.Username == "" {
		t.Fatalf("me lookup failed (code %d)", code)
	}
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Origin"}, &repositoryRecord); code != 200 {
		t.Fatalf("repository creation failed (code %d)", code)
	}

	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug // repository URL (2-segment)
	rid := repoIDForRemoteURLForTest(remoteURL)
	create := func(gitURL string) int {
		return doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{
			"id": rid, "remote_url": remoteURL, "default_branch": "main", "git_remote_url": gitURL,
		}, nil)
	}

	// 1) First connection — git origin confirmed.
	if code := create("https://github.com/acme/app.git"); code != 200 {
		t.Fatalf("first connection code %d (want 200)", code)
	}
	// 2) Reconnect with the same origin in SSH format — normalization same → allowed.
	if code := create("git@github.com:acme/app.git"); code != 200 {
		t.Fatalf("reconnect with same origin (SSH) code %d (want 200)", code)
	}
	// URL credentials, query, and fragment should be removed before comparison and not stored or returned in responses.
	if code := create("https://oauth:top-secret@github.com/acme/app.git?token=leak#fragment"); code != 200 {
		t.Fatalf("auth token included same origin reconnection code %d (want 200)", code)
	}
	var stored domain.Repo
	if code := doJSON(t, "GET", ts.URL+"/api/v1/repos/"+string(rid), nil, &stored); code != 200 {
		t.Fatalf("repo lookup code %d (want 200)", code)
	}
	if stored.GitRemoteURL != "https://github.com/acme/app.git" || strings.Contains(stored.GitRemoteURL, "top-secret") || strings.Contains(stored.GitRemoteURL, "token=") {
		t.Fatalf("git remote credential leaked through storage/response: %q", stored.GitRemoteURL)
	}
	// 3) Connect from another git origin folder — rejected (409).
	if code := create("https://github.com/evil/other.git"); code != http.StatusConflict {
		t.Fatalf("mismatched origin connection code %d (want 409)", code)
	}
	// 4) After rejection, the confirmed origin remains unchanged — the original origin continues to be allowed.
	if code := create("https://github.com/acme/app"); code != 200 {
		t.Fatalf("confirmed origin reconnection code %d (want 200)", code)
	}
}

func TestRepoRegistrationIsRepositoryScopedNotGitURLScoped(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	type meResponse struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	type repositoryResponse struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	const aliceToken = "dev:alice@example.com:Alice"
	const bobToken = "dev:bob@example.com:Bob"
	const charlieToken = "dev:charlie@example.com:Charlie"
	const gitURL = "git@github.com:acme/shared-orders.git"

	var alice meResponse
	if code := doJSONAs(t, aliceToken, "GET", ts.URL+"/api/v1/me", nil, &alice); code != 200 {
		t.Fatalf("alice me code %d", code)
	}
	var aliceRepository repositoryResponse
	if code := doJSONAs(t, aliceToken, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Orders"}, &aliceRepository); code != 200 {
		t.Fatalf("alice repository code %d", code)
	}
	aliceRemote := "http://cxthub.test/" + alice.Username + "/" + aliceRepository.Slug
	aliceRepoID := repoIDForRemoteURLForTest(aliceRemote)
	aliceRepo := map[string]any{
		"id": aliceRepoID, "remote_url": aliceRemote, "default_branch": "main", "git_remote_url": gitURL,
	}

	// Logged-in non-member cannot claim Alice's cxthub namespace.
	if code := doJSONAs(t, charlieToken, "POST", ts.URL+"/api/v1/repos", aliceRepo, nil); code != http.StatusForbidden {
		t.Fatalf("cross-repository registration code %d, want 403", code)
	}
	if code := doJSONAs(t, aliceToken, "POST", ts.URL+"/api/v1/repos", aliceRepo, nil); code != 200 {
		t.Fatalf("alice repo registration code %d", code)
	}

	var bob meResponse
	if code := doJSONAs(t, bobToken, "GET", ts.URL+"/api/v1/me", nil, &bob); code != 200 {
		t.Fatalf("bob me code %d", code)
	}
	var bobRepository repositoryResponse
	if code := doJSONAs(t, bobToken, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Orders"}, &bobRepository); code != 200 {
		t.Fatalf("bob repository code %d", code)
	}
	bobRemote := "http://cxthub.test/" + bob.Username + "/" + bobRepository.Slug
	bobRepoID := repoIDForRemoteURLForTest(bobRemote)
	if bobRepoID == aliceRepoID {
		t.Fatal("different cxthub repository URLs produced the same repo ID")
	}
	if code := doJSONAs(t, bobToken, "POST", ts.URL+"/api/v1/repos", map[string]any{
		"id": bobRepoID, "remote_url": bobRemote, "default_branch": "main", "git_remote_url": gitURL,
	}, nil); code != 200 {
		t.Fatalf("same Git origin in Bob repository code %d, want 200", code)
	}

	badID := domain.ContentHash("sha256:" + strings.Repeat("9", 64))
	if badID == aliceRepoID {
		badID = domain.ContentHash("sha256:" + strings.Repeat("8", 64))
	}
	if code := doJSONAs(t, aliceToken, "POST", ts.URL+"/api/v1/repos", map[string]any{
		"id": badID, "remote_url": aliceRemote, "default_branch": "main", "git_remote_url": gitURL,
	}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("mismatched cxthub remote repo ID code %d, want 422", code)
	}
}

func TestRepositoryRejectsUnregisteredChildPaths(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me); code != 200 {
		t.Fatal(code)
	}
	var repository domain.Repository
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Platform"}, &repository); code != 200 {
		t.Fatal(code)
	}
	remote := "http://cxthub.test/" + me.Username + "/" + repository.Slug
	var registered domain.Repo
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", domain.Repo{ID: repoIDForRemoteURLForTest(remote), RemoteURL: remote, DefaultBranch: "main"}, &registered); code != 200 {
		t.Fatal(code)
	}
	for _, child := range []string{"backend", "frontend"} {
		path := remote + "/" + child
		if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", domain.Repo{ID: repoIDForRemoteURLForTest(path), RemoteURL: path, DefaultBranch: "main"}, nil); code != 403 {
			t.Fatalf("unknown child registered: %d", code)
		}
	}
	var repos []domain.Repo
	if code := doJSON(t, "GET", ts.URL+"/api/v1/repos?repository="+url.QueryEscape(repository.ID), nil, &repos); code != 200 || len(repos) != 1 || repos[0].ID != registered.ID {
		t.Fatalf("repository binding: %d %+v", code, repos)
	}
}

func TestOrganizationOwnerReadsWritesAndDiscoversAllRepositories(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	const ownerToken = "dev:organization-owner@example.com:Organization Owner"
	const adminToken = "dev:organization-admin@example.com:Organization Admin"
	type meResponse struct {
		ID string `json:"id"`
	}
	var owner, admin meResponse
	if code := doJSONAs(t, ownerToken, http.MethodGet, ts.URL+"/api/v1/me", nil, &owner); code != http.StatusOK {
		t.Fatalf("owner me code %d", code)
	}
	if code := doJSONAs(t, adminToken, http.MethodGet, ts.URL+"/api/v1/me", nil, &admin); code != http.StatusOK {
		t.Fatalf("admin me code %d", code)
	}

	var organization domain.Organization
	if code := doJSONAs(t, ownerToken, http.MethodPost, ts.URL+"/api/v1/organizations", map[string]any{
		"name": "Acme", "slug": "acme",
	}, &organization); code != http.StatusOK {
		t.Fatalf("create organization code %d", code)
	}
	memberURL := ts.URL + "/api/v1/organizations/" + organization.ID + "/members/" + url.PathEscape(admin.ID)
	if code := doJSONAs(t, ownerToken, http.MethodPatch, memberURL, map[string]any{"role": "admin"}, nil); code != http.StatusOK {
		t.Fatalf("add organization admin code %d", code)
	}

	var repository domain.Repository
	if code := doJSONAs(t, adminToken, http.MethodPost, ts.URL+"/api/v1/organizations/"+organization.ID+"/repositories", map[string]any{
		"name": "Platform",
	}, &repository); code != http.StatusOK {
		t.Fatalf("create organization repository code %d", code)
	}
	remoteURL := "http://cxthub.test/acme/" + repository.Slug
	repoID := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSONAs(t, adminToken, http.MethodPost, ts.URL+"/api/v1/repos", map[string]any{
		"id": repoID, "remote_url": remoteURL, "default_branch": "main",
	}, nil); code != http.StatusOK {
		t.Fatalf("register organization repo code %d", code)
	}
	repoBase := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repoID))
	if code := doJSONAs(t, ownerToken, http.MethodGet, repoBase+"/anchored-sessions", nil, nil); code != http.StatusOK {
		t.Fatalf("organization owner denied private context read: code %d", code)
	}
	repositoryPath := ts.URL + "/api/v1/public/repositories/acme/" + url.PathEscape(repository.Slug)
	if code := doJSONAs(t, ownerToken, http.MethodGet, repositoryPath, nil, nil); code != http.StatusOK {
		t.Fatalf("private organization Repository denied owner: code %d", code)
	}

	var grant domain.BreakGlassGrant
	if code := doJSONAs(t, ownerToken, http.MethodPost, ts.URL+"/api/v1/organizations/"+organization.ID+"/break-glass", map[string]any{
		"repository_id": repository.ID, "reason": "production incident investigation", "minutes": 15,
	}, &grant); code != http.StatusOK {
		t.Fatalf("create break-glass code %d", code)
	}
	if grant.RepositoryID != repository.ID {
		t.Fatalf("grant repository = %q, want %q", grant.RepositoryID, repository.ID)
	}
	if code := doJSONAs(t, ownerToken, http.MethodGet, repoBase+"/anchored-sessions", nil, nil); code != http.StatusOK {
		t.Fatalf("break-glass read code %d", code)
	}
	if code := doJSONAs(t, ownerToken, http.MethodGet, repositoryPath, nil, nil); code != http.StatusOK {
		t.Fatalf("break-glass Repository entry code %d", code)
	}
	var repos []domain.Repo
	if code := doJSONAs(t, ownerToken, http.MethodGet, ts.URL+"/api/v1/repos?repository="+url.QueryEscape(repository.ID), nil, &repos); code != http.StatusOK || len(repos) != 1 || repos[0].ID != repoID {
		t.Fatalf("break-glass repository discovery code=%d repos=%+v", code, repos)
	}
	var access struct {
		EffectiveRole string `json:"effective_role"`
		CanTransfer   bool   `json:"can_transfer_ownership"`
	}
	if code := doJSONAs(t, ownerToken, http.MethodPatch, ts.URL+"/api/v1/repositories/"+repository.ID, map[string]any{"secrets_policy": "owner"}, &access); code != http.StatusOK {
		t.Fatalf("owner settings: %d", code)
	}
	var repositories []struct {
		ID            string `json:"id"`
		EffectiveRole string `json:"effective_role"`
		CanTransfer   bool   `json:"can_transfer_ownership"`
	}
	if code := doJSONAs(t, ownerToken, http.MethodGet, ts.URL+"/api/v1/repositories", nil, &repositories); code != 200 || len(repositories) != 1 || repositories[0].ID != repository.ID || repositories[0].EffectiveRole != "owner" || !repositories[0].CanTransfer {
		t.Fatalf("owner projection: %d %+v", code, repositories)
	}
	if code := doJSONAs(t, ownerToken, http.MethodGet, ts.URL+"/api/v1/repository-connections?remote_url="+url.QueryEscape(remoteURL), nil, nil); code != http.StatusOK {
		t.Fatalf("CLI connection: %d", code)
	}

	var audit []domain.OrganizationAuditEvent
	if code := doJSONAs(t, ownerToken, http.MethodGet, ts.URL+"/api/v1/organizations/"+organization.ID+"/audit", nil, &audit); code != http.StatusOK {
		t.Fatalf("audit code %d", code)
	}
	used := false
	for _, event := range audit {
		used = used || event.Action == "organization.break_glass.created"
	}
	if !used {
		t.Fatalf("explicit grant creation not audited: %+v", audit)
	}
}

func TestPublicRepositoryProjectionRedactsCapabilitiesAndPolicies(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "PublicSecure"}, &repositoryRecord); code != 200 {
		t.Fatalf("repository create code %d", code)
	}
	if code := doJSON(t, "PATCH", ts.URL+"/api/v1/repositories/"+url.PathEscape(repositoryRecord.ID), map[string]any{
		"visibility": "public", "webhook_url": "https://hooks.slack.test/services/secret",
		"secrets_policy": "owner", "settings_policy": "owner",
	}, nil); code != 200 {
		t.Fatalf("repository patch code %d", code)
	}

	assertRedacted := func(label string, raw map[string]any) {
		t.Helper()
		for _, key := range []string{"webhook_url", "secrets_policy", "settings_policy", "gh_visibility_sync", "gh_synced_at", "owner_id"} {
			if _, ok := raw[key]; ok {
				t.Fatalf("%s exposed %s: %+v", label, key, raw)
			}
		}
	}
	var publicRepository map[string]any
	if code := doJSONAs(t, "", "GET", ts.URL+"/api/v1/public/repositories/"+url.PathEscape(me.Username)+"/"+url.PathEscape(repositoryRecord.Slug), nil, &publicRepository); code != 200 {
		t.Fatalf("public repository code %d", code)
	}
	assertRedacted("public repository", publicRepository)

	var profile struct {
		User         map[string]any   `json:"user"`
		Repositories []map[string]any `json:"repositories"`
	}
	if code := doJSONAs(t, "", "GET", ts.URL+"/api/v1/public/users/"+url.PathEscape(me.Username), nil, &profile); code != 200 {
		t.Fatalf("public profile code %d", code)
	}
	if _, ok := profile.User["email"]; ok {
		t.Fatalf("public user exposed email: %+v", profile.User)
	}
	if _, ok := profile.User["load_mode"]; ok {
		t.Fatalf("public user exposed load_mode: %+v", profile.User)
	}
	if _, ok := profile.User["id"]; ok {
		t.Fatalf("public user exposed account id: %+v", profile.User)
	}
	if len(profile.Repositories) != 1 {
		t.Fatalf("public repositories: %+v", profile.Repositories)
	}
	assertRedacted("public profile repository", profile.Repositories[0])
}

func TestInternalErrorMessageIsRedacted(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewServer(nil, nil).writeError(recorder, http.StatusInternalServerError, "internal", "pq: password authentication failed for internal-host")
	if strings.Contains(recorder.Body.String(), "pq:") || strings.Contains(recorder.Body.String(), "internal-host") {
		t.Fatalf("internal detail leaked: %s", recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "internal server error") {
		t.Fatalf("generic internal message missing: %s", recorder.Body.String())
	}
}

// TestDocLessSnapshotRejected: If DocHash points to a doc that is not pushed and not in the store, the snapshot must be rejected (object-priority invariant S2 — prevent dangling doc reference, backlog #5).
func TestDocLessSnapshotRejected(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "DocLess"}, &repositoryRecord)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	rid := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": rid, "remote_url": remoteURL, "default_branch": "main"}, nil); code != 200 {
		t.Fatalf("repo create code %d", code)
	}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid))

	// Don't push snapshots without the doc; push only the hash → reject (422).
	missing := domain.ContentHash("sha256:" + strings.Repeat("d", 64))
	snap := domain.Snapshot{ID: missing, RepoID: rid, Branch: "main", DocHash: missing, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Message: "docless"}
	if code := doJSON(t, "POST", base+"/push/objects", map[string]any{"snapshots": []domain.Snapshot{snap}}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("push code for snapshot without doc %d (want 422)", code)
	}
}

// TestSecretsFingerprintConsistency: envelope fingerprint consistency — different fingerprints push is 409,
// same fingerprint/initial setting is 200, rotate=true is replacement, envelope without fingerprint is 400.
func TestSecretsFingerprintConsistency(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Sec"}, &repositoryRecord)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	rid := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": rid, "remote_url": remoteURL, "default_branch": "main"}, nil); code != 200 {
		t.Fatalf("repo create code %d", code)
	}
	sec := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid)) + "/secrets"
	env := func(fp string) map[string]any {
		return map[string]any{"version": 1, "kdf": "PBKDF2-SHA256", "iterations": 600000,
			"salt_b64": "AAAAAAAAAAAAAAAAAAAAAA==", "cipher": "AES-256-GCM",
			"nonce_b64": "AAAAAAAAAAAAAAAA", "ciphertext_b64": "AAAAAAAAAAAAAAAAAAAAAA==", "fingerprint": fp}
	}
	unbounded := env("aaaaaaaaaaaa")
	unbounded["iterations"] = 2_000_000_000
	if code := doJSON(t, "PUT", sec+"?expected_revision=absent", unbounded, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("unbounded KDF envelope code %d, want 422", code)
	}

	cases := []struct {
		name string
		url  string
		fp   string
		want int
	}{
		// Envelopes without fingerprints (old version cxt) are accepted only when the team is still not using the fingerprint system (grandfather —
		// old contract "transparent storage" maintained). After envelopes with fingerprints are in place, consistency checks are impossible and return 400.
		{"no fingerprint + existing none → 200 (old version compatibility)", sec, "", 200},
		{"initial setting(A) → 200", sec, "aaaaaaaaaaaa", 200},
		{"same fingerprint re-push(A) → 200", sec, "aaaaaaaaaaaa", 200},
		{"different fingerprint(B) → 409", sec, "bbbbbbbbbbbb", http.StatusConflict},
		{"No token + existing token → 400", sec, "", http.StatusBadRequest},
		// rotate CAS: expect = replacement base envelope token — must match to pass (stale update protection).
		{"rotate expect mismatch → 409", sec + "?rotate=true&expect=stale", "bbbbbbbbbbbb", http.StatusConflict},
		{"rotate expect match(B) → 200", sec + "?rotate=true&expect=aaaaaaaaaaaa", "bbbbbbbbbbbb", 200},
		{"After replacement A retry → 409", sec, "aaaaaaaaaaaa", http.StatusConflict},
	}
	for _, c := range cases {
		var baseline map[string]any
		code := doJSON(t, "GET", sec, nil, &baseline)
		revision := "absent"
		if code == 200 {
			revision = baseline["revision"].(string)
		}
		sep := "?"
		if strings.Contains(c.url, "?") {
			sep = "&"
		}
		if code := doJSON(t, "PUT", c.url+sep+"expected_revision="+url.QueryEscape(revision), env(c.fp), nil); code != c.want {
			t.Fatalf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
	var base map[string]any
	doJSON(t, "GET", sec, nil, &base)
	old := base["revision"].(string)
	if code := doJSON(t, "PUT", sec, env("bbbbbbbbbbbb"), nil); code != 428 {
		t.Fatalf("unguarded write: %d", code)
	}
	if code := doJSON(t, "PUT", sec+"?expected_revision=absent", env("bbbbbbbbbbbb"), nil); code != 409 {
		t.Fatalf("concurrent create: %d", code)
	}
	var ack map[string]string
	if code := doJSON(t, "PUT", sec+"?expected_revision="+old, env("bbbbbbbbbbbb"), &ack); code != 200 {
		t.Fatalf("first edit: %d", code)
	}
	if ack["revision"] == old || ack["revision"] == "" {
		t.Fatal("write did not advance generation")
	}
	for _, path := range []string{sec + "?expected_revision=" + old, sec + "?rotate=true&expect=bbbbbbbbbbbb&expected_revision=" + old} {
		if code := doJSON(t, "PUT", path, env("bbbbbbbbbbbb"), nil); code != 409 {
			t.Fatalf("stale same-key/rotation write: %d", code)
		}
	}
	var after map[string]any
	doJSON(t, "GET", sec, nil, &after)
	if after["revision"] != ack["revision"] {
		t.Fatal("rejected edit changed server version")
	}

	repositoryURL := ts.URL + "/api/v1/repositories/" + repositoryRecord.ID
	if code := doJSON(t, "PATCH", repositoryURL, map[string]any{"webhook_url": "https://example.test/private-webhook-credential"}, nil); code != 200 {
		t.Fatalf("configure webhook: %d", code)
	}
	if code := doJSON(t, "PUT", sec+"?expected_revision="+ack["revision"], env("bbbbbbbbbbbb"), nil); code != 200 {
		t.Fatalf("write with outbox: %d", code)
	}
	var jobs []domain.NotificationJob
	if code := doJSON(t, "GET", repositoryURL+"/notifications", nil, &jobs); code != 200 || len(jobs) != 1 {
		t.Fatalf("delivery list: %d %+v", code, jobs)
	}
	serialized, _ := json.Marshal(jobs)
	if strings.Contains(string(serialized), "private-webhook-credential") || strings.Contains(string(serialized), "ciphertext") {
		t.Fatal("notification metadata leaked protected values")
	}
	if code := doJSONAs(t, "dev:outsider@t.io:Outsider", "GET", repositoryURL+"/notifications", nil, nil); code != 403 {
		t.Fatalf("outsider read delivery: %d", code)
	}
	retryURL := repositoryURL + "/notifications/" + jobs[0].ID + "/retry"
	if code := doJSONAs(t, "dev:outsider@t.io:Outsider", "POST", retryURL, map[string]any{}, nil); code != 403 {
		t.Fatalf("outsider retried: %d", code)
	}
	if code := doJSON(t, "POST", retryURL, map[string]any{}, nil); code != 200 {
		t.Fatalf("owner retry: %d", code)
	}

}

func TestOptionalTeamAssetsReturnNoContentWhenUnset(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	doJSON(t, http.MethodPost, ts.URL+"/api/v1/repositories", map[string]any{"name": "Optional"}, &repositoryRecord)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	rid := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/repos", map[string]any{
		"id": rid, "remote_url": remoteURL, "default_branch": "main",
	}, nil); code != http.StatusOK {
		t.Fatalf("repo create code %d", code)
	}

	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid))
	for _, path := range []string{
		"/settings/claude",
		"/settings/agents",
		"/settings/codex",
		"/secrets",
	} {
		if code := doJSON(t, http.MethodGet, base+path, nil, nil); code != http.StatusNoContent {
			t.Errorf("GET %s = %d, want 204", path, code)
		}
	}
	if code := doJSON(t, http.MethodGet, base+"/settings/unknown", nil, nil); code != http.StatusNotFound {
		t.Errorf("unknown settings kind = %d, want 404", code)
	}

	if code := doJSON(t, http.MethodPut, base+"/settings/claude", map[string]any{"files": []any{}}, nil); code != http.StatusOK {
		t.Fatalf("configure claude settings = %d, want 200", code)
	}
	var configured domain.SettingsBundle
	if code := doJSON(t, http.MethodGet, base+"/settings/claude", nil, &configured); code != http.StatusOK {
		t.Fatalf("configured claude settings GET = %d, want 200", code)
	}
	if configured.Kind != "claude" || configured.Files == nil {
		t.Errorf("configured bundle = %+v", configured)
	}
}

// TestContentTypeGuard: status change body must be application/json — empty/incorrect Content-Type is 415
// (CSRF 2nd defense regardless of SameSite setting). Correct CT passes.
func TestContentTypeGuard(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()
	for _, ct := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/repositories", strings.NewReader(`{"name":"ctx"}`))
		req.Header.Set("Authorization", "Bearer dev:ct@t.io:CT")
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnsupportedMediaType {
			t.Fatalf("Content-Type %q: got %d, want 415", ct, resp.StatusCode)
		}
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "ctok"}, nil); code != 200 {
		t.Fatalf("application/json POST: got %d, want 200", code)
	}
}

func TestServerDoesNotExposeLocalLoad(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	for _, token := range []string{"", "dev:load@t.io:Load"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/repos/repo-1/load", strings.NewReader(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var body bytes.Buffer
		_, _ = body.ReadFrom(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("token %q status = %d, want 404", token, resp.StatusCode)
		}
		if strings.Contains(body.String(), "not_implemented") {
			t.Fatalf("removed load capability still advertised: %s", body.String())
		}
	}

	// The route is absent, but cookie-authenticated unsafe requests still pass
	// through the global CSRF boundary before mux dispatch.
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/repos/repo-1/load", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "removed-load-session"})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cookie-authenticated removed load route status = %d, want 403", resp.StatusCode)
	}
}

func TestRateLimitDoesNotCountRejectedRequests(t *testing.T) {
	s := &Server{runtime: store.NewFSStore(t.TempDir())}
	accepted := 0
	h := s.rateLimit(2, time.Minute, func(w http.ResponseWriter, _ *http.Request) {
		accepted++
		w.WriteHeader(http.StatusNoContent)
	})
	for i := 0; i < 100; i++ {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "[2001:db8::1]:" + fmt.Sprint(1000+i)
		res := httptest.NewRecorder()
		h(res, req)
		if i < 2 && res.Code != http.StatusNoContent {
			t.Fatalf("request %d status = %d, want 204", i, res.Code)
		}
		if i >= 2 && res.Code != http.StatusTooManyRequests {
			t.Fatalf("request %d status = %d, want 429", i, res.Code)
		}
	}
	if accepted != 2 {
		t.Fatalf("accepted = %d, want 2", accepted)
	}
}

func TestCookieUnsafeMethodsRequireTrustedOriginAndCSRFHeader(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	user := domain.User{ID: "cookie-user", Email: "cookie@example.com", Name: "Cookie", Username: "cookie"}
	if err := st.UpsertUser(systemTestContext(), user); err != nil {
		t.Fatal(err)
	}
	rawToken := domain.NewID("sess_")
	if err := st.CreateSession(systemTestContext(), domain.Session{
		Token: domain.HashToken(rawToken), UserID: user.ID, Kind: "web",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(svc, idSvc).Handler())
	defer ts.Close()

	request := func(origin, csrf string) int {
		body, _ := json.Marshal(map[string]any{"name": "CookieRepository"})
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/repositories", bytes.NewReader(body))
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: rawToken})
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if csrf != "" {
			req.Header.Set("X-Cxt-CSRF", csrf)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	if code := request("", "1"); code != http.StatusForbidden {
		t.Fatalf("missing Origin code %d", code)
	}
	if code := request("https://evil.example", "1"); code != http.StatusForbidden {
		t.Fatalf("foreign Origin code %d", code)
	}
	if code := request(ts.URL, ""); code != http.StatusForbidden {
		t.Fatalf("missing CSRF header code %d", code)
	}
	if code := request(ts.URL, "1"); code != http.StatusOK {
		t.Fatalf("trusted cookie request code %d", code)
	}

	// Bearer CLI continues to operate without Origin/header even if browser cookie CSRF surface is present.
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "BearerRepository"}, nil); code != http.StatusOK {
		t.Fatalf("Bearer request code %d", code)
	}
}

func TestConfiguredProxyOriginPassesCookieCSRF(t *testing.T) {
	t.Setenv("CXT_CORS_ORIGINS", "https://cxthub.com")

	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	user := domain.User{ID: "proxy-user", Email: "proxy@example.com", Name: "Proxy", Username: "proxy"}
	if err := st.UpsertUser(systemTestContext(), user); err != nil {
		t.Fatal(err)
	}
	rawToken := domain.NewID("sess_")
	if err := st.CreateSession(systemTestContext(), domain.Session{
		Token: domain.HashToken(rawToken), UserID: user.ID, Kind: "web",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"name": "ProxyRepository"})
	req := httptest.NewRequest(http.MethodPost, "https://cxtd-123456789.asia-northeast3.run.app/api/v1/repositories", bytes.NewReader(body))
	req.Host = "cxtd-123456789.asia-northeast3.run.app"
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: rawToken})
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://cxthub.com")
	req.Header.Set("X-Cxt-CSRF", "1")
	res := httptest.NewRecorder()
	NewServer(svc, idSvc).Handler().ServeHTTP(res, req)

	if res.Code != http.StatusOK {
		t.Fatalf("proxy cookie request status = %d, want %d; body=%s", res.Code, http.StatusOK, res.Body.String())
	}
	if got := res.Header().Get("Access-Control-Allow-Origin"); got != "https://cxthub.com" {
		t.Fatalf("allow origin = %q, want https://cxthub.com", got)
	}
}

func TestUnboundRepoHiddenAndRejected(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, idSvc).Handler())
	defer ts.Close()

	ctx := systemTestContext()
	rid := domain.ContentHash("sha256:" + strings.Repeat("e", 64))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: rid, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}

	var repos []domain.Repo
	if code := doJSON(t, "GET", ts.URL+"/api/v1/repos", nil, &repos); code != 200 {
		t.Fatalf("list repos code %d", code)
	}
	for _, repo := range repos {
		if repo.ID == rid {
			t.Fatalf("unbound repo leaked in list: %+v", repos)
		}
	}
	if code := doJSON(t, "GET", ts.URL+"/api/v1/repos/"+url.PathEscape(string(rid)), nil, nil); code != http.StatusForbidden {
		t.Fatalf("unbound repo get code %d, want 403", code)
	}

	next := domain.ContentHash("sha256:" + strings.Repeat("f", 64))
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{
		"id": next, "remote_url": "http://cxthub.test/missing/repository", "default_branch": "main",
	}, nil); code != http.StatusForbidden {
		t.Fatalf("repo create with missing repository code %d, want 403", code)
	}
	if _, err := st.GetRepo(ctx, next); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing-repository repo was stored: %v", err)
	}
}

func TestAboutWebsiteRejectsUnsafeScheme(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "About"}, &repositoryRecord); code != 200 {
		t.Fatalf("repository create code %d", code)
	}
	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	rid := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{
		"id": rid, "remote_url": remoteURL, "default_branch": "main",
	}, nil); code != 200 {
		t.Fatalf("repo create code %d", code)
	}
	aboutURL := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid)) + "/about"
	if code := doJSON(t, "PATCH", aboutURL, map[string]any{"website": "javascript:alert(1)"}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("unsafe website code %d, want 422", code)
	}
	if code := doJSON(t, "PATCH", aboutURL, map[string]any{"website": "https://ok.example\nhttps://evil.example"}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("multi-line website code %d, want 422", code)
	}
	var got domain.Repo
	if code := doJSON(t, "PATCH", aboutURL, map[string]any{"website": "example.com"}, &got); code != 200 {
		t.Fatalf("bare website code %d, want 200", code)
	}
	if got.Website != "https://example.com" {
		t.Fatalf("website not stored/normalized: %q", got.Website)
	}
}

func TestRepositoryPolicyLookupFailureBlocksAction(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	id := policyDownIdentity{
		IdentityService: idSvc,
		user:            domain.User{ID: "u_policy", Email: "policy@example.com", Username: "policy"},
	}
	ts := httptest.NewServer(NewServer(svc, id).Handler())
	defer ts.Close()

	rid := domain.ContentHash("sha256:" + strings.Repeat("d", 63) + "1")
	if _, err := st.PutRepo(systemTestContext(), domain.Repo{ID: rid, DefaultBranch: "main", RepositoryID: "ws_" + strings.Repeat("e", 32)}); err != nil {
		t.Fatal(err)
	}

	code := doJSON(t, "PUT", ts.URL+"/api/v1/repos/"+url.PathEscape(string(rid))+"/settings/claude", map[string]any{
		"files": []any{},
	}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("policy lookup failure code %d, want 404", code)
	}
}

func TestPublicPullerCannotManageRepositoryOrTeamAssets(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me); code != http.StatusOK {
		t.Fatalf("owner lookup code %d", code)
	}
	var repositoryRecord struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "PublicAssets"}, &repositoryRecord); code != http.StatusOK {
		t.Fatalf("repository create code %d", code)
	}
	if code := doJSON(t, "PATCH", ts.URL+"/api/v1/repositories/"+url.PathEscape(repositoryRecord.ID), map[string]any{
		"visibility": "public", "public_role": "puller",
	}, nil); code != http.StatusOK {
		t.Fatalf("repository public patch code %d", code)
	}

	remoteURL := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	rid := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{
		"id": rid, "remote_url": remoteURL, "default_branch": "main",
	}, nil); code != http.StatusOK {
		t.Fatalf("repo create code %d", code)
	}

	outsider := "dev:outsider@t.io:Outsider"
	repositoryURL := ts.URL + "/api/v1/repositories/" + url.PathEscape(repositoryRecord.ID)
	if code := doJSONAs(t, outsider, "PATCH", repositoryURL, map[string]any{"name": "Hijacked"}, nil); code != http.StatusForbidden {
		t.Fatalf("public puller repository patch code %d, want 403", code)
	}
	settingsURL := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid)) + "/settings/claude"
	if code := doJSONAs(t, outsider, "PUT", settingsURL, map[string]any{"files": []any{}}, nil); code != http.StatusForbidden {
		t.Fatalf("public puller settings write code %d, want 403", code)
	}
	secretsURL := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid)) + "/secrets"
	if code := doJSONAs(t, outsider, "PUT", secretsURL, map[string]any{"fingerprint": "fp"}, nil); code != http.StatusForbidden {
		t.Fatalf("public puller secrets write code %d, want 403", code)
	}
}
