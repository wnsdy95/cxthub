package http

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type viewRevisionHTTPBackend struct {
	Backend
	repo     domain.Repo
	calls    int
	actor    string
	asked    domain.ContentHash
	revision string
	err      error
}

func (backend *viewRevisionHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *viewRevisionHTTPBackend) ViewRevision(ctx context.Context, repo domain.ContentHash) (string, error) {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.asked = repo
	return backend.revision, backend.err
}

// The mark an open page follows is read by whoever may view the repository,
// carries nothing but the mark, and is kept by no cache.
func TestViewRevisionHTTP(t *testing.T) {
	for _, test := range []struct {
		name         string
		method       string
		role         domain.MemberRole
		authed       bool
		status       int
		reached      bool
		serviceError error
	}{
		{"anonymous", http.MethodGet, domain.RoleOwner, false, 401, false, nil},
		{"viewer", http.MethodGet, domain.RoleViewer, true, 200, true, nil},
		{"puller", http.MethodGet, domain.RolePuller, true, 200, true, nil},
		{"member", http.MethodGet, domain.RoleMember, true, 200, true, nil},
		{"owner", http.MethodGet, domain.RoleOwner, true, 200, true, nil},
		{"repository_gone", http.MethodGet, domain.RoleMember, true, 404, true, domain.ErrNotFound},
		{"storage_unavailable", http.MethodGet, domain.RoleMember, true, 409, true, domain.ErrConflict},
		{"not_a_repository", http.MethodGet, domain.RoleMember, true, 422, true, domain.ErrValidation},
		{"writing_it", http.MethodPost, domain.RoleOwner, true, 405, false, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &viewRevisionHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("e", 32)}, revision: "12.0.3", err: test.serviceError}
			identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: test.role}
			handler := NewServer(backend, identity).Handler()
			body := ""
			if test.method == http.MethodPost {
				body = "{}"
			}
			response := bpHTTP(t, handler, test.method, "/api/v1/repos/"+string(backend.repo.ID)+"/revision", body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if !test.reached {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" || backend.asked != backend.repo.ID {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status != 200 {
				if strings.Contains(response.Body.String(), "12.0.3") {
					t.Fatalf("a refusal carried the mark: %s", response.Body.String())
				}
				return
			}
			var answer map[string]any
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || !reflect.DeepEqual(answer, map[string]any{"revision": "12.0.3"}) {
				t.Fatalf("the answer: %s %v", response.Body.String(), err)
			}
			if cache := response.Header().Get("Cache-Control"); cache != "no-store" {
				t.Fatalf("Cache-Control=%q", cache)
			}
		})
	}
}

// A server whose backend has no mark to give says so.
func TestViewRevisionHTTPWithoutTheService(t *testing.T) {
	backend, handler, base := remoteGitHTTPFixture(t, domain.RoleMember)
	response := bpHTTP(t, handler, http.MethodGet, base+"/revision", "", true)
	if response.Code != http.StatusServiceUnavailable || backend.calls != 0 {
		t.Fatalf("status=%d calls=%d: %s", response.Code, backend.calls, response.Body.String())
	}
}
