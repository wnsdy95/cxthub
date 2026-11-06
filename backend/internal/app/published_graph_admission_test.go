package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type publishedGraphAdmissionStore struct {
	*graphProjectionReadStore
	active atomic.Int64
}

func (storage *publishedGraphAdmissionStore) InTransaction(ctx context.Context) bool {
	return ctx.Value(graphReadTransactionKey{}) != nil
}

func (storage *publishedGraphAdmissionStore) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	storage.active.Add(1)
	defer storage.active.Add(-1)
	return read(context.WithValue(ctx, graphReadTransactionKey{}, true))
}

func TestPublishedGraphReadsQueueBeforeLoadingAndRecheckAuthority(test *testing.T) {
	for _, revoke := range []bool{false, true} {
		test.Run(map[bool]string{false: "complete", true: "revoked"}[revoke], func(test *testing.T) {
			ctx, gate := admissionTestContext(time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			service, original, repo, _ := newGraphProjectionRead(test)
			storage := &publishedGraphAdmissionStore{graphProjectionReadStore: original}
			service.meta = storage
			queries, err := service.PublishedGraphQueries()
			if err != nil {
				test.Fatal(err)
			}
			var revoked atomic.Bool
			var authorizations atomic.Int64
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				authorizations.Add(1)
				if revoked.Load() {
					return bound, domain.ErrForbidden
				}
				return inbound.WithDocumentSnapshotReadAuthorizer(bound, func(snapshot context.Context, _ domain.ContentHash) (context.Context, error) {
					return snapshot, nil
				}), nil
			})
			_, releaseWorker, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer releaseWorker()
			done := make(chan error, 1)
			go func() {
				_, err := queries.GetRepositoryView(ctx, repo)
				done <- err
			}()
			deadline := time.After(time.Second)
			for {
				gate.mu.Lock()
				queued, active := len(gate.waiters[ResourceBulk]), gate.active
				gate.mu.Unlock()
				if queued == 1 {
					if active != 1 || storage.active.Load() != 0 {
						test.Fatal("published read queued while holding its snapshot or interactive permit")
					}
					break
				}
				select {
				case err := <-done:
					test.Fatal("published read did not queue", err)
				case <-deadline:
					test.Fatal("published read did not enter the shared bulk queue")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			revoked.Store(revoke)
			releaseWorker()
			err = <-done
			wantReads := 1
			var wantErr error
			if revoke {
				wantReads, wantErr = 0, domain.ErrForbidden
			}
			if !errors.Is(err, wantErr) || storage.reads != wantReads || authorizations.Load() != 2 || storage.active.Load() != 0 {
				test.Fatal("published read loaded before admission or bypassed fresh authority", err, storage.reads, authorizations.Load())
			}
			assertResourcesReleased(test, gate)
		})
	}
}
