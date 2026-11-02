package serverruntime

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app"
	"github.com/wnsdy95/cxthub/backend/internal/app/displaymaintenance"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type WorkerRuntime struct {
	Store          store.Store
	workers        *app.DocumentWorkers
	maintenance    *displaymaintenance.Worker
	additional     []workerBinding
	infrastructure *Infrastructure
	execution      *executionProfile
	mu             sync.Mutex
	running        bool
	closed         bool
	cancel         context.CancelFunc
	run            sync.WaitGroup
	closeOnce      sync.Once
	closeErr       error
}

type workerBinding struct {
	name string
	run  func(context.Context)
}

type WorkerSourceFactory func(store.Store) (ApplicationWorkerConfig, error)

func ValidateWorkerExecutionProfile() error {
	_, err := executionProfileFromEnvironment(outbound.ProcessWorker, true)
	return err
}

func OpenDocumentWorker(ctx context.Context) (_ *WorkerRuntime, err error) {
	return openWorker(ctx, true)
}

func OpenWorker(ctx context.Context, sources ...WorkerSourceFactory) (*WorkerRuntime, error) {
	return openWorker(ctx, false, sources...)
}

func openWorker(ctx context.Context, documentsOnly bool, sources ...WorkerSourceFactory) (_ *WorkerRuntime, err error) {
	execution, err := executionProfileFromEnvironment(outbound.ProcessWorker, true)
	if err != nil {
		return nil, err
	}
	complete := execution.budget.Profile == outbound.BoundedWorkersProfile
	if documentsOnly && complete {
		return nil, fmt.Errorf("%w: OpenDocumentWorker requires %s; use OpenWorker for %s", domain.ErrValidation, outbound.BoundedDocumentsProfile, outbound.BoundedWorkersProfile)
	}
	if complete {
		if _, err := executionDataDSN(os.Getenv("CXT_POSTGRES_DSN"), execution.role); err != nil {
			return nil, err
		}
		if len(sources) != 1 || sources[0] == nil {
			return nil, fmt.Errorf("%w: complete worker profile requires explicit provider sources", domain.ErrValidation)
		}
	} else if len(sources) != 0 {
		return nil, fmt.Errorf("%w: document-only worker cannot consume application sources", domain.ErrValidation)
	}
	infrastructure, err := OpenInfrastructure(ctx, InfrastructureConfig{
		DSN:                          os.Getenv("CXT_POSTGRES_DSN"),
		DataDir:                      os.Getenv("CXT_DATA"),
		RequirePostgres:              true,
		MigrationsDirectory:          os.Getenv("CXT_MIGRATIONS_DIR"),
		LegacyByteRangePublication:   EnvBool(os.Getenv("CXT_LEGACY_BYTE_RANGE_PUBLICATION")),
		LegacyDerivedBytePublication: EnvBool(os.Getenv("CXT_LEGACY_DERIVED_BYTE_PUBLICATION")),
		execution:                    execution,
	})
	if err != nil {
		return nil, err
	}
	worker := &WorkerRuntime{Store: infrastructure.Store, infrastructure: infrastructure, execution: execution}
	defer func() {
		if err != nil {
			_ = worker.Close()
		}
	}()
	fields, supportsFields := worker.Store.(outbound.DocFindFieldsStore)
	display, supportsDisplay := worker.Store.(outbound.DocDisplayStore)
	maintenance, supportsMaintenance := worker.Store.(outbound.DocDisplayMaintenanceStore)
	if !supportsFields || !supportsDisplay || !supportsMaintenance {
		return nil, fmt.Errorf("%w: document worker storage capabilities are required", domain.ErrCatalogUnsupported)
	}
	verifier := &domain.CanonicalDocVerifier{}
	worker.workers, err = app.NewDocumentWorkers(app.DocumentWorkersConfig{
		Metadata: worker.Store, Blobs: worker.Store, FindFields: fields, Display: display,
		Verifier: verifier, Profile: infrastructure.DocumentProfile,
	})
	if err != nil {
		return nil, err
	}
	worker.maintenance, err = app.NewDisplayMaintenanceWorker(maintenance)
	if err != nil {
		return nil, err
	}
	if complete {
		configuration, err := sources[0](worker.Store)
		if err != nil {
			return nil, err
		}
		worker.additional, err = newApplicationWorkers(worker.Store, configuration)
		if err != nil {
			return nil, err
		}
	}
	return worker, nil
}

func (worker *WorkerRuntime) bindings() []workerBinding {
	bindings := []workerBinding{
		{name: "document-finalization", run: worker.workers.Finalization.Run},
		{name: "document-range", run: worker.workers.Range.Run},
		{name: "document-find-fields", run: worker.workers.FindFields.Run},
		{name: "document-display", run: worker.workers.Display.Run},
		{name: "display-maintenance", run: worker.maintenance.Run},
	}
	return append(bindings, worker.additional...)
}

func (worker *WorkerRuntime) ProcessContext(ctx context.Context) context.Context {
	return worker.execution.processContext(ctx)
}

func (worker *WorkerRuntime) Ready() bool {
	return worker != nil && worker.execution != nil && worker.execution.ready()
}

func (worker *WorkerRuntime) Run(ctx context.Context) error {
	if worker == nil || ctx == nil || worker.workers == nil || worker.maintenance == nil {
		return domain.ErrValidation
	}
	worker.mu.Lock()
	if worker.running || worker.closed {
		worker.mu.Unlock()
		return domain.ErrConflict
	}
	current, cancel := context.WithCancel(worker.ProcessContext(ctx))
	worker.cancel = cancel
	worker.running = true
	worker.run.Add(1)
	worker.mu.Unlock()
	defer worker.run.Done()
	defer cancel()
	if len(worker.additional) != 0 {
		ready := time.NewTicker(25 * time.Millisecond)
		for !worker.Ready() && current.Err() == nil {
			select {
			case <-current.Done():
			case <-ready.C:
			}
		}
		ready.Stop()
	}
	var loops sync.WaitGroup
	bindings := worker.bindings()
	if current.Err() != nil {
		bindings = nil
	}
	for _, binding := range bindings {
		loops.Add(1)
		go func() {
			defer loops.Done()
			binding.run(current)
		}()
	}
	<-current.Done()
	loops.Wait()
	worker.mu.Lock()
	closed := worker.closed
	worker.mu.Unlock()
	if ctx.Err() != nil || closed {
		return nil
	}
	return worker.execution.admission.Err()
}

func (worker *WorkerRuntime) Close() error {
	if worker == nil {
		return nil
	}
	worker.closeOnce.Do(func() {
		worker.mu.Lock()
		worker.closed = true
		if worker.cancel != nil {
			worker.cancel()
		}
		worker.mu.Unlock()
		worker.run.Wait()
		worker.closeErr = worker.infrastructure.Close()
	})
	return worker.closeErr
}
