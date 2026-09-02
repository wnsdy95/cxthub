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
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type githubWebhookBackend struct {
	Backend
	promoted int
	err      error
	calls    []githubWebhookPromotion
}

type githubWebhookPromotion struct {
	gitURL string
	base   string
	head   string
}

func (b *githubWebhookBackend) PromoteMergedPR(
	_ context.Context,
	gitURL, baseBranch, headBranch string,
) (int, error) {
	b.calls = append(b.calls, githubWebhookPromotion{
		gitURL: gitURL,
		base:   baseBranch,
		head:   headBranch,
	})
	return b.promoted, b.err
}

type policyDownIdentity struct {
	*app.IdentityService
	user domain.User
}

func (p policyDownIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	return p.user, nil
}

func (p policyDownIdentity) GetWorkspace(context.Context, string) (domain.Workspace, error) {
	return domain.Workspace{}, domain.ErrNotFound
}

func (p policyDownIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return domain.RoleMaintainer, true
}

// newTestServer returns an httptest server with a real Handler using the FSStore backend.
func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	return httptest.NewServer(NewServer(svc, idSvc).Handler())
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

func TestGitHubWebhookSecurityAndPromotion(t *testing.T) {
	const secret = "test-webhook-secret"
	merged := []byte(`{
		"action":"closed",
		"pull_request":{
			"merged":true,
			"base":{"ref":"main"},
			"head":{"ref":"feature/x","repo":{"full_name":"acme/project"}}
		},
		"repository":{
			"full_name":"acme/project",
			"html_url":"https://github.com/acme/project",
			"clone_url":"https://github.com/acme/project.git"
		}
	}`)

	t.Run("disabled when secret is unset", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", "")
		backend := &githubWebhookBackend{}
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), "", "pull_request", merged)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
		if len(backend.calls) != 0 {
			t.Fatalf("promotion calls = %#v, want none", backend.calls)
		}
	})

	t.Run("rejects bad signature", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/hooks/github", bytes.NewReader(merged))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", "sha256=bad")
		rec := httptest.NewRecorder()
		NewServer(backend, nil).Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if len(backend.calls) != 0 {
			t.Fatalf("promotion calls = %#v, want none", backend.calls)
		}
	})

	t.Run("ignores other events", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{}
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, "ping", []byte(`{"zen":"safe"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if len(backend.calls) != 0 {
			t.Fatalf("promotion calls = %#v, want none", backend.calls)
		}
	})

	t.Run("ignores unmerged pull request", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{}
		body := bytes.Replace(merged, []byte(`"merged":true`), []byte(`"merged":false`), 1)
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, "pull_request", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if len(backend.calls) != 0 {
			t.Fatalf("promotion calls = %#v, want none", backend.calls)
		}
	})

	t.Run("ignores fork head", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{}
		body := bytes.Replace(merged, []byte(`"full_name":"acme/project"}}`), []byte(`"full_name":"fork/project"}}`), 1)
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, "pull_request", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if len(backend.calls) != 0 {
			t.Fatalf("promotion calls = %#v, want none", backend.calls)
		}
	})

	t.Run("promotes same-repository merged head", func(t *testing.T) {
		t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)
		backend := &githubWebhookBackend{promoted: 1}
		rec := githubWebhookRequest(t, NewServer(backend, nil).Handler(), secret, "pull_request", merged)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		if len(backend.calls) != 1 {
			t.Fatalf("promotion calls = %#v, want one", backend.calls)
		}
		want := githubWebhookPromotion{
			gitURL: "https://github.com/acme/project.git",
			base:   "main",
			head:   "feature/x",
		}
		if backend.calls[0] != want {
			t.Fatalf("promotion call = %#v, want %#v", backend.calls[0], want)
		}
		var response map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if response["status"] != "ok" || response["promoted"] != float64(1) {
			t.Fatalf("response = %#v, want status ok and promoted 1", response)
		}
	})
}

func TestGitHubWebhookPromotesDivergedContextEndToEnd(t *testing.T) {
	const secret = "integration-webhook-secret"
	t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", secret)

	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	repoID := domain.HashContent([]byte("github.com/acme/project"))
	if _, err := st.PutRepo(context.Background(), domain.Repo{
		ID:            repoID,
		DefaultBranch: "main",
		GitRemoteURL:  "git@github.com:acme/project.git",
	}); err != nil {
		t.Fatal(err)
	}
	base, mainTip, featureTip := domain.HashContent([]byte("base")), domain.HashContent([]byte("main")), domain.HashContent([]byte("feature"))
	for _, snap := range []domain.Snapshot{
		{ID: base, RepoID: repoID, DocHash: base},
		{ID: mainTip, RepoID: repoID, DocHash: mainTip, Parents: []domain.ContentHash{base}},
		{ID: featureTip, RepoID: repoID, DocHash: featureTip, Parents: []domain.ContentHash{base}},
	} {
		if err := st.PutSnapshot(context.Background(), snap); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]domain.ContentHash{"main": mainTip, "feature/x": featureTip} {
		if err := st.CompareAndSwapRef(context.Background(), repoID, domain.Ref{
			Kind: domain.RefBranch, Name: name, RepoID: repoID, Target: target,
		}, ""); err != nil {
			t.Fatal(err)
		}
	}

	body := []byte(`{
		"action":"closed",
		"pull_request":{
			"merged":true,
			"base":{"ref":"main"},
			"head":{"ref":"feature/x","repo":{"full_name":"acme/project"}}
		},
		"repository":{
			"full_name":"acme/project",
			"clone_url":"https://github.com/acme/project.git"
		}
	}`)
	handler := NewServer(svc, nil).Handler()
	for attempt := 1; attempt <= 2; attempt++ {
		rec := githubWebhookRequest(t, handler, secret, "pull_request", body)
		if rec.Code != http.StatusOK {
			t.Fatalf("attempt %d status = %d, body=%s", attempt, rec.Code, rec.Body.String())
		}
		var response struct {
			Promoted int `json:"promoted"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		want := 1
		if attempt == 2 {
			want = 0
		}
		if response.Promoted != want {
			t.Fatalf("attempt %d promoted = %d, want %d", attempt, response.Promoted, want)
		}
	}

	mainRef, err := st.GetRef(context.Background(), repoID, domain.RefBranch, "main")
	if err != nil || mainRef.Target != featureTip {
		t.Fatalf("main ref = (%s, %v), want feature tip %s", mainRef.Target, err, featureTip)
	}
	promoted, err := st.GetSnapshot(context.Background(), repoID, featureTip)
	if err != nil {
		t.Fatal(err)
	}
	if len(promoted.Parents) != 1 || promoted.Parents[0] != base {
		t.Fatalf("natural parents changed: %v, want [%s]", promoted.Parents, base)
	}
	if len(promoted.GraftParents) != 1 || promoted.GraftParents[0] != mainTip {
		t.Fatalf("graft parents = %v, want prior main tip %s", promoted.GraftParents, mainTip)
	}
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

func TestDeletePendingExpectedTargetCAS(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, idSvc).Handler())
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me); code != http.StatusOK {
		t.Fatalf("me status=%d", code)
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/workspaces", map[string]any{"name": "PendingCAS"}, &ws); code != http.StatusOK {
		t.Fatalf("workspace status=%d", code)
	}
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug
	repoID := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/repos", map[string]any{
		"id": repoID, "remote_url": remoteURL, "default_branch": "main",
	}, nil); code != http.StatusOK {
		t.Fatalf("repo status=%d", code)
	}
	target := domain.HashContent([]byte("pending target"))
	if err := st.PutSnapshot(context.Background(), domain.Snapshot{
		ID: target, RepoID: repoID, DocHash: target, Branch: "main", Message: "fixture",
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutPending(context.Background(), repoID, "session-cas", domain.Pending{Target: target, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repoID)) + "/pending/session-cas"
	var out map[string]string
	wrong := domain.HashContent([]byte("wrong target"))
	if code := doJSON(t, http.MethodDelete, base+"?expect="+url.QueryEscape(string(wrong)), nil, &out); code != http.StatusOK || out["status"] != "kept" {
		t.Fatalf("stale CAS status=%d body=%v", code, out)
	}
	out = nil
	if code := doJSON(t, http.MethodDelete, base+"?expect="+url.QueryEscape(string(target)), nil, &out); code != http.StatusOK || out["status"] != "deleted" {
		t.Fatalf("matching CAS status=%d body=%v", code, out)
	}
}

func TestPushChunksRejectsOversizedJSONBody(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me); code != http.StatusOK || me.Username == "" {
		t.Fatalf("me status=%d username=%q", code, me.Username)
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/workspaces", map[string]any{"name": "ChunkLimit"}, &ws); code != http.StatusOK {
		t.Fatalf("workspace status=%d", code)
	}
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug
	repoID := repoIDForRemoteURLForTest(remoteURL)
	if code := doJSON(t, http.MethodPost, ts.URL+"/api/v1/repos", map[string]any{
		"id": repoID, "remote_url": remoteURL, "default_branch": "main",
	}, nil); code != http.StatusOK {
		t.Fatalf("repo status=%d", code)
	}

	// Four raw MiB becomes >4 MiB after JSON base64 expansion, so the HTTP
	// boundary must return our explicit 413 before service/hash validation.
	body := map[string]any{"chunks": []map[string]any{{
		"hash": domain.HashContent([]byte("not the oversized body")),
		"data": bytes.Repeat([]byte{'x'}, 4<<20),
	}}}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repoID))
	if code := doJSON(t, http.MethodPost, base+"/push/chunks", body, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized push/chunks status=%d, want %d", code, http.StatusRequestEntityTooLarge)
	}
	pullBody := map[string]any{"chunk_wants": []string{strings.Repeat("x", inbound.MaxChunkWantJSONBody)}}
	if code := doJSON(t, http.MethodPost, base+"/pull/chunks", pullBody, nil); code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized pull/chunks status=%d, want %d", code, http.StatusRequestEntityTooLarge)
	}
}

// TestPushPullRoundtrip: validates the round trip of negotiate → objects → ref PUT → manifest → pull.
func TestPushPullRoundtrip(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	// 0) Workspace creation + repo registration — actual CLI flow (web ws creation → Connect → EnsureRepo).
	// Unowned repos have a write rejection policy (review #5), so they must be registered with the binding remote URL.
	var me struct {
		Username string `json:"username"`
	}
	if code := doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me); code != 200 || me.Username == "" {
		t.Fatalf("me lookup failed (code %d, username %q)", 0, me.Username)
	}
	var ws struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Roundtrip"}, &ws); code != 200 || ws.Slug == "" {
		t.Fatalf("workspace creation failed (slug %q)", ws.Slug)
	}
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug // workspace URL (2-segment)
	rid := repoIDForRemoteURLForTest(remoteURL)
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(rid))
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": rid, "remote_url": remoteURL, "default_branch": "main"}, nil); code != 200 {
		t.Fatalf("repo create code %d", code)
	}

	// content-addressed snapshot/doc configuration
	cir := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull, GitBranch: "main"},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Seq: 0, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "hi"}}}},
	}
	cb, _ := domain.CanonicalBytes(cir)
	h := domain.HashContent(cb)
	doc := domain.SessionDoc{Hash: h, CIR: cir}
	snap := domain.Snapshot{
		ID: h, RepoID: rid, Branch: "main", Branches: []string{"forged-client-projection"},
		DocHash: h, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Message: "first",
	}

	// 1) negotiate: server is empty, both want
	var neg struct {
		SnapshotWants []domain.ContentHash `json:"snapshot_wants"`
		DocWants      []domain.ContentHash `json:"doc_wants"`
	}
	if code := doJSON(t, "POST", base+"/push/negotiate", map[string]any{"snapshot_haves": []domain.ContentHash{h}, "doc_haves": []domain.ContentHash{h}}, &neg); code != 200 {
		t.Fatalf("negotiate code %d", code)
	}
	if len(neg.SnapshotWants) != 1 || len(neg.DocWants) != 1 {
		t.Fatalf("expected 1/1 wants, got %d/%d", len(neg.SnapshotWants), len(neg.DocWants))
	}

	// 2) objects upload
	if code := doJSON(t, "POST", base+"/push/objects", map[string]any{"snapshots": []domain.Snapshot{snap}, "docs": []domain.SessionDoc{doc}}, nil); code != 200 {
		t.Fatalf("objects code %d", code)
	}
	// expected_seq is a necessary CAS to prevent the delayed queue from reviving the supersede of join.
	if code := doJSON(t, "POST", base+"/snapshots/"+url.PathEscape(string(h))+"/graft", map[string]any{
		"parents": []domain.ContentHash{h},
	}, nil); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing expected_seq code %d (want 422)", code)
	}

	// 3) ref PUT (fast-forward, new)
	if code := doJSON(t, "PUT", base+"/refs/branch/main", map[string]any{"target": h}, nil); code != 200 {
		t.Fatalf("ref put code %d", code)
	}

	// 3b) bounded ref batch: lifecycle event precedes its mutable branch
	// projection, and a later archive removes only that projection.
	const batchBranchName = "feature/batch-lifecycle"
	activeEvent, err := domain.NewBranchLifecycleRef(rid, batchBranchName, h, 1, domain.BranchActive)
	if err != nil {
		t.Fatal(err)
	}
	batchBranch := domain.Ref{Kind: domain.RefBranch, Name: batchBranchName, RepoID: rid, Target: h}
	var batchOut inbound.UpdateRefsOutput
	if code := doJSON(t, "POST", base+"/refs/batch", map[string]any{"updates": []map[string]any{
		{"ref": activeEvent}, {"ref": batchBranch},
	}}, &batchOut); code != 200 || batchOut.Applied != 2 {
		t.Fatalf("ref batch create code=%d out=%+v", code, batchOut)
	}
	archiveEvent, err := domain.NewBranchLifecycleRef(rid, batchBranchName, h, 2, domain.BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	if code := doJSON(t, "POST", base+"/refs/batch", map[string]any{"updates": []map[string]any{{"ref": archiveEvent}}}, &batchOut); code != 200 || batchOut.Applied != 1 {
		t.Fatalf("ref batch archive code=%d out=%+v", code, batchOut)
	}
	var projectedRefs []domain.Ref
	if code := doJSON(t, "GET", base+"/refs", nil, &projectedRefs); code != 200 {
		t.Fatalf("list projected refs code %d", code)
	}
	foundArchivedBranch, foundArchiveEvent := false, false
	for _, ref := range projectedRefs {
		foundArchivedBranch = foundArchivedBranch || (ref.Kind == domain.RefBranch && ref.Name == batchBranchName)
		foundArchiveEvent = foundArchiveEvent || ref.Name == archiveEvent.Name
	}
	if foundArchivedBranch || !foundArchiveEvent {
		t.Fatalf("batch archive projection refs=%+v", projectedRefs)
	}

	// 4) negotiate re-request → no longer want (dedup)
	var neg2 struct {
		SnapshotWants []domain.ContentHash `json:"snapshot_wants"`
		DocWants      []domain.ContentHash `json:"doc_wants"`
	}
	doJSON(t, "POST", base+"/push/negotiate", map[string]any{"snapshot_haves": []domain.ContentHash{h}, "doc_haves": []domain.ContentHash{h}}, &neg2)
	if len(neg2.SnapshotWants) != 0 || len(neg2.DocWants) != 0 {
		t.Fatalf("after push expected 0 wants, got %d/%d", len(neg2.SnapshotWants), len(neg2.DocWants))
	}

	// 5) manifest: snapshot index + ref
	var man domain.Manifest
	if code := doJSON(t, "GET", base+"/manifest", nil, &man); code != 200 {
		t.Fatalf("manifest code %d", code)
	}
	if len(man.SnapshotIndex) != 1 || man.SnapshotIndex[0] != h {
		t.Fatalf("manifest index: %+v", man.SnapshotIndex)
	}
	foundRef := false
	for _, r := range man.Refs {
		if r.Kind == domain.RefBranch && r.Name == "main" && r.Target == h {
			foundRef = true
		}
	}
	if !foundRef {
		t.Fatalf("manifest missing main ref: %+v", man.Refs)
	}

	// 6) pull: snapshot + doc download
	var pull struct {
		Snapshots []domain.Snapshot   `json:"snapshots"`
		Docs      []domain.SessionDoc `json:"docs"`
	}
	doJSON(t, "POST", base+"/pull/objects", map[string]any{"snapshot_wants": []domain.ContentHash{h}}, &pull)
	if len(pull.Snapshots) != 1 || pull.Snapshots[0].Message != "first" {
		t.Fatalf("pull snapshots: %+v", pull.Snapshots)
	}
	if len(pull.Snapshots[0].Branches) != 0 {
		t.Fatalf("client-supplied branch projection persisted: %+v", pull.Snapshots[0].Branches)
	}
	var pull2 struct {
		Docs []domain.SessionDoc `json:"docs"`
	}
	doJSON(t, "POST", base+"/pull/objects", map[string]any{"doc_wants": []domain.ContentHash{h}}, &pull2)
	if len(pull2.Docs) != 1 || pull2.Docs[0].Hash != h {
		t.Fatalf("pull docs: %+v", pull2.Docs)
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
	var ws struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Origin"}, &ws); code != 200 {
		t.Fatalf("workspace creation failed (code %d)", code)
	}

	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug // workspace URL (2-segment)
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

func TestRepoRegistrationIsWorkspaceScopedNotGitURLScoped(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	type meResponse struct {
		ID       string `json:"id"`
		Username string `json:"username"`
	}
	type workspaceResponse struct {
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
	var aliceWS workspaceResponse
	if code := doJSONAs(t, aliceToken, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Orders"}, &aliceWS); code != 200 {
		t.Fatalf("alice workspace code %d", code)
	}
	aliceRemote := "http://cxthub.test/" + alice.Username + "/" + aliceWS.Slug
	aliceRepoID := repoIDForRemoteURLForTest(aliceRemote)
	aliceRepo := map[string]any{
		"id": aliceRepoID, "remote_url": aliceRemote, "default_branch": "main", "git_remote_url": gitURL,
	}

	// Logged-in non-member cannot claim Alice's cxthub namespace.
	if code := doJSONAs(t, charlieToken, "POST", ts.URL+"/api/v1/repos", aliceRepo, nil); code != http.StatusForbidden {
		t.Fatalf("cross-workspace registration code %d, want 403", code)
	}
	if code := doJSONAs(t, aliceToken, "POST", ts.URL+"/api/v1/repos", aliceRepo, nil); code != 200 {
		t.Fatalf("alice repo registration code %d", code)
	}

	var bob meResponse
	if code := doJSONAs(t, bobToken, "GET", ts.URL+"/api/v1/me", nil, &bob); code != 200 {
		t.Fatalf("bob me code %d", code)
	}
	var bobWS workspaceResponse
	if code := doJSONAs(t, bobToken, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Orders"}, &bobWS); code != 200 {
		t.Fatalf("bob workspace code %d", code)
	}
	bobRemote := "http://cxthub.test/" + bob.Username + "/" + bobWS.Slug
	bobRepoID := repoIDForRemoteURLForTest(bobRemote)
	if bobRepoID == aliceRepoID {
		t.Fatal("different cxthub workspace URLs produced the same repo ID")
	}
	if code := doJSONAs(t, bobToken, "POST", ts.URL+"/api/v1/repos", map[string]any{
		"id": bobRepoID, "remote_url": bobRemote, "default_branch": "main", "git_remote_url": gitURL,
	}, nil); code != 200 {
		t.Fatalf("same Git origin in Bob workspace code %d, want 200", code)
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

func TestPublicWorkspaceProjectionRedactsCapabilitiesAndPolicies(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var ws struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "PublicSecure"}, &ws); code != 200 {
		t.Fatalf("workspace create code %d", code)
	}
	if code := doJSON(t, "PATCH", ts.URL+"/api/v1/workspaces/"+url.PathEscape(ws.ID), map[string]any{
		"visibility": "public", "webhook_url": "https://hooks.slack.test/services/secret",
		"secrets_policy": "owner", "settings_policy": "owner",
	}, nil); code != 200 {
		t.Fatalf("workspace patch code %d", code)
	}

	assertRedacted := func(label string, raw map[string]any) {
		t.Helper()
		for _, key := range []string{"webhook_url", "secrets_policy", "settings_policy", "gh_visibility_sync", "gh_synced_at", "owner_id"} {
			if _, ok := raw[key]; ok {
				t.Fatalf("%s exposed %s: %+v", label, key, raw)
			}
		}
	}
	var publicWS map[string]any
	if code := doJSONAs(t, "", "GET", ts.URL+"/api/v1/public/workspaces/"+url.PathEscape(me.Username)+"/"+url.PathEscape(ws.Slug), nil, &publicWS); code != 200 {
		t.Fatalf("public workspace code %d", code)
	}
	assertRedacted("public workspace", publicWS)

	var profile struct {
		User       map[string]any   `json:"user"`
		Workspaces []map[string]any `json:"workspaces"`
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
	if len(profile.Workspaces) != 1 {
		t.Fatalf("public workspaces: %+v", profile.Workspaces)
	}
	assertRedacted("public profile workspace", profile.Workspaces[0])
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
	var ws struct {
		Slug string `json:"slug"`
	}
	doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "DocLess"}, &ws)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug
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
	var ws struct {
		Slug string `json:"slug"`
	}
	doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Sec"}, &ws)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug
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
	if code := doJSON(t, "PUT", sec, unbounded, nil); code != http.StatusUnprocessableEntity {
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
		if code := doJSON(t, "PUT", c.url, env(c.fp), nil); code != c.want {
			t.Fatalf("%s: got %d, want %d", c.name, code, c.want)
		}
	}
}

func TestOptionalTeamAssetsReturnNoContentWhenUnset(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, http.MethodGet, ts.URL+"/api/v1/me", nil, &me)
	var ws struct {
		Slug string `json:"slug"`
	}
	doJSON(t, http.MethodPost, ts.URL+"/api/v1/workspaces", map[string]any{"name": "Optional"}, &ws)
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug
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
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/workspaces", strings.NewReader(`{"name":"ctx"}`))
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
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "ctok"}, nil); code != 200 {
		t.Fatalf("application/json POST: got %d, want 200", code)
	}
}

func TestRateLimitDoesNotCountRejectedRequests(t *testing.T) {
	s := &Server{}
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
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	user := domain.User{ID: "cookie-user", Email: "cookie@example.com", Name: "Cookie", Username: "cookie"}
	if err := st.UpsertUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	rawToken := domain.NewID("sess_")
	if err := st.CreateSession(context.Background(), domain.Session{
		Token: domain.HashToken(rawToken), UserID: user.ID, Kind: "web",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(NewServer(svc, idSvc).Handler())
	defer ts.Close()

	request := func(origin, csrf string) int {
		body, _ := json.Marshal(map[string]any{"name": "CookieWorkspace"})
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/workspaces", bytes.NewReader(body))
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
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "BearerWorkspace"}, nil); code != http.StatusOK {
		t.Fatalf("Bearer request code %d", code)
	}
}

func TestConfiguredProxyOriginPassesCookieCSRF(t *testing.T) {
	t.Setenv("CXT_CORS_ORIGINS", "https://cxthub.com")

	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	user := domain.User{ID: "proxy-user", Email: "proxy@example.com", Name: "Proxy", Username: "proxy"}
	if err := st.UpsertUser(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	rawToken := domain.NewID("sess_")
	if err := st.CreateSession(context.Background(), domain.Session{
		Token: domain.HashToken(rawToken), UserID: user.ID, Kind: "web",
		CreatedAt: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"name": "ProxyWorkspace"})
	req := httptest.NewRequest(http.MethodPost, "https://cxtd-123456789.asia-northeast3.run.app/api/v1/workspaces", bytes.NewReader(body))
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
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, idSvc).Handler())
	defer ts.Close()

	ctx := context.Background()
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
		"id": next, "remote_url": "http://cxthub.test/missing/workspace", "default_branch": "main",
	}, nil); code != http.StatusForbidden {
		t.Fatalf("repo create with missing workspace code %d, want 403", code)
	}
	if _, err := st.GetRepo(ctx, next); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("missing-workspace repo was stored: %v", err)
	}
}

func TestAboutWebsiteRejectsUnsafeScheme(t *testing.T) {
	ts := newTestServer(t)
	defer ts.Close()

	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var ws struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "About"}, &ws); code != 200 {
		t.Fatalf("workspace create code %d", code)
	}
	remoteURL := "http://cxthub.test/" + me.Username + "/" + ws.Slug
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

func TestWorkspacePolicyLookupFailureBlocksAction(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	idSvc := app.NewIdentityService(auth.NewDevVerifier(), st)
	id := policyDownIdentity{
		IdentityService: idSvc,
		user:            domain.User{ID: "u_policy", Email: "policy@example.com", Username: "policy"},
	}
	ts := httptest.NewServer(NewServer(svc, id).Handler())
	defer ts.Close()

	rid := domain.ContentHash("sha256:" + strings.Repeat("d", 63) + "1")
	if _, err := st.PutRepo(context.Background(), domain.Repo{ID: rid, DefaultBranch: "main", WorkspaceID: "ws_missing"}); err != nil {
		t.Fatal(err)
	}

	code := doJSON(t, "PUT", ts.URL+"/api/v1/repos/"+url.PathEscape(string(rid))+"/settings/claude", map[string]any{
		"files": []any{},
	}, nil)
	if code != http.StatusNotFound {
		t.Fatalf("policy lookup failure code %d, want 404", code)
	}
}
