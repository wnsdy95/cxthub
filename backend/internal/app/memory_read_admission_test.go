package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryAdmissionReads(service *Service, repo, snapshot, memory domain.ContentHash) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"positions": func(ctx context.Context) error {
			_, err := service.QueryMemoryPositions(ctx, repo, snapshot, "")
			return err
		},
		"object": func(ctx context.Context) error { _, err := service.GetMemoryObject(ctx, repo, memory); return err },
		"digest": func(ctx context.Context) error { _, err := service.GetMemoryDigest(ctx, repo, snapshot); return err },
		"projection": func(ctx context.Context) error {
			_, err := service.GetMemoryProjection(ctx, repo, snapshot)
			return err
		},
		"branch": func(ctx context.Context) error {
			_, err := service.QueryBranchMemory(ctx, repo, "main", snapshot, "")
			return err
		},
		"effective": func(ctx context.Context) error {
			_, err := service.QueryEffectiveMemory(ctx, repo, domain.EffectiveMemoryRequest{Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot, CodeCommit: effectiveOID(3)}})
			return err
		},
	}
}

func TestMemoryReadsQueueBeforeOpeningTransactions(test *testing.T) {
	fixture := newEffectiveFixture(test)
	if err := fixture.st.CompareAndSwapRef(systemTestContext(), fixture.repo, domain.Ref{RepoID: fixture.repo, Kind: domain.RefBranch, Name: "main", Target: fixture.root}, ""); err != nil {
		test.Fatal(err)
	}
	storage := &graphReadTransactions{FSStore: fixture.st}
	fixture.svc.meta = storage
	for name, read := range memoryAdmissionReads(fixture.svc, fixture.repo, fixture.root, fixture.memory) {
		test.Run(name, func(test *testing.T) {
			ctx, gate := admissionTestContext(5 * time.Second)
			ctx, cancel := context.WithCancel(inbound.WithSystemActor(ctx))
			defer cancel()
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			initialReads := storage.reads.Load()
			done := make(chan error, 1)
			go func() { done <- read(ctx) }()
			deadline := time.After(time.Second)
			for {
				gate.mu.Lock()
				queued := len(gate.waiters[ResourceBulk])
				gate.mu.Unlock()
				if queued == 1 {
					break
				}
				select {
				case err := <-done:
					test.Fatalf("memory read bypassed shared bulk queue: %v", err)
				case <-deadline:
					test.Fatal("memory read did not join shared bulk queue")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			if storage.active.Load() != 0 || storage.reads.Load() != initialReads {
				test.Fatal("queued memory read opened a transaction")
			}
			release()
			if err := <-done; err != nil {
				test.Fatal(err)
			}
			if storage.active.Load() != 0 || storage.reads.Load() != initialReads+1 {
				test.Fatal("memory read did not use exactly one owned read snapshot")
			}
			assertResourcesReleased(test, gate)
		})
	}
}

func TestMemoryReadsBorrowedTransactionNeverQueues(test *testing.T) {
	for _, mode := range []string{"read", "write", "unknown"} {
		test.Run(mode, func(test *testing.T) {
			service, fs := newFsckSvc(test)
			storage := &graphReadTransactions{FSStore: fs}
			service.meta = storage
			ctx, gate := admissionTestContext(10 * time.Second)
			ctx, cancel := context.WithCancel(inbound.WithSystemActor(ctx))
			defer cancel()
			switch mode {
			case "read":
				ctx = context.WithValue(ctx, graphReadTransactionKey{}, true)
			case "write":
				ctx = context.WithValue(ctx, afterCommitKey{}, &afterCommitActions{})
			case "unknown":
				service.meta = struct {
					*store.FSStore
					outbound.RepositoryTransactions
				}{fs, storage}
			}
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			for name, read := range memoryAdmissionReads(service, hh("repo"), hh("snapshot"), hh("memory")) {
				done := make(chan error, 1)
				go func() { done <- read(ctx) }()
				select {
				case err := <-done:
					if !errors.Is(err, domain.ErrResourceOverloaded) {
						test.Fatalf("%s borrowed read bypassed resource admission: %v", name, err)
					}
				case <-time.After(time.Second):
					cancel()
					<-done
					test.Fatal("borrowed read waited for admission")
				}
			}
			if storage.reads.Load() != 0 {
				test.Fatal("overloaded read entered transaction body")
			}
			release()
			assertResourcesReleased(test, gate)
		})
	}
}

func TestMemoryReadReauthorizesAfterQueue(test *testing.T) {
	service, fs := newFsckSvc(test)
	storage := &graphReadTransactions{FSStore: fs}
	service.meta = storage
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var revoked atomic.Bool
	var authorizations atomic.Int64
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		authorizations.Add(1)
		if revoked.Load() {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() { _, err := service.GetMemoryDigest(ctx, hh("repo"), hh("snapshot")); done <- err }()
	waitResourceQueue(test, gate, ResourceBulk, 1)
	if authorizations.Load() != 0 || storage.reads.Load() != 0 {
		test.Fatal("memory access checked before admission")
	}
	revoked.Store(true)
	release()
	if err := <-done; !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("queued memory read retained obsolete permission", err)
	}
	if authorizations.Load() != 1 || storage.reads.Load() != 0 {
		test.Fatal("rejected read entered content transaction")
	}
	assertResourcesReleased(test, gate)
}

func TestMemoryReadScopeOwnsSerializationAndNestedReads(test *testing.T) {
	fixture := newEffectiveFixture(test)
	ctx, gate := admissionTestContext(time.Second)
	var authorizations int
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, repo domain.ContentHash) (context.Context, error) {
		authorizations++
		if repo != fixture.repo {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	var previous context.Context
	err := fixture.svc.WithMemoryReadResources(ctx, fixture.repo, func(bound context.Context) error {
		previous = bound
		for range 3 {
			if _, err := fixture.svc.GetMemoryDigest(bound, fixture.repo, fixture.root); err != nil {
				return err
			}
		}
		if authorizations != 1 {
			test.Fatal("nested memory reads repeated credential lookup")
		}
		gate.mu.Lock()
		active, bulk := gate.active, gate.bulk
		gate.mu.Unlock()
		if active != 1 || bulk != 1 {
			test.Fatal("permit released before caller response assembly")
		}
		if err := fixture.svc.WithMemoryReadResources(bound, hh("other repo"), func(context.Context) error {
			test.Fatal("foreign repository reused read authorization")
			return nil
		}); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal(err)
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	assertResourcesReleased(test, gate)
	previous = inbound.WithDocumentReadAuthorizer(previous, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		return bound, domain.ErrUnauthorized
	})
	if err := fixture.svc.WithMemoryReadResources(previous, fixture.repo, func(context.Context) error {
		test.Fatal("expired scope retained authorization")
		return nil
	}); !errors.Is(err, domain.ErrUnauthorized) {
		test.Fatal(err)
	}
	assertResourcesReleased(test, gate)
}

func TestMemoryReadQueueCancellationAndTimeout(test *testing.T) {
	for _, cause := range []string{"cancel", "deadline", "queue limit"} {
		test.Run(cause, func(test *testing.T) {
			service, _ := newFsckSvc(test)
			wait := 10 * time.Second
			if cause == "queue limit" {
				wait = 20 * time.Millisecond
			}
			ctx, gate := admissionTestContext(wait)
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				test.Fatal(err)
			}
			defer release()
			ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
				test.Error("cancelled/overloaded read invoked authorization")
				return bound, nil
			})
			var cancel context.CancelFunc
			if cause == "deadline" {
				ctx, cancel = context.WithTimeout(ctx, 20*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(ctx)
			}
			defer cancel()
			done := make(chan error, 1)
			go func() { _, err := service.GetMemoryDigest(ctx, hh("repo"), hh("snapshot")); done <- err }()
			waitResourceQueue(test, gate, ResourceBulk, 1)
			expected := domain.ErrResourceOverloaded
			if cause == "cancel" {
				cancel()
				expected = context.Canceled
			}
			if cause == "deadline" {
				expected = context.DeadlineExceeded
			}
			if err := <-done; !errors.Is(err, expected) {
				test.Fatal(err)
			}
			release()
			assertResourcesReleased(test, gate)
		})
	}
}

func TestMemoryReadLegacyCallerCannotQueueWithoutReauthorization(test *testing.T) {
	service, _ := newFsckSvc(test)
	ctx, gate := admissionTestContext(10 * time.Second)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	for _, actor := range []string{"", "trusted-caller"} {
		bound, cancel := context.WithTimeout(inbound.WithRepositoryActor(ctx, actor), time.Second)
		_, err := service.GetMemoryDigest(bound, hh("repo"), hh("snapshot"))
		cancel()
		if !errors.Is(err, domain.ErrResourceOverloaded) {
			test.Fatal("legacy caller waited without current authority resolver", err)
		}
	}
	release()
	assertResourcesReleased(test, gate)
}

func TestMemoryReadScopeCannotQueueWhileHoldingInteractivePermit(test *testing.T) {
	service, _ := newFsckSvc(test)
	ctx, gate := admissionTestContext(time.Second)
	ctx = inbound.WithSystemActor(ctx)
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseBulk()
	bound, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseRead()
	if _, err := service.GetMemoryDigest(bound, hh("repo"), hh("snapshot")); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal(err)
	}
	gate.mu.Lock()
	queued, active := len(gate.waiters[ResourceBulk]), gate.active
	gate.mu.Unlock()
	if queued != 0 || active != 2 {
		test.Fatal("memory read queued or released caller capacity")
	}
	releaseRead()
	releaseBulk()
	assertResourcesReleased(test, gate)
}

func TestMemoryReadScopeExpiresBeforeBorrowedPermit(test *testing.T) {
	service, _ := newFsckSvc(test)
	ctx, gate := admissionTestContext(time.Second)
	var revoked bool
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, _ domain.ContentHash) (context.Context, error) {
		if revoked {
			return bound, domain.ErrForbidden
		}
		return bound, nil
	})
	bound, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer release()
	var captured context.Context
	if err := service.WithMemoryReadResources(bound, hh("repo"), func(scoped context.Context) error { captured = scoped; return nil }); err != nil {
		test.Fatal(err)
	}
	revoked = true
	if err := service.WithMemoryReadResources(captured, hh("repo"), func(context.Context) error {
		test.Fatal("completed scope reused authorization from a live borrowed permit")
		return nil
	}); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal(err)
	}
	release()
	assertResourcesReleased(test, gate)
}
