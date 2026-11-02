package app

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphReadTransactionKey struct{}

type graphReadTransactions struct {
	*store.FSStore
	active atomic.Int64
	reads  atomic.Int64
}

func (storage *graphReadTransactions) InTransaction(ctx context.Context) bool {
	return ctx.Value(graphReadTransactionKey{}) != nil
}

func (storage *graphReadTransactions) WithinRepository(ctx context.Context, _ domain.ContentHash, read func(context.Context) error) error {
	return read(ctx)
}

func (storage *graphReadTransactions) WithinReadSnapshot(ctx context.Context, read func(context.Context) error) error {
	if ctx.Value(graphReadTransactionKey{}) != nil {
		return read(ctx)
	}
	storage.active.Add(1)
	storage.reads.Add(1)
	defer storage.active.Add(-1)
	return read(context.WithValue(ctx, graphReadTransactionKey{}, true))
}

func TestColdGraphReadsJoinBulkQueueOutsideTransaction(t *testing.T) {
	for _, endpoint := range []string{"graph", "view", "pending"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx, gate := admissionTestContext(time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			service, fs := newFsckSvc(t)
			storage := &graphReadTransactions{FSStore: fs}
			service.meta = storage
			repo := hh(t.Name())
			if _, err := fs.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
				t.Fatal(err)
			}
			_, releaseWorker, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				t.Fatal(err)
			}
			defer releaseWorker()
			done := make(chan error, 1)
			go func() {
				var err error
				switch endpoint {
				case "graph":
					_, err = service.QueryGraphState(ctx, repo, "")
				case "view":
					_, err = service.GetRepositoryView(ctx, repo)
				case "pending":
					_, err = service.GetPendingView(ctx, repo)
				}
				done <- err
			}()
			deadline := time.After(time.Second)
			for {
				gate.mu.Lock()
				queued, active := len(gate.waiters[ResourceBulk]), gate.active
				gate.mu.Unlock()
				if queued == 1 {
					if active != 1 || storage.active.Load() != 0 {
						t.Fatal("cold read queued while holding read capacity or transaction")
					}
					break
				}
				select {
				case err := <-done:
					t.Fatalf("cold read rejected instead of queued: %v", err)
				case <-deadline:
					t.Fatal("cold read never entered queue")
				default:
					time.Sleep(time.Millisecond)
				}
			}
			releaseWorker()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if storage.reads.Load() != 2 || storage.active.Load() != 0 {
				t.Fatalf("read was not reopened: reads=%d active=%d", storage.reads.Load(), storage.active.Load())
			}
			assertResourcesReleased(t, gate)
		})
	}
}

func TestColdGraphReadCancellationDoesNotRestart(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	service, fs := newFsckSvc(t)
	storage := &graphReadTransactions{FSStore: fs}
	service.meta = storage
	repo := hh(t.Name())
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	done := make(chan error, 1)
	go func() { _, err := service.QueryGraphState(ctx, repo, ""); done <- err }()
	waitResourceQueue(t, gate, ResourceBulk, 1)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if storage.reads.Load() != 1 || storage.active.Load() != 0 {
		t.Fatal("canceled read reopened or retained transaction")
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestColdGraphOuterBoundaryQueuesInWorkerOrder(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	_, releaseWorker, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseWorker()
	order := make(chan string, 3)
	finished := make(chan error, 3)
	worker := func(name string) {
		finished <- resourceWork(ctx, ResourceBulk, func(context.Context) error { order <- name; return nil })
	}
	go worker("earlier")
	waitResourceQueue(t, gate, ResourceBulk, 1)
	service, fs := newFsckSvc(t)
	storage := &graphReadTransactions{FSStore: fs}
	service.meta = storage
	go func() {
		finished <- WithGraphReadResources(ctx, func(bound context.Context) error {
			_, err := service.QueryGraphState(bound, hh(t.Name()), "")
			if err == nil {
				gate.mu.Lock()
				active, bulk := gate.active, gate.bulk
				gate.mu.Unlock()
				if active != 1 || bulk != 1 {
					return errors.New("outer response lost bulk admission")
				}
				order <- "graph"
			}
			return err
		})
	}()
	waitResourceQueue(t, gate, ResourceBulk, 2)
	go worker("later")
	waitResourceQueue(t, gate, ResourceBulk, 3)
	if storage.active.Load() != 0 {
		t.Fatal("waiting graph retained transaction")
	}
	releaseWorker()
	for _, expected := range []string{"earlier", "graph", "later"} {
		select {
		case actual := <-order:
			if actual != expected {
				t.Fatalf("FIFO order: got %s want %s", actual, expected)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("FIFO did not reach %s", expected)
		}
	}
	for range 3 {
		if err := <-finished; err != nil {
			t.Fatal(err)
		}
	}
	assertResourcesReleased(t, gate)
}

func TestGraphReadUnknownTransactionOwnershipCannotRestart(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	service, fs := newFsckSvc(t)
	storage := &graphReadTransactions{FSStore: fs}
	service.meta = struct {
		*store.FSStore
		outbound.RepositoryTransactions
	}{fs, storage}
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	calls := 0
	err = WithGraphReadResources(ctx, func(bound context.Context) error {
		calls++
		_, err := service.QueryGraphState(bound, hh(t.Name()), "")
		return err
	})
	if !errors.Is(err, domain.ErrResourceOverloaded) || calls != 1 || storage.reads.Load() != 1 {
		t.Fatalf("unknown transaction adapter retried: err=%v calls=%d reads=%d", err, calls, storage.reads.Load())
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestGraphBoundaryExpiredContextReacquiresAdmission(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	var previous context.Context
	if err := WithGraphReadResources(ctx, func(bound context.Context) error { previous = bound; return nil }); err != nil {
		t.Fatal(err)
	}
	assertResourcesReleased(t, gate)
	if err := WithGraphReadResources(previous, func(context.Context) error {
		gate.mu.Lock()
		defer gate.mu.Unlock()
		if gate.active != 1 {
			return errors.New("expired graph scope bypassed admission")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertResourcesReleased(t, gate)
}

func TestGraphBoundaryDoesNotRetryUnrelatedErrorsOrBorrowedPermit(t *testing.T) {
	ctx, gate := admissionTestContext(time.Second)
	for _, failure := range []error{domain.ErrResourceOverloaded, domain.ErrForbidden, context.DeadlineExceeded} {
		calls := 0
		err := WithGraphReadResources(ctx, func(context.Context) error { calls++; return failure })
		if !errors.Is(err, failure) || calls != 1 {
			t.Fatalf("unexpected retry: %v calls=%d", err, calls)
		}
		assertResourcesReleased(t, gate)
	}
	bound, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	calls := 0
	err = WithGraphReadResources(bound, func(context.Context) error { calls++; return errGraphReadNeedsBulk })
	if !errors.Is(err, errGraphReadNeedsBulk) || calls != 1 {
		t.Fatalf("borrowed permit retried: %v calls=%d", err, calls)
	}
	gate.mu.Lock()
	active := gate.active
	gate.mu.Unlock()
	if active != 1 {
		t.Fatal("nested boundary released caller permit")
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestGraphReadBorrowedTransactionNeverWaitsForInteractiveAdmission(t *testing.T) {
	for _, mode := range []string{"read", "write", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			ctx, gate := admissionTestContext(10 * time.Second)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			service, fs := newFsckSvc(t)
			storage := &graphReadTransactions{FSStore: fs}
			service.meta = storage
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
			go func() { _, err := service.GetRepositoryView(ctx, hh(t.Name())); done <- err }()
			select {
			case err := <-done:
				if !errors.Is(err, domain.ErrResourceOverloaded) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				cancel()
				<-done
				t.Fatal("borrowed transaction waited for interactive admission")
			}
			gate.mu.Lock()
			queued := len(gate.waiters[ResourceInteractive]) + len(gate.waiters[ResourceBulk])
			gate.mu.Unlock()
			if queued != 0 || storage.reads.Load() != 0 {
				t.Fatal("borrowed transaction entered the queue or read body")
			}
			release()
			second()
			assertResourcesReleased(t, gate)
		})
	}
}

func TestGraphBoundaryQueuedDeadlineAndTimeout(t *testing.T) {
	for _, deadline := range []bool{false, true} {
		t.Run(map[bool]string{false: "queue timeout", true: "request deadline"}[deadline], func(t *testing.T) {
			wait := 10 * time.Millisecond
			if deadline {
				wait = time.Second
			}
			ctx, gate := admissionTestContext(wait)
			if deadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
			}
			_, release, err := AcquireResources(ctx, ResourceBulk)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			calls := 0
			err = WithGraphReadResources(ctx, func(bound context.Context) error {
				calls++
				_, release, err := acquireGraphBuild(bound)
				if release != nil {
					release()
				}
				return err
			})
			expected := domain.ErrResourceOverloaded
			if deadline {
				expected = context.DeadlineExceeded
			}
			if !errors.Is(err, expected) || calls != 1 {
				t.Fatalf("wait limit changed: %v calls=%d", err, calls)
			}
			release()
			assertResourcesReleased(t, gate)
		})
	}
}
