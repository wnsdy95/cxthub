package systemmaintenance_test

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app/systemmaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type runtimeStore struct {
	prune func(context.Context, time.Time) error
}

func (store *runtimeStore) PruneRuntimeState(ctx context.Context, now time.Time) error {
	return store.prune(ctx, now)
}

type storageStore struct {
	reconcile func(context.Context) (bool, error)
}

func (store *storageStore) ReconcileNextStorageUsage(ctx context.Context) (bool, error) {
	return store.reconcile(ctx)
}

type nilRuntimeFunction func(context.Context, time.Time) error

func (store nilRuntimeFunction) PruneRuntimeState(ctx context.Context, now time.Time) error {
	return store(ctx, now)
}

type nilStorageMap map[string]bool

func (store nilStorageMap) ReconcileNextStorageUsage(context.Context) (bool, error) {
	return store["worked"], nil
}

type callerKey struct{}

func requireRuntime(test *testing.T, store systemmaintenance.RuntimeStore) *systemmaintenance.Runtime {
	test.Helper()
	worker, err := systemmaintenance.NewRuntime(systemmaintenance.RuntimeConfig{Profile: systemmaintenance.Complete, Store: store})
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

func requireStorage(test *testing.T, store systemmaintenance.StorageStore) *systemmaintenance.Storage {
	test.Helper()
	worker, err := systemmaintenance.NewStorage(systemmaintenance.StorageConfig{Profile: systemmaintenance.Complete, Store: store})
	if err != nil {
		test.Fatal(err)
	}
	return worker
}

func TestConstructorsValidateProfilesAndStores(test *testing.T) {
	var absentRuntime *runtimeStore
	var absentStorage *storageStore
	var absentFunction nilRuntimeFunction
	var absentMap nilStorageMap
	for _, fixture := range []struct {
		name    string
		profile systemmaintenance.Profile
		runtime systemmaintenance.RuntimeStore
		storage systemmaintenance.StorageStore
		invalid bool
	}{
		{name: "compatibility-unsupported", profile: systemmaintenance.Compatibility},
		{name: "compatibility-supported", profile: systemmaintenance.Compatibility, runtime: &runtimeStore{}, storage: &storageStore{}},
		{name: "complete-supported", profile: systemmaintenance.Complete, runtime: &runtimeStore{}, storage: &storageStore{}},
		{name: "complete-missing", profile: systemmaintenance.Complete, invalid: true},
		{name: "complete-typed-nil", profile: systemmaintenance.Complete, runtime: absentRuntime, storage: absentStorage, invalid: true},
		{name: "complete-nil-function-map", profile: systemmaintenance.Complete, runtime: absentFunction, storage: absentMap, invalid: true},
		{name: "compatibility-typed-nil", profile: systemmaintenance.Compatibility, runtime: absentRuntime, storage: absentStorage, invalid: true},
		{name: "compatibility-nil-function-map", profile: systemmaintenance.Compatibility, runtime: absentFunction, storage: absentMap, invalid: true},
		{name: "empty-profile", runtime: &runtimeStore{}, storage: &storageStore{}, invalid: true},
		{name: "unknown-profile", profile: "unknown", runtime: &runtimeStore{}, storage: &storageStore{}, invalid: true},
		{name: "unknown-profile-unsupported", profile: "unknown", invalid: true},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			runtimeWorker, runtimeErr := systemmaintenance.NewRuntime(systemmaintenance.RuntimeConfig{Profile: fixture.profile, Store: fixture.runtime})
			storageWorker, storageErr := systemmaintenance.NewStorage(systemmaintenance.StorageConfig{Profile: fixture.profile, Store: fixture.storage})
			if fixture.invalid {
				if runtimeWorker != nil || !errors.Is(runtimeErr, domain.ErrValidation) || storageWorker != nil || !errors.Is(storageErr, domain.ErrValidation) {
					test.Fatal("invalid configuration was accepted", runtimeWorker, runtimeErr, storageWorker, storageErr)
				}
				return
			}
			if runtimeWorker == nil || runtimeErr != nil || storageWorker == nil || storageErr != nil {
				test.Fatal("supported configuration was rejected", runtimeWorker, runtimeErr, storageWorker, storageErr)
			}
		})
	}
}

func TestOwnersContainOnlyTheirNarrowStore(test *testing.T) {
	for _, fixture := range []struct {
		owner reflect.Type
		store reflect.Type
	}{
		{owner: reflect.TypeOf(systemmaintenance.Runtime{}), store: reflect.TypeOf((*systemmaintenance.RuntimeStore)(nil)).Elem()},
		{owner: reflect.TypeOf(systemmaintenance.Storage{}), store: reflect.TypeOf((*systemmaintenance.StorageStore)(nil)).Elem()},
	} {
		if fixture.owner.NumField() != 1 {
			test.Fatal("owner retained dependencies beyond its narrow store", fixture.owner)
		}
		field := fixture.owner.Field(0)
		if field.Name != "store" || field.Type != fixture.store || field.IsExported() || field.Anonymous || fixture.store.NumMethod() != 1 {
			test.Fatal("owner store boundary widened", fixture.owner, field)
		}
	}
	for _, config := range []reflect.Type{reflect.TypeOf(systemmaintenance.RuntimeConfig{}), reflect.TypeOf(systemmaintenance.StorageConfig{})} {
		if config.NumField() != 2 || config.Field(0).Name != "Profile" || config.Field(0).Type != reflect.TypeOf(systemmaintenance.Profile("")) || config.Field(1).Name != "Store" {
			test.Fatal("configuration acquired an additional dependency", config)
		}
	}
}

func TestConstructionDoesNotStartWorkOrGoroutines(test *testing.T) {
	_, sourcePath, _, found := runtime.Caller(0)
	if !found {
		test.Fatal("test source path unavailable")
	}
	source, err := parser.ParseFile(token.NewFileSet(), filepath.Join(filepath.Dir(sourcePath), "workers.go"), nil, 0)
	if err != nil {
		test.Fatal(err)
	}
	ast.Inspect(source, func(node ast.Node) bool {
		if _, asynchronous := node.(*ast.GoStmt); asynchronous {
			test.Error("maintenance owner starts a hidden goroutine")
		}
		return true
	})
	synctest.Test(test, func(test *testing.T) {
		calls := 0
		started := time.Now()
		requireRuntime(test, &runtimeStore{prune: func(context.Context, time.Time) error {
			calls++
			return nil
		}})
		requireStorage(test, &storageStore{reconcile: func(context.Context) (bool, error) {
			calls++
			return true, nil
		}})
		if time.Since(started) != 0 || calls != 0 {
			test.Fatal("construction performed work or waited", calls, time.Since(started))
		}
		synctest.Wait()
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if calls != 0 {
			test.Fatal("construction started a maintenance loop", calls)
		}
	})
}

func TestCompatibilityUnsupportedWorkersReturnImmediately(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		runtimeWorker, err := systemmaintenance.NewRuntime(systemmaintenance.RuntimeConfig{Profile: systemmaintenance.Compatibility})
		if err != nil {
			test.Fatal(err)
		}
		storageWorker, err := systemmaintenance.NewStorage(systemmaintenance.StorageConfig{Profile: systemmaintenance.Compatibility})
		if err != nil {
			test.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		started := time.Now()
		if err := runtimeWorker.Process(ctx); err != nil {
			test.Fatal(err)
		}
		if processed, err := storageWorker.Process(ctx); processed != 0 || err != nil {
			test.Fatal("unsupported storage performed work", processed, err)
		}
		runtimeDone := make(chan struct{})
		storageDone := make(chan struct{})
		go func() { runtimeWorker.Run(ctx); close(runtimeDone) }()
		go func() { storageWorker.Run(ctx); close(storageDone) }()
		synctest.Wait()
		select {
		case <-runtimeDone:
		default:
			test.Error("unsupported runtime did not return immediately")
		}
		select {
		case <-storageDone:
		default:
			test.Error("unsupported storage did not return immediately")
		}
		cancel()
		<-runtimeDone
		<-storageDone
		if err := runtimeWorker.Process(ctx); err != nil {
			test.Fatal("unsupported runtime changed cancellation no-op", err)
		}
		if processed, err := storageWorker.Process(ctx); processed != 0 || err != nil || time.Since(started) != 0 {
			test.Fatal("unsupported worker waited or changed cancellation no-op", processed, err, time.Since(started))
		}
	})
}

func TestProcessPreservesCallerContextAndAuthority(test *testing.T) {
	for _, fixture := range []struct {
		name string
		ctx  context.Context
	}{
		{name: "anonymous", ctx: context.Background()},
		{name: "user", ctx: inbound.WithRepositoryActor(context.Background(), "caller-user")},
		{name: "system", ctx: inbound.WithSystemActor(context.Background())},
		{name: "readonly-user", ctx: outbound.WithDocReadOnly(inbound.WithRepositoryActor(context.Background(), "readonly-user"))},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			ctx := context.WithValue(fixture.ctx, callerKey{}, "caller-value")
			actor, system := inbound.RepositoryActor(ctx)
			var runtimeCall, storageCall context.Context
			check := func(bound context.Context, timeout time.Duration) {
				test.Helper()
				actualActor, actualSystem := inbound.RepositoryActor(bound)
				deadline, exists := bound.Deadline()
				if bound == ctx || bound.Value(callerKey{}) != "caller-value" || actualActor != actor || actualSystem != system || outbound.DocReadOnly(bound) != outbound.DocReadOnly(ctx) || !exists || time.Until(deadline) <= 0 || time.Until(deadline) > timeout {
					test.Error("maintenance changed caller context, authority, or timeout", actualActor, actualSystem, deadline)
				}
			}
			runtimeWorker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, now time.Time) error {
				runtimeCall = bound
				check(bound, 10*time.Second)
				if now.Location() != time.UTC || now.After(time.Now()) || time.Since(now) > time.Second {
					test.Error("prune did not receive the current UTC time", now)
				}
				return nil
			}})
			storageWorker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) {
				storageCall = bound
				check(bound, 30*time.Second)
				return false, nil
			}})
			if err := runtimeWorker.Process(ctx); err != nil {
				test.Fatal(err)
			}
			if processed, err := storageWorker.Process(ctx); processed != 0 || err != nil {
				test.Fatal(processed, err)
			}
			if runtimeCall == nil || storageCall == nil || runtimeCall.Err() != context.Canceled || storageCall.Err() != context.Canceled || ctx.Err() != nil {
				test.Fatal("child contexts escaped or caller was canceled", runtimeCall, storageCall, ctx.Err())
			}
		})
	}
}

func TestRuntimeProcessPreservesStoreErrors(test *testing.T) {
	for _, failure := range []error{nil, errors.New("prune failed"), domain.ErrNotFound, domain.ErrForbidden, context.Canceled, context.DeadlineExceeded} {
		calls := 0
		var bound context.Context
		worker := requireRuntime(test, &runtimeStore{prune: func(ctx context.Context, _ time.Time) error {
			calls++
			bound = ctx
			return failure
		}})
		if err := worker.Process(context.Background()); err != failure || calls != 1 || bound == nil || bound.Err() != context.Canceled {
			test.Fatal("prune changed the original error or leaked its context", err, failure, calls, bound)
		}
	}
}

func TestStorageProcessPreservesBatchIdleAndPartialErrors(test *testing.T) {
	failure := errors.New("reconciliation failed")
	for _, fixture := range []struct {
		name        string
		productive  int
		failure     error
		workedError bool
	}{
		{name: "idle"},
		{name: "partial-idle", productive: 3},
		{name: "eight-productive", productive: 8},
		{name: "failure", failure: failure},
		{name: "partial-failure", productive: 3, failure: failure},
		{name: "worked-and-failed", productive: 3, failure: failure, workedError: true},
		{name: "not-found-is-error", productive: 2, failure: domain.ErrNotFound},
		{name: "forbidden-is-error", productive: 2, failure: domain.ErrForbidden},
		{name: "canceled-is-error", productive: 2, failure: context.Canceled},
		{name: "deadline-is-error", productive: 2, failure: context.DeadlineExceeded},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			calls := 0
			var contexts []context.Context
			worker := requireStorage(test, &storageStore{reconcile: func(ctx context.Context) (bool, error) {
				if len(contexts) != 0 && contexts[len(contexts)-1].Err() != context.Canceled {
					test.Error("previous call context was retained into the next step")
				}
				contexts = append(contexts, ctx)
				calls++
				if calls <= fixture.productive {
					return true, nil
				}
				if calls > fixture.productive+1 {
					test.Error("storage continued after idle or error")
				}
				return fixture.workedError, fixture.failure
			}})
			processed, err := worker.Process(context.Background())
			wantCalls := min(fixture.productive+1, 8)
			if processed != fixture.productive || err != fixture.failure || calls != wantCalls {
				test.Fatal("batch bound, partial progress, or original error changed", processed, err, calls)
			}
			for _, ctx := range contexts {
				if ctx.Err() != context.Canceled {
					test.Fatal("storage retained a completed call context", ctx.Err())
				}
			}
		})
	}
}

func TestProcessTimeoutsAndFreshStorageDeadlines(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		ctx := context.WithValue(context.Background(), callerKey{}, "caller-value")
		started := time.Now()
		runtimeWorker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, now time.Time) error {
			deadline, exists := bound.Deadline()
			if !exists || deadline.Sub(time.Now()) != 10*time.Second || !now.Equal(time.Now().UTC()) || now.Location() != time.UTC {
				test.Error("runtime timeout or UTC prune instant changed", deadline, now)
			}
			time.Sleep(10*time.Second - time.Nanosecond)
			if bound.Err() != nil {
				test.Error("runtime expired before ten seconds", bound.Err())
			}
			<-bound.Done()
			return bound.Err()
		}})
		if err := runtimeWorker.Process(ctx); err != context.DeadlineExceeded || time.Since(started) != 10*time.Second || ctx.Err() != nil {
			test.Fatal("runtime timeout changed or canceled the caller", err, time.Since(started), ctx.Err())
		}
		calls := 0
		var previous context.Context
		storageWorker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) {
			calls++
			if previous != nil && previous.Err() != context.Canceled {
				test.Error("previous productive call was not canceled")
			}
			previous = bound
			deadline, exists := bound.Deadline()
			if !exists || deadline.Sub(time.Now()) != 30*time.Second || bound.Value(callerKey{}) != "caller-value" {
				test.Error("storage did not receive a fresh thirty-second child context", calls, deadline)
			}
			if calls <= 2 {
				time.Sleep(20 * time.Second)
				return true, nil
			}
			time.Sleep(30*time.Second - time.Nanosecond)
			if bound.Err() != nil {
				test.Error("storage expired before thirty seconds", bound.Err())
			}
			<-bound.Done()
			return false, bound.Err()
		}})
		started = time.Now()
		if processed, err := storageWorker.Process(ctx); processed != 2 || err != context.DeadlineExceeded || calls != 3 || time.Since(started) != 70*time.Second || ctx.Err() != nil {
			test.Fatal("per-call timeout lost partial progress or canceled the caller", processed, err, calls, time.Since(started), ctx.Err())
		}
	})
}

func TestProcessHonorsEarlierCallerDeadline(test *testing.T) {
	for _, kind := range []string{"runtime", "storage"} {
		test.Run(kind, func(test *testing.T) {
			synctest.Test(test, func(test *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				deadline, _ := ctx.Deadline()
				check := func(bound context.Context) error {
					actual, exists := bound.Deadline()
					if !exists || !actual.Equal(deadline) {
						test.Error("maintenance extended the caller deadline", actual, deadline)
					}
					<-bound.Done()
					return bound.Err()
				}
				started := time.Now()
				var err error
				if kind == "runtime" {
					worker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, _ time.Time) error { return check(bound) }})
					err = worker.Process(ctx)
				} else {
					worker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) { return false, check(bound) }})
					var processed int
					processed, err = worker.Process(ctx)
					if processed != 0 {
						test.Error("expired work was counted", processed)
					}
				}
				if err != context.DeadlineExceeded || time.Since(started) != 3*time.Second {
					test.Fatal("earlier caller deadline was lost", err, time.Since(started))
				}
			})
		})
	}
}

func TestProcessPreservesCanceledCallsAndPartialProgress(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtimeCalls := 0
	runtimeWorker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, _ time.Time) error {
		runtimeCalls++
		return bound.Err()
	}})
	if err := runtimeWorker.Process(ctx); err != context.Canceled || runtimeCalls != 1 {
		test.Fatal("runtime changed the existing canceled store call", err, runtimeCalls)
	}
	storageCalls := 0
	storageWorker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) {
		storageCalls++
		return false, bound.Err()
	}})
	if processed, err := storageWorker.Process(ctx); processed != 0 || err != context.Canceled || storageCalls != 1 {
		test.Fatal("storage changed the existing canceled store call", processed, err, storageCalls)
	}
	partial, cancelPartial := context.WithCancel(context.Background())
	defer cancelPartial()
	partialCalls := 0
	partialWorker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) {
		partialCalls++
		if partialCalls == 1 {
			cancelPartial()
			return true, nil
		}
		return false, bound.Err()
	}})
	if processed, err := partialWorker.Process(partial); processed != 1 || err != context.Canceled || partialCalls != 2 {
		test.Fatal("cancellation changed completed progress or the next canceled store call", processed, err, partialCalls)
	}
}

func TestRunWaitsExactlyOneMinuteAndUsesOriginalTickBatches(test *testing.T) {
	for _, kind := range []string{"runtime", "storage"} {
		for _, outcome := range []string{"productive", "idle", "error"} {
			test.Run(kind+"-"+outcome, func(test *testing.T) {
				synctest.Test(test, func(test *testing.T) {
					ctx, cancel := context.WithCancel(inbound.WithRepositoryActor(context.WithValue(context.Background(), callerKey{}, "caller-value"), "caller-user"))
					defer cancel()
					var calls atomic.Int64
					var contextsMutex sync.Mutex
					var contexts []context.Context
					started := time.Now()
					check := func(bound context.Context) error {
						calls.Add(1)
						contextsMutex.Lock()
						contexts = append(contexts, bound)
						contextsMutex.Unlock()
						actor, system := inbound.RepositoryActor(bound)
						if bound.Value(callerKey{}) != "caller-value" || actor != "caller-user" || system || bound.Err() != nil {
							test.Error("Run changed caller context or actor", actor, system, bound.Err())
						}
						if outcome == "error" {
							return errors.New("tick failed")
						}
						return nil
					}
					var run func(context.Context)
					if kind == "runtime" {
						worker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, now time.Time) error {
							if !now.Equal(time.Now().UTC()) || now.Location() != time.UTC {
								test.Error("Run changed the prune timestamp", now)
							}
							return check(bound)
						}})
						run = worker.Run
					} else {
						worker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) {
							return outcome == "productive", check(bound)
						}})
						run = worker.Run
					}
					done := make(chan struct{})
					go func() { run(ctx); close(done) }()
					synctest.Wait()
					if currentCalls := calls.Load(); currentCalls != 0 {
						test.Fatal("Run processed immediately instead of waiting one minute", currentCalls)
					}
					perTick := int64(1)
					if kind == "storage" && outcome == "productive" {
						perTick = 8
					}
					for tick := 1; tick <= 2; tick++ {
						time.Sleep(time.Minute - time.Nanosecond)
						synctest.Wait()
						if currentCalls := calls.Load(); currentCalls != int64(tick-1)*perTick {
							test.Fatal("Run processed before its minute tick", tick, currentCalls, time.Since(started))
						}
						time.Sleep(time.Nanosecond)
						synctest.Wait()
						if currentCalls := calls.Load(); currentCalls != int64(tick)*perTick || time.Since(started) != time.Duration(tick)*time.Minute {
							test.Fatal("Run changed its tick cadence, batch size, or idle/error break", tick, currentCalls, time.Since(started))
						}
						contextsMutex.Lock()
						observedContexts := append([]context.Context(nil), contexts...)
						contextsMutex.Unlock()
						for _, bound := range observedContexts {
							if bound.Err() != context.Canceled {
								test.Fatal("Run retained a finished call context", bound.Err())
							}
						}
					}
					cancel()
					<-done
					time.Sleep(2 * time.Minute)
					synctest.Wait()
					if currentCalls := calls.Load(); currentCalls != 2*perTick {
						test.Fatal("joined Run continued maintenance", currentCalls)
					}
				})
			})
		}
	}
}

func TestRunCancellationBeforeFirstTickDoesNoWork(test *testing.T) {
	synctest.Test(test, func(test *testing.T) {
		calls := 0
		runtimeWorker := requireRuntime(test, &runtimeStore{prune: func(context.Context, time.Time) error {
			calls++
			return nil
		}})
		storageWorker := requireStorage(test, &storageStore{reconcile: func(context.Context) (bool, error) {
			calls++
			return true, nil
		}})
		for _, alreadyCanceled := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			if alreadyCanceled {
				cancel()
			}
			runtimeDone := make(chan struct{})
			storageDone := make(chan struct{})
			go func() { runtimeWorker.Run(ctx); close(runtimeDone) }()
			go func() { storageWorker.Run(ctx); close(storageDone) }()
			synctest.Wait()
			if !alreadyCanceled {
				time.Sleep(time.Minute - time.Nanosecond)
			}
			cancel()
			<-runtimeDone
			<-storageDone
		}
		time.Sleep(2 * time.Minute)
		synctest.Wait()
		if calls != 0 {
			test.Fatal("canceled Run reached storage or survived join", calls)
		}
	})
}

type lockedLogBuffer struct {
	mutex  sync.Mutex
	buffer bytes.Buffer
}

func (logs *lockedLogBuffer) Write(contents []byte) (int, error) {
	logs.mutex.Lock()
	defer logs.mutex.Unlock()
	return logs.buffer.Write(contents)
}

func (logs *lockedLogBuffer) Len() int {
	logs.mutex.Lock()
	defer logs.mutex.Unlock()
	return logs.buffer.Len()
}

func (logs *lockedLogBuffer) String() string {
	logs.mutex.Lock()
	defer logs.mutex.Unlock()
	return logs.buffer.String()
}

func captureLogs(test *testing.T) *lockedLogBuffer {
	test.Helper()
	output, flags, prefix := log.Writer(), log.Flags(), log.Prefix()
	buffer := &lockedLogBuffer{}
	log.SetOutput(buffer)
	log.SetFlags(0)
	log.SetPrefix("")
	test.Cleanup(func() {
		log.SetOutput(output)
		log.SetFlags(flags)
		log.SetPrefix(prefix)
	})
	return buffer
}

func TestRunPreservesOriginalLogReasonsAndCancellationDifference(test *testing.T) {
	for _, kind := range []string{"runtime", "storage"} {
		for _, canceled := range []bool{false, true} {
			name := kind + "-failure"
			if canceled {
				name = kind + "-canceled"
			}
			test.Run(name, func(test *testing.T) {
				logs := captureLogs(test)
				synctest.Test(test, func(test *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					entered := make(chan struct{})
					release := make(chan struct{})
					calls := 0
					failure := errors.New("store failed")
					check := func(bound context.Context) error {
						calls++
						close(entered)
						if canceled {
							<-bound.Done()
							<-release
							return bound.Err()
						}
						return failure
					}
					var run func(context.Context)
					reason := "runtime state cleanup: "
					if kind == "runtime" {
						worker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, _ time.Time) error { return check(bound) }})
						run = worker.Run
					} else {
						worker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) { return false, check(bound) }})
						run = worker.Run
						reason = "storage reconciliation failed: "
					}
					done := make(chan struct{})
					go func() { run(ctx); close(done) }()
					synctest.Wait()
					time.Sleep(time.Minute)
					<-entered
					synctest.Wait()
					cancel()
					if canceled {
						synctest.Wait()
						select {
						case <-done:
							test.Error("Run returned before its in-flight store call joined")
						default:
						}
						close(release)
					}
					<-done
					want := reason + failure.Error() + "\n"
					if canceled {
						want = reason + context.Canceled.Error() + "\n"
						if kind == "runtime" {
							want = ""
						}
					}
					if logs.String() != want || calls != 1 {
						test.Fatal("Run changed original logging or repeated work after cancellation", logs.String(), want, calls)
					}
				})
			})
		}
	}
}

func TestRunLogsTimeoutsAndWaitsForNextTicker(test *testing.T) {
	for _, kind := range []string{"runtime", "storage"} {
		test.Run(kind, func(test *testing.T) {
			logs := captureLogs(test)
			synctest.Test(test, func(test *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var calls atomic.Int64
				check := func(bound context.Context) error {
					calls.Add(1)
					<-bound.Done()
					return bound.Err()
				}
				var run func(context.Context)
				timeout := 10 * time.Second
				reason := "runtime state cleanup: "
				if kind == "runtime" {
					worker := requireRuntime(test, &runtimeStore{prune: func(bound context.Context, _ time.Time) error { return check(bound) }})
					run = worker.Run
				} else {
					worker := requireStorage(test, &storageStore{reconcile: func(bound context.Context) (bool, error) { return false, check(bound) }})
					run = worker.Run
					timeout = 30 * time.Second
					reason = "storage reconciliation failed: "
				}
				done := make(chan struct{})
				go func() { run(ctx); close(done) }()
				synctest.Wait()
				time.Sleep(time.Minute + timeout - time.Nanosecond)
				synctest.Wait()
				if currentCalls := calls.Load(); currentCalls != 1 || logs.Len() != 0 {
					test.Fatal("Run timed out early", currentCalls, logs.String())
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				want := reason + context.DeadlineExceeded.Error() + "\n"
				if currentCalls := calls.Load(); currentCalls != 1 || logs.String() != want || ctx.Err() != nil {
					test.Fatal("Run lost timeout logging or canceled its caller", currentCalls, logs.String(), ctx.Err())
				}
				time.Sleep(time.Minute - timeout - time.Nanosecond)
				synctest.Wait()
				if currentCalls := calls.Load(); currentCalls != 1 {
					test.Fatal("Run retried before the original minute ticker", currentCalls)
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if currentCalls := calls.Load(); currentCalls != 2 || strings.Count(logs.String(), want) != 1 {
					test.Fatal("Run did not retry at the next minute tick", currentCalls, logs.String())
				}
				cancel()
				<-done
			})
		})
	}
}
