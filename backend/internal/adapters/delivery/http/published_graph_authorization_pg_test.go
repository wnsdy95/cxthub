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

type publishedGraphAuthorizationPGStore struct {
	*repositoryStateAuthorizationPGStore
	reads int
}

func (storage *publishedGraphAuthorizationPGStore) ReadQualifiedGraphProjection(ctx context.Context, repo domain.ContentHash) (domain.GraphProjectionRead, error) {
	if !storage.InReadOnlyTransaction(ctx) || inbound.DocumentSnapshotReadAuthorizer(ctx) == nil {
		return domain.GraphProjectionRead{}, domain.ErrIntegrity
	}
	storage.reads++
	return storage.PostgresStore.ReadQualifiedGraphProjection(ctx, repo)
}

func TestPGPublishedGraphHTTPRechecksSessionBeforeBundle(test *testing.T) {
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
			name := "current-session"
			if revoked {
				name = "revoked-after-admission"
			}
			test.Run(route+"/"+name, func(test *testing.T) {
				fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
				worker := app.NewService(storage, storage, nil, nil, storage)
				if err := worker.ConfigureGraphProjections(systemTestContext(), []domain.ContentHash{fixture.repo}); err != nil {
					test.Fatal(err)
				}
				for attempt := 0; attempt < 20; attempt++ {
					if err := worker.ProcessGraphProjections(systemTestContext(), 1); err != nil {
						test.Fatal(err)
					}
					status, err := storage.ReadGraphProjectionStatus(systemTestContext(), fixture.repo)
					if err != nil {
						test.Fatal(err)
					}
					if status.Ready {
						break
					}
				}
				if err := worker.ConfigureGraphQualifications(systemTestContext(), []domain.ContentHash{fixture.repo}); err != nil {
					test.Fatal(err)
				}
				if err := worker.ProcessGraphQualifications(systemTestContext(), 1); err != nil {
					test.Fatal(err)
				}
				observed := &publishedGraphAuthorizationPGStore{repositoryStateAuthorizationPGStore: &repositoryStateAuthorizationPGStore{PostgresStore: storage}}
				service := app.NewService(observed, storage, nil, nil, storage)
				if err := service.ConfigureGraphReads([]domain.ContentHash{fixture.repo}); err != nil {
					test.Fatal(err)
				}
				states, err := service.RepositoryStateQueries()
				if err != nil {
					test.Fatal(err)
				}
				server := NewServer(service, fixture.identity)
				queries, err := service.GraphQueriesForProfile(inbound.RepositoryGraphPinnedSnapshots)
				if err != nil {
					test.Fatal(err)
				}
				if err := server.SetRepositoryGraphQueries(queries, inbound.RepositoryGraphPinnedSnapshots); err != nil {
					test.Fatal(err)
				}
				if err := server.SetRepositoryStateQueries(states, inbound.RepositoryStatePinnedSnapshots); err != nil {
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
				want, reads := http.StatusOK, 1
				if revoked {
					want, reads = http.StatusUnauthorized, 0
				}
				if response.Code != want || observed.scopes == 0 || observed.reads != reads || observed.payload != 0 {
					test.Fatal("published graph bypassed current pinned session authorization", response.Code, want, observed.scopes, observed.reads, observed.payload, response.Body)
				}
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}
