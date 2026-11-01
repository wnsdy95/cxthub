package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var pendingEvidenceFailure = errors.New("full graph evidence unavailable")

type pendingStateStore struct {
	*store.FSStore
	snapshotLists int
	snapshotReads int
	evidenceReads int
}

func (storage *pendingStateStore) ListSnapshots(ctx context.Context, repo domain.ContentHash, branch string) ([]domain.Snapshot, error) {
	storage.snapshotLists++
	return storage.FSStore.ListSnapshots(ctx, repo, branch)
}

func (storage *pendingStateStore) GetSnapshot(ctx context.Context, repo, hash domain.ContentHash) (domain.Snapshot, error) {
	storage.snapshotReads++
	return storage.FSStore.GetSnapshot(ctx, repo, hash)
}

func (storage *pendingStateStore) ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error) {
	storage.evidenceReads++
	return nil, pendingEvidenceFailure
}

func TestPendingStateIndependentOfGraphEvidenceAndBulkWork(t *testing.T) {
	svc, storage := newFsckSvc(t)
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo, published, live := hh(t.Name()), hh("published"), hh("live")
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main", GitRemoteURL: "https://github.com/cache/projection.git"}); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []domain.ContentHash{published, live} {
		if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: hash, RepoID: repo, DocHash: hash, CreatedAt: time.Unix(123, 0), Branches: []string{"unowned"}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: published}, ""); err != nil {
		t.Fatal(err)
	}
	if err := storage.PutPending(ctx, repo, domain.Pending{RepoID: repo, SessionID: "live", Target: live, Branch: "main"}); err != nil {
		t.Fatal(err)
	}
	spy := &pendingStateStore{FSStore: storage}
	svc.meta = spy
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	for revision := range 2 {
		pending, err := svc.GetPendingState(ctx, repo)
		if err != nil || pending.Revision.Pending != uint64(revision) || len(pending.OrphanSessions) != 1 || len(pending.Snapshots) != 1 || pending.Snapshots[0].ID != live || len(pending.Snapshots[0].Branches) != 0 || !pending.Pending[0].UpdatedAt.Equal(time.Unix(123, 0)) {
			t.Fatalf("cold On Hold depended on bulk graph: %+v %v", pending, err)
		}
		if err := storage.AdvanceRepositoryRevision(ctx, repo, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := storage.AdvanceEvidenceRevision(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := storage.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: live}, published); err != nil {
		t.Fatal(err)
	}
	if err := storage.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	changed, err := svc.GetPendingState(ctx, repo)
	if err != nil || changed.Revision != (domain.RepositoryRevision{Graph: 1, Pending: 2, Evidence: 1}) || len(changed.OrphanSessions) != 0 || len(changed.HoldCounts) != 0 {
		t.Fatalf("fresh publication generation not classified: %+v %v", changed, err)
	}
	if spy.snapshotLists != 3 || spy.snapshotReads != 0 || spy.evidenceReads != 0 || svc.graphCache.Stats().Entries != 0 {
		t.Fatalf("not one metadata scan per independent projection: %+v", spy)
	}
	release()
	if _, err := svc.GetPendingView(ctx, repo); !errors.Is(err, pendingEvidenceFailure) || spy.evidenceReads != 1 {
		t.Fatalf("legacy complete graph silently downgraded: %v", err)
	}
	assertResourcesReleased(t, gate)
}
