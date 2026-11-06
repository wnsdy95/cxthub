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

type bootstrapAuthorizationPGStore struct {
	*repositoryStateAuthorizationPGStore
	points int
}

func (storage *bootstrapAuthorizationPGStore) GetSnapshot(ctx context.Context, repo, id domain.ContentHash) (domain.Snapshot, error) {
	storage.points++
	return storage.PostgresStore.GetSnapshot(ctx, repo, id)
}

func TestPGRepositoryBootstrapRechecksSessionWithoutFullGraph(test *testing.T) {
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
	test.Run("selection-archive-contract", func(test *testing.T) {
		checkRepositoryBootstrapHTTPSelectionAndAuthorization(test, storage)
	})
	for _, revoked := range []bool{false, true} {
		test.Run(map[bool]string{false: "current", true: "revoked-after-admission"}[revoked], func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
			if err := storage.CompareAndSwapRef(systemTestContext(), fixture.repo, domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: fixture.snapshot}, ""); err != nil {
				test.Fatal(err)
			}
			if err := storage.EnableContextProtocol(systemTestContext(), fixture.repo); err != nil {
				test.Fatal(err)
			}
			observed := &bootstrapAuthorizationPGStore{repositoryStateAuthorizationPGStore: &repositoryStateAuthorizationPGStore{PostgresStore: storage}}
			service := app.NewService(observed, storage, nil, nil, storage)
			queries, err := service.RepositoryStateQueries()
			if err != nil {
				test.Fatal(err)
			}
			server := NewServer(fixture.backend.Service, fixture.identity)
			if err := server.SetRepositoryStateQueries(queries, inbound.RepositoryStatePinnedSnapshots); err != nil {
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
			request := httptest.NewRequest(http.MethodGet, "/api/v1/repos/"+url.PathEscape(string(fixture.repo))+"/bootstrap", nil)
			request.Header.Set("Authorization", "Bearer "+fixture.session.Token)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			want, points := http.StatusOK, 1
			if revoked {
				want, points = http.StatusUnauthorized, 0
			}
			if response.Code != want || observed.scopes == 0 || observed.payload != 0 || observed.points != points {
				test.Fatal("bootstrap lost authorization or bounded reads", response.Code, want, observed.scopes, observed.payload, observed.points, response.Body)
			}
			memoryAdmissionAssertReleased(test)
		})
	}
}
