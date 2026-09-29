package http

import (
	"encoding/json"
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
)

func TestEmptyBootstrapFullCatalogWireAndAuthorization(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	ids := app.NewIdentityService(auth.NewDevVerifier(), st)
	ts := httptest.NewServer(NewServer(svc, ids).Handler())
	defer ts.Close()
	var me struct{ Username string }
	if doJSON(t, "GET", ts.URL+"/api/v1/me", nil, &me) != 200 {
		t.Fatal("login")
	}
	var record struct{ Slug string }
	if doJSON(t, "POST", ts.URL+"/api/v1/repositories", map[string]any{"name": "EmptyBootstrap"}, &record) != 200 {
		t.Fatal("repository")
	}
	remote := "http://cxthub.test/" + me.Username + "/" + record.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if doJSON(t, "POST", ts.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote}, nil) != 200 {
		t.Fatal("repo")
	}
	endpoint := ts.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/context-query?scope=all"
	if status := doJSONAs(t, "dev:outsider@example.test:Other", "GET", endpoint, nil, nil); status != 403 {
		t.Fatalf("unauthorized empty query: %d", status)
	}
	var wire map[string]json.RawMessage
	if status := doJSON(t, "GET", endpoint, nil, &wire); status != 200 {
		t.Fatalf("empty query: %d", status)
	}
	if string(wire["snapshots"]) != "[]" || string(wire["history"]) != "[]" {
		t.Fatalf("empty arrays missing/null: %s %s", wire["snapshots"], wire["history"])
	}
	var state domain.ContentHash
	if err := json.Unmarshal(wire["state_hash"], &state); err != nil || domain.ValidateContentHash(state) != nil {
		t.Fatalf("invalid state hash: %s %v", state, err)
	}
	var rev map[string]string
	if err := json.Unmarshal(wire["revision"], &rev); err != nil || rev["graph"] == "" || rev["pending"] == "" {
		t.Fatalf("missing revision: %s %v", wire["revision"], err)
	}
	// An unreferenced capture must prevent a false empty proof.
	id := domain.HashContent([]byte("unreachable source"))
	if err := st.PutSnapshot(systemTestContext(), domain.Snapshot{ID: id, DocHash: id, RepoID: repo}); err != nil {
		t.Fatal(err)
	}
	var got domain.ContextQueryView
	if doJSON(t, "GET", endpoint, nil, &got) != 200 || len(got.Snapshots) != 1 || got.Snapshots[0].ID != id {
		t.Fatalf("unreachable source omitted: %+v", got)
	}
	// An empty-root history observation is still history and must be exposed.
	e := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: "position", Kind: "position", CreatedAt: time.Now().UTC()}
	if err := st.ApplyHistoryEvent(systemTestContext(), e); err != nil {
		t.Fatal(err)
	}
	if doJSON(t, "GET", endpoint, nil, &got) != 200 || len(got.History) != 1 || got.History[0].ID != e.ID {
		t.Fatalf("history omitted from full catalog: %+v", got)
	}
}
