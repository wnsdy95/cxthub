package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type anchoredNoteHTTPBackend struct {
	Backend
	repo     domain.Repo
	calls    int
	actor    string
	target   domain.AnchoredNoteTarget
	body     string
	noteID   string
	report   domain.WorktreeReport
	provider domain.ProviderKind
	session  string
	err      error
}

func (backend *anchoredNoteHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *anchoredNoteHTTPBackend) seen(ctx context.Context) {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
}

func (backend *anchoredNoteHTTPBackend) note() domain.AnchoredNote {
	at := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC)
	return domain.AnchoredNote{RepoID: backend.repo.ID, ID: "note_" + strings.Repeat("a", 32), Target: backend.target, Author: backend.actor, Body: backend.body, CreatedAt: at, UpdatedAt: at}
}

func (backend *anchoredNoteHTTPBackend) ListAnchoredNotes(ctx context.Context, _ domain.ContentHash) ([]domain.AnchoredNote, error) {
	backend.seen(ctx)
	return nil, backend.err
}

func (backend *anchoredNoteHTTPBackend) CreateAnchoredNote(ctx context.Context, _ domain.ContentHash, target domain.AnchoredNoteTarget, body string) (domain.AnchoredNote, error) {
	backend.seen(ctx)
	backend.target, backend.body = target, body
	return backend.note(), backend.err
}

func (backend *anchoredNoteHTTPBackend) UpdateAnchoredNote(ctx context.Context, _ domain.ContentHash, id, body string) (domain.AnchoredNote, error) {
	backend.seen(ctx)
	backend.noteID, backend.body = id, body
	return backend.note(), backend.err
}

func (backend *anchoredNoteHTTPBackend) DeleteAnchoredNote(ctx context.Context, _ domain.ContentHash, id string) error {
	backend.seen(ctx)
	backend.noteID = id
	return backend.err
}

func (backend *anchoredNoteHTTPBackend) ReportWorktrees(ctx context.Context, _ domain.ContentHash, report domain.WorktreeReport) ([]domain.ReportedWorktree, error) {
	backend.seen(ctx)
	backend.report = report
	return nil, backend.err
}

func (backend *anchoredNoteHTTPBackend) ListReportedWorktrees(ctx context.Context, _ domain.ContentHash) ([]domain.ReportedWorktree, error) {
	backend.seen(ctx)
	return nil, backend.err
}

func (backend *anchoredNoteHTTPBackend) PutAnchoredSession(context.Context, domain.ContentHash, domain.ProviderKind, string, domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error) {
	return domain.AnchoredSessionReceipt{}, nil
}

func (backend *anchoredNoteHTTPBackend) GetAnchoredSession(context.Context, domain.ContentHash, domain.ProviderKind, string) (domain.AnchoredSession, error) {
	return domain.AnchoredSession{}, nil
}

func (backend *anchoredNoteHTTPBackend) ListAnchoredSessions(context.Context, domain.ContentHash) ([]domain.AnchoredSessionSummary, error) {
	return nil, nil
}

func (backend *anchoredNoteHTTPBackend) AnchoredCommitSegments(context.Context, domain.ContentHash, string) ([]domain.AnchoredCommitSegment, error) {
	return nil, nil
}

func (backend *anchoredNoteHTTPBackend) AnchoredSessionCalls(ctx context.Context, _ domain.ContentHash, provider domain.ProviderKind, sessionID string) ([]domain.AnchoredAgentCall, error) {
	backend.seen(ctx)
	backend.provider, backend.session = provider, sessionID
	return nil, backend.err
}

func anchoredNoteHTTPFixture(t *testing.T, role domain.MemberRole) (*anchoredNoteHTTPBackend, http.Handler, string) {
	t.Helper()
	backend := &anchoredNoteHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("c", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	return backend, NewServer(backend, identity).Handler(), "/api/v1/repos/" + string(backend.repo.ID)
}

func TestAnchoredNoteWriteHTTP(t *testing.T) {
	sha := strings.Repeat("a", 40)
	id := "note_" + strings.Repeat("a", 32)
	create := `{"target":{"kind":"commit","commit":"` + sha + `"},"body":"why"}`
	for _, test := range []struct {
		name, method, path, body string
		role                     domain.MemberRole
		authed                   bool
		status                   int
		serviceError             error
	}{
		{"anonymous", http.MethodPost, "/anchored-notes", create, domain.RoleOwner, false, 401, nil},
		{"viewer", http.MethodPost, "/anchored-notes", create, domain.RoleViewer, true, 403, nil},
		{"puller", http.MethodPost, "/anchored-notes", create, domain.RolePuller, true, 403, nil},
		{"member_creates", http.MethodPost, "/anchored-notes", create, domain.RoleMember, true, 200, nil},
		{"missing_target", http.MethodPost, "/anchored-notes", `{"body":"why"}`, domain.RoleMember, true, 400, nil},
		{"missing_body", http.MethodPost, "/anchored-notes", `{"target":{"kind":"commit","commit":"` + sha + `"}}`, domain.RoleMember, true, 400, nil},
		{"author_field", http.MethodPost, "/anchored-notes", `{"target":{"kind":"commit","commit":"` + sha + `"},"body":"why","author":"someone-else"}`, domain.RoleMember, true, 400, nil},
		{"unknown_target_field", http.MethodPost, "/anchored-notes", `{"target":{"kind":"commit","commit":"` + sha + `","path":"/work/app"},"body":"why"}`, domain.RoleMember, true, 400, nil},
		{"trailing_body", http.MethodPost, "/anchored-notes", create + ` {}`, domain.RoleMember, true, 400, nil},
		{"no_json", http.MethodPost, "/anchored-notes", ``, domain.RoleMember, true, 415, nil},
		{"oversized", http.MethodPost, "/anchored-notes", `{"target":{"kind":"commit","commit":"` + sha + `"},"body":"` + strings.Repeat("x", anchoredNoteBodyLimit) + `"}`, domain.RoleMember, true, 413, nil},
		{"invalid_note", http.MethodPost, "/anchored-notes", create, domain.RoleMember, true, 422, domain.ErrValidation},
		{"member_rewrites", http.MethodPut, "/anchored-notes/" + id, `{"body":"why, again"}`, domain.RoleMember, true, 200, nil},
		{"rewrite_without_body", http.MethodPut, "/anchored-notes/" + id, `{}`, domain.RoleMember, true, 400, nil},
		{"rewrite_with_target", http.MethodPut, "/anchored-notes/" + id, `{"body":"x","target":{"kind":"commit","commit":"` + sha + `"}}`, domain.RoleMember, true, 400, nil},
		{"rewrite_of_anothers", http.MethodPut, "/anchored-notes/" + id, `{"body":"x"}`, domain.RoleMember, true, 404, domain.ErrNotFound},
		{"viewer_rewrites", http.MethodPut, "/anchored-notes/" + id, `{"body":"x"}`, domain.RoleViewer, true, 403, nil},
		{"member_removes", http.MethodDelete, "/anchored-notes/" + id, ``, domain.RoleMember, true, 204, nil},
		{"removes_anothers", http.MethodDelete, "/anchored-notes/" + id, ``, domain.RoleMember, true, 404, domain.ErrNotFound},
		{"viewer_removes", http.MethodDelete, "/anchored-notes/" + id, ``, domain.RoleViewer, true, 403, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := anchoredNoteHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, test.method, base+test.path, test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status >= 300 && test.serviceError == nil {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status != 200 {
				return
			}
			var note domain.AnchoredNote
			if err := json.Unmarshal(response.Body.Bytes(), &note); err != nil || note.Author != "actor" || note.Body != backend.body || note.ID != id {
				t.Fatalf("answer: %s %v", response.Body.String(), err)
			}
			if test.method == http.MethodPost && (backend.target != domain.AnchoredNoteTarget{Kind: domain.AnchoredNoteCommit, Commit: sha}) {
				t.Fatalf("target that reached the service: %+v", backend.target)
			}
			if test.method == http.MethodPut && backend.noteID != id {
				t.Fatalf("note that reached the service: %q", backend.noteID)
			}
		})
	}
}

func TestAnchoredNoteAndWorktreeReadHTTP(t *testing.T) {
	for _, path := range []string{"/anchored-notes", "/worktrees", "/anchored-sessions/codex/thread-1/calls"} {
		for _, test := range []struct {
			name         string
			role         domain.MemberRole
			authed       bool
			status       int
			serviceError error
		}{
			{"anonymous", domain.RoleOwner, false, 401, nil},
			{"viewer", domain.RoleViewer, true, 403, nil},
			{"member", domain.RoleMember, true, 200, nil},
			{"not_found", domain.RoleMember, true, 404, domain.ErrNotFound},
		} {
			t.Run(path+"/"+test.name, func(t *testing.T) {
				backend, handler, base := anchoredNoteHTTPFixture(t, test.role)
				backend.err = test.serviceError
				response := bpHTTP(t, handler, http.MethodGet, base+path, "", test.authed)
				if response.Code != test.status {
					t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
				}
				if test.status == 200 {
					// An empty list is a list, never null.
					if strings.TrimSpace(response.Body.String()) != "[]" || backend.calls != 1 || backend.actor != "actor" {
						t.Fatalf("answer %q, service %+v", response.Body.String(), backend)
					}
					if strings.HasSuffix(path, "/calls") && (backend.provider != domain.ProviderCodex || backend.session != "thread-1") {
						t.Fatalf("session that reached the service: %s %s", backend.provider, backend.session)
					}
				}
			})
		}
	}
}

func TestWorktreeReportHTTP(t *testing.T) {
	valid := `{"machine":"laptop","worktrees":[{"name":"app","branch":"main"},{"name":"app-detached"}]}`
	for _, test := range []struct {
		name         string
		role         domain.MemberRole
		authed       bool
		body         string
		status       int
		serviceError error
	}{
		{"anonymous", domain.RoleOwner, false, valid, 401, nil},
		{"viewer", domain.RoleViewer, true, valid, 403, nil},
		{"member", domain.RoleMember, true, valid, 200, nil},
		{"empty_list", domain.RoleMember, true, `{"worktrees":[]}`, 200, nil},
		{"missing_list", domain.RoleMember, true, `{"machine":"laptop"}`, 400, nil},
		{"null_list", domain.RoleMember, true, `{"worktrees":null}`, 400, nil},
		{"path_field", domain.RoleMember, true, `{"worktrees":[{"name":"app","path":"/work/app"}]}`, 400, nil},
		{"account_field", domain.RoleMember, true, `{"worktrees":[],"account":"someone-else"}`, 400, nil},
		{"no_json", domain.RoleMember, true, ``, 415, nil},
		{"oversized", domain.RoleMember, true, `{"worktrees":[{"name":"` + strings.Repeat("n", worktreeReportBodyLimit) + `"}]}`, 413, nil},
		{"invalid_list", domain.RoleMember, true, valid, 422, domain.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := anchoredNoteHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, http.MethodPut, base+"/worktrees", test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status != 200 && test.serviceError == nil {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" || backend.report.Worktrees == nil {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.name == "member" && (backend.report.Machine != "laptop" || len(backend.report.Worktrees) != 2 || backend.report.Worktrees[0] != (domain.WorktreeReportEntry{Name: "app", Branch: "main"})) {
				t.Fatalf("list that reached the service: %+v", backend.report)
			}
		})
	}
}
