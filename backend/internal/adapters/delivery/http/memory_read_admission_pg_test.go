//go:build postgres

package http

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

func TestPGMemoryReadHTTPReauthorizesQueuedRequestsAcrossReplicas(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 30*time.Second)
	test.Cleanup(cancel)
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	peer, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(peer.Close)
	revoker := app.NewIdentityService(auth.NewDevVerifier(), peer)
	for _, route := range []string{"effective", "digest", "object", "positions"} {
		for _, change := range []string{"membership", "token"} {
			test.Run(route+"/"+change, func(test *testing.T) {
				fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
				fixture.backend.entered = make(chan struct{})
				_, release, err := app.AcquireResources(context.Background(), app.ResourceBulk)
				if err != nil {
					test.Fatal(err)
				}
				parent, stop := context.WithTimeout(context.Background(), 5*time.Second)
				response := httptest.NewRecorder()
				done := make(chan struct{})
				test.Cleanup(func() {
					stop()
					release()
					select {
					case <-done:
					case <-time.After(5 * time.Second):
						test.Error("queued PG memory handler did not exit")
					}
				})
				go func() {
					defer close(done)
					fixture.server.Handler().ServeHTTP(response, fixture.request(parent, route))
				}()
				select {
				case <-fixture.backend.entered:
				case <-done:
					test.Fatalf("initial PG viewer guard failed: %d %s", response.Code, response.Body)
				case <-parent.Done():
					test.Fatal("PG memory request never reached admission")
				}
				want, code := http.StatusForbidden, "forbidden"
				if change == "membership" {
					err = revoker.RemoveMember(ctx, fixture.owner.ID, fixture.repository.ID, fixture.viewer.ID)
				} else {
					err = revoker.Logout(ctx, fixture.session.Token)
					want, code = http.StatusUnauthorized, "unauthenticated"
				}
				if err != nil {
					test.Fatal(err)
				}
				release()
				select {
				case <-done:
				case <-parent.Done():
					test.Fatal("PG memory request failed to finish after admission")
				}
				if response.Code != want || !strings.Contains(response.Body.String(), `"code":"`+code+`"`) ||
					fixture.backend.reads.Load() != 0 || fixture.backend.authorizations.Load() != 1 {
					test.Fatalf("queued HTTP read missed committed cross-replica revocation: status=%d reads=%d authorizations=%d body=%s",
						response.Code, fixture.backend.reads.Load(), fixture.backend.authorizations.Load(), response.Body)
				}
				memoryAdmissionAssertReleased(test)
			})
		}
	}
}
