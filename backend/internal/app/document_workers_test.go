package app

import (
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
}

func TestDocumentWorkersRejectInvalidAndIncompleteProductionComposition(test *testing.T) {
	for _, scenario := range []string{"profile", "metadata", "blobs", "verifier", "verified-fs"} {
		test.Run(scenario, func(test *testing.T) {
			config, _ := documentWorkersConfig(test)
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
			case "typed-nil-display":
			case "verified-fs":
				config.Profile, expected = documentquery.VerifiedRoots, domain.ErrCatalogUnsupported
			case "verified-incomplete-workers":
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
