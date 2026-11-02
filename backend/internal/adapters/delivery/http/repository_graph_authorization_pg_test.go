//go:build postgres

package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

func TestPGLegacyGraphHTTPRechecksSessionBeforeMetadata(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	storage, err := store.NewPostgresStore(systemTestContext(), dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(systemTestContext(), "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	for _, route := range []string{"view", "graph-state", "pending-view"} {
		for _, revoked := range []bool{false, true} {
			test.Run(route+"/"+map[bool]string{false: "current", true: "revoked"}[revoked], func(test *testing.T) {
				fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
				observed := &repositoryStateAuthorizationPGStore{PostgresStore: storage}
				service := app.NewService(observed, storage, nil, nil, storage)
				queries, err := service.GraphQueriesForProfile(inbound.RepositoryGraphPinnedSnapshots)
				if err != nil {
					test.Fatal(err)
				}
				server := NewServer(fixture.backend.Service, fixture.identity)
				if err := server.SetRepositoryGraphQueries(queries, inbound.RepositoryGraphPinnedSnapshots); err != nil {
					test.Fatal(err)
				}
				observed.before = func(bound context.Context) error {
					if storage.InTransaction(bound) || inbound.DocumentSnapshotReadAuthorizer(bound) == nil {
						return domain.ErrIntegrity
					}
					if revoked {
						return fixture.identity.Logout(context.Background(), fixture.session.Token)
					}
					return nil
				}
				request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+url.PathEscape(string(fixture.repo))+"/"+route, nil)
				request.Header.Set("Authorization", "Bearer "+fixture.session.Token)
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				want, payload := http.StatusOK, 1
				if revoked {
					want, payload = http.StatusUnauthorized, 0
				}
				if response.Code != want || observed.scopes == 0 || observed.payload != payload {
					test.Fatal("legacy graph bypassed pinned current authority", response.Code, want, observed.scopes, observed.payload, response.Body)
				}
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}
