package http

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type anchoredSessionHTTPBackend struct {
	Backend
	repo     domain.Repo
	calls    int
	actor    string
	provider domain.ProviderKind
	session  string
	sha      string
	body     domain.AnchoredSessionBody
	err      error
}

func (backend *anchoredSessionHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *anchoredSessionHTTPBackend) seen(ctx context.Context) {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
}

func (backend *anchoredSessionHTTPBackend) PutAnchoredSession(ctx context.Context, _ domain.ContentHash, provider domain.ProviderKind, session string, body domain.AnchoredSessionBody) (domain.AnchoredSessionReceipt, error) {
	backend.seen(ctx)
	backend.provider, backend.session, backend.body = provider, session, body
	return domain.AnchoredSessionReceipt{Revision: 1, BodyHash: domain.HashContent([]byte("body")), Changed: true}, backend.err
}

func (backend *anchoredSessionHTTPBackend) GetAnchoredSession(ctx context.Context, _ domain.ContentHash, provider domain.ProviderKind, session string) (domain.AnchoredSession, error) {
	backend.seen(ctx)
	backend.provider, backend.session = provider, session
	return domain.AnchoredSession{Provider: provider, SessionID: session, Uploader: backend.actor}, backend.err
}

func (backend *anchoredSessionHTTPBackend) AnchoredSessionCalls(context.Context, domain.ContentHash, domain.ProviderKind, string) ([]domain.AnchoredAgentCall, error) {
	return nil, backend.err
}

func (backend *anchoredSessionHTTPBackend) AnchoredSessionContentAccess(context.Context, domain.ContentHash, domain.ProviderKind, string) (domain.AnchoredContentAccess, error) {
	return domain.AnchoredContentAccess{}, backend.err
}

func (backend *anchoredSessionHTTPBackend) ListAnchoredSessions(ctx context.Context, _ domain.ContentHash) ([]domain.AnchoredSessionSummary, error) {
	backend.seen(ctx)
	return []domain.AnchoredSessionSummary{}, backend.err
}

func (backend *anchoredSessionHTTPBackend) AnchoredCommitSegments(ctx context.Context, _ domain.ContentHash, sha string) ([]domain.AnchoredCommitSegment, error) {
	backend.seen(ctx)
	backend.sha = sha
	return []domain.AnchoredCommitSegment{}, backend.err
}

func anchoredSessionHTTPFixture(t *testing.T, role domain.MemberRole) (*anchoredSessionHTTPBackend, http.Handler, string) {
	t.Helper()
	backend := &anchoredSessionHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("a", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	return backend, NewServer(backend, identity).Handler(), "/api/v1/repos/" + string(backend.repo.ID)
}

func TestAnchoredSessionUploadHTTP(t *testing.T) {
	const valid = `{"rule_version":1,"scope":"working-folder","records":0}`
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
		{"puller", domain.RolePuller, true, valid, 403, nil},
		{"member", domain.RoleMember, true, valid, 200, nil},
		{"owner", domain.RoleOwner, true, valid, 200, nil},
		{"uploader_field", domain.RoleMember, true, `{"rule_version":1,"scope":"working-folder","uploader":"someone-else"}`, 400, nil},
		{"wrong_type", domain.RoleMember, true, `{"rule_version":"1"}`, 400, nil},
		{"trailing_body", domain.RoleMember, true, valid + ` {}`, 400, nil},
		{"array", domain.RoleMember, true, `[]`, 400, nil},
		{"no_json", domain.RoleMember, true, ``, 415, nil},
		{"oversized", domain.RoleMember, true, `{"machine":"` + strings.Repeat("m", domain.AnchoredSessionBodyLimit) + `"}`, 413, nil},
		{"removed_member", domain.RoleMember, true, valid, 403, domain.ErrForbidden},
		{"another_uploader", domain.RoleMember, true, valid, 409, domain.ErrConflict},
		{"invalid_report", domain.RoleMember, true, valid, 422, domain.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := anchoredSessionHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, http.MethodPut, base+"/anchored-sessions/claude/session-1", test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status != 200 && test.serviceError == nil {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" || backend.provider != domain.ProviderClaude || backend.session != "session-1" ||
				backend.body.RuleVersion != 1 || backend.body.Scope != domain.AnchoredScopeFolder {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status != 200 {
				return
			}
			var receipt domain.AnchoredSessionReceipt
			if err := json.Unmarshal(response.Body.Bytes(), &receipt); err != nil || receipt.Revision != 1 || !receipt.Changed {
				t.Fatalf("receipt: %s %v", response.Body.String(), err)
			}
		})
	}
}

func TestAnchoredSessionReadHTTP(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, test := range []struct {
		name         string
		path         string
		role         domain.MemberRole
		authed       bool
		status       int
		serviceError error
	}{
		{"list_anonymous", "/anchored-sessions", domain.RoleOwner, false, 401, nil},
		{"list_viewer", "/anchored-sessions", domain.RoleViewer, true, 403, nil},
		{"list_member", "/anchored-sessions", domain.RoleMember, true, 200, nil},
		{"one_viewer", "/anchored-sessions/codex/session-1", domain.RoleViewer, true, 403, nil},
		{"one_member", "/anchored-sessions/codex/session-1", domain.RoleMember, true, 200, nil},
		{"one_of_another_uploader", "/anchored-sessions/codex/session-1", domain.RoleOwner, true, 404, domain.ErrNotFound},
		{"commit_viewer", "/anchored-commits/" + sha, domain.RoleViewer, true, 403, nil},
		{"commit_member", "/anchored-commits/" + sha, domain.RoleMember, true, 200, nil},
		{"commit_invalid", "/anchored-commits/abc1234", domain.RoleMember, true, 422, domain.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := anchoredSessionHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, http.MethodGet, base+test.path, "", test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status != 200 && test.serviceError == nil {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if strings.HasPrefix(test.path, "/anchored-commits/") && backend.sha != strings.TrimPrefix(test.path, "/anchored-commits/") {
				t.Fatalf("commit number = %q", backend.sha)
			}
			if strings.HasPrefix(test.path, "/anchored-sessions/") && (backend.provider != domain.ProviderCodex || backend.session != "session-1") {
				t.Fatalf("session key = %s %s", backend.provider, backend.session)
			}
		})
	}
}

func TestAnchoredSessionHTTPWithoutService(t *testing.T) {
	backend := &sessionArchiveHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("b", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
	response := bpHTTP(t, NewServer(backend, identity).Handler(), http.MethodGet, "/api/v1/repos/"+string(backend.repo.ID)+"/anchored-sessions", "", true)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d: %s", response.Code, response.Body.String())
	}
}
