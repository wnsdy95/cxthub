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
	history, err := svc.ListHistory(context.Background(), repo)
	if err != nil || len(history) != 0 {
		t.Fatal("verification acceptance changed history", history, err)
	}
}
