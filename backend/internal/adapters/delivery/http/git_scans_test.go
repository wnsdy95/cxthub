package http

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

func TestGitScanSignedDeliveryAuthorizationAndReadback(t *testing.T) {
	t.Setenv("CXT_GITHUB_WEBHOOK_SECRET", "synthetic-secret")
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	identity := app.NewIdentityService(auth.NewDevVerifier(), st)
	api := NewServer(svc, identity)
	scans, err := app.NewGitScans(svc, gitevidence.NewGitHub(nil))
	if err != nil {
		t.Fatal(err)
	}
	api.SetGitScans(scans)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	var me struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me)
	var repositoryRecord struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "GitScans"}, &repositoryRecord); code != 200 {
		t.Fatal(code)
	}
	remote := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	repo := repoIDForRemoteURLForTest(remote)
	origin := "https://github.com/example/scans"
	if code := doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote, "git_remote_url": origin, "default_branch": "main"}, nil); code != 200 {
		t.Fatal(code)
	}
	endpoint := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/git-scans"
	before, after := strings.Repeat("a", 40), strings.Repeat("b", 40)
	push := func(delivery, b, a string, valid bool) int {
		payload, _ := json.Marshal(map[string]any{"ref": "refs/heads/main", "before": b, "after": a, "forced": true, "repository": map[string]string{"clone_url": origin}, "commits": []any{}})
		req, _ := http.NewRequest("POST", ts.URL+"/api/v1/hooks/github", bytes.NewReader(payload))
		req.Header.Set("X-GitHub-Event", "push")
		req.Header.Set("X-GitHub-Delivery", delivery)
		mac := hmac.New(sha256.New, []byte("synthetic-secret"))
		mac.Write(payload)
		sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !valid {
			sig = "invalid"
		}
		req.Header.Set("X-Hub-Signature-256", sig)
		res, e := ts.Client().Do(req)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		return res.StatusCode
	}
	if code := push("delivery-1", before, after, false); code != 401 {
		t.Fatal("unsigned delivery", code)
	}
	for i := 0; i < 2; i++ {
		if code := push("delivery-1", before, after, true); code != 200 {
			t.Fatal("signed retry", code)
		}
	}
	if code := push("delivery-1", after, before, true); code != 409 {
		t.Fatal("delivery identity overwritten", code)
	}
	var jobs domain.GitScanPage
	if code := doJSON(t, "GET", endpoint, nil, &jobs); code != 200 || len(jobs.Items) != 2 {
		t.Fatal("both immutable tips not queued", code, jobs)
	}
	if code := doJSONAs(t, "dev:outsider@example.test:Other", "GET", endpoint, nil, nil); code != 403 {
		t.Fatal("cross repository disclosure", code)
	}
	if code := doJSON(t, "GET", endpoint+"?limit=1001", nil, nil); code != 422 {
		t.Fatal("unbounded query", code)
	}
	req, _ := http.NewRequest("POST", endpoint+"/"+jobs.Items[0].ID+"/retry", nil)
	req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatal("CSRF", res.StatusCode)
	}
	if code := doJSON(t, "PATCH", ts.URL+"/api/v1/repositories/"+repositoryRecord.ID, map[string]any{"visibility": "public", "public_role": "viewer"}, nil); code != 200 {
		t.Fatal(code)
	}
	if code := doJSONAs(t, "", "GET", endpoint, nil, nil); code != 200 {
		t.Fatal("public reader", code)
	}
	if code := doJSONAs(t, "", "POST", endpoint+"/"+jobs.Items[0].ID+"/retry", map[string]any{}, nil); code != 401 && code != 403 {
		t.Fatal("viewer wrote", code)
	}
	// Delete and then recreate can repeat the same tips with distinct deliveries.
	if code := push("delivery-delete", before, strings.Repeat("0", 40), true); code != 200 {
		t.Fatal("deleted ref", code)
	}
	refs, _ := svc.ListRefs(context.Background(), repo)
	events, _ := svc.ListHistory(context.Background(), repo)
	if len(refs) != 0 || len(events) != 0 {
		t.Fatal("push moved context state")
	}
}
