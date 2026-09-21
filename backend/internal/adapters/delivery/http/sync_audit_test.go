package http

import (
	"context"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"net/http/httptest"
	"testing"
)

type auditStub struct{ calls int }

func (a *auditStub) CheckGitHubSync(context.Context, domain.ContentHash, string) (domain.SyncAuditPage, error) {
	a.calls++
	return domain.SyncAuditPage{Version: 1, Checks: []domain.SyncAuditCheck{}}, nil
}
func TestSyncAuditRequiresMaintainerAndJSON(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	svc := app.NewService(st, st, auth.NewTeamTokenAuth(), gitengine.NewEngine(st), st)
	api := NewServer(svc, app.NewIdentityService(auth.NewDevVerifier(), st))
	a := &auditStub{}
	api.SetGitSyncAudit(a)
	ts := httptest.NewServer(api.Handler())
	defer ts.Close()
	var ws domain.Workspace
	if code := doJSON(t, "POST", ts.URL+"/api/v1/workspaces", map[string]any{"name": "Audit", "slug": "audit"}, &ws); code != 201 && code != 200 {
		t.Fatal(code)
	}
	repo := domain.HashContent([]byte("audit"))
	if _, err := st.PutRepo(context.Background(), domain.Repo{ID: repo, WorkspaceID: ws.ID}); err != nil {
		t.Fatal(err)
	}
	endpoint := ts.URL + "/api/v1/repos/" + string(repo) + "/github-sync-check"
	if code := doJSONAs(t, "dev:outsider@example.test:Other", "POST", endpoint, map[string]any{}, nil); code != 403 {
		t.Fatal(code)
	}
	if a.calls != 0 {
		t.Fatal("unauthorized audit executed")
	}
	if code := doJSON(t, "POST", endpoint, map[string]any{}, nil); code != 200 {
		t.Fatal(code)
	}
	req, _ := http.NewRequest("POST", endpoint, nil)
	req.Header.Set("Authorization", "Bearer dev:test@t.io:Test")
	res, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 415 {
		t.Fatal(res.StatusCode)
	}
}
