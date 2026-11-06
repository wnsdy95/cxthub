package http

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type bootstrapHTTPStore interface {
	memoryAdmissionHTTPStore
	outbound.SessionArchiveStore
	EnableContextProtocol(context.Context, domain.ContentHash) error
}

func checkRepositoryBootstrapHTTPSelectionAndAuthorization(test *testing.T, storage bootstrapHTTPStore) {
	test.Helper()
	fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
	queries, err := fixture.backend.RepositoryStateQueries()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.server.SetRepositoryStateQueries(queries, inbound.RepositoryStatePinnedSnapshots); err != nil {
		test.Fatal(err)
	}
	ctx := systemTestContext()
	ref := domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: fixture.snapshot}
	if err := storage.CompareAndSwapRef(ctx, fixture.repo, ref, ""); err != nil {
		test.Fatal(err)
	}
	if err := storage.EnableContextProtocol(ctx, fixture.repo); err != nil {
		test.Fatal(err)
	}
	endpoint := "/api/v1/repos/" + url.PathEscape(string(fixture.repo)) + "/bootstrap"
	read := func(path, token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(response, request)
		return response
	}
	response := read(endpoint, fixture.session.Token)
	var view domain.RepositoryBootstrap
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &view) != nil || view.Snapshot == nil || view.Snapshot.ID != fixture.snapshot || view.Parent != nil {
		test.Fatal("head was not selected", response.Code, response.Body)
	}
	for _, invalid := range []string{"?branch=main&branch=other", "?branch=%ZZ", "?branch=..%2Fescape"} {
		response := read(endpoint+invalid, fixture.session.Token)
		if response.Code != http.StatusUnprocessableEntity {
			test.Fatal("invalid query accepted", invalid, response.Code, response.Body)
		}
	}
	response = read(endpoint+"?branch=missing", fixture.session.Token)
	if response.Code != http.StatusOK || strings.Contains(response.Body.String(), `"snapshot":`) || strings.Contains(response.Body.String(), `"ref":`) {
		test.Fatal("unknown branch selected another head", response.Code, response.Body)
	}
	snapshot := *view.Snapshot
	if err := storage.PutSessionArchive(ctx, domain.SessionArchive{RepoID: fixture.repo, Key: domain.SessionArchiveKey(snapshot), SnapshotID: snapshot.ID,
		Provider: snapshot.Provider, SessionID: snapshot.SessionID, ArchivedAt: time.Now().UTC(), ArchivedBy: fixture.owner.ID}); err != nil {
		test.Fatal(err)
	}
	response = read(endpoint, fixture.session.Token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"archived":true`) || strings.Contains(response.Body.String(), `"snapshot":`) {
		test.Fatal("archive leaked conversation into first selection", response.Code, response.Body)
	}
	if err := fixture.identity.Logout(ctx, fixture.session.Token); err != nil {
		test.Fatal(err)
	}
	response = read(endpoint, fixture.session.Token)
	if response.Code != http.StatusUnauthorized || strings.Contains(response.Body.String(), string(fixture.snapshot)) {
		test.Fatal("revoked session read bootstrap", response.Code, response.Body)
	}
}

func TestRepositoryBootstrapFSUnsupported(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixtureWithStore(test, store.NewFSStore(test.TempDir()))
	queries, err := fixture.backend.RepositoryStateQueries()
	if err != nil {
		test.Fatal(err)
	}
	if err := fixture.server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
		test.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+url.PathEscape(string(fixture.repo))+"/bootstrap", nil)
	request.Header.Set("Authorization", "Bearer "+fixture.session.Token)
	response := httptest.NewRecorder()
	fixture.server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "repository_bootstrap_unavailable") || strings.Contains(response.Body.String(), string(fixture.snapshot)) {
		test.Fatal("nontransactional store claimed a coherent bounded selection", response.Code, response.Body)
	}
}

func TestRepositoryBootstrapUnavailableDoesNotFallBackToFullGraph(test *testing.T) {
	server := NewServer(&repositoryStatePoisonedBackend{}, nil)
	response := httptest.NewRecorder()
	server.repositoryBootstrap(response, httptest.NewRequest(http.MethodGet, "/bootstrap", nil))
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "repository_bootstrap_unavailable") {
		test.Fatal(response.Code, response.Body)
	}
}
