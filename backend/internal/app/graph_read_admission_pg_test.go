//go:build postgres

package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type graphReadPGTransactions struct {
	*store.PostgresStore
	active atomic.Int64
	reads  atomic.Int64
}

func (storage *graphReadPGTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if storage.InReadOnlyTransaction(ctx) {
		return storage.PostgresStore.WithinReadSnapshot(ctx, read)
	}
	storage.active.Add(1)
	storage.reads.Add(1)
	defer storage.active.Add(-1)
	return storage.PostgresStore.WithinReadSnapshot(ctx, read)
}

func TestPGColdGraphReloadsAfterQueuedAdmission(t *testing.T) {
	for _, policyChange := range []bool{false, true} {
		t.Run(map[bool]string{false: "new data", true: "new compatibility policy"}[policyChange], func(t *testing.T) {
			service, storage, repo := collaborationPG(t)
			observed := &graphReadPGTransactions{PostgresStore: storage}
			service.meta = observed
			first := collaborationSnapshot(t, storage, repo, "first")
			if err := service.PutPending(systemTestContext(), repo, "synthetic", domain.Pending{Target: first}); err != nil {
				t.Fatal(err)
			}
			ctx, gate := admissionTestContext(5 * time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			type result struct {
				view domain.RepositoryView
				err  error
			}
			done := make(chan result, 1)
			go func() { view, err := service.GetRepositoryView(ctx, repo); done <- result{view, err} }()
			waitResourceQueue(t, gate, ResourceBulk, 1)
			if observed.active.Load() != 0 {
				t.Fatal("queued request retains PostgreSQL transaction")
			}
			building := service.graphCache.Stats().Building
			if building != 0 {
				t.Fatal("queued request reserves a cache build")
			}
			if policyChange {
				if err := storage.RequireDocumentIdentity(systemTestContext(), repo, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
			} else {
				collaborationSnapshot(t, storage, repo, "arrived while queued")
			}
			release()
			got := <-done
			if policyChange {
				if !errors.Is(got.err, domain.ErrDocumentIdentityUpgradeRequired) || len(got.view.Snapshots) != 0 {
					t.Fatalf("old policy/data returned: %v %+v", got.err, got.view)
				}
			} else if got.err != nil || len(got.view.Snapshots) != 2 || len(got.view.Graph.SnapshotIDs) != 2 {
				t.Fatalf("old read snapshot returned: %v %+v", got.err, got.view)
			}
			if observed.reads.Load() != 2 || observed.active.Load() != 0 {
				t.Fatalf("read transaction lifetime: reads=%d active=%d", observed.reads.Load(), observed.active.Load())
			}
			assertResourcesReleased(t, gate)
		})
	}
}

func TestPGColdGraphDoesNotQueueInsideBorrowedTransaction(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			service, storage, repo := collaborationPG(t)
			ctx, gate := admissionTestContext(time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			done := make(chan error, 1)
			go func() {
				read := func(bound context.Context) error {
					return WithGraphReadResources(bound, func(admitted context.Context) error {
						_, err := service.QueryGraphState(admitted, repo, "")
						return err
					})
				}
				if write {
					done <- storage.WithinRepository(ctx, repo, read)
				} else {
					done <- storage.WithinReadSnapshot(ctx, read)
				}
			}()
			deadline := time.After(3 * time.Second)
			for {
				gate.mu.Lock()
				queued := len(gate.waiters[ResourceBulk])
				gate.mu.Unlock()
				if queued != 0 {
					t.Fatal("borrowed transaction entered bulk queue")
				}
				select {
				case err := <-done:
					if !errors.Is(err, domain.ErrResourceOverloaded) {
						t.Fatalf("unexpected result: %v", err)
					}
					release()
					assertResourcesReleased(t, gate)
					return
				case <-deadline:
					t.Fatal("borrowed transaction failed to return")
				default:
					time.Sleep(time.Millisecond)
				}
			}
		})
	}
}

func TestPGColdGraphQueuedReadersReuseCompletedBuild(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	first := collaborationSnapshot(t, storage, repo, "shared build")
	if err := service.PutPending(systemTestContext(), repo, "synthetic", domain.Pending{Target: first}); err != nil {
		t.Fatal(err)
	}
	ctx, gate := admissionTestContext(5 * time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	type result struct {
		cached *byte
		err    error
	}
	done := make(chan result, 2)
	for range 2 {
		go func() {
			var cached *byte
			err := WithGraphReadResources(ctx, func(bound context.Context) error {
				_, err := service.QueryGraphState(bound, repo, "")
				if err != nil {
					return err
				}
				stats := service.graphCache.Stats()
				if stats.Entries != 1 || stats.Building != 0 {
					return errors.New("incomplete or duplicate cache build")
				}
				keys := service.graphCache.Keys()
				encoded, found := service.graphCache.Get(keys[0])
				if !found || len(encoded) == 0 {
					return errors.New("completed cache encoding is unavailable")
				}
				cached = &encoded[0]
				return nil
			})
			done <- result{cached, err}
		}()
	}
	waitResourceQueue(t, gate, ResourceBulk, 2)
	release()
	left, right := <-done, <-done
	if left.err != nil || right.err != nil || left.cached == nil || left.cached != right.cached {
		t.Fatalf("queued readers did not reuse completed build: %+v %+v", left, right)
	}
	assertResourcesReleased(t, gate)
}

func TestPGGraphReadSaturatedAdmissionDoesNotRetainBorrowedTransaction(t *testing.T) {
	for _, write := range []bool{false, true} {
		t.Run(map[bool]string{false: "read", true: "write"}[write], func(t *testing.T) {
			service, storage, repo := collaborationPG(t)
			ctx, gate := admissionTestContext(10 * time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			_, release, err := AcquireResources(ctx, ResourceInteractive)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			_, second, err := AcquireResources(ctx, ResourceInteractive)
			if err != nil {
				t.Fatal(err)
			}
			defer second()
			done := make(chan error, 1)
			go func() {
				read := func(bound context.Context) error {
					_, err := service.GetRepositoryView(bound, repo)
					return err
				}
				if write {
					done <- storage.WithinRepository(ctx, repo, read)
				} else {
					done <- storage.WithinReadSnapshot(ctx, read)
				}
			}()
			select {
			case err := <-done:
				if !errors.Is(err, domain.ErrResourceOverloaded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("read queued while retaining caller PostgreSQL transaction")
			}
			release()
			second()
			assertResourcesReleased(t, gate)
			if _, err := storage.GetRepo(ctx, repo); err != nil {
				t.Fatal("transaction did not release its connection", err)
			}
		})
	}
}
