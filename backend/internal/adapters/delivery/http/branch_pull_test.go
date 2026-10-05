package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type branchPullHTTPBackend struct {
	Backend
	repo  domain.Repo
	calls int
	got   domain.BranchPullRequest
	err   error
}

func (b *branchPullHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return b.repo, nil
}
func (b *branchPullHTTPBackend) BranchPullVersion() int { return 1 }
func (b *branchPullHTTPBackend) PullBranchPlan(_ context.Context, repo domain.ContentHash, r domain.BranchPullRequest) (domain.BranchPullPlan, error) {
	b.calls++
	b.got = r
	if err := r.Validate(); err != nil {
		return domain.BranchPullPlan{}, err
	}
	if b.err != nil {
		return domain.BranchPullPlan{}, b.err
	}
	return domain.BranchPullPlan{Version: 1, RepoID: repo, Branch: r.Branch, SnapshotIndex: []domain.ContentHash{}, SnapshotStates: map[domain.ContentHash]domain.ContentHash{}, Refs: []domain.Ref{}, History: []domain.HistoryEvent{}, SettingsObjects: []domain.BranchPullSettings{}, AbsentRoots: []domain.ContentHash{}}, nil
}

type branchPullHTTPIdentity struct {
	IdentityBackend
	repo       domain.Repository
	role       domain.MemberRole
	breakGlass bool
}

func (i branchPullHTTPIdentity) GetRepository(context.Context, string) (domain.Repository, error) {
	return i.repo, nil
}
func (i branchPullHTTPIdentity) ResolveUser(context.Context, string) (domain.User, error) {
	return domain.User{ID: "actor"}, nil
}
func (i branchPullHTTPIdentity) RoleOf(context.Context, string, string) (domain.MemberRole, bool) {
	return i.role, i.role != ""
}
func (i branchPullHTTPIdentity) HasBreakGlassAccess(context.Context, string, string) (bool, error) {
	return i.breakGlass, nil
}
func bpHTTP(t *testing.T, h http.Handler, method, path, body string, authed bool) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	if authed {
		r.Header.Set("Authorization", "Bearer fixture-token")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestBranchPullHTTPAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name                                         string
		role                                         domain.MemberRole
		public, puller, archived, breakGlass, authed bool
		status                                       int
	}{
		{name: "private_viewer", role: domain.RoleViewer, authed: true, status: 403},
		{name: "private_puller", role: domain.RolePuller, authed: true, status: 200},
		{name: "member", role: domain.RoleMember, authed: true, status: 200},
		{name: "public_viewer", public: true, status: 401},
		{name: "public_puller", public: true, puller: true, status: 200},
		{name: "archived", role: domain.RoleOwner, authed: true, archived: true, status: 403},
		{name: "break_glass", authed: true, breakGlass: true, status: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &branchPullHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("http plan")), RepositoryID: "ws_" + strings.Repeat("e", 32)}}
			id := branchPullHTTPIdentity{repo: domain.Repository{ID: "ws_" + strings.Repeat("e", 32), Archived: tc.archived}, role: tc.role, breakGlass: tc.breakGlass}
			if tc.public {
				id.repo.Visibility = domain.VisibilityPublic
			}
			if tc.puller {
				id.repo.PublicRole = "puller"
			}
			w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodPost, "/api/v1/repos/"+string(b.repo.ID)+"/pull/branch-plan", `{"version":1,"branch":"main"}`, tc.authed)
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
			wantCalls := 0
			if tc.status == 200 {
				wantCalls = 1
			}
			if b.calls != wantCalls {
				t.Fatalf("planner called before authorization: %d", b.calls)
			}
		})
	}
}
func TestBranchPullHTTPCapabilityAndFailures(t *testing.T) {
	b := &branchPullHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("http plan")), RepositoryID: "ws_" + strings.Repeat("e", 32), ContextProtocol: 0}}
	id := branchPullHTTPIdentity{repo: domain.Repository{ID: "ws_" + strings.Repeat("e", 32)}, role: domain.RolePuller}
	h := NewServer(b, id).Handler()
	path := "/api/v1/repos/" + string(b.repo.ID)
	w := bpHTTP(t, h, http.MethodGet, path, "", true)
	var wire map[string]any
	if json.Unmarshal(w.Body.Bytes(), &wire) != nil || wire["branch_pull_version"] != float64(1) {
		t.Fatalf("capability: %s", w.Body.String())
	}
	if _, ok := wire["context_protocol"]; ok && wire["context_protocol"] != float64(0) {
		t.Fatal("capability overloaded context protocol")
	}
	raw, err := json.Marshal(b.repo)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "branch_pull_version") {
		t.Fatal("runtime capability persisted in repo type")
	}
	root := domain.HashContent([]byte("optional"))
	body := `{"version":1,"branch":"main","observation_roots":["` + string(root) + `"]}`
	w = bpHTTP(t, h, http.MethodPost, path+"/pull/branch-plan", body, true)
	if w.Code != 200 || len(b.got.ObservationRoots) != 1 || b.got.ObservationRoots[0] != root {
		t.Fatalf("request lost: %d %+v", w.Code, b.got)
	}
	for _, tc := range []struct {
		body   string
		err    error
		status int
	}{{`{"version":2,"branch":"main"}`, nil, 400}, {`{`, nil, 400}, {strings.Repeat(" ", 64<<10) + body, nil, 413}, {body, domain.ErrNotFound, 404}, {body, domain.ErrIntegrity, 422}, {body, domain.ErrRefConflict, 409}} {
		b.err = tc.err
		w = bpHTTP(t, h, http.MethodPost, path+"/pull/branch-plan", tc.body, true)
		if w.Code != tc.status {
			t.Fatalf("status=%d want=%d", w.Code, tc.status)
		}
	}
}
func TestBranchPullHTTPFSExplicitlyUnsupported(t *testing.T) {
	st := store.NewFSStore(t.TempDir())
	repo := domain.Repo{ID: domain.HashContent([]byte("FS unsupported")), RepositoryID: "ws_" + strings.Repeat("e", 32)}
	if _, err := st.PutRepo(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(st, st, nil, nil, nil)
	id := branchPullHTTPIdentity{repo: domain.Repository{ID: "ws_" + strings.Repeat("e", 32)}, role: domain.RolePuller}
	h := NewServer(svc, id).Handler()
	path := "/api/v1/repos/" + string(repo.ID)
	w := bpHTTP(t, h, http.MethodGet, path, "", true)
	var wire repoPullView
	if json.Unmarshal(w.Body.Bytes(), &wire) != nil || w.Code != 200 || wire.BranchPullVersion != 0 {
		t.Fatalf("FS capability: %d %+v", w.Code, wire)
	}
	w = bpHTTP(t, h, http.MethodPost, path+"/pull/branch-plan", `{"version":1,"branch":"main"}`, true)
	if w.Code != 501 || !strings.Contains(w.Body.String(), `"code":"branch_pull_unsupported"`) {
		t.Fatalf("unsupported response: %d %s", w.Code, w.Body.String())
	}
}
