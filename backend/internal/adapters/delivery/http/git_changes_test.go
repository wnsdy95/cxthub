package http

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitevidence"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestGitChangeAuthorizationAndDurableReadback(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	identity := app.NewIdentityService(auth.NewDevVerifier(), st)
	api := NewServer(svc, identity)
	changes, err := app.NewGitChanges(svc, gitevidence.NewGitHub(nil))
	if err != nil {
		t.Fatal(err)
	}
	api.SetGitChanges(changes)
	api.SetCodeApplicability(svc)
	api.SetEffectiveMemory(svc)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var ws struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "GitChanges"}, &ws); code != 200 {
		t.Fatal(code)
	}
	remote := "http://cxthub.test/" + me.Username + "/" + ws.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote, "git_remote_url": "https://github.com/example/changes", "default_branch": "main"}, nil); code != 200 {
		t.Fatal(code)
	}
	endpoint := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/git-changes"
	request := domain.GitChangeRequest{Target: strings.Repeat("a", 40), Commit: strings.Repeat("b", 40)}
	for _, method := range []string{"GET", "POST"} {
		if code := doJSONAs(t, "dev:outsider@example.test:Other", method, endpoint, request, nil); code != 403 {
			t.Fatalf("outsider %s=%d", method, code)
		}
	}
	var j domain.GitChangeJob
	if code := doJSON(t, "POST", endpoint, request, &j); code != 200 || j.State != "waiting" {
		t.Fatalf("submit=%d %+v", code, j)
	}
	var got domain.GitChangeJob
	if code := doJSON(t, "GET", endpoint+"/"+j.ID, nil, &got); code != 200 || got.ID != j.ID || got.Request != request || got.Result != nil {
		t.Fatalf("readback=%d %+v", code, got)
	}
	page := domain.GitChangePage{}
	if code := doJSON(t, "GET", endpoint+"?limit=1", nil, &page); code != 200 || len(page.Items) != 1 {
		t.Fatalf("page=%d %+v", code, page)
	}
	if code := doJSON(t, "GET", endpoint+"?limit=1000", nil, nil); code != 422 {
		t.Fatalf("unbounded list=%d", code)
	}
	req, _ := http.NewRequest("POST", endpoint+"/"+j.ID+"/retry", nil)
	req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatal("missing content type accepted", res.StatusCode)
	}
	if code := doJSON(t, "PATCH", ts.URL+"/api/v1/workspaces/"+ws.ID, map[string]any{"visibility": "public", "public_role": "viewer"}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := doJSONAs(t, "", "GET", endpoint, nil, nil); code != 200 {
		t.Fatal("public read", code)
	}
	if code := doJSONAs(t, "", "POST", endpoint, request, nil); code != 401 && code != 403 {
		t.Fatal("public write", code)
	}

	codeURL := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/code-applicability?code_commit=" + request.Commit + "&source_commit=" + request.Target + "&path=feature.go"
	var codeState domain.CodeApplicability
	if status := doJSONAs(t, "", "GET", codeURL, nil, &codeState); status != 200 || codeState.Reason != "selected_tree_pending" || codeState.Paths[0].State != "unknown" {
		t.Fatalf("code read %d %+v", status, codeState)
	}
	if status := doJSON(t, "GET", strings.Replace(codeURL, request.Commit, "HEAD", 1), nil, nil); status != 422 {
		t.Fatal("mutable code selection accepted", status)
	}
	// Effective memory uses the same viewer guard and application service.
	ctx := context.Background()
	snapshotID := domain.HashContent([]byte("synthetic effective memory"))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: snapshotID, RepoID: repo, DocHash: snapshotID}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PutMemoryDigestCAS(ctx, repo, domain.MemoryDigest{SnapshotID: snapshotID, ClaimsVersion: 1, Fragments: []domain.MemoryFragment{{SourceSnapshot: snapshotID, Claims: []domain.MemoryClaim{{Kind: "rationale", Text: "Keep the reason"}}}}}); err != nil {
		t.Fatal(err)
	}
	effectiveURL := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/effective-memory?snapshot_id=" + url.QueryEscape(string(snapshotID)) + "&code_commit=" + request.Commit
	var effective domain.EffectiveMemoryPage
	if status := doJSONAs(t, "", "GET", effectiveURL, nil, &effective); status != 200 || len(effective.Items) != 1 || effective.Items[0].State != "retained" {
		t.Fatalf("effective read %d %+v", status, effective)
	}
	for _, suffix := range []string{"&limit=0", "&limit=bad", "&limit=51", "&cursor=bad"} {
		if status := doJSON(t, "GET", effectiveURL+suffix, nil, nil); status != 422 {
			t.Fatal("invalid effective input", status)
		}
	}
	// Return to a private repository: the read must obey the same viewer guard.
	if status := doJSON(t, "PATCH", ts.URL+"/api/v1/workspaces/"+ws.ID, map[string]any{"visibility": "private"}, nil); status != 200 {
		t.Fatal(status)
	}
	if status := doJSONAs(t, "dev:outsider@example.test:Other", "GET", codeURL, nil, nil); status != 403 {
		t.Fatal("private code evidence leaked", status)
	}
	if status := doJSONAs(t, "dev:outsider@example.test:Other", "GET", effectiveURL, nil, nil); status != 403 {
		t.Fatal("effective memory leaked", status)
	}
	history, err := svc.ListHistory(context.Background(), repo)
	if err != nil || len(history) != 0 {
		t.Fatal("verification acceptance changed history", history, err)
	}
}
