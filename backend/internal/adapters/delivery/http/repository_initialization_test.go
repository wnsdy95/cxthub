package http

import (
	"context"
	"encoding/json"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

type initializationHTTPBackend struct {
	Backend
	repo  domain.Repo
	calls int
	actor string
	err   error
}

func (b *initializationHTTPBackend) GetRepo(context.Context, domain.ContentHash) (domain.Repo, error) {
	return b.repo, nil
}
func (b *initializationHTTPBackend) BeginRepositoryInitialization(_ context.Context, actor string, _ domain.ContentHash, in domain.RepositoryInitializationRequest) (domain.RepositoryInitializationReceipt, error) {
	b.calls++
	b.actor = actor
	return domain.RepositoryInitializationReceipt{Version: 1, CreationID: "init_" + strings.Repeat("a", 32), Repo: b.repo}, b.err
}
func (b *initializationHTTPBackend) FinalizeRepositoryInitialization(context.Context, domain.ContentHash, domain.RepositoryInitializationFinalize) (domain.RepositoryInitializationReceipt, error) {
	b.calls++
	return domain.RepositoryInitializationReceipt{}, b.err
}
func TestRepositoryInitializationHTTPBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, path, body string
		role             domain.MemberRole
		authed           bool
		err              error
		status, calls    int
	}{
		{"begin_no_auth", "", `{"remote_url":"https://host/alice/code"}`, domain.RoleOwner, false, nil, 401, 0},
		{"begin_actor", "", `{"remote_url":"https://host/alice/code"}`, domain.RoleOwner, true, nil, 200, 1},
		{"begin_unknown_protocol", "", `{"remote_url":"https://host/alice/code","context_protocol":1}`, domain.RoleOwner, true, nil, 400, 0},
		{"unsupported", "", `{}`, domain.RoleOwner, true, domain.ErrRepositoryInitializationUnsupported, 501, 1},
		{"existing_empty", "", `{}`, domain.RoleOwner, true, domain.ErrRepositoryInitializationConflict, 409, 1},
		{"begin_authority_current", "", `{}`, domain.RoleOwner, true, domain.ErrForbidden, 403, 1},
		{"finalize_member", "/finalize", `{}`, domain.RoleMember, true, nil, 403, 0},
		{"finalize_maintainer", "/finalize", `{}`, domain.RoleMaintainer, true, nil, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &initializationHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("http init")), RepositoryID: "ws_" + strings.Repeat("e", 32)}, err: tc.err}
			id := branchPullHTTPIdentity{repo: domain.Repository{ID: b.repo.RepositoryID}, role: tc.role}
			h := NewServer(b, id).Handler()
			w := bpHTTP(t, h, http.MethodPost, "/api/v1/repos/"+string(b.repo.ID)+"/initialization"+tc.path, tc.body, tc.authed)
			if w.Code != tc.status || b.calls != tc.calls {
				t.Fatalf("status=%d calls=%d body=%s", w.Code, b.calls, w.Body.String())
			}
			if tc.name == "begin_actor" && b.actor != "actor" {
				t.Fatal("actor not from authenticated context")
			}
		})
	}
}

type initializationHTTPView struct {
	*initializationHTTPBackend
	pending bool
}

func (b initializationHTTPView) BranchPullVersion() int { return 0 }
func (b initializationHTTPView) GetRepositoryInitializationView(_ context.Context, _ domain.ContentHash, branch string) (domain.Repo, bool, error) {
	return b.repo, b.pending && branch != "", nil
}
func TestRepositoryInitializationHTTPPendingReadable(t *testing.T) {
	for _, pending := range []bool{false, true} {
		b := initializationHTTPView{initializationHTTPBackend: &initializationHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("read init")), RepositoryID: "ws_" + strings.Repeat("e", 32)}}, pending: pending}
		id := branchPullHTTPIdentity{repo: domain.Repository{ID: b.repo.RepositoryID}, role: domain.RoleMember}
		w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodGet, "/api/v1/repos/"+string(b.repo.ID)+"?initial_branch=main", "", true)
		var body map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
			t.Fatal("read discovery requires manage", w.Code)
		}
		if (body["initial_anchor_available"] == true) != pending || b.calls != 0 {
			t.Fatal("incorrect pending signal or mutation")
		}
	}
}

func TestRepositoryInitializationSchemaFields(t *testing.T) {
	for name, typ := range map[string]reflect.Type{
		"RepositoryInitializationRequest":  reflect.TypeOf(domain.RepositoryInitializationRequest{}),
		"RepositoryInitializationAnchor":   reflect.TypeOf(domain.RepositoryInitializationAnchor{}),
		"RepositoryInitializationFinalize": reflect.TypeOf(domain.RepositoryInitializationFinalize{}),
		"RepositoryInitializationReceipt":  reflect.TypeOf(domain.RepositoryInitializationReceipt{}),
	} {
		props := schemaProps(t, "../../../../../schemas/openapi.yaml", name)
		fields := jsonFields(typ)
		if len(props) != len(fields) {
			t.Fatalf("%s field count drift", name)
		}
		for _, field := range fields {
			if !props[field] {
				t.Fatalf("%s missing %s", name, field)
			}
		}
	}
}

func (b *initializationHTTPBackend) GetRepositoryInitialization(context.Context, domain.ContentHash) (domain.RepositoryInitializationReceipt, error) {
	b.calls++
	return domain.RepositoryInitializationReceipt{Version: 1, CreationID: "init_" + strings.Repeat("a", 32), Repo: b.repo}, b.err
}

func TestRepositoryInitializationHTTPBranchQuery(t *testing.T) {
	for _, tc := range []struct {
		query     string
		status    int
		available bool
	}{
		{"", 200, false}, {"?initial_branch=main", 200, true}, {"?initial_branch=feature%2Fone", 200, true},
		{"?initial_branch=", 400, false}, {"?initial_branch=main&initial_branch=other", 400, false},
		{"?initial_branch=bad%0Aname", 400, false}, {"?initial_branch=../main", 400, false}, {"?initial_branch=%zz", 400, false},
	} {
		t.Run(tc.query, func(t *testing.T) {
			b := initializationHTTPView{initializationHTTPBackend: &initializationHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("branch-query")), RepositoryID: "ws_" + strings.Repeat("e", 32)}}, pending: true}
			id := branchPullHTTPIdentity{repo: domain.Repository{ID: b.repo.RepositoryID}, role: domain.RoleMember}
			w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodGet, "/api/v1/repos/"+string(b.repo.ID)+tc.query, "", true)
			var out struct {
				Available bool `json:"initial_anchor_available"`
			}
			if w.Code != tc.status {
				t.Fatal(w.Code, w.Body.String())
			}
			if tc.status == 200 && (json.Unmarshal(w.Body.Bytes(), &out) != nil || out.Available != tc.available) {
				t.Fatal("incorrect scope", w.Body.String())
			}
			if b.calls != 0 {
				t.Fatal("discovery mutated")
			}
		})
	}
}

func TestRepositoryInitializationHTTPReceiptRecovery(t *testing.T) {
	for _, role := range []domain.MemberRole{domain.RoleMember, domain.RoleMaintainer} {
		b := &initializationHTTPBackend{repo: domain.Repo{ID: domain.HashContent([]byte("receipt-query")), RepositoryID: "ws_" + strings.Repeat("e", 32)}}
		id := branchPullHTTPIdentity{repo: domain.Repository{ID: b.repo.RepositoryID}, role: role}
		w := bpHTTP(t, NewServer(b, id).Handler(), http.MethodGet, "/api/v1/repos/"+string(b.repo.ID)+"/initialization", "", true)
		if role == domain.RoleMember {
			if w.Code != 403 || b.calls != 0 {
				t.Fatal("unprivileged receipt", w.Code)
			}
			continue
		}
		var out map[string]any
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil || b.calls != 1 {
			t.Fatal("receipt GET", w.Code)
		}
		if _, present := out["anchor"]; present {
			t.Fatal("GET included anchor")
		}
	}
}
