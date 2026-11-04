//go:build postgres

package store

import (
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGPendingProjectionRebuildsChangedRulesWithoutSourceWrite(t *testing.T) {
	storage, ctx, repo, ready := pendingProjectionEmptyDeltaReadyPG(t)
	if _, err := storage.pool.Exec(ctx, `UPDATE pending_projection_ready SET rules=rules+1 WHERE repo_id=$1`, string(repo)); err != nil {
		t.Fatal(err)
	}
	read, err := storage.ReadPendingProjection(ctx, repo)
	if err != nil || read.Ready || read.Parts != nil || read.Applied != ready.Applied {
		t.Fatalf("old rule version was served: %+v err=%v", read, err)
	}
	job := pendingProjectionClaimPG(t, ctx, storage, repo)
	if job.Dataset == ready.Dataset || job.AppliedSequence != -1 || job.Phase != "capture" || job.Cursor != "" {
		t.Fatalf("rules change did not restart capture: %+v", job)
	}
	job = pendingProjectionCapturePG(t, ctx, storage, job, 2)
	view, parts := pendingProjectionPartsPG(t, ctx, storage, job)
	if err := storage.PublishPendingProjection(ctx, job, parts); err != nil {
		t.Fatal(err)
	}
	current := pendingProjectionReadPG(t, ctx, storage, job, view)
	if current.Source != ready.Source || current.Applied != ready.Applied {
		t.Fatalf("rebuild changed source: before=%+v after=%+v", ready.Source, current.Source)
	}
	if _, err := storage.ClaimPendingProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("unchanged rebuilt projection remained runnable: %v", err)
	}
}
