package app

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/storage"
	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

type statusCodeTestError int

func (e statusCodeTestError) Error() string   { return fmt.Sprintf("server answered %d", int(e)) }
func (e statusCodeTestError) StatusCode() int { return int(e) }

// pacedHistoricalRemote records which retained documents were sent and can
// answer every upload with a server status or take a fixed time.
type pacedHistoricalRemote struct {
	historicalRemote
	status   int
	delay    time.Duration
	uploaded []domain.ContentHash
}

func (r *pacedHistoricalRemote) Push(ctx context.Context, repo string, snaps []domain.Snapshot, docs []domain.SessionDoc, refs []domain.Ref, force, appendMode bool) error {
	for _, doc := range docs {
		r.uploaded = append(r.uploaded, doc.Hash)
	}
	if r.delay > 0 && len(docs) > 0 {
		time.Sleep(r.delay)
	}
	if r.status != 0 {
		return fmt.Errorf("upload document: %w", statusCodeTestError(r.status))
	}
	return r.historicalRemote.Push(ctx, repo, snaps, docs, refs, force, appendMode)
}

func stageRetained(t *testing.T, st *storage.FileStore, repo string, markers ...string) []domain.ContentHash {
	t.Helper()
	ctx := context.Background()
	ids := make([]domain.ContentHash, 0, len(markers))
	for _, marker := range markers {
		id := pendingRetrySnapshot(t, st, repo, marker)
		snap, err := st.GetSnapshot(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func TestHistoricalPauseUploadsNothingAndKeepsRetention(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	ids := stageRetained(t, st, repo, "archive")
	if _, err := st.UpdateHistoricalUploadControl(ctx, repo, func(control domain.HistoricalUploadControl) domain.HistoricalUploadControl {
		control.PausedAt = time.Now().UTC()
		return control
	}); err != nil {
		t.Fatal(err)
	}
	remote := &pacedHistoricalRemote{}
	svc := newTestSyncService(st, remote, nil)
	out, err := svc.SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 8)
	if err != nil || !out.Paused || out.Busy || out.Pending != 1 || out.Completed != 0 || len(remote.uploaded) != 0 {
		t.Fatal("paused queue uploaded", out, remote.uploaded, err)
	}
	if pinned, err := st.HasBackfillPin(ctx, ids[0]); err != nil || !pinned {
		t.Fatal("pause released the retention pin", pinned, err)
	}
	if _, err := st.UpdateHistoricalUploadControl(ctx, repo, func(control domain.HistoricalUploadControl) domain.HistoricalUploadControl {
		control.PausedAt = time.Time{}
		return control
	}); err != nil {
		t.Fatal(err)
	}
	out, err = svc.SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 8)
	if err != nil || out.Paused || out.Completed != 1 || out.Pending != 0 {
		t.Fatal("resumed queue did not upload", out, err)
	}
}

func TestHistoricalBusyServerEndsRoundAndHoldsUploads(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	stageRetained(t, st, repo, "first", "second", "third")
	remote := &pacedHistoricalRemote{status: 503}
	svc := newTestSyncService(st, remote, nil)
	started := time.Now()
	out, err := svc.SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 8)
	if err != nil || out.Failed != 1 || len(remote.uploaded) != 1 || out.Pending != 3 {
		t.Fatal("busy server was asked for more than one snapshot", out, remote.uploaded, err)
	}
	control, err := st.HistoricalUploadControl(ctx, repo)
	if err != nil || control.HoldUntil.Before(started.Add(historicalBusyHold)) || !out.NextAttempt.Equal(control.HoldUntil) {
		t.Fatal("busy server did not hold uploads", control, out.NextAttempt, err)
	}
	jobs, err := st.ListBackfills(ctx, repo)
	if err != nil || len(jobs) != 3 {
		t.Fatal(jobs, err)
	}
	for _, job := range jobs {
		if job.Attempts == 1 && job.Reason != "unavailable" {
			t.Fatal("busy failure lost its retry reason", job)
		}
	}
	// A new round during the hold, for example from another wake, sends nothing.
	remote.status = 0
	if out, err = svc.SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 8); err != nil || len(remote.uploaded) != 1 || out.Completed != 0 {
		t.Fatal("hold ignored", out, remote.uploaded, err)
	}
}

func TestHistoricalLongUploadRestsBeforeTheNext(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	stageRetained(t, st, repo, "first", "second")
	remote := &pacedHistoricalRemote{delay: historicalUploadRestFloor + 50*time.Millisecond}
	svc := newTestSyncService(st, remote, nil)
	out, err := svc.SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 8)
	finished := time.Now()
	if err != nil || out.Completed != 1 || out.Pending != 1 || len(remote.uploaded) != 1 {
		t.Fatal("long upload was followed by another at once", out, remote.uploaded, err)
	}
	control, err := st.HistoricalUploadControl(ctx, repo)
	if err != nil || control.HoldUntil.Before(finished.Add(historicalUploadRestFloor-time.Second/10)) || !out.NextAttempt.Equal(control.HoldUntil) {
		t.Fatal("long upload left no rest", control, out.NextAttempt, err)
	}
}

func TestHistoricalShortUploadsNeedNoRest(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	stageRetained(t, st, repo, "first", "second")
	out, err := newTestSyncService(st, &pacedHistoricalRemote{}, nil).SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 8)
	if err != nil || out.Completed != 2 || out.Pending != 0 {
		t.Fatal(out, err)
	}
	if control, err := st.HistoricalUploadControl(ctx, repo); err != nil || !control.HoldUntil.IsZero() {
		t.Fatal("short uploads were held", control, err)
	}
}

func TestHistoricalUploadsFollowCaptureOrder(t *testing.T) {
	ctx := context.Background()
	st := storage.NewFileStore(t.TempDir())
	repo := string(domain.HashContent([]byte(t.Name())))
	// Queue order follows staging, so stage the newer capture first.
	captured := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	var ids []domain.ContentHash
	for index, marker := range []string{"newer", "older"} {
		h, err := st.PutDoc(ctx, domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.Envelope{CIRVersion: "1", SessionOriginID: marker}}})
		if err != nil {
			t.Fatal(err)
		}
		snap := domain.Snapshot{ID: h, DocHash: h, RepoID: repo, Branch: "main", CreatedAt: captured.Add(time.Duration(2-index) * time.Hour)}
		if err := st.PutSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if stored, err := st.GetSnapshot(ctx, h); err != nil || !stored.CreatedAt.Equal(snap.CreatedAt) {
			t.Fatal("fixture capture time not stored", stored.CreatedAt, err)
		}
		if err := st.StageBackfills(ctx, repo, []domain.Snapshot{snap}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, h)
	}
	remote := &pacedHistoricalRemote{}
	if _, err := newTestSyncService(st, remote, nil).SyncHistorical(ctx, inbound.SyncInput{RepoID: repo}, 1); err != nil {
		t.Fatal(err)
	}
	if len(remote.uploaded) != 1 || remote.uploaded[0] != ids[1] {
		t.Fatal("older capture was not uploaded first", remote.uploaded, ids)
	}
}
