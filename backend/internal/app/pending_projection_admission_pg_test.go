//go:build postgres

package app

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGPendingProjectionBlockedClaimReleasesBulkAdmission(t *testing.T) {
	service, storage, repo := collaborationPG(t)
	ctx, gate := admissionTestContext(resourceWaitLimit)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := storage.ConfigurePendingProjection(cleanup, repo, false); err != nil {
			t.Error(err)
		}
	})
	if err := service.ConfigurePendingProjections(ctx, []domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	blocker, err := pgx.Connect(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = blocker.Close(cleanup)
	}()
	transaction, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = transaction.Rollback(cleanup)
	}()
	var head, priorFence int64
	if err := transaction.QueryRow(ctx, `SELECT head_seq FROM repository_catalog_state WHERE repo_id=$1 FOR UPDATE`, string(repo)).Scan(&head); err != nil {
		t.Fatal(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT fence FROM pending_projection_work WHERE repo_id=$1`, string(repo)).Scan(&priorFence); err != nil {
		t.Fatal(err)
	}
	worker, cancelWorker := context.WithTimeout(ctx, 5*time.Second)
	defer cancelWorker()
	finished := make(chan error, 1)
	go func() { finished <- service.ProcessPendingProjections(worker, 1) }()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := transaction.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_locks WHERE NOT granted AND $1::integer=ANY(pg_blocking_pids(pid))
		)`, int32(blocker.PgConn().PID())).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-finished:
			t.Fatalf("claim exited before reaching catalog lock: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	gate.mu.Lock()
	active, bulk := gate.active, gate.bulk
	gate.mu.Unlock()
	if active != 1 || bulk != 1 {
		t.Fatalf("blocked claim did not own bulk admission: active=%d bulk=%d", active, bulk)
	}
	_, releaseInteractive, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatalf("blocked claim consumed interactive reserve: %v", err)
	}
	releaseInteractive()
	nextBulk := make(chan error, 1)
	go func() {
		nextBulk <- resourceWork(ctx, ResourceBulk, func(context.Context) error { return nil })
	}()
	waitResourceQueue(t, gate, ResourceBulk, 1)
	select {
	case err := <-finished:
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "55P03" || worker.Err() != nil {
			t.Fatalf("blocked claim did not return its independent lock timeout: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("blocked claim exceeded caller deadline", ctx.Err())
	}
	select {
	case err := <-nextBulk:
		if err != nil {
			t.Fatalf("catalog blocker prevented unrelated bulk work after timeout: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("timed-out claim retained bulk admission", ctx.Err())
	}
	if ctx.Err() != nil {
		t.Fatalf("claim timeout canceled unrelated work: %v", ctx.Err())
	}
	assertResourcesReleased(t, gate)
	var fence int64
	var phase, reason string
	var nextAttempt time.Time
	var unleased bool
	if _, err := transaction.Exec(ctx, `SET LOCAL lock_timeout = '1s'`); err != nil {
		t.Fatal(err)
	}
	if err := transaction.QueryRow(ctx, `SELECT fence,phase,lease_until IS NULL,reason,next_attempt
		FROM pending_projection_work WHERE repo_id=$1 FOR UPDATE`, string(repo)).Scan(&fence, &phase, &unleased, &reason, &nextAttempt); err != nil {
		t.Fatalf("timed-out claim retained its work-row lock: %v", err)
	}
	if fence != priorFence+1 || phase != "capture" || !unleased || reason != "temporary_failure" || !nextAttempt.After(time.Now()) {
		t.Fatalf("timed-out claim lost its retry receipt: fence=%d prior=%d phase=%s unleased=%v reason=%s next=%v", fence, priorFence, phase, unleased, reason, nextAttempt)
	}
	if err := transaction.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-time.After(time.Until(nextAttempt) + 10*time.Millisecond):
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	job, err := storage.ClaimPendingProjection(ctx, time.Minute)
	if err != nil || job.RepoID != repo || job.Phase != "capture" || job.Fence <= priorFence {
		t.Fatalf("claim did not recover after blocker released: job=%+v err=%v", job, err)
	}
}
