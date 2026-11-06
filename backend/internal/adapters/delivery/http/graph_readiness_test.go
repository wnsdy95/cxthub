package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type graphReadinessHTTPQueries struct {
	inbound.RepositoryStateQueries
	calls int
}

func (queries *graphReadinessHTTPQueries) GetGraphReadiness(ctx context.Context, _ domain.ContentHash) (domain.GraphReadiness, error) {
	if inbound.DocumentReadAuthorizer(ctx) == nil {
		return domain.GraphReadiness{}, domain.ErrForbidden
	}
	queries.calls++
	return domain.GraphReadiness{Projection: domain.GraphProjectionStatus{State: "catching_up"}}, nil
}

func TestGraphReadinessHTTPGuardsAndDoesNotActivateReads(test *testing.T) {
	fixture := newMemoryAdmissionHTTPFixtureWithStore(test, store.NewFSStore(test.TempDir()))
	queries := &graphReadinessHTTPQueries{}
	if err := fixture.server.SetRepositoryStateQueries(queries, inbound.RepositoryStateCompatibility); err != nil {
		test.Fatal(err)
	}
	endpoint := "/api/v1/repos/" + url.PathEscape(string(fixture.repo)) + "/graph-readiness"
	read := func(token string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, endpoint, nil)
		request.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		fixture.server.Handler().ServeHTTP(response, request)
		return response
	}
	response := read(fixture.session.Token)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"selected":false`) || queries.calls != 1 {
		test.Fatal("candidate probe failed", response.Code, response.Body)
	}
	if err := fixture.identity.Logout(systemTestContext(), fixture.session.Token); err != nil {
		test.Fatal(err)
	}
	response = read(fixture.session.Token)
	if response.Code != http.StatusUnauthorized || queries.calls != 1 || strings.Contains(response.Body.String(), "catching_up") {
		test.Fatal("revoked caller read readiness", response.Code, response.Body)
	}
}

func TestGraphReadinessUnavailableNeverBuildsGraph(test *testing.T) {
	server := NewServer(&repositoryStatePoisonedBackend{}, nil)
	response := httptest.NewRecorder()
	server.graphReadiness(response, httptest.NewRequest(http.MethodGet, "/graph-readiness", nil))
	if response.Code != http.StatusNotImplemented || !strings.Contains(response.Body.String(), "graph_readiness_unavailable") {
		test.Fatal(response.Code, response.Body)
	}
}
