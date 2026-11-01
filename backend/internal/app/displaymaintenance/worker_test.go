package displaymaintenance_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/displaymaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type maintenanceStore struct {
	step func(context.Context) (domain.DocDisplayMaintenanceStep, error)
}

func (storage *maintenanceStore) StepDocDisplayMaintenance(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
	return storage.step(ctx)
}

type maintenanceScopeKey struct{}
type maintenanceCallerKey struct{}

type maintenanceScope struct {
	systemCalls  int
	acquisitions int
	releases     int
	active       bool
	failAt       int
	failure      error
}

func (scope *maintenanceScope) SystemContext(ctx context.Context) context.Context {
	scope.systemCalls++
	return inbound.WithSystemActor(ctx)
}

func (scope *maintenanceScope) AcquireBulk(ctx context.Context) (context.Context, func(), error) {
	scope.acquisitions++
	if err := ctx.Err(); err != nil {
		return ctx, nil, err
	}
	if scope.acquisitions == scope.failAt {
		return ctx, nil, scope.failure
	}
	scope.active = true
	return context.WithValue(ctx, maintenanceScopeKey{}, scope), func() {
		scope.active = false
		scope.releases++
	}, nil
}

func TestNewRequiresExplicitDependencies(test *testing.T) {
	storage := &maintenanceStore{}
	scope := &maintenanceScope{}
	var nilStorage *maintenanceStore
	var nilScope *maintenanceScope
	for _, fixture := range []struct {
		name    string
		storage outbound.DocDisplayMaintenanceStore
		scope   displaymaintenance.Scope
		missing string
	}{
		{name: "missing-storage", scope: scope, missing: "storage"},
		{name: "typed-nil-storage", storage: nilStorage, scope: scope, missing: "storage"},
		{name: "missing-scope", storage: storage, missing: "scope"},
		{name: "typed-nil-scope", storage: storage, scope: nilScope, missing: "scope"},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			worker, err := displaymaintenance.New(fixture.storage, fixture.scope)
			if worker != nil || !errors.Is(err, domain.ErrValidation) {
				test.Fatal("missing dependency was accepted", worker, err)
			}
			if !strings.Contains(err.Error(), fixture.missing) {
				test.Fatal("missing dependency was not identified", err)
			}
		})
	}
	worker, err := displaymaintenance.New(storage, scope)
	if err != nil || worker == nil || scope.systemCalls != 0 || scope.acquisitions != 0 {
		test.Fatal("construction performed maintenance work", worker, err, scope)
	}
	for _, absent := range []outbound.DocDisplayMaintenanceStore{nil, nilStorage} {
		if worker, err := app.NewDisplayMaintenanceWorker(absent); worker != nil || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("root factory accepted absent storage", worker, err)
		}
	}
}

func TestProcessReadOnlyPrecedesLimitAndLegacyNoop(test *testing.T) {
	scope := &maintenanceScope{}
	worker, err := displaymaintenance.New(&maintenanceStore{}, scope)
	if err != nil {
		test.Fatal(err)
	}
	service := app.NewService(nil, nil, nil, nil, nil)
	for _, limit := range []int{-1, 0, 1} {
		readonly := outbound.WithDocReadOnly(context.Background())
		if processed, err := worker.Process(readonly, limit); processed != 0 || err != domain.ErrForbidden {
			test.Fatal("owner read-only rejection did not precede limit", limit, processed, err)
		}
		if err := service.ProcessDocDisplayMaintenance(readonly, limit); err != domain.ErrForbidden {
			test.Fatal("unset legacy read-only rejection did not precede noop", limit, err)
		}
		if err := service.ProcessDocDisplayMaintenance(context.Background(), limit); err != nil {
			test.Fatal("unset legacy service did work", limit, err)
		}
		if limit <= 0 {
			if processed, err := worker.Process(context.Background(), limit); processed != 0 || err != nil {
				test.Fatal("nonpositive owner limit did work", limit, processed, err)
			}
		}
	}
	service.ConfigureDocumentDisplayMaintenance(&maintenanceStore{})
	for _, limit := range []int{-1, 0} {
		if err := service.ProcessDocDisplayMaintenance(context.Background(), limit); err != nil {
			test.Fatal("configured nonpositive legacy limit did work", limit, err)
		}
	}
	if scope.systemCalls != 0 || scope.acquisitions != 0 {
		test.Fatal("no-op or read-only boundary acquired a scope", scope)
	}
	service.ConfigureDocumentDisplayMaintenance(nil)
	service.RunDocDisplayMaintenanceWorker(context.Background())
}

func TestProcessOwnsBoundedStepsAndSystemScope(test *testing.T) {
	scope := &maintenanceScope{}
	calls := 0
	storage := &maintenanceStore{step: func(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
		calls++
		actor, system := inbound.RepositoryActor(ctx)
		if actor != "" || !system || ctx.Value(maintenanceCallerKey{}) != "caller" {
			test.Error("system scope lost caller context or retained user authority", actor, system)
		}
		if ctx.Value(maintenanceScopeKey{}) != scope || !scope.active || scope.releases != calls-1 {
			test.Error("step was outside its own admitted scope", calls, scope)
		}
		return domain.DocDisplayMaintenanceStep{}, nil
	}}
	worker, err := displaymaintenance.New(storage, scope)
	if err != nil {
		test.Fatal(err)
	}
	ctx := inbound.WithRepositoryActor(context.WithValue(context.Background(), maintenanceCallerKey{}, "caller"), "user")
	if processed, err := worker.Process(ctx, 3); processed != 3 || err != nil || calls != 3 {
		test.Fatal("owner did not enforce the step limit", processed, err, calls)
	}
	if scope.systemCalls != 1 || scope.acquisitions != 3 || scope.releases != 3 || scope.active {
		test.Fatal("step scope ownership changed", scope)
	}
	if actor, system := inbound.RepositoryActor(ctx); actor != "user" || system {
		test.Fatal("owner changed caller authority", actor, system)
	}
}

func TestProcessPreservesPartialCountsAndErrors(test *testing.T) {
	idle := fmt.Errorf("idle: %w", domain.ErrNotFound)
	failure := fmt.Errorf("failed: %w", domain.ErrIntegrity)
	for _, fixture := range []struct {
		name      string
		outcomes  []error
		processed int
		failure   error
	}{
		{name: "idle", outcomes: []error{idle}},
		{name: "partial-idle", outcomes: []error{nil, nil, idle}, processed: 2},
		{name: "failure", outcomes: []error{failure}, failure: failure},
		{name: "partial-failure", outcomes: []error{nil, nil, failure}, processed: 2, failure: failure},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			calls := 0
			scope := &maintenanceScope{}
			storage := &maintenanceStore{step: func(context.Context) (domain.DocDisplayMaintenanceStep, error) {
				outcome := fixture.outcomes[calls]
				calls++
				return domain.DocDisplayMaintenanceStep{}, outcome
			}}
			worker, err := displaymaintenance.New(storage, scope)
			if err != nil {
				test.Fatal(err)
			}
			processed, err := worker.Process(context.Background(), 8)
			if processed != fixture.processed || err != fixture.failure || calls != len(fixture.outcomes) {
				test.Fatal("partial result or original error changed", processed, err, calls)
			}
			if scope.acquisitions != calls || scope.releases != calls || scope.active {
				test.Fatal("idle or failed step leaked scope", scope)
			}
		})
	}
}

func TestProcessAdmissionFailureKeepsPartialCount(test *testing.T) {
	failure := fmt.Errorf("admission: %w", domain.ErrResourceOverloaded)
	scope := &maintenanceScope{failAt: 2, failure: failure}
	calls := 0
	storage := &maintenanceStore{step: func(context.Context) (domain.DocDisplayMaintenanceStep, error) {
		calls++
		return domain.DocDisplayMaintenanceStep{}, nil
	}}
	worker, err := displaymaintenance.New(storage, scope)
	if err != nil {
		test.Fatal(err)
	}
	if processed, err := worker.Process(context.Background(), 4); processed != 1 || err != failure || calls != 1 {
		test.Fatal("admission failure reached storage or changed partial result", processed, err, calls)
	}
	if scope.acquisitions != 2 || scope.releases != 1 || scope.active {
		test.Fatal("failed acquisition changed release ownership", scope)
	}
}

func TestProcessCancellationKeepsCompletedStepAndReleasesScope(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scope := &maintenanceScope{}
	calls := 0
	storage := &maintenanceStore{step: func(context.Context) (domain.DocDisplayMaintenanceStep, error) {
		calls++
		cancel()
		return domain.DocDisplayMaintenanceStep{}, nil
	}}
	worker, err := displaymaintenance.New(storage, scope)
	if err != nil {
		test.Fatal(err)
	}
	if processed, err := worker.Process(ctx, 4); processed != 1 || err != context.Canceled || calls != 1 {
		test.Fatal("cancellation lost completed work or entered another step", processed, err, calls)
	}
	if scope.acquisitions != 2 || scope.releases != 1 || scope.active {
		test.Fatal("cancellation leaked scope", scope)
	}
}

func TestProcessReleasesScopeOnPanic(test *testing.T) {
	scope := &maintenanceScope{}
	failure := errors.New("step panic")
	worker, err := displaymaintenance.New(&maintenanceStore{step: func(context.Context) (domain.DocDisplayMaintenanceStep, error) {
		panic(failure)
	}}, scope)
	if err != nil {
		test.Fatal(err)
	}
	func() {
		defer func() {
			if recovered := recover(); recovered != failure {
				test.Fatal("step panic changed", recovered)
			}
		}()
		_, _ = worker.Process(context.Background(), 1)
		test.Fatal("owner swallowed step panic")
	}()
	if scope.acquisitions != 1 || scope.releases != 1 || scope.active {
		test.Fatal("panic leaked scope", scope)
	}
}

func TestRootOwnerUsesSharedBulkAdmissionAndCancellation(test *testing.T) {
	_, releaseBusy, err := app.AcquireResources(context.Background(), app.ResourceBulk)
	if err != nil {
		test.Fatal(err)
	}
	defer releaseBusy()
	entered := make(chan struct{}, 1)
	storage := &maintenanceStore{step: func(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
		if _, system := inbound.RepositoryActor(ctx); !system {
			return domain.DocDisplayMaintenanceStep{}, domain.ErrForbidden
		}
		entered <- struct{}{}
		return domain.DocDisplayMaintenanceStep{}, nil
	}}
	worker, err := app.NewDisplayMaintenanceWorker(storage)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		processed int
		err       error
	}
	finished := make(chan result, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		processed, err := worker.Process(ctx, 1)
		finished <- result{processed: processed, err: err}
	}()
	<-started
	select {
	case <-entered:
		test.Fatal("maintenance bypassed existing global bulk permit")
	case outcome := <-finished:
		test.Fatal("busy maintenance returned before cancellation", outcome)
	case <-time.After(25 * time.Millisecond):
	}
	cancel()
	select {
	case outcome := <-finished:
		if outcome.processed != 0 || outcome.err != context.Canceled {
			test.Fatal("waiting owner did not preserve cancellation", outcome)
		}
	case <-time.After(time.Second):
		test.Fatal("waiting owner did not join after cancellation")
	}
	select {
	case <-entered:
		test.Fatal("canceled maintenance reached storage")
	default:
	}
	releaseBusy()
	if processed, err := worker.Process(context.Background(), 1); processed != 1 || err != nil {
		test.Fatal("productive owner could not reuse global admission", processed, err)
	}
	select {
	case <-entered:
	default:
		test.Fatal("productive owner did not enter actual storage port")
	}
	check, cancelCheck := context.WithTimeout(context.Background(), time.Second)
	defer cancelCheck()
	_, releaseCheck, err := app.AcquireResources(check, app.ResourceBulk)
	if err != nil {
		test.Fatal("owner leaked global bulk permit", err)
	}
	releaseCheck()
}

func TestRunProductiveStepsDoNotWait(test *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	storage := &maintenanceStore{step: func(context.Context) (domain.DocDisplayMaintenanceStep, error) {
		calls++
		if calls == 3 {
			cancel()
		}
		return domain.DocDisplayMaintenanceStep{}, nil
	}}
	scope := &maintenanceScope{}
	worker, err := displaymaintenance.New(storage, scope)
	if err != nil {
		test.Fatal(err)
	}
	finished := make(chan struct{})
	go func() {
		worker.Run(ctx)
		close(finished)
	}()
	select {
	case <-finished:
	case <-time.After(time.Second):
		test.Fatal("productive steps waited for the idle delay")
	}
	if calls != 3 || scope.releases != 3 || scope.active {
		test.Fatal("productive run changed step or release count", calls, scope)
	}
}

func TestRunIdleAndErrorsWaitAndLog(test *testing.T) {
	for _, fixture := range []struct {
		name    string
		failure error
		logs    int
	}{
		{name: "idle", failure: fmt.Errorf("idle: %w", domain.ErrNotFound)},
		{name: "error", failure: domain.ErrIntegrity, logs: 1},
	} {
		test.Run(fixture.name, func(test *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			steps := make(chan time.Time, 2)
			calls := 0
			storage := &maintenanceStore{step: func(context.Context) (domain.DocDisplayMaintenanceStep, error) {
				calls++
				steps <- time.Now()
				if calls == 2 {
					cancel()
				}
				return domain.DocDisplayMaintenanceStep{}, fixture.failure
			}}
			scope := &maintenanceScope{}
			worker, err := displaymaintenance.New(storage, scope)
			if err != nil {
				test.Fatal(err)
			}
			finished := make(chan struct{})
			go func() {
				worker.Run(ctx)
				close(finished)
			}()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				test.Fatal("idle or failed run did not retry and join")
			}
			if calls != 2 || scope.releases != 2 || scope.active {
				test.Fatal("idle or failed run leaked scope or stopped early", calls, scope)
			}
			first, second := <-steps, <-steps
			if second.Sub(first) < time.Second {
				test.Fatal("idle or error retry skipped one-second delay", second.Sub(first))
			}
			logged := strings.Contains(output.String(), "document display maintenance: "+fixture.failure.Error())
			if logged != (fixture.logs > 0) || strings.Count(output.String(), "document display maintenance:") != fixture.logs {
				test.Fatal("idle or canceled failure logging changed", output.String())
			}
		})
	}
}

func TestRunCancellationDuringStepAndBackoff(test *testing.T) {
	for _, duringStep := range []bool{false, true} {
		test.Run(fmt.Sprint(duringStep), func(test *testing.T) {
			var output bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&output)
			defer log.SetOutput(previous)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			entered := make(chan struct{})
			storage := &maintenanceStore{step: func(ctx context.Context) (domain.DocDisplayMaintenanceStep, error) {
				close(entered)
				if duringStep {
					<-ctx.Done()
					return domain.DocDisplayMaintenanceStep{}, ctx.Err()
				}
				return domain.DocDisplayMaintenanceStep{}, domain.ErrNotFound
			}}
			scope := &maintenanceScope{}
			worker, err := displaymaintenance.New(storage, scope)
			if err != nil {
				test.Fatal(err)
			}
			finished := make(chan struct{})
			go func() {
				worker.Run(ctx)
				close(finished)
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				test.Fatal("run did not enter step")
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(500 * time.Millisecond):
				test.Fatal("canceled run did not join before backoff elapsed")
			}
			if scope.acquisitions != 1 || scope.releases != 1 || scope.active || output.Len() != 0 {
				test.Fatal("canceled run leaked scope or logged cancellation", scope, output.String())
			}
		})
	}
	scope := &maintenanceScope{}
	worker, err := displaymaintenance.New(&maintenanceStore{}, scope)
	if err != nil {
		test.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	worker.Run(ctx)
	if scope.systemCalls != 0 || scope.acquisitions != 0 {
		test.Fatal("already canceled run entered maintenance", scope)
	}
}
