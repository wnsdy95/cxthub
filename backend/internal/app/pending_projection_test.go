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

type pendingProjectionReadStore struct {
	*pendingStateStore
	outbound.PendingProjectionStore
	outbound.RepositoryViewSourceStore
	configured []domain.ContentHash
	reads      int
	result     domain.PendingProjectionRead
}

func (storage *pendingProjectionReadStore) ConfigurePendingProjection(_ context.Context, repo domain.ContentHash, enabled bool) error {
	if !enabled {
		return domain.ErrValidation
	}
	storage.configured = append(storage.configured, repo)
	return nil
}

func (storage *pendingProjectionReadStore) ReadPendingProjection(context.Context, domain.ContentHash) (domain.PendingProjectionRead, error) {
	storage.reads++
	return storage.result, nil
}

func newPendingProjectionRead(t *testing.T) (*Service, *pendingProjectionReadStore, domain.ContentHash, domain.PendingStateView) {
	t.Helper()
	service, fs := newFsckSvc(t)
	ctx, repo := systemTestContext(), hh(t.Name())
	if _, err := fs.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.Snapshot{ID: hh(t.Name() + "snapshot"), RepoID: repo, DocHash: hh(t.Name() + "snapshot"), CreatedAt: time.Unix(123, 0), Provider: domain.ProviderCodex, SessionID: "live"}
	if err := fs.PutSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := fs.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "live", Branch: "main", Target: snapshot.ID}); err != nil {
		t.Fatal(err)
	}
	want, err := service.GetPendingState(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := domain.EncodePendingProjection(ctx, repo, want)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryViewSourceScope, RepoID: repo, Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 4}
	storage := &pendingProjectionReadStore{pendingStateStore: &pendingStateStore{FSStore: fs}, result: domain.PendingProjectionRead{Enabled: true, Ready: true, Dataset: "22222222-2222-4222-8222-222222222222", Source: checkpoint, Applied: checkpoint, Parts: parts}}
	service.meta = storage
	if err := service.ConfigurePendingProjections(ctx, []domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	return service, storage, repo, want
}

func TestPendingProjectionReadyReadsDoNotScanSourceOrJoinBulk(t *testing.T) {
	service, storage, repo, want := newPendingProjectionRead(t)
	ctx, gate := admissionTestContext(100 * time.Millisecond)
	ctx = inbound.WithSystemActor(ctx)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for range 3 {
		got, err := service.GetPendingState(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if got.ReadGeneration == nil || !got.ReadGeneration.Complete || got.ReadGeneration.SourceEpoch != got.ReadGeneration.AppliedEpoch || got.ReadGeneration.Rules != 1 {
			t.Fatal("missing completed generation", got.ReadGeneration)
		}
		got.ReadGeneration = nil
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("durable read changed source facts\ngot: %+v\nwant: %+v", got, want)
		}
	}
	if storage.reads != 3 || storage.snapshotLists != 0 || storage.snapshotReads != 0 || storage.evidenceReads != 0 || service.graphCache.Stats().Entries != 0 {
		t.Fatal("durable read scanned original metadata", storage)
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestPendingProjectionNotReadyNeverFallsBackOrReturnsStale(t *testing.T) {
	service, storage, repo, _ := newPendingProjectionRead(t)
	for _, alter := range []func(){func() { storage.result.Ready = false }, func() { storage.result.Ready = true; storage.result.Source.Sequence++ }} {
		alter()
		got, err := service.GetPendingState(systemTestContext(), repo)
		if !errors.Is(err, domain.ErrPendingProjectionNotReady) || got.Projection != "" || storage.snapshotLists != 0 {
			t.Fatal("lagged projection became current", got, err)
		}
	}
	storage.result.NeedsAttention = true
	if _, err := service.GetPendingState(systemTestContext(), repo); !errors.Is(err, domain.ErrPendingProjectionFailed) {
		t.Fatal("terminal projection failure disguised as preparation", err)
	}
}

func TestPendingProjectionExplicitRepositoryRolloutAndDisabledFallback(t *testing.T) {
	service, storage, repo, want := newPendingProjectionRead(t)
	storage.result.Enabled = false
	got, err := service.GetPendingState(systemTestContext(), repo)
	if err != nil || !reflect.DeepEqual(got, want) || storage.snapshotLists != 1 {
		t.Fatal("disabled fallback changed", got, err)
	}
	if err := service.ConfigurePendingProjections(systemTestContext(), []domain.ContentHash{hh("different repository")}); err != nil {
		t.Fatal(err)
	}
	storage.result.Enabled = true
	before := storage.reads
	got, err = service.GetPendingState(context.Background(), repo)
	if err != nil || storage.reads != before || got.ReadGeneration != nil {
		t.Fatal("persisted opt-in leaked across current repository allowlist", err)
	}
	if err := service.ConfigurePendingProjections(systemTestContext(), nil); err != nil || len(service.pendingProjectionRepos) != 0 {
		t.Fatal("empty startup selection did not disable reads", err)
	}
}

func TestPendingProjectionReauthorizesAfterAdmissionBeforeCacheRead(t *testing.T) {
	service, storage, repo, _ := newPendingProjectionRead(t)
	ctx, gate := admissionTestContext(time.Second)
	_, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, releaseSecond, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	authorized := make(chan struct{}, 1)
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		if requested != repo {
			return bound, domain.ErrIntegrity
		}
		authorized <- struct{}{}
		return bound, domain.ErrForbidden
	})
	done := make(chan error, 1)
	go func() { _, err := service.GetPendingState(ctx, repo); done <- err }()
	waitResourceQueue(t, gate, ResourceInteractive, 1)
	select {
	case <-authorized:
		t.Fatal("authorization happened before capacity wait")
	default:
	}
	release()
	if err := <-done; !errors.Is(err, domain.ErrForbidden) || storage.reads != 0 {
		t.Fatal("revoked access read cached projection", err)
	}
	if _, err := service.GetPendingState(context.Background(), repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("missing authorizer was accepted", err)
	}
	releaseSecond()
	assertResourcesReleased(t, gate)
}
