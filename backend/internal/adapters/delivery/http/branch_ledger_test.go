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

type branchLedgerHTTPBackend struct {
	Backend
	repo     domain.Repo
	calls    int
	actor    string
	branches []domain.BranchRegistration
	name     string
	err      error
}

func (backend *branchLedgerHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return backend.repo, nil
}

func (backend *branchLedgerHTTPBackend) RegisterBranchIDs(ctx context.Context, _ domain.ContentHash, branches []domain.BranchRegistration) ([]domain.BranchRegistrationResult, error) {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.branches = branches
	results := []domain.BranchRegistrationResult{}
	for _, branch := range branches {
		results = append(results, domain.BranchRegistrationResult{Name: branch.Name, LocalID: branch.LocalID, LedgerID: branch.LocalID, Outcome: domain.BranchLedgerOpened})
	}
	return results, backend.err
}

func (backend *branchLedgerHTTPBackend) ListBranchLifetimes(ctx context.Context, _ domain.ContentHash, name string) ([]domain.BranchLifetime, error) {
	backend.calls++
	backend.actor, _ = inbound.RepositoryActor(ctx)
	backend.name = name
	return []domain.BranchLifetime{}, backend.err
}

func branchLedgerHTTPFixture(t *testing.T, role domain.MemberRole) (*branchLedgerHTTPBackend, http.Handler, string) {
	t.Helper()
	backend := &branchLedgerHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("c", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: role}
	return backend, NewServer(backend, identity).Handler(), "/api/v1/repos/" + string(backend.repo.ID)
}

func TestBranchIDRegistrationHTTP(t *testing.T) {
	id := strings.Repeat("a", 32)
	valid := `{"branches":[{"name":"topic","local_id":"` + id + `"}]}`
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
		{"empty_report", domain.RoleMember, true, `{"branches":[]}`, 200, nil},
		{"missing_list", domain.RoleMember, true, `{}`, 400, nil},
		{"null_list", domain.RoleMember, true, `{"branches":null}`, 400, nil},
		{"account_field", domain.RoleMember, true, `{"branches":[],"account":"someone-else"}`, 400, nil},
		{"ledger_id_field", domain.RoleMember, true, `{"branches":[{"name":"topic","local_id":"` + id + `","ledger_id":"` + id + `"}]}`, 400, nil},
		{"wrong_type", domain.RoleMember, true, `{"branches":"topic"}`, 400, nil},
		{"trailing_body", domain.RoleMember, true, valid + ` {}`, 400, nil},
		{"no_json", domain.RoleMember, true, ``, 415, nil},
		{"oversized", domain.RoleMember, true, `{"branches":[{"name":"` + strings.Repeat("n", branchRegistrationBodyLimit) + `"}]}`, 413, nil},
		{"invalid_branch", domain.RoleMember, true, valid, 422, domain.ErrValidation},
		{"removed_member", domain.RoleMember, true, valid, 403, domain.ErrForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := branchLedgerHTTPFixture(t, test.role)
			backend.err = test.serviceError
			response := bpHTTP(t, handler, http.MethodPut, base+"/branch-ids", test.body, test.authed)
			if response.Code != test.status {
				t.Fatalf("status=%d expected=%d: %s", response.Code, test.status, response.Body.String())
			}
			if test.status != 200 && test.serviceError == nil {
				if backend.calls != 0 {
					t.Fatal("a refused request reached the service")
				}
				return
			}
			if backend.calls != 1 || backend.actor != "actor" || backend.branches == nil {
				t.Fatalf("what reached the service: %+v", backend)
			}
			if test.status != 200 {
				return
			}
			var answer struct {
				Branches []domain.BranchRegistrationResult `json:"branches"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil || answer.Branches == nil || len(answer.Branches) != len(backend.branches) {
				t.Fatalf("answer: %s %v", response.Body.String(), err)
			}
		})
	}
}

func TestBranchLifetimeReadHTTP(t *testing.T) {
	for _, test := range []struct {
		name         string
		path         string
		role         domain.MemberRole
		authed       bool
		status       int
		wantName     string
		serviceError error
	}{
		{"anonymous", "/branch-lifetimes", domain.RoleOwner, false, 401, "", nil},
		{"viewer", "/branch-lifetimes", domain.RoleViewer, true, 403, "", nil},
		{"member_all", "/branch-lifetimes", domain.RoleMember, true, 200, "", nil},
		{"member_one_name", "/branch-lifetimes?name=feature%2Fone", domain.RoleMember, true, 200, "feature/one", nil},
		{"invalid_name", "/branch-lifetimes?name=not%20a%20name", domain.RoleMember, true, 422, "not a name", domain.ErrValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, handler, base := branchLedgerHTTPFixture(t, test.role)
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
			if backend.calls != 1 || backend.actor != "actor" || backend.name != test.wantName {
				t.Fatalf("what reached the service: %+v", backend)
			}
		})
	}
}

func TestBranchLedgerHTTPWithoutService(t *testing.T) {
	backend := &sessionArchiveHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte(t.Name())), RepositoryID: "ws_" + strings.Repeat("d", 32)}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: backend.repo.RepositoryID}, role: domain.RoleMember}
	handler := NewServer(backend, identity).Handler()
	for method, path := range map[string]string{http.MethodGet: "/branch-lifetimes", http.MethodPut: "/branch-ids"} {
		body := ""
		if method == http.MethodPut {
			body = `{"branches":[]}`
		}
		if response := bpHTTP(t, handler, method, "/api/v1/repos/"+string(backend.repo.ID)+path, body, true); response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s: status=%d: %s", method, path, response.Code, response.Body.String())
		}
	}
}
