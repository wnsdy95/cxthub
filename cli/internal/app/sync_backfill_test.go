package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub/cli/internal/domain"
	"github.com/wnsdy95/cxthub/cli/internal/ports/inbound"
)

type historicalRemote struct {
	retryPendingRemote
	fail         domain.ContentHash
	refs         int
	memories     map[domain.ContentHash]domain.MemoryDigest
	attachments  map[domain.ContentHash]domain.ContentHash
	beforeUpload func()
}

func (r *historicalRemote) RemoteManifest(_ context.Context, repo string) (domain.Manifest, error) {
	return domain.Manifest{RepoID: repo, MemoryAttachments: r.attachments}, nil
}
func (r *historicalRemote) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	if r.beforeUpload != nil {
		r.beforeUpload()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, doc := range docs {
		if doc.Hash == r.fail {
			return errors.New("historical document transport unavailable")
		}
	}
	if len(refs) > 0 {
		r.refs++
	}
	return r.retryPendingRemote.Push(ctx, repo, snaps, docs, refs, force, appendMode)
}

type failingBackfillStore struct{ *storage.FileStore }

func (*failingBackfillStore) StageBackfills(context.Context, string, []domain.Snapshot) error {
	return errors.New("queue disk unavailable")
}

func TestForegroundFailsBeforeRefsWhenRetentionOrPrerequisiteFails(t *testing.T) {
	for _, failQueue := range []bool{false, true} {
		t.Run(map[bool]string{false: "document", true: "queue"}[failQueue], func(t *testing.T) {
			ctx := context.Background()
			st := storage.NewFileStore(t.TempDir())
			repo := string(domain.HashContent([]byte(t.Name())))
			head := pendingRetrySnapshot(t, st, repo, "current")
			pendingRetrySnapshot(t, st, repo, "archive")
			if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: head}); err != nil {
				t.Fatal(err)
			}
			remote := &historicalRemote{fail: head}
			svc := newTestSyncService(st, remote, nil)
			if failQueue {
				svc = newTestSyncService(&failingBackfillStore{st}, remote, nil)
			}
			if _, err := svc.Push(ctx, inbound.SyncInput{RepoID: repo, ForegroundOnly: true}); err == nil || remote.refs != 0 {
				t.Fatal("failed prerequisite published a ref", remote.refs, err)
			}
		})
	}
}

func TestHistoricalDelayedAcknowledgmentPreservesChangedJob(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	id := pendingRetrySnapshot(t, st, repo, "archive")
	snap, _ := st.GetSnapshot(ctx, id)
	if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	remote := &historicalRemote{}
	remote.beforeUpload = func() {
		remote.beforeUpload = nil
		snap.Message = "new projection queued during upload"
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
			t.Fatal(err)
		}
	}
	svc := newTestSyncService(st, remote, nil)
	in := inbound.SyncInput{RepoID: repo}
	out, err := svc.SyncHistorical(ctx, in, 1)
	if err != nil || out.Pending != 1 || remote.refs != 0 {
		t.Fatal("stale acknowledgment lost newer job", out, err)
	}
	if _, err := svc.SyncHistorical(ctx, in, 1); err != nil {
		t.Fatal(err)
	}
	jobs, err := st.ListBackfills(ctx, repo)
	if err != nil || len(jobs) != 0 {
		t.Fatal(jobs, err)
	}
}

func TestForegroundUsesServerMemoryProofWithoutReadingLargeLocalDigest(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	id, hash := domain.HashContent([]byte("snapshot")), domain.HashContent([]byte("memory"))
	snaps := []domain.Snapshot{{ID: id, DocHash: id, MemoryHash: hash}}
	svc := newTestSyncService(st, nil, nil)
	// There deliberately is no local body. Without a verified server attachment,
	// selection must fail; an equal attachment already includes its prerequisites.
	if _, _, err := svc.snapshotDependencyClosure(ctx, snaps, []domain.ContentHash{id}, nil, nil); err == nil {
		t.Fatal("missing dependency accepted")
	}
	selected, retained, err := svc.snapshotDependencyClosure(ctx, snaps, []domain.ContentHash{id}, nil, map[domain.ContentHash]domain.ContentHash{id: hash})
	if err != nil || len(selected) != 1 || len(retained) != 0 {
		t.Fatal(selected, retained, err)
	}
}

func TestHistoricalCancellationKeepsDurableRetryAndReleasesWorker(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	id := pendingRetrySnapshot(t, st, repo, "archive")
	snap, _ := st.GetSnapshot(ctx, id)
	if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	remote := &historicalRemote{beforeUpload: cancel}
	out, err := newTestSyncService(st, remote, nil).SyncHistorical(cancelled, inbound.SyncInput{RepoID: repo}, 1)
	if !errors.Is(err, context.Canceled) || out.Failed != 1 {
		t.Fatal(out, err)
	}
	jobs, err := st.ListBackfills(ctx, repo)
	if err != nil || len(jobs) != 1 || jobs[0].Reason != "cancelled" || jobs[0].Attempts != 1 {
		t.Fatal(jobs, err)
	}
	if entered, err := st.WithBackfillWorker(ctx, func() error { return nil }); err != nil || !entered {
		t.Fatal("cancelled worker retained its lock", entered, err)
	}
}
func (r *historicalRemote) PushMemory(_ context.Context, _ string, digest domain.MemoryDigest) error {
	hash, err := domain.MemoryDigestHash(digest)
	if err != nil {
		return err
	}
	if r.attachments[digest.SnapshotID] == hash {
		return nil
	}
	if r.attachments[digest.SnapshotID] != digest.PreviousMemoryHash {
		return domain.ErrSyncConflict
	}
	r.memories[hash] = digest
	r.attachments[digest.SnapshotID] = hash
	return nil
}

func TestForegroundPublishesBeforeRetainedFailureAndRestartsBackfill(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st := storage.NewFileStore(root)
	repo := string(domain.HashContent([]byte(t.Name())))
	head := pendingRetrySnapshot(t, st, repo, "current")
	archive := pendingRetrySnapshot(t, st, repo, "retained")
	memory := domain.MemoryDigest{SnapshotID: archive, Summary: "retained project decision"}
	hash := putMemoryObject(t, ctx, st, memory)
	snap, _ := st.GetSnapshot(ctx, archive)
	snap.MemoryHash = hash
	if err := st.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := st.PutRef(ctx, domain.Ref{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: head}); err != nil {
		t.Fatal(err)
	}
	remote := &historicalRemote{fail: archive, memories: map[domain.ContentHash]domain.MemoryDigest{}, attachments: map[domain.ContentHash]domain.ContentHash{}}
	in := inbound.SyncInput{RepoID: repo, ForegroundOnly: true}
	out, err := newTestSyncService(st, remote, nil).Push(ctx, in)
	if err != nil || out.BackfillPending != 1 || remote.refs != 1 || !remote.docs[head] || remote.docs[archive] {
		t.Fatalf("foreground waited for archive: %+v refs=%d err=%v", out, remote.refs, err)
	}
	if pin, err := st.HasBackfillPin(ctx, archive); err != nil || !pin {
		t.Fatal("archive not retained", err)
	}
	reopened := storage.NewFileStore(root)
	svc := newTestSyncService(reopened, remote, nil)
	backlog, err := svc.SyncHistorical(ctx, in, 1)
	if err != nil || backlog.Failed != 1 || backlog.Pending != 1 || remote.refs != 1 {
		t.Fatal("failed retry lost obligation or changed refs", backlog, err)
	}
	jobs, err := reopened.ListBackfills(ctx, repo)
	if err != nil || len(jobs) != 1 || jobs[0].Attempts != 1 || jobs[0].Reason != "unavailable" {
		t.Fatal("retry status", jobs, err)
	}
	// Advance only the synthetic retry schedule through the queue CAS API.
	next := jobs[0]
	next.Version++
	next.NextAttempt = time.Now().Add(-time.Second).UTC()
	if err := reopened.UpdateBackfill(ctx, jobs[0], &next); err != nil {
		t.Fatal(err)
	}
	remote.fail = ""
	backlog, err = svc.SyncHistorical(ctx, in, 1)
	if err != nil || backlog.Completed != 1 || backlog.Pending != 0 || !remote.docs[archive] || remote.refs != 1 {
		t.Fatal("restart did not finish archive only", backlog, err)
	}
	if remote.attachments[archive] != hash || remote.memories[hash].Summary != memory.Summary {
		t.Fatal("backfill lost attached memory")
	}
	if _, err := reopened.GetDoc(ctx, archive); err != nil {
		t.Fatal("archival source was deleted", err)
	}
}

func TestForegroundIncludesMemoryOnlySourcesAndAllGraphParents(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	ids := make([]domain.ContentHash, 10)
	snaps := make([]domain.Snapshot, len(ids))
	for i := range ids {
		ids[i] = domain.HashContent([]byte{byte(i)})
		snaps[i] = domain.Snapshot{RepoID: repo, ID: ids[i], DocHash: ids[i]}
	}
	snaps[0].Parents = []domain.ContentHash{ids[1]}
	snaps[0].GraftParents = []domain.ContentHash{ids[2]}
	old := domain.MemoryDigest{SnapshotID: ids[0], Fragments: []domain.MemoryFragment{{SourceSnapshot: ids[3], Summary: "old provenance"}}}
	oldHash := putMemoryObject(t, ctx, st, old)
	current := domain.MemoryDigest{SnapshotID: ids[0], PreviousMemoryHash: oldHash, Fragments: []domain.MemoryFragment{{SourceSnapshot: ids[4], Summary: "current provenance"}}, GraftCoverage: &domain.MemoryGraftCoverage{ProjectionVersion: domain.MemoryProjectionVersion, PinnedSources: []domain.ContentHash{ids[5]}}}
	snaps[0].MemoryHash = putMemoryObject(t, ctx, st, current)
	snaps[4].Parents = []domain.ContentHash{ids[6]}
	// An unrelated snapshot with its own memory is still eligible for backfill.
	snaps[9].MemoryHash = putMemoryObject(t, ctx, st, domain.MemoryDigest{SnapshotID: ids[9], Summary: "retained"})
	if err := st.PutPending(ctx, domain.Pending{RepoID: repo, SessionID: "live", Branch: "main", Provider: domain.ProviderCodex, Target: ids[8]}); err != nil {
		t.Fatal(err)
	}
	foreground, retained, err := newTestSyncService(st, nil, nil).foregroundSnapshots(ctx, repo, "", snaps, []domain.Ref{{Target: ids[0]}}, []domain.HistoryEvent{{Source: ids[7]}}, nil)
	if err != nil || len(foreground) != 9 || len(retained) != 1 || retained[0].ID != ids[9] {
		t.Fatal("dependency selection lost provenance", len(foreground), retained, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := newTestSyncService(st, nil, nil).foregroundSnapshots(cancelled, repo, "", snaps, nil, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled selection accepted", err)
	}
}
