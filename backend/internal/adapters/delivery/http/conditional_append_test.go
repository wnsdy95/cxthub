package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type conditionalAppendHTTPBackend struct {
	*branchPullHTTPBackend
	got     inbound.UpdateRefInput
	updates int
}

func (backend *conditionalAppendHTTPBackend) UpdateRef(_ context.Context, in inbound.UpdateRefInput) (inbound.UpdateRefOutput, error) {
	backend.updates++
	backend.got = in
	return inbound.UpdateRefOutput{Ref: in.Ref, Result: inbound.RefAppended}, backend.err
}

func conditionalAppendHTTPFixture() (*conditionalAppendHTTPBackend, branchPullHTTPIdentity, string, map[string]any) {
	repo := domain.Repo{ID: domain.HashContent([]byte("conditional append HTTP")), RepositoryID: "ws_" + strings.Repeat("a", 32)}
	backend := &conditionalAppendHTTPBackend{branchPullHTTPBackend: &branchPullHTTPBackend{repo: repo}}
	identity := branchPullHTTPIdentity{repo: domain.Repository{ID: repo.RepositoryID}, role: domain.RoleMember}
	body := map[string]any{
		"branch_id":         "branch-identity",
		"target":            domain.HashContent([]byte("target")),
		"expected_target":   domain.HashContent([]byte("prior")),
		"expected_revision": map[string]any{"graph": "9007199254740993", "evidence": "0", "pending": "12"},
	}
	return backend, identity, "/api/v1/repos/" + string(repo.ID) + "/conditional-appends/v1/feature/nested", body
}

func TestConditionalAppendHTTPContract(t *testing.T) {
	backend, identity, path, body := conditionalAppendHTTPFixture()
	raw, _ := json.Marshal(body)
	handler := NewServer(backend, identity).Handler()
	response := bpHTTP(t, handler, http.MethodPost, path, string(raw), true)
	if response.Code != 200 || backend.updates != 1 {
		t.Fatalf("status=%d calls=%d body=%s", response.Code, backend.updates, response.Body.String())
	}
	input := backend.got
	if input.RepoID != backend.repo.ID || input.Ref.Kind != domain.RefBranch || input.Ref.Name != "feature/nested" || input.Ref.BranchID != body["branch_id"] || input.Ref.Target != body["target"] || input.ExpectedTarget != body["expected_target"] || !input.Append || input.Force || input.ExpectedRevision == nil || *input.ExpectedRevision != (domain.RepositoryRevision{Graph: 9007199254740993, Pending: 12}) {
		t.Fatalf("lost binding: %+v", input)
	}
	backend.err = domain.ErrRefConflict
	response = bpHTTP(t, handler, http.MethodPost, path, string(raw), true)
	if response.Code != 409 || !strings.Contains(response.Body.String(), `"ref_conflict"`) {
		t.Fatalf("conflict mapping: %d %s", response.Code, response.Body.String())
	}
}

func TestConditionalAppendHTTPRequiresConditions(t *testing.T) {
	for _, mode := range []string{"missing revision", "null revision", "missing graph", "missing evidence", "numeric graph", "empty revision", "negative revision", "missing target", "invalid target", "empty branch identity", "force", "not append", "invalid destination"} {
		t.Run(mode, func(t *testing.T) {
			backend, identity, path, body := conditionalAppendHTTPFixture()
			revision := body["expected_revision"].(map[string]any)
			switch mode {
			case "missing revision":
				delete(body, "expected_revision")
			case "null revision":
				body["expected_revision"] = nil
			case "missing graph":
				delete(revision, "graph")
			case "missing evidence":
				delete(revision, "evidence")
			case "numeric graph":
				revision["graph"] = 1
			case "empty revision":
				body["expected_revision"] = map[string]any{}
			case "negative revision":
				revision["graph"] = "-1"
			case "missing target":
				delete(body, "expected_target")
			case "invalid target":
				body["expected_target"] = "invalid"
			case "empty branch identity":
				body["branch_id"] = ""
			case "force":
				body["force"] = true
			case "not append":
				body["append"] = false
			case "invalid destination":
				body["target"] = ""
			}
			raw, _ := json.Marshal(body)
			response := bpHTTP(t, NewServer(backend, identity).Handler(), http.MethodPost, path, string(raw), true)
			if response.Code != 400 || backend.updates != 0 {
				t.Fatalf("invalid condition accepted: status=%d calls=%d body=%s", response.Code, backend.updates, response.Body.String())
			}
		})
	}
}

func TestConditionalAppendHTTPAuthorization(t *testing.T) {
	for _, mode := range []string{"anonymous", "viewer", "puller", "archived", "break glass"} {
		t.Run(mode, func(t *testing.T) {
			backend, identity, path, body := conditionalAppendHTTPFixture()
			authed, want := true, 403
			switch mode {
			case "anonymous":
				authed, want = false, 401
			case "viewer":
				identity.role = domain.RoleViewer
			case "puller":
				identity.role = domain.RolePuller
			case "archived":
				identity.repo.Archived = true
			case "break glass":
				identity.role, identity.breakGlass = "", true
			}
			raw, _ := json.Marshal(body)
			response := bpHTTP(t, NewServer(backend, identity).Handler(), http.MethodPost, path, string(raw), authed)
			if response.Code != want || backend.updates != 0 {
				t.Fatalf("authorization bypass: status=%d calls=%d", response.Code, backend.updates)
			}
		})
	}
}

func TestConditionalAppendHTTPStrictBody(t *testing.T) {
	for _, mode := range []string{"unknown field", "trailing object", "oversized prefix", "oversized suffix", "wrong content type"} {
		t.Run(mode, func(t *testing.T) {
			backend, identity, path, body := conditionalAppendHTTPFixture()
			if mode == "unknown field" {
				body["symbolic"] = "refs/heads/other"
			}
			raw, _ := json.Marshal(body)
			payload, want := string(raw), 400
			switch mode {
			case "trailing object":
				payload += `{}`
			case "oversized prefix":
				payload, want = strings.Repeat(" ", 16<<10)+payload, 413
			case "oversized suffix":
				payload, want = payload+strings.Repeat(" ", 16<<10), 413
			case "wrong content type":
				payload, want = "", 415
			}
			response := bpHTTP(t, NewServer(backend, identity).Handler(), http.MethodPost, path, payload, true)
			if response.Code != want || backend.updates != 0 {
				t.Fatalf("body not rejected: status=%d calls=%d %s", response.Code, backend.updates, response.Body.String())
			}
		})
	}
}

func TestConditionalAppendHTTPContentTypeAndCSRF(t *testing.T) {
	for _, test := range []struct {
		name, contentType, origin, csrf string
		cookie, malformed               bool
		status                          int
	}{
		{name: "missing content type", status: 415},
		{name: "plain text JSON", contentType: "text/plain", status: 415},
		{name: "form JSON", contentType: "application/x-www-form-urlencoded", status: 415},
		{name: "multipart JSON", contentType: "multipart/form-data; boundary=test", status: 415},
		{name: "malformed JSON", contentType: "application/json", malformed: true, status: 400},
		{name: "cookie missing origin", contentType: "application/json", cookie: true, csrf: "1", status: 403},
		{name: "cookie foreign origin", contentType: "application/json", cookie: true, origin: "https://foreign.example", csrf: "1", status: 403},
		{name: "cookie missing CSRF", contentType: "application/json", cookie: true, origin: "http://conditional.example", status: 403},
		{name: "cookie plain text", contentType: "text/plain", cookie: true, origin: "http://conditional.example", csrf: "1", status: 415},
		{name: "cookie valid JSON", contentType: "application/json", cookie: true, origin: "http://conditional.example", csrf: "1", status: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend, identity, path, body := conditionalAppendHTTPFixture()
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			if test.malformed {
				raw = []byte(`{"expected_revision":`)
			}
			request := httptest.NewRequest(http.MethodPost, "http://conditional.example"+path, strings.NewReader(string(raw)))
			request.Header.Set("Content-Type", test.contentType)
			request.Header.Set("Origin", test.origin)
			request.Header.Set("X-Cxt-CSRF", test.csrf)
			if test.cookie {
				request.AddCookie(&http.Cookie{Name: sessionCookie, Value: "fixture-token"})
			} else {
				request.Header.Set("Authorization", "Bearer fixture-token")
			}
			response := httptest.NewRecorder()
			NewServer(backend, identity).Handler().ServeHTTP(response, request)
			wantCalls := 0
			if test.status == 200 {
				wantCalls = 1
			}
			if response.Code != test.status || backend.updates != wantCalls {
				t.Fatalf("status=%d calls=%d body=%s", response.Code, backend.updates, response.Body.String())
			}
			if test.status == 403 && !strings.Contains(response.Body.String(), `"csrf_rejected"`) {
				t.Fatalf("did not exercise CSRF boundary: %s", response.Body.String())
			}
		})
	}
}
