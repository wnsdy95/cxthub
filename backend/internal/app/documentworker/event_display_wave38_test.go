package documentworker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type wave38EventDisplayStore struct {
	*wave38DisplayStore
	outbound.DocEventDisplayStore
	preparation     domain.DocEventDisplayPreparation
	objects         map[domain.ContentHash][]byte
	saved, opened   int
	completed       int
	completionError error
	probeBounds     bool
}

func (storage *wave38EventDisplayStore) LoadDocEventDisplay(ctx context.Context, _ domain.DocDisplayJob) (domain.DocEventDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	return storage.preparation, nil
}

func (storage *wave38EventDisplayStore) OpenDocEventDisplayWork(ctx context.Context, _ domain.DocDisplayJob, checkpoint domain.ContentHash) (outbound.DocEventDisplayWorkReader, error) {
	storage.scope.requireRead(ctx)
	encoded, err := storage.preparation.Encode()
	if err != nil || domain.HashContent(encoded) != checkpoint {
		storage.scope.test.Fatal("event display opened work with an unbound checkpoint", err)
	}
	storage.opened++
	return &wave38EventDisplayRead{storage: storage, ctx: ctx}, nil
}

func (storage *wave38EventDisplayStore) SaveDocEventDisplay(ctx context.Context, _ domain.DocDisplayJob, step domain.DocEventDisplayStep) (domain.DocEventDisplayPreparation, error) {
	storage.scope.requireWrite(ctx)
	encoded, err := storage.preparation.Encode()
	if err != nil || step.Prior() != domain.HashContent(encoded) {
		return domain.DocEventDisplayPreparation{}, domain.ErrConflict
	}
	for _, object := range step.Objects() {
		if domain.HashContent(object.Bytes) != object.Hash {
			storage.scope.test.Fatal("event display staging changed object identity")
		}
		storage.objects[object.Hash] = object.Bytes
	}
	storage.saved++
	storage.preparation = step.Preparation()
	return storage.preparation, nil
}

func (storage *wave38EventDisplayStore) CompleteDocEventDisplay(ctx context.Context, _ domain.DocDisplayJob, checkpoint domain.ContentHash, _ time.Time) error {
	storage.scope.requireWrite(ctx)
	encoded, err := storage.preparation.Encode()
	if err != nil || storage.preparation.Phase != "completed" || domain.HashContent(encoded) != checkpoint {
		storage.scope.test.Fatal("event display completed without its final encoded checkpoint", err)
	}
	storage.completed++
	return storage.completionError
}

type wave38EventDisplayRead struct {
	storage *wave38EventDisplayStore
	ctx     context.Context
}

func (reader *wave38EventDisplayRead) LoadObject(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
	reader.storage.scope.requireRead(ctx)
	if reader.ctx.Err() != nil {
		reader.storage.scope.test.Fatal("event display work reader survived its read scope")
	}
	raw, exists := reader.storage.objects[hash]
	if !exists {
		return nil, domain.ErrNotFound
	}
	return raw, nil
}

func (reader *wave38EventDisplayRead) PrepareStep(ctx context.Context, state domain.DocEventDisplayPreparation, load func(context.Context, int, int) ([]byte, error), tools ...domain.DocToolPreparationReader) (domain.DocEventDisplayStep, error) {
	reader.storage.scope.requireRead(ctx)
	if reader.storage.probeBounds {
		for _, bounds := range [][2]int{{-1, 1}, {0, 0}, {0, domain.DocFieldDiscoveryStepBytes + 1}, {state.Source.EventBytes, 1}} {
			if _, err := load(ctx, bounds[0], bounds[1]); !errors.Is(err, domain.ErrIntegrity) {
				reader.storage.scope.test.Fatal("event display source bounds defense changed", bounds, err)
			}
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := load(canceled, 0, 1); !errors.Is(err, context.Canceled) {
			reader.storage.scope.test.Fatal("event display raw loader ignored cancellation", err)
		}
	}
	return domain.StepDocEventDisplay(ctx, state, load, reader.LoadObject, tools...)
}

func TestWave38EventDisplayOwnerRunsActualProjectionWithinBoundedReads(test *testing.T) {
	fixture, queries := newWave38DisplayFixture(test, []domain.CIREvent{{Kind: domain.EventReasoning,
		RedactedSummary: strings.Repeat("public **needle**🙂\n", 12_000), Locked: &domain.LockedBlob{
			Provider: domain.ProviderCodex, Scheme: "encrypted", Blob: "private state must never become display text"}}})
	state, err := domain.NewDocEventDisplayPreparation(fixture.pages[0][0])
	if err != nil {
		test.Fatal(err)
	}
	storage := &wave38EventDisplayStore{wave38DisplayStore: &wave38DisplayStore{scope: fixture.scope,
		job: domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{RepoID: fixture.repo, DocHash: fixture.hash}, Target: fixture.target(0, domain.DocDisplayProjectionProfile)}},
		preparation: state, objects: map[domain.ContentHash][]byte{}, probeBounds: true}
	sources := &wave38DisplaySources{queries: queries, scope: fixture.scope}
	published := false
	for attempt := 0; attempt < 100 && !published; attempt++ {
		before := storage.saved
		published, err = NewDisplayWorker(storage, sources, fixture.scope).Build(context.Background(), storage.job)
		if err != nil || storage.saved-before > DisplayJobSteps || storage.saved-before == 0 && !published {
			test.Fatal("event projection failed or lost bounded productive continuation", published, err, storage.preparation.Phase)
		}
	}
	if !published || storage.completed != 1 || storage.saved != storage.opened || sources.view != 0 || sources.display != storage.saved || fixture.metadata != storage.saved {
		test.Fatal("event projection bypassed actual read/stage/publication path", published, storage)
	}
	if _, err := sources.last.Entry(0); !errors.Is(err, context.Canceled) {
		test.Fatal("event projection retained a source past its callback", err)
	}
	storage.completionError = domain.ErrConflict
	published, err = NewDisplayWorker(storage, nil, fixture.scope).BuildEvent(context.Background(), storage.job)
	if !published || !errors.Is(err, domain.ErrConflict) {
		test.Fatal("event completion lost returned published flag or conflict", published, err)
	}
}

func TestWave38EventDisplayOwnerUnsupportedAndReadFailureDoNotStage(test *testing.T) {
	storage := newWave38CompletedDisplayStore(test)
	job := storage.job
	job.Target.Profile = domain.DocDisplayProjectionProfile
	if published, err := NewDisplayWorker(storage, nil, storage.scope).Build(context.Background(), job); published || !errors.Is(err, domain.ErrDocFindDisplayUnavailable) {
		test.Fatal("projection silently fell back to string preparation", published, err)
	}
	fixture, queries := newWave38DisplayFixture(test, []domain.CIREvent{{Kind: domain.EventReasoning, RedactedSummary: "public"}})
	state, err := domain.NewDocEventDisplayPreparation(fixture.pages[0][0])
	if err != nil {
		test.Fatal(err)
	}
	events := &wave38EventDisplayStore{wave38DisplayStore: &wave38DisplayStore{scope: fixture.scope}, preparation: state, objects: map[domain.ContentHash][]byte{}}
	job = domain.DocDisplayJob{DocRangeIndexJob: domain.DocRangeIndexJob{RepoID: fixture.repo, DocHash: fixture.hash}, Target: fixture.target(0, domain.DocDisplayProjectionProfile)}
	fixture.scope.afterRead = errors.New("read snapshot release failed")
	if published, err := NewDisplayWorker(events, queries, fixture.scope).BuildEvent(context.Background(), job); published || !errors.Is(err, fixture.scope.afterRead) || events.saved != 0 || events.completed != 0 {
		test.Fatal("failed event read published staged progress", published, err, events.saved, events.completed)
	}
}
