package http

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type sessionArchiveHTTPBackend struct {
	Backend
	repo     domain.Repo
	calls    int
	actor    string
	snapshot domain.ContentHash
	archived bool
	err      error
}

func (backend *sessionArchiveHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *sessionArchiveHTTPBackend) SetSessionArchived(ctx context.Context, repo, snapshot domain.ContentHash, archived bool) error {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.snapshot, backend.archived = snapshot, archived
	return backend.err
}

func TestSessionArchiveHTTP(t *testing.T) {
	for _, test := range []struct {
		name         string
		role         domain.MemberRole
		authed       bool
		body         string
		status       int
		serviceError error
	}{
		{"anonymous", domain.RoleOwner, false, `{"archived":true}`, 401, nil},
		{"viewer", domain.RoleViewer, true, `{"archived":true}`, 403, nil},
		{"member", domain.RoleMember, true, `{"archived":false}`, 403, nil},
		{"maintainer", domain.RoleMaintainer, true, `{"archived":true}`, 200, nil},
		{"owner_restore", domain.RoleOwner, true, `{"archived":false}`, 200, nil},
		{"missing_flag", domain.RoleOwner, true, `{}`, 400, nil},
		{"null_flag", domain.RoleOwner, true, `{"archived":null}`, 400, nil},
		{"invalid_flag", domain.RoleOwner, true, `{"archived":"false"}`, 400, nil},
		{"unknown_field", domain.RoleOwner, true, `{"archived":true,"archived_by":"other"}`, 400, nil},
		{"trailing_body", domain.RoleOwner, true, `{"archived":true} {}`, 400, nil},
		{"no_json", domain.RoleOwner, true, ``, 415, nil},
		{"revoked", domain.RoleOwner, true, `{"archived":true}`, 403, domain.ErrForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &sessionArchiveHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("e", 32)}, err: test.serviceError}
			identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: test.role}
			snapshot := domain.HashContent([]byte("selected"))
			response := bpHTTP(t, NewServer(backend, identity).Handler(), http.MethodPost, "/api/v1/repos/"+string(backend.repo.ID)+"/snapshots/"+string(snapshot)+"/archive", test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status == 200 || test.serviceError != nil {
				if backend.calls != 1 || backend.actor != "actor" || backend.snapshot != snapshot {
					t.Fatalf("incorrect mutation identity: %+v", backend)
				}
			} else if backend.calls != 0 {
				t.Fatal("invalid request reached archive service")
			}
		})
	}
}
