package http

import (
	"bufio"
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRevisionHubSharesReadAndReleasesObservers(t *testing.T) {
	var calls atomic.Int32
	h := repositoryChangeHub{}
	repo := domain.HashContent([]byte(t.Name()))
	read := func(context.Context, domain.ContentHash) (domain.RepositoryRevision, error) {
		calls.Add(1)
		return domain.RepositoryRevision{Graph: 3, Pending: 4}, nil
	}
	a, releaseA := h.subscribe(repo, read)
	b, releaseB := h.subscribe(repo, read)
	for _, ch := range []<-chan revisionNotice{a, b} {
		select {
		case n := <-ch:
			if n.revision.Graph != 3 || n.err != nil {
				t.Fatal(n)
			}
		case <-time.After(time.Second):
			t.Fatal("no initial durable cursor")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("read per browser: %d", calls.Load())
	}
	releaseA()
	releaseB()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.repos) != 0 {
		t.Fatal("observer leaked")
	}
}

func TestRepositoryChangesAuthorizationAndPendingDelivery(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	ids := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, ids).Handler())
	defer ts.Close()
	var me struct{ Username string }
	if doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me) != 200 {
		t.Fatal("login")
	}
	var ws struct{ Slug string }
	if doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Revision"}, &ws) != 200 {
		t.Fatal("workspace")
	}
	remote := "http://cxthub.test/" + me.Username + "/" + ws.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil) != 200 {
		t.Fatal("repo")
	}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo))
	for _, path := range []string{"/revision", "/pending-view", "/changes"} {
		if code := doJSONAs(t, "dev:outsider@example.test:Other", "GET", base+path, nil, nil); code != 403 {
			t.Fatalf("%s exposed: %d", path, code)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/changes", nil)
	req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal(resp.Status)
	}
	reader := bufio.NewScanner(resp.Body)
	next := func() domain.RepositoryRevision {
		t.Helper()
		for reader.Scan() {
			line := reader.Text()
			if strings.HasPrefix(line, "data: ") {
				var v domain.RepositoryRevision
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &v); err != nil {
					t.Fatal(err)
				}
				return v
			}
		}
		t.Fatalf("stream ended: %v", reader.Err())
		return domain.RepositoryRevision{}
	}
	initial := next()
	target := domain.HashContent([]byte(t.Name()))
	if err := st.PutSnapshot(ctx, domain.Snapshot{ID: target, DocHash: target, RepoID: repo, Provider: domain.ProviderCodex, SessionID: "synthetic"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.PutPending(ctx, repo, "synthetic", domain.Pending{Target: target, Provider: domain.ProviderCodex}); err != nil {
		t.Fatal(err)
	}
	changed := next()
	if changed.Graph != initial.Graph || changed.Pending != initial.Pending+1 {
		t.Fatalf("wrong notification %+v -> %+v", initial, changed)
	}
	var v domain.PendingView
	if doJSON(t, "GET", base+"/pending-view", nil, &v) != 200 || len(v.Snapshots) != 1 || v.Revision != changed {
		t.Fatalf("%+v", v)
	}
}
