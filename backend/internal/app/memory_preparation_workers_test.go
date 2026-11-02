package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave44MemoryPreparationCapabilities interface {
	outbound.MetadataStore
	outbound.BlobStore
	outbound.MemoryQueryReader
	outbound.GraphEvidenceReader
	outbound.MemoryReadWorkStore
	outbound.MemoryCaptureStore
	outbound.MemoryCaptureAnchorStore
	outbound.MemoryReadStore
	outbound.CurrentMemoryReadStore
	outbound.MemoryReadWorkReconciler
	outbound.MemoryReadObjectWakeDispatcher
	outbound.MinimumMemoryReadWorkStore
	outbound.MinimumMemoryReadStore
	outbound.MemoryReadComparisonStore
	outbound.MemoryReadQualificationStore
	outbound.RepositoryTransactions
	outbound.RepositoryTransactionState
	outbound.RepositoryTransactionPresence
	outbound.RepositoryGraphSourceStore
	outbound.RepositoryGraphSourceGenerationStore
	outbound.RepositoryMemorySourceGenerationStore
	outbound.GraphSourceReaderFactory
	outbound.MemoryCapturePinReader
	outbound.BoundedMemoryReader
	outbound.WitnessedMemoryReader
}

type wave44MemoryPreparationStore struct {
	wave44MemoryPreparationCapabilities
	test       *testing.T
	calls      []string
	configured []domain.ContentHash
	selected   []domain.ContentHash
	failAt     string
	failure    error
}

func (*wave44MemoryPreparationStore) InTransaction(context.Context) bool {
	return false
}

func (storage *wave44MemoryPreparationStore) ConfigureMemoryReadWork(_ context.Context, repo domain.ContentHash, enabled bool) error {
	if !enabled {
		storage.test.Fatal("explicit configuration unexpectedly disabled durable work")
	}
	storage.configured = append(storage.configured, repo)
	return nil
}

func (storage *wave44MemoryPreparationStore) claimStep(ctx context.Context, operation string) error {
	storage.test.Helper()
	_, system := inbound.RepositoryActor(ctx)
	deadline, bounded := ctx.Deadline()
	permit, _ := ctx.Value(resourcePermitKey{}).(*resourcePermit)
	if !system || !reflect.DeepEqual(inbound.DocumentIdentities(ctx), []domain.DocumentIdentity{domain.DocumentIdentityLegacy}) ||
		!bounded || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second || permit == nil || !permit.live.Load() {
		storage.test.Fatal("claim lost original worker authority, admission or five-second budget", operation)
	}
	storage.calls = append(storage.calls, operation)
	if storage.failAt == operation {
		return storage.failure
	}
	return nil
}

func (storage *wave44MemoryPreparationStore) DispatchMemoryReadObjectWakes(ctx context.Context, limit int) (int, error) {
	if limit != 1 {
		storage.test.Fatal("dispatcher batch changed", limit)
	}
	return 0, storage.claimStep(ctx, "dispatch")
}

func (storage *wave44MemoryPreparationStore) ReconcileMemoryReadWork(ctx context.Context, repos []domain.ContentHash, limit int) (int, error) {
	if limit != 1 {
		storage.test.Fatal("reconciliation batch changed", limit)
	}
	storage.selected = append([]domain.ContentHash(nil), repos...)
	return 0, storage.claimStep(ctx, "reconcile")
}

func (storage *wave44MemoryPreparationStore) ClaimMemoryReadWork(ctx context.Context, repos []domain.ContentHash, lease time.Duration) (domain.MemoryReadWork, error) {
	if lease != time.Minute || !reflect.DeepEqual(repos, storage.selected) {
		storage.test.Fatal("claim changed its repository selection or original lease", repos, lease)
	}
	if err := storage.claimStep(ctx, "claim"); err != nil {
		return domain.MemoryReadWork{}, err
	}
	return domain.MemoryReadWork{}, domain.ErrNotFound
}

func wave44PreparationWorkers(test *testing.T, config MemoryPreparationWorkerConfig) *MemoryPreparationWorkers {
	test.Helper()
	workers, err := NewMemoryPreparationWorkers(config)
	if err != nil || workers == nil {
		test.Fatal("direct worker construction failed", err)
	}
	return workers
}

func TestWave44MemoryPreparationFactoryRejectsInvalidDeclarations(test *testing.T) {
	resources := wave44PreparationResources(test, &wave44BaseMetadata{}, &wave44BaseBlobs{})
	for _, profile := range []MemoryPreparationProfile{"", "unknown", MemoryPreparationCompatibility, MemoryPreparationComplete} {
		for _, absent := range []bool{false, true} {
			config := MemoryPreparationWorkerConfig{Profile: profile, Resources: resources}
			if absent {
				config.Resources = nil
			}
			workers, err := NewMemoryPreparationWorkers(config)
			valid := !absent && (profile == MemoryPreparationCompatibility || profile == MemoryPreparationComplete)
			if valid && (err != nil || workers == nil) || !valid && (!errors.Is(err, domain.ErrValidation) || workers != nil) {
				test.Fatal("profile or declared nil resources validation changed", profile, absent, err)
			}
		}
	}
	for _, selected := range [][]domain.ContentHash{{"invalid"}, make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1)} {
		expected := domain.ErrValidation
		if len(selected) <= domain.MaxMemoryReadWorkerRepos {
			expected = domain.ErrIntegrity
		}
		workers, err := NewMemoryPreparationWorkers(MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources, Repositories: selected})
		if !errors.Is(err, expected) || workers != nil {
			test.Fatal("selection validation must precede missing active capabilities", err)
		}
	}
	for _, missing := range []string{"metadata", "blobs", "work", "scope", "resource-scope", "delivery"} {
		broken := *resources
		switch missing {
		case "metadata":
			broken.meta = nil
		case "blobs":
			broken.blobs = nil
		case "work":
			broken.work = nil
		case "scope":
			broken.scope = nil
		case "resource-scope":
			scope := *broken.scope
			scope.resources = nil
			broken.scope = &scope
		case "delivery":
			broken.delivery = nil
		}
		workers, err := NewMemoryPreparationWorkers(MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: &broken})
		if !errors.Is(err, domain.ErrValidation) || workers != nil {
			test.Fatal("empty complete selection accepted incomplete base resources", missing, err)
		}
	}
}

func TestWave44MemoryPreparationCompleteRequiresActualActivePorts(test *testing.T) {
	storage := &wave44MemoryPreparationStore{test: test}
	for _, missing := range []struct {
		name     string
		metadata outbound.MetadataStore
		blobs    outbound.BlobStore
	}{
		{"work", struct {
			*wave44MemoryPreparationStore
			ConfigureMemoryReadWork struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"capture", struct {
			*wave44MemoryPreparationStore
			WriteMemoryCapture struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"anchor", struct {
			*wave44MemoryPreparationStore
			FindMemoryCaptureAnchor struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"read", struct {
			*wave44MemoryPreparationStore
			WriteMemoryRead struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"current", struct {
			*wave44MemoryPreparationStore
			WithCurrentMemoryReadRange struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"reconciler", struct {
			*wave44MemoryPreparationStore
			ReconcileMemoryReadWork struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"dispatcher", struct {
			*wave44MemoryPreparationStore
			DispatchMemoryReadObjectWakes struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"minimum", struct {
			*wave44MemoryPreparationStore
			FinishMinimumMemoryReadWork struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"comparison", struct {
			*wave44MemoryPreparationStore
			RecordMemoryReadComparison struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"transactions", struct {
			*wave44MemoryPreparationStore
			WithinRepository struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"snapshot", struct {
			*wave44MemoryPreparationStore
			WithinReadSnapshot struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"presence", struct {
			*wave44MemoryPreparationStore
			InTransaction struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"state", struct {
			*wave44MemoryPreparationStore
			InReadOnlyTransaction struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"graph", struct {
			*wave44MemoryPreparationStore
			RepositoryGraphSourceChanges struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"factory", struct {
			*wave44MemoryPreparationStore
			BuildGraphSourceReader struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"generation", struct {
			*wave44MemoryPreparationStore
			ReadRepositoryMemorySourceGeneration struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"live-graph-generation", struct {
			*wave44MemoryPreparationStore
			ReadRepositoryGraphSourceGeneration struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"live-revision", struct {
			*wave44MemoryPreparationStore
			RepositoryRevision struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"pins", struct {
			*wave44MemoryPreparationStore
			ReadMemoryPinsForCapture struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"live-pr-pins", struct {
			*wave44MemoryPreparationStore
			ListPRContributionMemories struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"live-git-pins", struct {
			*wave44MemoryPreparationStore
			ListGitContributionMemories struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"live-graph-evidence", struct {
			*wave44MemoryPreparationStore
			ListGitContributions struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"public-qualification", struct {
			*wave44MemoryPreparationStore
			QualifyMemoryRead struct{}
		}{wave44MemoryPreparationStore: storage}, storage},
		{"bounded-blobs", storage, struct {
			*wave44MemoryPreparationStore
			GetMemoryForCapture struct{}
		}{wave44MemoryPreparationStore: storage}},
		{"witnessed-blobs", storage, struct {
			*wave44MemoryPreparationStore
			CaptureMemoryObject struct{}
		}{wave44MemoryPreparationStore: storage}},
	} {
		test.Run(missing.name, func(test *testing.T) {
			resources := wave44PreparationResources(test, missing.metadata, missing.blobs)
			config := MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources}
			wave44PreparationWorkers(test, config)
			config.Repositories = []domain.ContentHash{hh(test.Name())}
			workers, err := NewMemoryPreparationWorkers(config)
			if !errors.Is(err, domain.ErrCatalogUnsupported) || workers != nil {
				test.Fatal("active complete selection accepted a missing capability", err)
			}
		})
	}
	if len(storage.calls) != 0 || len(storage.configured) != 0 {
		test.Fatal("capability validation executed work or configuration")
	}
}

func TestWave44MemoryPreparationCompatibilityKeepsLazyPreparationErrors(test *testing.T) {
	storage := &wave44MemoryPreparationStore{test: test}
	metadata := struct {
		*wave44MemoryPreparationStore
		InTransaction struct{}
	}{wave44MemoryPreparationStore: storage}
	resources := wave44PreparationResources(test, metadata, &wave44BaseBlobs{})
	workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationCompatibility, Resources: resources, Repositories: []domain.ContentHash{hh(test.Name())}})
	request := domain.EffectiveMemoryRequest{}
	if _, err := workers.PrepareIncrementalMemoryRead(context.Background(), "invalid", request, time.Time{}, nil); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("preparation no longer checks system-only authority before source, selection or expiration", err)
	}
	if _, err := workers.PrepareIncrementalMemoryRead(inbound.WithSystemActor(context.Background()), "invalid", request, time.Time{}, nil); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("compatibility replaced its lazy transaction capability error", err)
	}
	if len(storage.calls) != 0 || len(storage.configured) != 0 {
		test.Fatal("compatibility factory or early validation executed worker commands")
	}
	base := wave44PreparationResources(test, &wave44BaseMetadata{}, &wave44BaseBlobs{})
	if owner, err := NewMemoryPreparationWorkers(MemoryPreparationWorkerConfig{Profile: MemoryPreparationCompatibility, Resources: base, Repositories: []domain.ContentHash{hh(test.Name())}}); !errors.Is(err, domain.ErrCatalogUnsupported) || owner != nil {
		test.Fatal("public active compatibility declaration accepted missing core worker ports", err)
	}
}

func TestWave44MemoryPreparationSelectionIsCopiedAndConfigureIsExplicit(test *testing.T) {
	storage := &wave44MemoryPreparationStore{test: test}
	resources := wave44PreparationResources(test, storage, storage)
	repo, replacement := hh(test.Name()), hh("replacement")
	selected := []domain.ContentHash{repo, repo}
	workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources, Repositories: selected})
	selected[0], selected[1] = replacement, replacement
	if workers.MemoryPreparationResources != resources || !reflect.DeepEqual(workers.memoryReadRepos, []domain.ContentHash{repo}) || len(storage.calls) != 0 || len(storage.configured) != 0 {
		test.Fatal("factory aliased selection, replaced resources, or started/configured work")
	}
	ctx, gate := admissionTestContext(time.Second)
	ctx = inbound.WithDocumentIdentities(inbound.WithRepositoryActor(ctx, "caller"), []domain.DocumentIdentity{domain.DocumentIdentityRootV1})
	if err := workers.ProcessMemoryReadPreparations(ctx, 1); err != nil || !reflect.DeepEqual(storage.selected, []domain.ContentHash{repo}) || len(storage.configured) != 0 {
		test.Fatal("selected processing implicitly configured durable jobs or used caller-owned selection", err)
	}
	assertResourcesReleased(test, gate)
	service := &Service{meta: storage, blobs: storage}
	configured := []domain.ContentHash{replacement, replacement}
	if err := service.ConfigureMemoryReadPreparations(ctx, configured); err != nil || !reflect.DeepEqual(storage.configured, []domain.ContentHash{replacement}) {
		test.Fatal("explicit Configure lost original deduplication", err, storage.configured)
	}
	configured[0], configured[1] = repo, repo
	if !reflect.DeepEqual(service.memoryReadRepos, []domain.ContentHash{replacement}) || !reflect.DeepEqual(workers.memoryReadRepos, []domain.ContentHash{repo}) {
		test.Fatal("Service Configure aliased its input or changed the independent worker selection")
	}
	if err := service.ConfigureMemoryReadPreparations(ctx, nil); err != nil || len(service.memoryReadRepos) != 0 || len(storage.configured) != 1 || !reflect.DeepEqual(workers.memoryReadRepos, []domain.ContentHash{repo}) {
		test.Fatal("empty Configure changed its original local-only disable behavior", err)
	}
	disabled := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources})
	disabled.Run(ctx)
}

func TestWave44MemoryPreparationDisabledAndErrorPriority(test *testing.T) {
	resources := wave44PreparationResources(test, &wave44BaseMetadata{}, &wave44BaseBlobs{})
	workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationCompatibility, Resources: resources})
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, limit := range []int{-1, 0, 129} {
		if err := workers.ProcessMemoryReadPreparations(canceled, limit); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("batch validation no longer precedes disabled/canceled work", limit, err)
		}
	}
	if err := workers.ProcessMemoryReadPreparations(canceled, 1); err != nil {
		test.Fatal("disabled owner reached dependencies or cancellation", err)
	}
	workers.RunMemoryReadPreparationWorker(context.Background())
	service := &Service{meta: resources.meta, blobs: resources.blobs}
	if err := service.ConfigureMemoryReadPreparations(canceled, []domain.ContentHash{"invalid"}); !errors.Is(err, domain.ErrCatalogUnsupported) {
		test.Fatal("legacy Configure capability failure no longer precedes hash validation", err)
	}
	legacy := (&Service{}).memoryPreparation()
	if err := legacy.ProcessMemoryReadPreparations(context.Background(), 1); err != nil {
		test.Fatal("private empty legacy facade lost its nil-dependency no-op", err)
	}
	for _, operation := range []string{"dispatch", "reconcile", "claim"} {
		test.Run(operation, func(test *testing.T) {
			failure := errors.New("claim-stage failure")
			storage := &wave44MemoryPreparationStore{test: test, failAt: operation, failure: failure}
			resources := wave44PreparationResources(test, storage, storage)
			workers := wave44PreparationWorkers(test, MemoryPreparationWorkerConfig{Profile: MemoryPreparationComplete, Resources: resources, Repositories: []domain.ContentHash{hh(test.Name())}})
			ctx, gate := admissionTestContext(time.Second)
			if err := workers.ProcessMemoryReadPreparations(ctx, 1); !errors.Is(err, failure) {
				test.Fatal("claim-stage error was hidden or replaced", err)
			}
			order := []string{"dispatch", "reconcile", "claim"}
			for index, expected := range order {
				if expected == operation && !reflect.DeepEqual(storage.calls, order[:index+1]) {
					test.Fatal("claim error ordering changed", storage.calls)
				}
			}
			assertResourcesReleased(test, gate)
		})
	}
}
