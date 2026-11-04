//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type countedMemoryAnchorStore struct {
	*store.PostgresStore
	reads        int
	graphFailure error
	sourceReads  int
	partialDelta bool
}

func (storage *countedMemoryAnchorStore) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	storage.sourceReads++
	if storage.partialDelta && request.After != nil {
		checkpoint := *request.After
		checkpoint.Sequence++
		entry := domain.RepositoryViewSourceEntry{Kind: "git_tree", Key: strings.Repeat("e", 40), Sequence: checkpoint.Sequence, Value: []byte(`{"tree":{}}`)}
		return graphCapturePage(checkpoint, "delta", []domain.RepositoryViewSourceEntry{entry}, "exhausted-journal"), nil
	}
	if storage.partialDelta && request.Cursor == "exhausted-journal" {
		return domain.RepositoryViewSourcePage{}, domain.ErrRepositoryViewSourceLimit
	}
	if request.After != nil && storage.graphFailure != nil {
		failure := storage.graphFailure
		storage.graphFailure = nil
		return domain.RepositoryViewSourcePage{}, failure
	}
	return storage.PostgresStore.RepositoryGraphSourceChanges(ctx, repo, request)
}

func (storage *countedMemoryAnchorStore) ReadMemoryCapture(ctx context.Context, repo, identity domain.ContentHash) (domain.MemoryCaptureBundle, error) {
	storage.reads++
	return storage.PostgresStore.ReadMemoryCapture(ctx, repo, identity)
}

func TestPGIncrementalMemoryPreparationAcrossWritesAndRestart(test *testing.T) {
	service, storage, _, repo, request, initial := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	deadline := time.Now().Add(time.Hour)
	if _, err := service.PublishPreparedMemoryRead(ctx, initial, deadline); err != nil {
		test.Fatal("full publication", err)
	}
	counted := &countedMemoryAnchorStore{PostgresStore: storage}
	service.meta = counted
	previous := initial
	if initial.graphAnchor == nil || initial.graphAnchor.graph.decoded == nil {
		test.Fatal("full preparation did not retain validated metadata")
	}
	for iteration := 0; iteration < 3; iteration++ {
		digest := capturedMemoryDigestPG(request.Selection.SnapshotID, "updated memory")
		digest.PreviousMemoryHash = request.Selection.MemoryHash
		digest.Summary = strings.Repeat("new body ", iteration+1)
		hash, err := service.PutMemoryDigestCAS(ctx, repo, digest)
		if err != nil {
			test.Fatal(err)
		}
		request.Selection.MemoryHash = hash
		prepared, err := service.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, previous)
		if err != nil {
			test.Fatal("incremental preparation", err)
		}
		base, err := domain.MemoryCaptureGraphBase(prepared.input)
		if err != nil || prepared.input.Header.Version != domain.MemoryCaptureDeltaVersion || base.ID != initial.input.ID || counted.reads != 0 {
			test.Fatal("warm preparation copied/reread/chained graph", counted.reads, base.ID, err)
		}
		if prepared.graphAnchor.graph.decoded != initial.graphAnchor.graph.decoded {
			test.Fatal("warm delta replaced its immutable durable anchor decoder")
		}
		deltas := 0
		for _, part := range prepared.input.Parts {
			if strings.HasPrefix(part.Kind, "graph:") {
				test.Fatal("incremental preparation serialized full graph")
			}
			if strings.HasPrefix(part.Kind, "graph_delta:") {
				deltas++
			}
		}
		if deltas == 0 || deltas > 8 {
			test.Fatal("ordinary memory change must capture a bounded graph delta", deltas)
		}
		replayed, err := BuildMemoryReadWithBase(ctx, prepared.input, initial.input, storage)
		if err != nil || !reflect.DeepEqual(replayed, prepared.output) {
			test.Fatal("persisted overlay differs from direct result", err)
		}
		identifier, err := service.PublishPreparedMemoryRead(ctx, prepared, deadline)
		if err != nil {
			test.Fatal("delta publication", err)
		}
		want, err := service.QueryEffectiveMemory(ctx, repo, request)
		if err != nil {
			test.Fatal(err)
		}
		actual, err := service.QueryQualifiedMemory(ctx, repo, identifier, request)
		if err != nil || !reflect.DeepEqual(actual, want) {
			test.Fatal("qualified delta changed current memory", err)
		}
		previous = prepared
	}
	reopened, err := store.NewPostgresStore(ctx, collaborationDSN(test))
	if err != nil {
		test.Fatal(err)
	}
	defer reopened.Close()
	restarted := NewService(reopened, reopened, auth.NewTeamTokenAuth(), gitengine.NewEngine(reopened), reopened)
	cold, err := restarted.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, nil)
	if err != nil || cold.input.ID != previous.input.ID || !reflect.DeepEqual(cold.output, previous.output) {
		test.Fatal("cold discovery/replay differs from warm preparation", err)
	}
	if cold.graphAnchor == nil || cold.graphAnchor.graph.decoded == nil || cold.graphAnchor.graph.decoded == initial.graphAnchor.graph.decoded {
		test.Fatal("restart did not reconstruct private metadata from durable input")
	}
	later, err := restarted.PrepareIncrementalMemoryRead(ctx, repo, request, deadline.Add(time.Minute), previous)
	if err != nil || later.input.Header.Version != domain.MemoryCaptureVersion {
		test.Fatal("insufficient anchor lifetime did not rotate to full", err)
	}
}

func TestPGIncrementalMemoryPreparationRequiresDurableAnchor(test *testing.T) {
	service, storage, _, repo, request, unsaved := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	deadline := time.Now().Add(time.Hour)
	prepared, err := service.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, unsaved)
	if err != nil || prepared.input.Header.Version != domain.MemoryCaptureVersion || prepared.input.ID != unsaved.input.ID {
		test.Fatal("unsaved previous capture was treated as durable anchor", err)
	}
	if _, err := service.PublishPreparedMemoryRead(ctx, prepared, deadline); err != nil {
		test.Fatal(err)
	}
	delta, err := service.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, prepared)
	if err != nil || delta.input.Header.Version != domain.MemoryCaptureDeltaVersion {
		test.Fatal("published anchor not reused", err)
	}
	if _, err := service.PublishPreparedMemoryRead(ctx, delta, deadline.Add(time.Second)); err == nil {
		test.Fatal("publication accepted longer lifetime than graph base")
	}
	if _, err := service.PrepareIncrementalMemoryRead(context.Background(), repo, request, deadline, nil); !errors.Is(err, domain.ErrForbidden) {
		test.Fatal("internal preparation exposed to unauthenticated caller", err)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, delta.input.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("failed retention check leaked child capture", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		got, err := service.PrepareIncrementalMemoryRead(bound, repo, request, deadline, prepared)
		if !errors.Is(err, domain.ErrConflict) || got != nil {
			test.Fatal("preparation borrowed a stale transaction", err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

func TestPGIncrementalMemoryPreparationDiscardsPartialJournal(test *testing.T) {
	service, storage, _, repo, request, initial := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	deadline := time.Now().Add(time.Hour)
	if _, err := service.PublishPreparedMemoryRead(ctx, initial, deadline); err != nil {
		test.Fatal(err)
	}
	counted := &countedMemoryAnchorStore{PostgresStore: storage, partialDelta: true}
	service.meta = counted
	got, err := service.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, initial)
	if err != nil || got.input.Header.Version != domain.MemoryCaptureVersion || got.input.ID != initial.input.ID || counted.sourceReads != 3 {
		test.Fatal("partial journal leaked into full retry", counted.sourceReads, err)
	}
}

func TestPGIncrementalMemoryPreparationResetDoesNotHideCorruption(test *testing.T) {
	for _, scenario := range []struct {
		name string
		err  error
	}{
		{name: "journal-reset", err: domain.ErrCatalogResetRequired},
		{name: "bounded-journal-exhausted", err: domain.ErrRepositoryViewSourceLimit},
		{name: "integrity", err: domain.ErrIntegrity},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			service, storage, _, repo, request, initial := qualifiedMemoryFixturePG(test)
			ctx := systemTestContext()
			deadline := time.Now().Add(time.Hour)
			if _, err := service.PublishPreparedMemoryRead(ctx, initial, deadline); err != nil {
				test.Fatal(err)
			}
			counted := &countedMemoryAnchorStore{PostgresStore: storage, graphFailure: scenario.err}
			service.meta = counted
			got, err := service.PrepareIncrementalMemoryRead(ctx, repo, request, deadline, initial)
			if errors.Is(scenario.err, domain.ErrIntegrity) {
				if !errors.Is(err, domain.ErrIntegrity) || got != nil || counted.sourceReads != 1 {
					test.Fatal("corruption was hidden by full fallback", counted.sourceReads, err)
				}
			} else if err != nil || got.input.Header.Version != domain.MemoryCaptureVersion || got.input.ID != initial.input.ID || counted.sourceReads != 2 {
				test.Fatal("explicit journal reset did not restart a baseline", counted.sourceReads, err)
			}
		})
	}
}
