package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type remoteGitHTTPBackend struct {
	Backend
	repo      domain.Repo
	calls     int
	actor     string
	before    domain.RemoteCommitCursor
	limit     int
	page      domain.RemoteGitPage
	err       error
	refreshes int
}

func (backend *remoteGitHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *remoteGitHTTPBackend) ReadRemoteGit(ctx context.Context, _ domain.ContentHash, before domain.RemoteCommitCursor, limit int) (domain.RemoteGitPage, error) {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.before, backend.limit = before, limit
	if limit < 1 || limit > domain.RemoteGitPageLimit {
		return domain.RemoteGitPage{}, domain.ErrValidation
	}
	return backend.page, backend.err
}

func (backend *remoteGitHTTPBackend) RequestRemoteGitRefresh(ctx context.Context, _ domain.ContentHash) error {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	if backend.err == nil {
		backend.refreshes++
	}
	return backend.err
}

func remoteGitHTTPFixture(t *testing.T, role domain.MemberRole) (*remoteGitHTTPBackend, http.Handler, string) {
	t.Helper()
	backend := &remoteGitHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("e", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	return backend, NewServer(backend, identity).Handler(), "/api/v1/repos/" + string(backend.repo.ID)
}

func remoteGitHTTPKeys(t *testing.T, raw []byte) []string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("answer is not an object: %s %v", raw, err)
	}
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestRemoteGitReadHTTP(t *testing.T) {
	a, b := strings.Repeat("a", 40), strings.Repeat("b", 40)
	cursor := domain.RemoteCommitCursor{CommittedAt: time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC), SHA: a}
	for _, test := range []struct {
		name         string
		query        string
		role         domain.MemberRole
		authed       bool
		status       int
		reached      bool
		wantLimit    int
		wantBefore   domain.RemoteCommitCursor
		serviceError error
	}{
		{"anonymous", "", domain.RoleOwner, false, 401, false, 0, domain.RemoteCommitCursor{}, nil},
		{"viewer", "", domain.RoleViewer, true, 200, true, 200, domain.RemoteCommitCursor{}, nil},
		{"puller", "", domain.RolePuller, true, 200, true, 200, domain.RemoteCommitCursor{}, nil},
		{"member", "", domain.RoleMember, true, 200, true, 200, domain.RemoteCommitCursor{}, nil},
		{"owner", "", domain.RoleOwner, true, 200, true, 200, domain.RemoteCommitCursor{}, nil},
		{"limit", "?limit=7", domain.RoleMember, true, 200, true, 7, domain.RemoteCommitCursor{}, nil},
		{"largest_limit", "?limit=500", domain.RoleMember, true, 200, true, 500, domain.RemoteCommitCursor{}, nil},
		{"limit_over_the_largest", "?limit=501", domain.RoleMember, true, 422, true, 501, domain.RemoteCommitCursor{}, nil},
		{"limit_zero", "?limit=0", domain.RoleMember, true, 422, true, 0, domain.RemoteCommitCursor{}, nil},
		{"limit_not_a_number", "?limit=many", domain.RoleMember, true, 422, false, 0, domain.RemoteCommitCursor{}, nil},
		{"before", "?before=" + url.QueryEscape(cursor.String()) + "&limit=3", domain.RoleMember, true, 200, true, 3, cursor, nil},
		{"before_not_a_cursor", "?before=yesterday", domain.RoleMember, true, 422, false, 0, domain.RemoteCommitCursor{}, nil},
		{"before_a_commit_number", "?before=" + a, domain.RoleMember, true, 422, false, 0, domain.RemoteCommitCursor{}, nil},
		{"storage_unavailable", "", domain.RoleMember, true, 409, true, 200, domain.RemoteCommitCursor{}, domain.ErrConflict},
		{"repository_gone", "", domain.RoleMember, true, 404, true, 200, domain.RemoteCommitCursor{}, domain.ErrNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := remoteGitHTTPFixture(t, test.role)
			backend.err = test.serviceError
			backend.page = domain.RemoteGitPage{Branches: []domain.RemoteBranch{}, Pulls: []domain.RemotePull{}, Commits: []domain.RemoteCommit{}}
			response := bpHTTP(t, handler, http.MethodGet, base+"/remote-git"+test.query, "", test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if !test.reached {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" || backend.limit != test.wantLimit || backend.before != test.wantBefore {
				t.Fatalf("what reached the service: %+v", backend)
			}
		})
	}

	// A repository never read answers with empty lists, never with null.
	backend, handler, base := remoteGitHTTPFixture(t, domain.RoleMember)
	response := bpHTTP(t, handler, http.MethodGet, base+"/remote-git", "", true)
	if response.Code != 200 || strings.TrimSpace(response.Body.String()) != `{"branches":[],"pulls":[],"commits":[]}` {
		t.Fatalf("a repository never read: %d %s", response.Code, response.Body.String())
	}

	// The first page carries everything; the author's email is not in it.
	synced := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	commit := domain.RemoteCommit{SHA: a, Parents: []string{b}, AuthorName: "Author", AuthorEmail: "author@example.com",
		AuthoredAt: synced.Add(-time.Hour), CommittedAt: synced.Add(-time.Minute), Subject: "A subject"}
	root := domain.RemoteCommit{SHA: b, AuthorName: "Author", AuthorEmail: "author@example.com", AuthoredAt: synced.Add(-2 * time.Hour), CommittedAt: synced.Add(-2 * time.Hour), Subject: "Start"}
	next := domain.RemoteCommitCursorAt(root)
	backend.page = domain.RemoteGitPage{DefaultBranch: "main", SyncedAt: &synced,
		Branches: []domain.RemoteBranch{{Name: "main", SHA: a}},
		Pulls: []domain.RemotePull{
			{Number: 2, Title: "Second", State: domain.RemotePullMerged, HeadRef: "topic", HeadSHA: a, BaseRef: "main", MergeSHA: b, Fork: true, Commits: []string{a}, Truncated: false, UpdatedAt: synced},
			{Number: 1, Title: "First", State: domain.RemotePullOpen, HeadRef: "other", HeadSHA: b, BaseRef: "main", UpdatedAt: synced},
		},
		Commits: []domain.RemoteCommit{commit, root}, Next: &next}
	response = bpHTTP(t, handler, http.MethodGet, base+"/remote-git?limit=2", "", true)
	if response.Code != 200 {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	if strings.Contains(body, "email") || strings.Contains(body, "author@example.com") || strings.Contains(body, "null") {
		t.Fatalf("the answer carries an email or a null list: %s", body)
	}
	if keys := remoteGitHTTPKeys(t, response.Body.Bytes()); !reflect.DeepEqual(keys, []string{"branches", "commits", "default_branch", "next", "pulls", "synced_at"}) {
		t.Fatalf("fields of the first page: %v", keys)
	}
	var first struct {
		DefaultBranch string                `json:"default_branch"`
		SyncedAt      time.Time             `json:"synced_at"`
		Branches      []domain.RemoteBranch `json:"branches"`
		Pulls         []json.RawMessage     `json:"pulls"`
		Commits       []json.RawMessage     `json:"commits"`
		Next          string                `json:"next"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &first); err != nil {
		t.Fatal(err)
	}
	if first.DefaultBranch != "main" || !first.SyncedAt.Equal(synced) || !reflect.DeepEqual(first.Branches, backend.page.Branches) || first.Next != next.String() || len(first.Pulls) != 2 || len(first.Commits) != 2 {
		t.Fatalf("the first page: %+v", first)
	}
	if got := string(first.Commits[0]); got != `{"sha":"`+a+`","parents":["`+b+`"],"author_name":"Author","authored_at":"2031-01-02T02:00:00Z","committed_at":"2031-01-02T02:59:00Z","subject":"A subject"}` {
		t.Fatalf("a commit: %s", got)
	}
	if got := string(first.Commits[1]); !strings.Contains(got, `"parents":[]`) {
		t.Fatalf("a commit without parents: %s", got)
	}
	if got := string(first.Pulls[0]); got != `{"number":2,"title":"Second","state":"merged","head_ref":"topic","head_sha":"`+a+`","base_ref":"main","merge_sha":"`+b+`","fork":true,"commits":["`+a+`"],"truncated":false,"updated_at":"2031-01-02T03:00:00Z"}` {
		t.Fatalf("a merged pull request: %s", got)
	}
	if got := string(first.Pulls[1]); got != `{"number":1,"title":"First","state":"open","head_ref":"other","head_sha":"`+b+`","base_ref":"main","fork":false,"commits":[],"truncated":false,"updated_at":"2031-01-02T03:00:00Z"}` {
		t.Fatalf("an open pull request without listed commits: %s", got)
	}
	// The next value is opaque and comes back as the cursor of the next page.
	if strings.Contains(first.Next, b) {
		t.Fatalf("the next value shows what it holds: %s", first.Next)
	}
	// A following page carries commits and where the page after it starts,
	// and nothing else.
	backend.page = domain.RemoteGitPage{Commits: []domain.RemoteCommit{root}}
	response = bpHTTP(t, handler, http.MethodGet, base+"/remote-git?before="+url.QueryEscape(first.Next), "", true)
	if response.Code != 200 || backend.before != next {
		t.Fatalf("the following page: %d %+v %s", response.Code, backend.before, response.Body.String())
	}
	if keys := remoteGitHTTPKeys(t, response.Body.Bytes()); !reflect.DeepEqual(keys, []string{"commits"}) || strings.Contains(response.Body.String(), "email") {
		t.Fatalf("fields of the last page: %v %s", keys, response.Body.String())
	}
	backend.page = domain.RemoteGitPage{Commits: []domain.RemoteCommit{commit}, Next: &next, DefaultBranch: "main", SyncedAt: &synced,
		Branches: []domain.RemoteBranch{{Name: "main", SHA: a}}}
	response = bpHTTP(t, handler, http.MethodGet, base+"/remote-git?before="+url.QueryEscape(first.Next), "", true)
	if keys := remoteGitHTTPKeys(t, response.Body.Bytes()); response.Code != 200 || !reflect.DeepEqual(keys, []string{"commits", "next"}) {
		t.Fatalf("fields of a following page: %d %v", response.Code, keys)
	}
}

func TestRemoteGitRefreshHTTP(t *testing.T) {
	for _, test := range []struct {
		name         string
		role         domain.MemberRole
		authed       bool
		body         string
		status       int
		reached      bool
		serviceError error
	}{
		{"anonymous", domain.RoleOwner, false, `{}`, 401, false, nil},
		{"viewer", domain.RoleViewer, true, `{}`, 403, false, nil},
		{"puller", domain.RolePuller, true, `{}`, 403, false, nil},
		{"member", domain.RoleMember, true, `{}`, 202, true, nil},
		{"owner", domain.RoleOwner, true, `{}`, 202, true, nil},
		{"a_body_nobody_reads", domain.RoleMember, true, `{"origin":"https://example.invalid/other"}`, 202, true, nil},
		{"no_json", domain.RoleMember, true, ``, 415, false, nil},
		{"no_origin", domain.RoleMember, true, `{}`, 409, true, domain.ErrConflict},
		{"removed_member", domain.RoleMember, true, `{}`, 403, true, domain.ErrForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := remoteGitHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, http.MethodPost, base+"/remote-git/refresh", test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if !test.reached {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status != 202 {
				if backend.refreshes != 0 {
					t.Fatal("a refused request was recorded")
				}
				return
			}
			if got := strings.TrimSpace(response.Body.String()); got != `{"requested":true}` || backend.refreshes != 1 ||
				!strings.HasPrefix(response.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("answer: %s", got)
			}
		})
	}
}

func TestRemoteGitHTTPWithoutService(t *testing.T) {
	backend := &sessionArchiveHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("f", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
	handler := NewServer(backend, identity).Handler()
	for method, path := range map[string]string{http.MethodGet: "/remote-git", http.MethodPost: "/remote-git/refresh"} {
		body := ""
		if method == http.MethodPost {
			body = `{}`
		}
		if response := bpHTTP(t, handler, method, "/api/v1/repos/"+string(backend.repo.ID)+path, body, true); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status=%d: %s", method, path, response.Code, response.Body.String())
		}
	}
}
