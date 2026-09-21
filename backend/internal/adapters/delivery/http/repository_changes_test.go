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
	var repositoryRecord struct{ Slug string }
	if doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "Revision"}, &repositoryRecord) != 200 {
		t.Fatal("repository")
	}
	remote := "http://cxthub.test/" + me.Username + "/" + repositoryRecord.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil) != 200 {
		t.Fatal("repo")
	}
	base := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo))
	for _, path := range []string{"/revision", "/pending-view", "/changes", "/context-query", "/graph-state", "/view"} {
		if code := doJSONAs(t, "dev:outsider@example.test:Other", "GET", base+path, nil, nil); code != 403 {
			t.Fatalf("%s exposed: %d", path, code)
		}
	}
	var graph struct {
		Version int `json:"version"`
	}
	if code := doJSON(t, "GET", base+"/graph-state", nil, &graph); code != 200 || graph.Version != 1 {
		t.Fatalf("graph query: %d %+v", code, graph)
	}
	if code := doJSON(t, "GET", base+"/graph-state?position=missing", nil, nil); code != 404 {
		t.Fatalf("unknown graph position: %d", code)
	}
	var query domain.ContextQueryView
	if code := doJSON(t, "GET", base+"/context-query", nil, &query); code != 200 || query.Version != 1 || query.Semantics.Version != 1 {
		t.Fatalf("shared context query: %d %+v", code, query)
	}
	if code := doJSON(t, "GET", base+"/context-query?scope=previous", nil, nil); code != 422 {
		t.Fatalf("implicit past selection accepted: %d", code)
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
	var v struct {
		Graph     json.RawMessage           `json:"graph"`
		Revision  domain.RepositoryRevision `json:"revision"`
		Pending   []domain.Pending          `json:"pending"`
		Snapshots []domain.Snapshot         `json:"snapshots"`
	}
	if doJSON(t, "GET", base+"/pending-view", nil, &v) != 200 || len(v.Snapshots) != 1 || v.Revision != changed {
		t.Fatalf("%+v", v)
	}
	var full struct {
		Graph json.RawMessage `json:"graph"`
	}
	if doJSON(t, "GET", base+"/view", nil, &full) != 200 || string(full.Graph) != string(v.Graph) {
		t.Fatal("full/pending graph wire contracts diverged")
	}
	var selected json.RawMessage
	if doJSON(t, "GET", base+"/graph-state", nil, &selected) != 200 || string(selected) != string(v.Graph) {
		t.Fatal("positionless graph query disagrees with view")
	}
	var indexed struct {
		Encoding    string               `json:"encoding"`
		Dictionary  []domain.ContentHash `json:"dictionary"`
		SnapshotIDs []uint32             `json:"snapshot_ids"`
	}
	if err := json.Unmarshal(v.Graph, &indexed); err != nil {
		t.Fatal(err)
	}
	if indexed.Encoding != "indexed-v1" || len(indexed.SnapshotIDs) != 1 || indexed.Dictionary[indexed.SnapshotIDs[0]] != target {
		t.Fatalf("unexpected wire dictionary: %+v", indexed)
	}
}
