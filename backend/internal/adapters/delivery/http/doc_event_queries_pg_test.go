//go:build postgres

package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentEventHTTPAdmissionBoundary struct {
	inbound.DocumentEventQueries
	before func(context.Context)
}

func (boundary documentEventHTTPAdmissionBoundary) ReadDocEvents(ctx context.Context, repo, hash, base domain.ContentHash, offset, limit int) (domain.DocEventPage, error) {
	authorize := inbound.DocumentReadAuthorizer(ctx)
	if authorize == nil {
		return domain.DocEventPage{}, domain.ErrUnauthorized
	}
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(admitted context.Context, requested domain.ContentHash) (context.Context, error) {
		bound, err := authorize(admitted, requested)
		if err == nil {
			boundary.before(bound)
		}
		return bound, err
	})
	return boundary.DocumentEventQueries.ReadDocEvents(ctx, repo, hash, base, offset, limit)
}

func TestPGDocumentEventQueriesHTTPPinnedAuthorization(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx := systemTestContext()
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	revoker := app.NewIdentityService(auth.NewDevVerifier(), peer)
	for _, change := range []string{"current", "session revoked", "membership revoked"} {
		test.Run(change, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
			queries, err := fixture.backend.Service.DocumentEventQueriesForProfile(documentquery.VerifiedRoots)
			if err != nil {
				test.Fatal(err)
			}
			checks := 0
			boundary := documentEventHTTPAdmissionBoundary{DocumentEventQueries: queries, before: func(admitted context.Context) {
				checks++
				if storage.InTransaction(admitted) || inbound.DocumentSnapshotReadAuthorizer(admitted) == nil {
					test.Fatal("normal authentication did not precede the pinned document read")
				}
				switch change {
				case "session revoked":
					if err := revoker.Logout(ctx, fixture.session.Token); err != nil {
						test.Fatal(err)
					}
				case "membership revoked":
					if err := revoker.RemoveMember(ctx, fixture.owner.ID, fixture.repository.ID, fixture.viewer.ID); err != nil {
						test.Fatal(err)
					}
				}
			}}
			if err := fixture.server.SetDocumentEventQueries(boundary, inbound.DocumentQueryVerifiedRoots); err != nil {
				test.Fatal(err)
			}
			path := "/api/v1/repos/" + url.PathEscape(string(fixture.repo)) + "/docs/" + url.PathEscape(string(fixture.snapshot)) + "/events?offset=0&limit=1"
			request := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			request.Header.Set("Authorization", "Bearer "+fixture.session.Token)
			response := httptest.NewRecorder()
			fixture.server.Handler().ServeHTTP(response, request)
			expected := http.StatusOK
			if change == "session revoked" {
				expected = http.StatusUnauthorized
			} else if change == "membership revoked" {
				expected = http.StatusForbidden
			}
			if response.Code != expected || checks != 1 || expected == http.StatusOK && !strings.Contains(response.Body.String(), `"total":1`) ||
				expected != http.StatusOK && strings.Contains(response.Body.String(), `"events"`) {
				test.Fatal("real document query did not enforce current pinned authorization", response.Code, checks, response.Body)
			}
			memoryAdmissionAssertReleased(test)
		})
	}
}
