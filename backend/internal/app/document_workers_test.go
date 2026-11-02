package app

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/app/documentquery"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type documentWorkersFindFieldsStore struct {
	outbound.DocFindFieldsStore
}

type documentWorkersDisplayStore struct {
	outbound.DocDisplayStore
}

type documentWorkersReadOnlyJobStore struct {
	*store.FSStore
	claims int
}

func (storage *documentWorkersReadOnlyJobStore) ClaimDocJob(context.Context, domain.ContentHash, time.Time, time.Duration) (domain.DocFinalizationJob, error) {
	storage.claims++
	return domain.DocFinalizationJob{}, domain.ErrNotFound
}

func documentWorkersConfig(test *testing.T) (DocumentWorkersConfig, *store.FSStore) {
	test.Helper()
	storage := store.NewFSStore(test.TempDir())
	return DocumentWorkersConfig{Metadata: storage, Blobs: storage,
		Verifier: &domain.CanonicalDocVerifier{}, Profile: documentquery.LegacyCompatibility}, storage
}

func TestDocumentWorkersWithoutServicePublishesAcceptedLegacyJob(test *testing.T) {
	config, storage := documentWorkersConfig(test)
	ctx := inbound.WithSystemActor(context.Background())
	repo := domain.HashContent([]byte(test.Name()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: test.Name()},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: "standalone accepted work"}}}}}
	raw, err := domain.CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	plan, ok := domain.PlanDocChunks(raw)
	if !ok {
		test.Fatal("document did not produce a chunk plan")
	}
	if _, _, err := storage.PutChunks(ctx, repo, plan.Bodies); err != nil {
		test.Fatal(err)
	}
	job, err := domain.NewDocFinalizationJob(repo, domain.HashContent(raw), plan.Manifest, time.Now().UTC())
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocJob(ctx, job); err != nil {
		test.Fatal(err)
	}
	workers, err := NewDocumentWorkers(config)
	if err != nil {
		test.Fatal(err)
	}
	if workers.Finalization == nil || workers.Range != nil || workers.FindFields != nil || workers.Display != nil {
		test.Fatal("legacy composition changed optional worker availability")
	}
	if have, err := storage.HasDocs(ctx, repo, []domain.ContentHash{job.DocHash}); err != nil || len(have) != 0 {
		test.Fatal("acceptance published before execution", have, err)
	}
	if err := workers.Finalization.Process(context.Background(), 1); err != nil {
		test.Fatal(err)
	}
	completed, err := storage.GetDocJob(ctx, repo, job.ID)
	if err != nil || completed.State != "completed" {
		test.Fatal("standalone owner did not complete durable work", completed, err)
	}
	actual, err := storage.GetDoc(ctx, repo, job.DocHash)
	if err != nil || actual.Hash != job.DocHash || !reflect.DeepEqual(actual.CIR, document) {
		test.Fatal("standalone publication changed document identity or events", actual, err)
	}
	foreign := domain.HashContent([]byte("foreign " + test.Name()))
	if _, err := storage.GetDoc(ctx, foreign, job.DocHash); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("published proof granted foreign ownership", err)
	}
	if snapshots, err := storage.ListSnapshots(ctx, repo, ""); err != nil || len(snapshots) != 0 {
		test.Fatal("document finalization created a snapshot", snapshots, err)
	}
}

func TestDocumentWorkersShareQueriesScopeAndOriginalVerifier(test *testing.T) {
	config, _ := documentWorkersConfig(test)
	config.FindFields = &documentWorkersFindFieldsStore{}
	config.Display = &documentWorkersDisplayStore{}
	workers, err := NewDocumentWorkers(config)
	if err != nil {
		test.Fatal(err)
	}
	find := reflect.ValueOf(workers.FindFields).Elem()
	display := reflect.ValueOf(workers.Display).Elem()
	findSources := find.FieldByName("sources").Elem()
	displaySources := display.FieldByName("sources").Elem()
	if findSources.Type() != reflect.TypeOf((*documentquery.Queries)(nil)) || findSources.Pointer() != displaySources.Pointer() {
		test.Fatal("find and display did not share the actual document query reader")
	}
	if !findSources.Elem().FieldByName("readOnly").Bool() {
		test.Fatal("worker source queries retained preparation capabilities")
	}
	finalization := reflect.ValueOf(workers.Finalization).Elem()
	if finalization.FieldByName("scope").Elem().Pointer() != find.FieldByName("scope").Elem().Pointer() ||
		find.FieldByName("scope").Elem().Pointer() != display.FieldByName("scope").Elem().Pointer() {
		test.Fatal("composition created separate worker scopes")
	}
	if finalization.FieldByName("verifier").Pointer() != reflect.ValueOf(config.Verifier).Pointer() {
		test.Fatal("composition copied or replaced its verifier")
	}
	service := NewService(config.Metadata, config.Blobs, nil, nil, nil)
	service.ConfigureDocumentFindFields(config.FindFields)
	service.ConfigureDocumentDisplay(config.Display)
	bound, err := service.DocumentWorkersForProfile(config.Profile)
	if err != nil {
		test.Fatal(err)
	}
	if reflect.ValueOf(bound.Finalization).Elem().FieldByName("verifier").Pointer() != reflect.ValueOf(&service.docVerifier).Pointer() {
		test.Fatal("service composition did not retain its original verifier cache")
	}
}

func TestDocumentWorkersRejectInvalidAndIncompleteProductionComposition(test *testing.T) {
	for _, scenario := range []string{"profile", "metadata", "blobs", "verifier", "typed-nil-find", "typed-nil-display", "verified-fs", "verified-incomplete-workers"} {
		test.Run(scenario, func(test *testing.T) {
			config, storage := documentWorkersConfig(test)
			expected := domain.ErrValidation
			switch scenario {
			case "profile":
				config.Profile = 0
			case "metadata":
				config.Metadata = nil
			case "blobs":
				config.Blobs = (*store.FSStore)(nil)
			case "verifier":
				config.Verifier = nil
			case "typed-nil-find":
				config.FindFields = (*documentWorkersFindFieldsStore)(nil)
			case "typed-nil-display":
				config.Display = (*documentWorkersDisplayStore)(nil)
			case "verified-fs":
				config.Profile, expected = documentquery.VerifiedRoots, domain.ErrCatalogUnsupported
			case "verified-incomplete-workers":
				config.Metadata = &graphReadTransactions{FSStore: storage}
				config.Profile, expected = documentquery.VerifiedRoots, domain.ErrCatalogUnsupported
			}
			if workers, err := NewDocumentWorkers(config); workers != nil || !errors.Is(err, expected) {
				test.Fatal("invalid composition did not fail closed", workers, err)
			}
		})
	}
}

func TestDocumentWorkersReadOnlyAndCanceledWorkNeverClaimOrAcquire(test *testing.T) {
	config, actual := documentWorkersConfig(test)
	storage := &documentWorkersReadOnlyJobStore{FSStore: actual}
	config.Blobs = storage
	workers, err := NewDocumentWorkers(config)
	if err != nil {
		test.Fatal(err)
	}
	ctx, gate := admissionTestContext(time.Second)
	readonly := outbound.WithDocReadOnly(ctx)
	if err := workers.Finalization.Process(readonly, 1); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("read-only work was promoted to a system claim", err)
	}
	scope := newDocumentWorkerScope(config.Metadata, config.Blobs, config.Profile)
	callbacks := 0
	work := func(context.Context) error { callbacks++; return nil }
	if err := scope.WithinWork(readonly, work); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("read-only work reached shared admission", err)
	}
	canceled, cancel := context.WithCancel(readonly)
	cancel()
	if err := scope.WithinWork(canceled, work); !errors.Is(err, context.Canceled) {
		test.Fatal("read-only guard changed cancellation precedence", err)
	}
	if err := workers.Finalization.Process(canceled, 1); !errors.Is(err, context.Canceled) {
		test.Fatal("canceled worker reached a durable claim", err)
	}
	gate.mu.Lock()
	unused := gate.active == 0 && gate.bulk == 0 && gate.retained == nil && len(gate.waiters[ResourceBulk]) == 0 && len(gate.waiters[ResourceInteractive]) == 0
	gate.mu.Unlock()
	if callbacks != 0 || storage.claims != 0 || !unused {
		test.Fatal("rejected work touched a claim, callback, or admission slot", callbacks, storage.claims, unused)
	}
}

func TestDocumentWorkersLegacySourceDefersToLoadCompletionAndValidationOrder(test *testing.T) {
	raw := []byte(`{"kind":"tool_result","output":" "}`)
	source, err := domain.BuildDocDisplayStringSourceReader(context.Background(), domain.HashContent(raw), len(raw), bytes.NewReader(raw))
	if err != nil {
		test.Fatal(err)
	}
	state, err := domain.NewDocDisplayStringState(source)
	if err != nil || !state.Done {
		test.Fatal("empty display did not have a completed string state", err)
	}
	encoded, err := state.Encode(source)
	if err != nil {
		test.Fatal(err)
	}
	job := domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{
		RepoID: domain.HashContent([]byte(test.Name())), DocHash: domain.HashContent([]byte("legacy source precedence")), Attempts: 1,
	}}
	job.Target = domain.DocDisplayTarget{RepoID: job.RepoID, DocHash: job.DocHash,
		Binding: domain.DocReadGenerationBinding{SourceID: "11111111-1111-4111-8111-111111111111", RangeGeneration: domain.HashContent([]byte("generation"))},
		Event:   domain.DocEventIndex{Hash: source.EventHash, Length: source.EventBytes},
	}
	for _, scenario := range []string{"completed", "load-error", "complete-error"} {
		test.Run(scenario, func(test *testing.T) {
			storage := &docDisplayCompletionStore{job: job, preparation: domain.DocDisplayPreparation{
				Source: &source, State: state, Checkpoint: domain.HashContent(encoded),
			}}
			var expected error
			if scenario == "load-error" {
				expected = errors.New("load state failed before source demand")
				storage.loadError = expected
			}
			if scenario == "complete-error" {
				expected = domain.ErrConflict
				storage.completeError = expected
			}
			service := NewService(nil, nil, nil, nil, nil)
			service.ConfigureDocumentDisplay(storage)
			worker, err := service.documentDisplayWorker()
			if err != nil {
				test.Fatal("unused source dependencies failed legacy construction", err)
			}
			published, err := worker.Build(context.Background(), job)
			if !errors.Is(err, expected) || published != (scenario != "load-error") {
				test.Fatal("source construction changed load or completion result", published, err)
			}
			if err := worker.RunJob(context.Background(), job); !errors.Is(err, expected) {
				test.Fatal("source construction replaced the actual job failure", err)
			}
			if scenario == "completed" && storage.finishes != 0 || scenario != "completed" && storage.finishes != 1 {
				test.Fatal("unused source dependencies changed finish behavior", storage.finished)
			}
			if scenario == "complete-error" && (storage.finished.State != "rejected" || storage.finished.Reason != "source_changed") {
				test.Fatal("source construction replaced completion classification", storage.finished)
			}
			if _, err := worker.Source(context.Background(), domain.DocDisplayTarget{}); err != domain.ErrValidation {
				test.Fatal("invalid target did not precede source construction", err)
			}
			if _, err := worker.AdvanceDiscovery(context.Background(), job, domain.DocDisplayPreparation{Checkpoint: domain.HashContent([]byte("mixed"))}); err != domain.ErrIntegrity {
				test.Fatal("mixed discovery checkpoint reached source dependencies", err)
			}
			projection := job
			projection.Target.Profile = domain.DocDisplayProjectionProfile
			if published, err := worker.Build(context.Background(), projection); published || err != domain.ErrDocFindDisplayUnavailable {
				test.Fatal("unsupported projection reached source dependencies", published, err)
			}
		})
	}
}

func TestDocumentWorkersLegacySourcesCopyPortsAndOpenBothActualQueries(test *testing.T) {
	config, storage := documentWorkersConfig(test)
	repo := domain.HashContent([]byte(test.Name()))
	document := seedLegacyFindDocument(test, storage, repo, []domain.CIREvent{
		{Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "copied legacy query ports"}}},
	})
	service := NewService(storage, storage, nil, nil, nil)
	scope := newDocumentWorkerScope(storage, storage, config.Profile)
	sources, err := service.documentWorkerSources(scope)
	if err != nil {
		test.Fatal(err)
	}
	service.meta, service.blobs = nil, nil
	ctx := scope.WorkerContext(context.Background())
	if err := scope.WithinWork(ctx, func(work context.Context) error {
		return scope.WithinRepositoryRead(work, repo, func(read context.Context) error {
			for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (documentquery.Source, error){sources.OpenSource, sources.OpenViewSource} {
				source, err := open(read, repo, document.Hash)
				if err != nil {
					return err
				}
				entry, err := source.Entry(0)
				if err != nil {
					return err
				}
				raw, err := source.Read(entry.Offset, entry.Length)
				if err != nil || domain.HashContent(raw) != entry.Hash || source.Total() != 1 {
					test.Fatal("lazy source did not use the actual copied-port query factory", err)
				}
			}
			return nil
		})
	}); err != nil {
		test.Fatal("legacy source rediscovered changed Service dependencies", err)
	}
}

func TestDocumentWorkersLegacySourceFailureOccursOnlyOnDemand(test *testing.T) {
	service := NewService(nil, nil, nil, nil, nil)
	scope := newDocumentWorkerScope(nil, nil, documentquery.LegacyCompatibility)
	sources, err := service.documentWorkerSources(scope)
	if err != nil || sources == nil {
		test.Fatal("missing source dependencies failed before source demand", err)
	}
	ctx := context.Background()
	repo, hash := domain.HashContent([]byte(test.Name())), domain.HashContent([]byte("missing source"))
	for _, open := range []func(context.Context, domain.ContentHash, domain.ContentHash) (documentquery.Source, error){sources.OpenSource, sources.OpenViewSource} {
		if _, err := open(ctx, repo, hash); err != domain.ErrCatalogUnsupported {
			test.Fatal("missing legacy source changed its fail-closed error", err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := open(canceled, repo, hash); err != context.Canceled {
			test.Fatal("deferred source construction hid cancellation", err)
		}
	}
	config, _ := documentWorkersConfig(test)
	config.Blobs = nil
	if workers, err := NewDocumentWorkers(config); workers != nil || !errors.Is(err, domain.ErrValidation) {
		test.Fatal("actual startup became lazy or accepted incomplete ports", workers, err)
	}
}
