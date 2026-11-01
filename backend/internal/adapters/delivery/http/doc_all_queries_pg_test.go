//go:build postgres

package http

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type documentPreparationRevocationStore struct {
	*store.PostgresStore
	before    func() error
	triggered atomic.Bool
	enqueues  atomic.Int32
}

func (storage *documentPreparationRevocationStore) readiness(ready bool, err error) (bool, error) {
	if err == nil && !ready && !storage.triggered.Swap(true) {
		err = storage.before()
	}
	return ready, err
}

func (storage *documentPreparationRevocationStore) HasDocRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocRangeIndex(ctx, repo, hash)
	return storage.readiness(ready, err)
}

func (storage *documentPreparationRevocationStore) HasDocByteRangeIndex(ctx context.Context, repo, hash domain.ContentHash) (bool, error) {
	ready, err := storage.PostgresStore.HasDocByteRangeIndex(ctx, repo, hash)
	return storage.readiness(ready, err)
}

func (storage *documentPreparationRevocationStore) EnqueueDocRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.enqueues.Add(1)
	return storage.PostgresStore.EnqueueDocRangeJob(ctx, repo, hash, now)
}

func (storage *documentPreparationRevocationStore) EnqueueDocByteRangeJob(ctx context.Context, repo, hash domain.ContentHash, now time.Time) (domain.DocRangeIndexJob, error) {
	storage.enqueues.Add(1)
	return storage.PostgresStore.EnqueueDocByteRangeJob(ctx, repo, hash, now)
}

func TestPGDocumentQueriesPreparationReauthenticatesAfterPinnedRead(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("requires disposable CXT_TEST_DSN")
	}
	ctx, cancel := context.WithTimeout(systemTestContext(), 40*time.Second)
	defer cancel()
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
	for _, byteRange := range []bool{false, true} {
		name := "metadata"
		if byteRange {
			name = "bytes"
		}
		test.Run(name, func(test *testing.T) {
			fixture := newMemoryAdmissionHTTPFixtureWithStore(test, storage)
			storage.ConfigureLegacyByteRangePublication(byteRange)
			wrapped := &documentPreparationRevocationStore{PostgresStore: storage, before: func() error {
				return revoker.Logout(ctx, fixture.session.Token)
			}}
			service := app.NewService(wrapped, wrapped, nil, nil, nil)
			queries, err := service.DocumentQueriesForProfile(inbound.DocumentQueryVerifiedRoots)
			if err != nil {
				test.Fatal(err)
			}
			request := fixture.request(ctx, "digest")
			authorize := fixture.server.memoryReadAuthorizer(request, fixture.repo)
			admitted, err := authorize(ctx, fixture.repo)
			if err != nil || inbound.DocumentSnapshotReadAuthorizer(admitted) == nil {
				test.Fatal("initial authorization did not capture the valid snapshot branch", err)
			}
			admitted = inbound.WithDocumentReadAuthorizer(admitted, authorize)
			page, err := queries.ReadDocViewEvents(admitted, fixture.repo, fixture.snapshot, "", 0, 1, "chat")
			if !wrapped.triggered.Load() || !errors.Is(err, domain.ErrUnauthorized) || wrapped.enqueues.Load() != 0 || page.Hash != "" {
				test.Fatal("derived preparation reused authority from its completed pinned read", wrapped.triggered.Load(), wrapped.enqueues.Load(), page, err)
			}
			memoryAdmissionAssertReleased(test)
		})
	}
}
