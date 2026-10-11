package http

import (
	"context"
	"net/http/httptest"
	"net/url"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type associationStatusHTTPBackend struct {
	Backend
	requests []domain.GitAssociationStatusRequest
	page     domain.GitAssociationStatusPage
	err      error
}

func (backend *associationStatusHTTPBackend) GetGitAssociationStatus(_ context.Context, repo domain.ContentHash, request domain.GitAssociationStatusRequest) (domain.GitAssociationStatusPage, error) {
	backend.requests = append(backend.requests, request)
	page := backend.page
	page.RepoID, page.Kind, page.Filter = repo, request.Kind, request.State
	return page, backend.err
}

func TestGitAssociationStatusHTTPAuthorizationContract(t *testing.T) {
	storage := store.NewFSStore(t.TempDir())
	service := app.NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	backend := &associationStatusHTTPBackend{Backend: service, page: domain.GitAssociationStatusPage{
		Items: []domain.GitAssociationStatus{}, Discovery: domain.GitAssociationDiscoveryStatus{State: "checkpoint_recorded", Version: 3},
		Reconciliation: domain.GitAssociationHeadStatus{State: "retrying", Reason: "temporary_failure"}, CurrentApplicability: "not_evaluated",
	}}
	api := NewServer(backend, app.NewIdentityService(auth.NewDevVerifier(), storage))
	server := httptest.NewServer(api.Handler())
	defer server.Close()
	var user struct {
		Username string `json:"username"`
	}
	doJSON(t, "GET", server.URL+"/api/v1/me", nil, &user)
	var repository struct {
		Slug string `json:"slug"`
	}
	if code := doJSON(t, "POST", server.URL+"/api/v1/repositories", map[string]any{"name": "Associations"}, &repository); code != 200 {
		t.Fatal(code)
	}
	remote := "http://cxthub.test/" + user.Username + "/" + repository.Slug
	repo := repoIDForRemoteURLForTest(remote)
	if code := doJSON(t, "POST", server.URL+"/api/v1/repos", map[string]any{"id": repo, "remote_url": remote, "git_remote_url": "https://github.com/example/associations", "default_branch": "main"}, nil); code != 200 {
		t.Fatal(code)
	}
	endpoint := server.URL + "/api/v1/repos/" + url.PathEscape(string(repo)) + "/git/associations"
	var page domain.GitAssociationStatusPage
	if code := doJSON(t, "GET", endpoint, nil, &page); code != 200 || page.Items == nil || page.RepoID != repo || page.Discovery != backend.page.Discovery || page.CurrentApplicability != "not_evaluated" {
		t.Fatalf("query contract = %d %+v", code, page)
	}
	if !reflect.DeepEqual(backend.requests, []domain.GitAssociationStatusRequest{{Kind: "integration", State: "pending", Limit: 30}}) {
		t.Fatal("wrong defaults", backend.requests)
	}
	for _, suffix := range []string{"?kind=unknown", "?kind=", "?state=completed", "?limit=0", "?limit=101", "?limit=bad", "?cursor=ABC", "?kind=memory&kind=integration", "?unexpected=1"} {
		if code := doJSON(t, "GET", endpoint+suffix, nil, nil); code != 422 {
			t.Fatal("accepted invalid selection", suffix, code)
		}
	}
	if code := doJSONAs(t, "dev:outsider@example.test:Other", "GET", endpoint, nil, nil); code != 403 {
		t.Fatal("private state leaked", code)
	}
	if len(backend.requests) != 1 {
		t.Fatal("invalid or unauthorized call reached query")
	}
	backend.err = domain.ErrGitAssociationStatusUnavailable
	if code := doJSON(t, "GET", endpoint+"?kind=memory&state=all&limit=1", nil, nil); code != 503 {
		t.Fatal("unsupported status was reported as successful", code)
	}
	backend.err = domain.ErrIntegrity
	if code := doJSON(t, "GET", endpoint, nil, nil); code != 422 {
		t.Fatal("corrupt evidence was reported as successful", code)
	}
}
