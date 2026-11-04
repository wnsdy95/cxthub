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

type graphProjectionReadStore struct {
	*pendingStateStore
	outbound.GraphProjectionStore
	outbound.GraphQualificationStore
	result                domain.GraphProjectionRead
	readErr               error
	reads, notices        int
	repairs               []domain.GraphProjectionCorruption
	repairErr             error
	qualificationErr      error
	qualificationMismatch bool
	qualificationDigest   domain.ContentHash
	candidateReads        int
	servingRepairs        int
}

func (storage *graphProjectionReadStore) ReadGraphQualification(_ context.Context, key domain.GraphQualificationKey) (domain.GraphQualificationResult, error) {
	result := domain.GraphQualificationResult{GraphQualificationKey: key, Expected: storage.qualificationDigest, Actual: storage.qualificationDigest}
	if storage.qualificationMismatch {
		result.Actual = hh("different bundle")
	}
	return result, storage.qualificationErr
}

func (storage *graphProjectionReadStore) RequestGraphProjectionRepair(_ context.Context, failure domain.GraphProjectionCorruption) error {
	storage.repairs = append(storage.repairs, failure)
	return storage.repairErr
}

func (storage *graphProjectionReadStore) ReadGraphProjection(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error) {
	storage.candidateReads++
	return storage.result, storage.readErr
}

func (storage *graphProjectionReadStore) ReadGraphProjectionStatus(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error) {
	result := storage.result
	result.Parts = nil
	return result, storage.readErr
}

func (storage *graphProjectionReadStore) qualifiedResult(ctx context.Context) (domain.GraphProjectionRead, error) {
	result := storage.result
	if result.Qualification == nil {
		qualification, err := storage.ReadGraphQualification(ctx, result.QualificationKey())
		if err != nil && !errors.Is(err, domain.ErrNotFound) {
			return result, err
		}
		if err == nil {
			result.Qualification = &qualification
		}
	}
	return result, storage.readErr
}

func (storage *graphProjectionReadStore) ReadQualifiedGraphProjection(ctx context.Context, _ domain.ContentHash) (domain.GraphProjectionRead, error) {
	storage.reads++
	return storage.qualifiedResult(ctx)
}

func (storage *graphProjectionReadStore) ReadQualifiedGraphProjectionStatus(ctx context.Context, _ domain.ContentHash) (domain.GraphProjectionRead, error) {
	storage.notices++
	result, err := storage.qualifiedResult(ctx)
	result.Parts = nil
	return result, err
}

func (storage *graphProjectionReadStore) RequestGraphServingRepair(ctx context.Context, failure domain.GraphProjectionCorruption) error {
	storage.servingRepairs++
	return storage.RequestGraphProjectionRepair(ctx, failure)
}

func newGraphProjectionRead(t *testing.T) (*Service, *graphProjectionReadStore, domain.ContentHash, domain.RepositoryView) {
	t.Helper()
	service, fs := newFsckSvc(t)
	repo, ctx := hh(t.Name()), systemTestContext()
	if _, err := fs.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	snapshot := domain.Snapshot{ID: hh(t.Name() + "capture"), RepoID: repo, DocHash: hh(t.Name() + "capture"),
		CreatedAt: time.Unix(123, 0).UTC(), Branch: "main", Provider: domain.ProviderCodex, SessionID: "live"}
	if err := fs.PutSnapshot(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := fs.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "live", Branch: "main", Target: snapshot.ID}); err != nil {
		t.Fatal(err)
	}
	want, err := service.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	parts, err := domain.EncodeGraphViewProjection(ctx, repo, want)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := domain.GraphViewProjectionManifestDigest(ctx, repo, parts)
	if err != nil {
		t.Fatal(err)
	}
	source := domain.RepositoryViewSourceCheckpoint{Version: 1, Scope: domain.RepositoryGraphSourceScope, RepoID: repo,
		Epoch: "11111111-1111-4111-8111-111111111111", Sequence: 4}
	storage := &graphProjectionReadStore{pendingStateStore: &pendingStateStore{FSStore: fs}, qualificationDigest: digest,
		result: domain.GraphProjectionRead{Enabled: true, Ready: true, Complete: true, Rules: domain.GraphViewProjectionRules, ServingFence: 1,
			Dataset: "22222222-2222-4222-8222-222222222222", Source: source, Applied: source, Parts: parts}}
	service.meta = storage
	if err := service.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	return service, storage, repo, want
}

func TestGraphProjectionReadsUseOneCompleteBundleWithoutBulk(t *testing.T) {
	service, storage, repo, want := newGraphProjectionRead(t)
	ctx, gate := admissionTestContext(100 * time.Millisecond)
	ctx = inbound.WithSystemActor(ctx)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for range 2 {
		full, err := service.GetRepositoryView(ctx, repo)
		if err != nil {
			t.Fatal(err)
		}
		if full.ReadGeneration == nil || !full.ReadGeneration.Complete || !full.ReadGeneration.Current ||
			!reflect.DeepEqual(full.ReadGeneration, full.Graph.ReadGeneration) {
			t.Fatal("missing coherent generation", full.ReadGeneration)
		}
		full.ReadGeneration, full.Graph.ReadGeneration = nil, nil
		if !reflect.DeepEqual(full, want) {
			t.Fatalf("full bundle differs: got=%+v want=%+v", full, want)
		}
		graph, err := service.QueryGraphState(ctx, repo, "")
		if err != nil || graph.ReadGeneration == nil {
			t.Fatal(err)
		}
		graph.ReadGeneration = nil
		if !reflect.DeepEqual(graph, *want.Graph) {
			t.Fatal("graph differs from full bundle")
		}
		live, err := service.GetPendingView(ctx, repo)
		if err != nil || live.ReadGeneration == nil || !reflect.DeepEqual(live.Graph.ReadGeneration, live.ReadGeneration) {
			t.Fatal("live generation", err)
		}
		live.ReadGeneration, live.Graph.ReadGeneration = nil, nil
		expected, err := pendingViewFromRepository(want)
		if err != nil || !reflect.DeepEqual(live, expected) {
			t.Fatal("live metadata differs from bundle", err)
		}
	}
	if storage.snapshotLists != 0 || storage.snapshotReads != 0 || storage.evidenceReads != 0 || service.graphCache.Stats().Entries != 0 {
		t.Fatal("durable read scanned live source", storage.snapshotLists, storage.snapshotReads, storage.evidenceReads)
	}
	release()
	assertResourcesReleased(t, gate)
}

func TestGraphProjectionLagRetainsBundleAndAdvancesNoticeOnly(t *testing.T) {
	service, storage, repo, want := newGraphProjectionRead(t)
	before, err := service.RepositoryChangeState(systemTestContext(), repo)
	if err != nil || before.GraphProjection == nil {
		t.Fatal(err)
	}
	storage.result.Ready = false
	storage.result.Source.Sequence++
	after, err := service.RepositoryChangeState(systemTestContext(), repo)
	if err != nil || after.GraphProjection.State != "catching_up" || before.Equal(after) ||
		before.GraphProjection.Generation != after.GraphProjection.Generation || before.GraphProjection.AppliedEpoch != after.GraphProjection.AppliedEpoch {
		t.Fatal("moving source rewrote completed generation", before, after, err)
	}
	if storage.reads != 0 || storage.notices != 2 {
		t.Fatal("notice read body", storage.reads, storage.notices)
	}
	full, err := service.GetRepositoryView(systemTestContext(), repo)
	if err != nil || full.ReadGeneration == nil || full.ReadGeneration.Current || !full.ReadGeneration.Complete ||
		full.ReadGeneration.SourceEpoch == full.ReadGeneration.AppliedEpoch {
		t.Fatal("lag hidden", full.ReadGeneration, err)
	}
	full.ReadGeneration, full.Graph.ReadGeneration = nil, nil
	if !reflect.DeepEqual(full, want) {
		t.Fatal("lagged graph mixed current metadata")
	}
}

func TestGraphProjectionUnavailableNeverFallsBack(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*graphProjectionReadStore)
		want   error
	}{
		{"disabled", func(storage *graphProjectionReadStore) {
			storage.result.Enabled = false
			storage.result.Ready = false
			storage.result.Complete = false
			storage.result.Parts = nil
		}, domain.ErrGraphProjectionNotReady},
		{"missing", func(storage *graphProjectionReadStore) {
			storage.result.Ready = false
			storage.result.Complete = false
			storage.result.Parts = nil
		}, domain.ErrGraphProjectionNotReady},
		{"failed", func(storage *graphProjectionReadStore) {
			storage.result.NeedsAttention = true
			storage.result.Ready = false
			storage.result.Complete = false
			storage.result.Parts = nil
		}, domain.ErrGraphProjectionFailed},
		{"corrupt", func(storage *graphProjectionReadStore) { storage.result.Parts = storage.result.Parts[:1] }, domain.ErrIntegrity},
		{"store-error", func(storage *graphProjectionReadStore) { storage.readErr = domain.ErrIntegrity }, domain.ErrIntegrity},
		{"foreign", func(storage *graphProjectionReadStore) { storage.result.Source.RepoID = hh("foreign") }, domain.ErrIntegrity},
	} {
		t.Run(test.name, func(t *testing.T) {
			service, storage, repo, _ := newGraphProjectionRead(t)
			test.change(storage)
			got, err := service.GetRepositoryView(systemTestContext(), repo)
			if !errors.Is(err, test.want) || !reflect.DeepEqual(got, domain.RepositoryView{}) {
				t.Fatal("unavailable result exposed or fell back", got, err)
			}
			if storage.snapshotLists != 0 || storage.evidenceReads != 0 {
				t.Fatal("unavailable projection fell back to source")
			}
		})
	}
}

func TestGraphProjectionReauthorizesAfterAdmission(t *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(t)
	ctx, gate := admissionTestContext(time.Second)
	_, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, second, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	authorized := make(chan struct{}, 1)
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		if requested != repo {
			return bound, domain.ErrIntegrity
		}
		authorized <- struct{}{}
		return bound, domain.ErrForbidden
	})
	done := make(chan error, 1)
	go func() { _, err := service.GetRepositoryView(ctx, repo); done <- err }()
	waitResourceQueue(t, gate, ResourceInteractive, 1)
	select {
	case <-authorized:
		t.Fatal("checked access before admission")
	default:
	}
	release()
	if err := <-done; !errors.Is(err, domain.ErrForbidden) || storage.reads != 0 {
		t.Fatal("revoked access read bundle", err)
	}
	if _, err := service.GetRepositoryView(context.Background(), repo); !errors.Is(err, domain.ErrForbidden) {
		t.Fatal("accepted unauthenticated internal read", err)
	}
	second()
	assertResourcesReleased(t, gate)
}

func TestGraphProjectionReadConfigurationIsIndependentOfWorker(t *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(t)
	if len(service.graphProjectionRepos) != 0 {
		t.Fatal("serving configuration enabled worker")
	}
	if err := service.ConfigureGraphReads([]domain.ContentHash{repo, "invalid"}); err == nil || !service.graphReadRepos[repo] {
		t.Fatal("bad config replaced selection", err)
	}
	if err := service.ConfigureGraphReads(nil); err != nil || len(service.graphReadRepos) != 0 {
		t.Fatal(err)
	}
	if _, err := service.GetRepositoryView(systemTestContext(), repo); err != nil {
		t.Fatal(err)
	}
	if storage.reads != 0 || storage.snapshotLists == 0 {
		t.Fatal("disabled rollout did not return to direct path")
	}
	unsupported, _ := newFsckSvc(t)
	if err := unsupported.ConfigureGraphReads([]domain.ContentHash{repo}); !errors.Is(err, domain.ErrCatalogUnsupported) {
		t.Fatal(err)
	}
}

func TestGraphProjectionNoticeRejectsForeignSource(t *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(t)
	storage.result.Source.RepoID = hh("another repository")
	storage.result.Applied.RepoID = storage.result.Source.RepoID
	got, err := service.RepositoryChangeState(systemTestContext(), repo)
	if !errors.Is(err, domain.ErrIntegrity) || !reflect.DeepEqual(got, domain.RepositoryChangeState{}) {
		t.Fatal("notice accepted foreign publication identity", got, err)
	}
	if storage.reads != 0 || storage.notices != 1 {
		t.Fatal("notice read body", storage.reads, storage.notices)
	}
}

func TestGraphReadDirectReauthorizesAfterAdmission(t *testing.T) {
	service, storage, repo, _ := newGraphProjectionRead(t)
	if err := service.ConfigureGraphReads(nil); err != nil {
		t.Fatal(err)
	}
	ctx, gate := admissionTestContext(time.Second)
	_, release, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, second, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer second()
	ctx = inbound.WithDocumentReadAuthorizer(ctx, func(bound context.Context, requested domain.ContentHash) (context.Context, error) {
		return bound, domain.ErrForbidden
	})
	done := make(chan error, 1)
	go func() { _, err := service.GetRepositoryView(ctx, repo); done <- err }()
	waitResourceQueue(t, gate, ResourceInteractive, 1)
	release()
	if err := <-done; !errors.Is(err, domain.ErrForbidden) || storage.snapshotLists != 0 || storage.reads != 0 {
		t.Fatal("direct read skipped current authorization", err)
	}
	second()
	assertResourcesReleased(t, gate)
}
