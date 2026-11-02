package app

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/projectionworker"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave40WorkerDependencies struct {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.HistoryStore
	outbound.PendingProjectionStore
	outbound.RepositoryViewSourceStore
	outbound.GraphProjectionStore
	outbound.RepositoryGraphSourceStore
	outbound.GraphSourceReaderFactory
	outbound.GraphProjectionReferenceStore
	outbound.RepositoryGraphSourceGenerationStore
	outbound.GraphQualificationStore
	outbound.GraphProjectionRepairStore
	outbound.RepositoryTransactions
	claim         func(context.Context, time.Duration) error
	claimedRepos  []domain.ContentHash
	returnedLease domain.GraphQualificationLease
}

type wave40QualificationCapabilities struct {
	outbound.MetadataStore
	outbound.HistoryStore
	outbound.GraphProjectionStore
	outbound.RepositoryGraphSourceStore
	outbound.GraphSourceReaderFactory
	outbound.GraphProjectionReferenceStore
	outbound.RepositoryGraphSourceGenerationStore
	outbound.GraphQualificationStore
	outbound.GraphProjectionRepairStore
}

func (*wave40WorkerDependencies) InTransaction(context.Context) bool { return false }

func (*wave40WorkerDependencies) InReadOnlyTransaction(context.Context) bool { return false }

func (*wave40WorkerDependencies) Reference(context.Context, domain.ContentHash) (domain.ContentHash, error) {
	return "", domain.ErrNotFound
}

func (dependencies *wave40WorkerDependencies) ClaimPendingProjection(ctx context.Context, duration time.Duration) (domain.PendingProjectionLease, error) {
	return domain.PendingProjectionLease{}, dependencies.claim(ctx, duration)
}

func (dependencies *wave40WorkerDependencies) ClaimGraphProjection(ctx context.Context, duration time.Duration) (domain.GraphProjectionLease, error) {
	return domain.GraphProjectionLease{}, dependencies.claim(ctx, duration)
}

func (dependencies *wave40WorkerDependencies) ClaimGraphQualification(ctx context.Context, repo domain.ContentHash, duration time.Duration) (domain.GraphQualificationLease, error) {
	dependencies.claimedRepos = append(dependencies.claimedRepos, repo)
	return dependencies.returnedLease, dependencies.claim(ctx, duration)
}

func TestWave40ProjectionWorkersFactoryProfilesAndRequiredAdapters(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	for _, profile := range []projectionworker.Profile{projectionworker.Compatibility, projectionworker.Complete} {
		test.Run(string(profile), func(test *testing.T) {
			config := ProjectionWorkersConfig{Metadata: storage, Blobs: storage, Profile: profile}
			workers, err := NewProjectionWorkers(config)
			if err != nil || workers == nil {
				test.Fatal("disabled FS startup failed", err)
			}
			if profile == projectionworker.Complete {
				if workers.Pending != nil || workers.Graph != nil || workers.Qualification != nil {
					test.Fatal("complete profile fabricated unsupported FS owners")
				}
			} else {
				if workers.Pending == nil || workers.Graph == nil || workers.Qualification == nil {
					test.Fatal("compatibility lost disabled owner no-ops")
				}
				for _, process := range []func(context.Context, int) error{workers.Pending.Process, workers.Graph.Process, workers.Qualification.Process} {
					if err := process(context.Background(), 1); err != nil {
						test.Fatal("unsupported compatibility processing was not a no-op", err)
					}
				}
				workers.Pending.Run(context.Background())
				workers.Graph.Run(context.Background())
				workers.Qualification.Run(context.Background())
			}
			for _, typed := range []bool{false, true} {
				for _, field := range []string{"Metadata", "Blobs"} {
					invalid := config
					value := reflect.ValueOf(&invalid).Elem().FieldByName(field)
					value.Set(reflect.Zero(value.Type()))
					if typed {
						value.Set(reflect.ValueOf((*store.FSStore)(nil)))
					}
					if result, err := NewProjectionWorkers(invalid); err == nil || result != nil {
						test.Fatal("missing or typed-nil adapter accepted", field, typed, err)
					}
				}
			}
			invalid := config
			invalid.Profile = "unrecognized"
			if workers, err := NewProjectionWorkers(invalid); !errors.Is(err, domain.ErrValidation) || workers != nil {
				test.Fatal("unknown profile accepted", err)
			}
		})
	}
	for _, enabled := range []string{"pending", "graph", "qualification"} {
		config := ProjectionWorkersConfig{Metadata: storage, Blobs: storage, Profile: projectionworker.Complete}
		switch enabled {
		case "pending":
			config.PendingEnabled = true
		case "graph":
			config.GraphEnabled = true
		case "qualification":
			config.QualificationRepositories = []domain.ContentHash{hh(test.Name())}
		}
		if workers, err := NewProjectionWorkers(config); err == nil || workers != nil {
			test.Fatal("active complete profile accepted unsupported FS capability", enabled, err)
		}
	}
}

func TestWave40NativeProjectionConstructorsRejectMissingAndTypedNilCapabilities(test *testing.T) {
	dependencies := &wave40WorkerDependencies{}
	scope := newProjectionWorkerScope(dependencies, dependencies)
	pending := projectionworker.PendingConfig{Profile: projectionworker.Complete, Scope: scope, Jobs: dependencies, Source: dependencies, Enabled: true}
	graph := projectionworker.GraphConfig{Profile: projectionworker.Complete, Scope: scope, Jobs: dependencies, Source: dependencies,
		Factory: dependencies, References: dependencies, Generations: dependencies, Reference: dependencies, Enabled: true}
	qualification := projectionworker.QualificationConfig{Profile: projectionworker.Complete, Scope: scope, Jobs: dependencies,
		Projections: dependencies, Repairs: dependencies, Generations: dependencies, Reference: dependencies,
		Repositories: []domain.ContentHash{hh(test.Name())}, Next: &atomic.Uint64{}}
	for _, scenario := range []struct {
		name   string
		config any
		fields []string
		create func(any) error
	}{
		{name: "pending", config: pending, fields: []string{"Scope", "Jobs", "Source"}, create: func(config any) error {
			worker, err := projectionworker.NewPending(config.(projectionworker.PendingConfig))
			if err == nil && worker == nil {
				test.Fatal("successful pending constructor returned nil")
			}
			return err
		}},
		{name: "graph", config: graph, fields: []string{"Scope", "Jobs", "Source", "Factory", "Generations", "Reference"}, create: func(config any) error {
			worker, err := projectionworker.NewGraph(config.(projectionworker.GraphConfig))
			if err == nil && worker == nil {
				test.Fatal("successful graph constructor returned nil")
			}
			return err
		}},
		{name: "qualification", config: qualification, fields: []string{"Scope", "Jobs", "Projections", "Repairs", "Generations", "Reference"}, create: func(config any) error {
			worker, err := projectionworker.NewQualification(config.(projectionworker.QualificationConfig))
			if err == nil && worker == nil {
				test.Fatal("successful qualification constructor returned nil")
			}
			return err
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if err := scenario.create(scenario.config); err != nil {
				test.Fatal("complete capability set rejected", err)
			}
			for _, profile := range []projectionworker.Profile{projectionworker.Compatibility, "invalid"} {
				config := reflect.New(reflect.TypeOf(scenario.config)).Elem()
				config.Set(reflect.ValueOf(scenario.config))
				config.FieldByName("Profile").SetString(string(profile))
				err := scenario.create(config.Interface())
				if profile == projectionworker.Compatibility && err != nil || profile == "invalid" && err == nil {
					test.Fatal("native profile contract changed", profile, err)
				}
			}
			for _, field := range scenario.fields {
				for _, typed := range []bool{false, true} {
					config := reflect.New(reflect.TypeOf(scenario.config)).Elem()
					config.Set(reflect.ValueOf(scenario.config))
					value := config.FieldByName(field)
					value.Set(reflect.Zero(value.Type()))
					if typed {
						missing := any((*wave40WorkerDependencies)(nil))
						if field == "Scope" {
							missing = (*projectionWorkerScope)(nil)
						}
						value.Set(reflect.ValueOf(missing))
					}
					if err := scenario.create(config.Interface()); err == nil {
						test.Fatal("complete native constructor accepted absent capability", field, typed)
					}
				}
			}
		})
	}
	graph.References = (*wave40WorkerDependencies)(nil)
	if _, err := projectionworker.NewGraph(graph); err == nil {
		test.Fatal("complete graph accepted a typed-nil optional reference store")
	}
	graph.Profile = projectionworker.Compatibility
	graph.References, graph.Generations, graph.Reference = nil, nil, nil
	if worker, err := projectionworker.NewGraph(graph); err != nil || worker == nil {
		test.Fatal("compatibility now requires complete first-page reference ports", err)
	}
}

func TestWave40ProjectionWorkersCompleteSelectionRequiresFullTransactionCapabilities(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	dependencies := &wave40WorkerDependencies{}
	metadata := &wave40QualificationCapabilities{storage, storage, dependencies, dependencies, dependencies, dependencies, dependencies, dependencies, dependencies}
	for _, incomplete := range []outbound.MetadataStore{
		metadata,
		struct {
			*wave40QualificationCapabilities
			outbound.RepositoryTransactions
		}{metadata, dependencies},
		struct {
			*wave40QualificationCapabilities
			outbound.RepositoryTransactions
			outbound.RepositoryTransactionPresence
		}{metadata, dependencies, dependencies},
		struct {
			*wave40QualificationCapabilities
			outbound.RepositoryTransactions
			outbound.RepositoryTransactionState
		}{metadata, dependencies, dependencies},
	} {
		for _, kind := range []string{"graph", "qualification"} {
			config := ProjectionWorkersConfig{Metadata: incomplete, Blobs: storage, Profile: projectionworker.Complete}
			if kind == "graph" {
				config.GraphEnabled = true
			} else {
				config.QualificationRepositories = []domain.ContentHash{hh(test.Name())}
			}
			workers, err := NewProjectionWorkers(config)
			if !errors.Is(err, domain.ErrCatalogUnsupported) || workers != nil {
				test.Fatal("active complete projection accepted incomplete transaction ownership", kind, err)
			}
		}
	}
	complete := struct {
		*wave40QualificationCapabilities
		outbound.RepositoryTransactions
		outbound.RepositoryTransactionPresence
		outbound.RepositoryTransactionState
	}{metadata, dependencies, dependencies, dependencies}
	workers, err := NewProjectionWorkers(ProjectionWorkersConfig{Metadata: complete, Blobs: storage,
		Profile: projectionworker.Complete, GraphEnabled: true, QualificationRepositories: []domain.ContentHash{hh(test.Name())}})
	if err != nil || workers.Graph == nil || workers.Qualification == nil {
		test.Fatal("full transaction capability control was rejected", err)
	}
}

func TestWave40ProjectionWorkersCompleteReferenceRequiresInputBudget(test *testing.T) {
	storage := store.NewFSStore(test.TempDir())
	dependencies := &wave40WorkerDependencies{}
	metadata := struct {
		outbound.MetadataStore
		outbound.HistoryStore
		outbound.GraphProjectionStore
		outbound.RepositoryGraphSourceStore
		outbound.GraphSourceReaderFactory
		outbound.GraphProjectionReferenceStore
		outbound.RepositoryGraphSourceGenerationStore
		outbound.RepositoryTransactions
		outbound.RepositoryTransactionPresence
		outbound.RepositoryTransactionState
	}{storage, storage, dependencies, dependencies, dependencies, dependencies, dependencies, dependencies, dependencies, dependencies}
	config := ProjectionWorkersConfig{Metadata: metadata, Blobs: storage, Profile: projectionworker.Complete, GraphEnabled: true}
	if workers, err := NewProjectionWorkers(config); !errors.Is(err, domain.ErrCatalogUnsupported) || workers != nil {
		test.Fatal("active complete reference accepted missing input budget", err)
	}
	config.Profile = projectionworker.Compatibility
	if workers, err := NewProjectionWorkers(config); err != nil || workers.Graph == nil {
		test.Fatal("compatibility lost its lazy reference-capability behavior", err)
	}
}

func TestWave40ProjectionWorkersCompleteReferenceRequiresHistory(test *testing.T) {
	type withoutHistory interface {
		outbound.MetadataStore
		outbound.GraphProjectionStore
		outbound.RepositoryGraphSourceStore
		outbound.GraphSourceReaderFactory
		outbound.GraphProjectionReferenceStore
		outbound.RepositoryGraphSourceGenerationStore
		outbound.GraphQualificationStore
		outbound.GraphProjectionRepairStore
		outbound.RepositoryTransactions
		outbound.RepositoryTransactionPresence
		outbound.RepositoryTransactionState
	}
	storage := store.NewFSStore(test.TempDir())
	dependencies := &wave40WorkerDependencies{}
	metadata := struct{ withoutHistory }{dependencies}
	for _, kind := range []string{"graph", "qualification"} {
		config := ProjectionWorkersConfig{Metadata: metadata, Blobs: storage, Profile: projectionworker.Complete}
		if kind == "graph" {
			config.GraphEnabled = true
		} else {
			config.QualificationRepositories = []domain.ContentHash{hh(test.Name())}
		}
		if workers, err := NewProjectionWorkers(config); !errors.Is(err, domain.ErrCatalogUnsupported) || workers != nil {
			test.Fatal("complete reference accepted missing history", kind, err)
		}
		if kind == "qualification" {
			worker, err := projectionQualification(config, newProjectionWorkerScope(metadata, storage))
			if !errors.Is(err, domain.ErrCatalogUnsupported) || worker != nil {
				test.Fatal("qualification reference independently accepted missing history", err)
			}
		}
		config.Profile = projectionworker.Compatibility
		if workers, err := NewProjectionWorkers(config); err != nil || workers.Graph == nil || workers.Qualification == nil {
			test.Fatal("compatibility lost lazy history-capability behavior", err)
		}
	}
}

func TestWave40ProjectionWorkersRejectForeignQualificationClaimBeforeRead(test *testing.T) {
	repo, foreign := hh(test.Name()), hh("wave40 foreign claim")
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: domain.RepositoryViewSourceVersion,
		Scope: domain.RepositoryGraphSourceScope, RepoID: foreign, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 7}
	dependencies := &wave40WorkerDependencies{claim: func(context.Context, time.Duration) error { return nil },
		returnedLease: domain.GraphQualificationLease{GraphQualificationKey: domain.GraphQualificationKey{
			RepoID: foreign, Dataset: "22222222-2222-4222-8222-222222222222", Applied: checkpoint,
			Rules: domain.GraphViewProjectionRules, ComparisonRules: domain.GraphQualificationRules},
			Fence: 3, LeaseUntil: time.Now().Add(time.Minute)}}
	workers, err := NewProjectionWorkers(ProjectionWorkersConfig{Metadata: dependencies, Blobs: dependencies,
		Profile: projectionworker.Complete, QualificationRepositories: []domain.ContentHash{repo}})
	if err != nil {
		test.Fatal(err)
	}
	ctx, gate := admissionTestContext(time.Second)
	if err := workers.Qualification.Process(ctx, 1); !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(dependencies.claimedRepos, []domain.ContentHash{repo}) {
		test.Fatal("foreign claim reached comparison, repair, or completion", err, dependencies.claimedRepos)
	}
	assertResourcesReleased(test, gate)
}

func TestWave40ProjectionWorkersCopyStartupSelectionAndShareQualificationCursor(test *testing.T) {
	dependencies := &wave40WorkerDependencies{claim: func(context.Context, time.Duration) error { return domain.ErrForbidden }}
	repositories := []domain.ContentHash{hh("wave40 second"), hh("wave40 first")}
	if repositories[0] > repositories[1] {
		repositories[0], repositories[1] = repositories[1], repositories[0]
	}
	selection := append([]domain.ContentHash(nil), repositories...)
	var next atomic.Uint64
	next.Store(5)
	config := ProjectionWorkersConfig{Metadata: dependencies, Blobs: dependencies, Profile: projectionworker.Complete,
		QualificationRepositories: selection, QualificationNext: &next}
	first, err := NewProjectionWorkers(config)
	if err != nil {
		test.Fatal(err)
	}
	second, err := NewProjectionWorkers(config)
	if err != nil || first.Pending == nil || first.Graph == nil || first.Qualification == nil || second.Qualification == nil {
		test.Fatal("public factory lost native owners", err)
	}
	if next.Load() != 5 || len(dependencies.claimedRepos) != 0 {
		test.Fatal("assembly reset cursor or started work")
	}
	for index := range selection {
		selection[index] = hh("mutated caller selection")
	}
	ctx, gate := admissionTestContext(time.Second)
	for _, worker := range []*projectionworker.Qualification{first.Qualification, second.Qualification, first.Qualification} {
		if err := worker.Process(ctx, 1); !errors.Is(err, domain.ErrForbidden) {
			test.Fatal("claim failure was swallowed", err)
		}
		assertResourcesReleased(test, gate)
	}
	if next.Load() != 8 || !reflect.DeepEqual(dependencies.claimedRepos, []domain.ContentHash{repositories[1], repositories[0], repositories[1]}) {
		test.Fatal("owners reset, copied, or failed to rotate the shared cursor", next.Load(), dependencies.claimedRepos)
	}
}

func TestWave40ProjectionOwnersUseSameGateAndOriginalClaimBudgets(test *testing.T) {
	for _, owner := range []string{"pending", "graph", "qualification"} {
		for _, queued := range []time.Duration{0, 20 * time.Second} {
			test.Run(owner+"/"+queued.String(), func(test *testing.T) {
				synctest.Test(test, func(test *testing.T) {
					ctx, gate := admissionTestContext(time.Minute)
					budget := 30 * time.Second
					if owner == "qualification" {
						budget = 5 * time.Second
					}
					entered := make(chan context.Context, 1)
					dependencies := &wave40WorkerDependencies{claim: func(claim context.Context, duration time.Duration) error {
						deadline, bounded := claim.Deadline()
						permit, present := claim.Value(resourcePermitKey{}).(*resourcePermit)
						if !bounded || time.Until(deadline) != budget || duration != time.Minute || !present || permit.owner != gate || !permit.live.Load() {
							return errors.New("claim changed original budget, lease, or admission owner")
						}
						entered <- claim
						<-claim.Done()
						return claim.Err()
					}}
					workers, err := NewProjectionWorkers(ProjectionWorkersConfig{Metadata: dependencies, Blobs: dependencies,
						Profile: projectionworker.Complete, PendingEnabled: true, GraphEnabled: true,
						QualificationRepositories: []domain.ContentHash{hh(test.Name())}})
					if err != nil {
						test.Fatal(err)
					}
					process := workers.Pending.Process
					switch owner {
					case "graph":
						process = workers.Graph.Process
					case "qualification":
						process = workers.Qualification.Process
					}
					for _, limit := range []int{0, -1, 129} {
						if err := process(ctx, limit); !errors.Is(err, domain.ErrValidation) || len(entered) != 0 {
							test.Fatal("invalid batch limit reached claim", limit, err)
						}
					}
					var release func()
					if queued != 0 {
						_, release, err = AcquireResources(ctx, ResourceBulk)
						if err != nil {
							test.Fatal(err)
						}
						defer release()
					}
					started := time.Now()
					done := make(chan error, 1)
					go func() { done <- process(ctx, 1) }()
					if queued != 0 {
						synctest.Wait()
						gate.mu.Lock()
						waiting := len(gate.waiters[ResourceBulk])
						gate.mu.Unlock()
						if waiting != 1 || len(entered) != 0 {
							test.Fatal("owner escaped the caller's existing bulk gate", waiting)
						}
						time.Sleep(queued)
						release()
					}
					if err := <-done; !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != queued+budget {
						test.Fatal("claim deadline swallowed or charged admission wait", err, time.Since(started))
					}
					select {
					case claim := <-entered:
						if !errors.Is(claim.Err(), context.DeadlineExceeded) || ctx.Err() != nil {
							test.Fatal("claim cancellation escaped to caller", claim.Err(), ctx.Err())
						}
					default:
						test.Fatal("factory owner never reached its native claim")
					}
					assertResourcesReleased(test, gate)
				})
			})
		}
	}
}
