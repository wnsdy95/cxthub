//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocRangeEnqueueErrorClassification(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	parents := []struct {
		name string
		ctx  context.Context
	}{
		{name: "live_parent", ctx: context.Background()},
		{name: "canceled_parent", ctx: canceled},
		{name: "expired_parent", ctx: expired},
	}
	unknown := errors.New("unexpected enqueue failure")
	lockTimeout := &pgconn.PgError{Code: "55P03", Message: "lock timeout"}
	otherPostgres := &pgconn.PgError{Code: "XX001", Message: "data corrupted"}
	wrappedNotFound := fmt.Errorf("enqueue: %w", domain.ErrNotFound)
	wrappedIntegrity := fmt.Errorf("enqueue: %w", domain.ErrIntegrity)
	wrappedUnknown := fmt.Errorf("enqueue: %w", unknown)
	wrappedCanceled := fmt.Errorf("enqueue: %w", context.Canceled)
	wrappedDeadline := fmt.Errorf("enqueue: %w", context.DeadlineExceeded)
	wrappedLockTimeout := fmt.Errorf("enqueue: %w", lockTimeout)
	wrappedOtherPostgres := fmt.Errorf("enqueue: %w", otherPostgres)
	cases := []struct {
		name    string
		failure error
		want    [3]error
	}{
		{name: "success", want: [3]error{nil, nil, nil}},
		{name: "not_found", failure: domain.ErrNotFound, want: [3]error{domain.ErrNotFound, domain.ErrNotFound, domain.ErrNotFound}},
		{name: "wrapped_not_found", failure: wrappedNotFound, want: [3]error{wrappedNotFound, wrappedNotFound, wrappedNotFound}},
		{name: "integrity", failure: domain.ErrIntegrity, want: [3]error{domain.ErrIntegrity, domain.ErrIntegrity, domain.ErrIntegrity}},
		{name: "wrapped_integrity", failure: wrappedIntegrity, want: [3]error{wrappedIntegrity, wrappedIntegrity, wrappedIntegrity}},
		{name: "unknown", failure: unknown, want: [3]error{unknown, unknown, unknown}},
		{name: "wrapped_unknown", failure: wrappedUnknown, want: [3]error{wrappedUnknown, wrappedUnknown, wrappedUnknown}},
		{name: "context_canceled", failure: context.Canceled, want: [3]error{context.Canceled, context.Canceled, context.DeadlineExceeded}},
		{name: "wrapped_context_canceled", failure: wrappedCanceled, want: [3]error{wrappedCanceled, context.Canceled, context.DeadlineExceeded}},
		{name: "context_deadline", failure: context.DeadlineExceeded, want: [3]error{domain.ErrResourceOverloaded, context.Canceled, context.DeadlineExceeded}},
		{name: "wrapped_context_deadline", failure: wrappedDeadline, want: [3]error{domain.ErrResourceOverloaded, context.Canceled, context.DeadlineExceeded}},
		{name: "postgres_lock_timeout", failure: lockTimeout, want: [3]error{domain.ErrResourceOverloaded, domain.ErrResourceOverloaded, domain.ErrResourceOverloaded}},
		{name: "wrapped_postgres_lock_timeout", failure: wrappedLockTimeout, want: [3]error{domain.ErrResourceOverloaded, domain.ErrResourceOverloaded, domain.ErrResourceOverloaded}},
		{name: "other_postgres", failure: otherPostgres, want: [3]error{otherPostgres, otherPostgres, otherPostgres}},
		{name: "wrapped_other_postgres", failure: wrappedOtherPostgres, want: [3]error{wrappedOtherPostgres, wrappedOtherPostgres, wrappedOtherPostgres}},
	}
	for parentIndex, parent := range parents {
		for _, testCase := range cases {
			t.Run(parent.name+"/"+testCase.name, func(t *testing.T) {
				want := testCase.want[parentIndex]
				if got := docRangeEnqueueError(parent.ctx, testCase.failure); got != want {
					t.Fatalf("classification replaced the wrong error: failure=%v, parent=%v, got=%v, want=%v", testCase.failure, parent.ctx.Err(), got, want)
				}
			})
		}
	}
}

type docRangeEnqueueBudgetPGFixture struct {
	storage         *PostgresStore
	ctx             context.Context
	repo, doc, base domain.ContentHash
}

type docRangeEnqueueBudgetPGResult struct {
	job     domain.DocRangeIndexJob
	err     error
	elapsed time.Duration
}

func docRangeEnqueueBudgetPG(t *testing.T, kind string) docRangeEnqueueBudgetPGFixture {
	t.Helper()
	storage, ctx := chunkReusePG(t)
	repo, _ := docRangeJobPGRepo(t, storage, ctx)
	fixture := docRangeEnqueueBudgetPGFixture{storage: storage, ctx: ctx, repo: repo}
	if kind == "boundary" {
		pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 1, 0)
		fixture.doc, fixture.base = pair[0].hash, pair[1].hash
	} else {
		fixture.doc = docRangeJobPGSource(t, storage, ctx, repo, 1).hash
	}
	return fixture
}

func (fixture docRangeEnqueueBudgetPGFixture) enqueue(ctx context.Context) docRangeEnqueueBudgetPGResult {
	started := time.Now()
	var job domain.DocRangeIndexJob
	var err error
	if fixture.base == "" {
		job, err = fixture.storage.EnqueueDocRangeJob(ctx, fixture.repo, fixture.doc, time.Now().UTC())
	} else {
		job, err = fixture.storage.EnqueueDocRangeBoundary(ctx, fixture.repo, fixture.doc, fixture.base, time.Now().UTC())
	}
	return docRangeEnqueueBudgetPGResult{job: job, err: err, elapsed: time.Since(started)}
}

func (fixture docRangeEnqueueBudgetPGFixture) start(ctx context.Context) <-chan docRangeEnqueueBudgetPGResult {
	done := make(chan docRangeEnqueueBudgetPGResult, 1)
	go func() { done <- fixture.enqueue(ctx) }()
	return done
}

func (fixture docRangeEnqueueBudgetPGFixture) locks() []string {
	locks := []string{"advisory", "doc_snapshot", "doc_ownership"}
	if fixture.base != "" {
		locks = append(locks, "base_snapshot", "base_ownership")
	}
	return locks
}

func (fixture docRangeEnqueueBudgetPGFixture) hold(t *testing.T, locks ...string) pgx.Tx {
	t.Helper()
	blocker, err := fixture.storage.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rollbackPG(blocker) })
	for _, lock := range locks {
		if lock == "advisory" {
			if _, err := blocker.Exec(fixture.ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "cxthub-doc-range-enqueue:"+string(fixture.repo)); err != nil {
				t.Fatal(err)
			}
			continue
		}
		hash := fixture.doc
		statement := `SELECT 1 FROM snapshots WHERE repo_id=$1 AND id=$2 FOR UPDATE`
		args := []any{fixture.repo, hash}
		switch lock {
		case "doc_snapshot":
		case "base_snapshot":
			args[1] = fixture.base
		case "doc_ownership", "base_ownership":
			statement = `SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2 FOR UPDATE`
			if lock == "base_ownership" {
				args[1] = fixture.base
			}
		case "queue":
			statement = `SELECT 1 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3 FOR UPDATE`
			args = append(args, fixture.base)
		default:
			t.Fatalf("unknown lock: %s", lock)
		}
		var present int
		if err := blocker.QueryRow(fixture.ctx, statement, args...).Scan(&present); err != nil {
			t.Fatal(err)
		}
	}
	return blocker
}

func (fixture docRangeEnqueueBudgetPGFixture) queue(t *testing.T) string {
	t.Helper()
	var image string
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT COALESCE(jsonb_agg(to_jsonb(job) ORDER BY doc_hash,base_hash),'[]'::jsonb)::text
 FROM doc_range_jobs_v1 job WHERE repo_id=$1`, fixture.repo).Scan(&image); err != nil {
		t.Fatal(err)
	}
	return image
}

func (fixture docRangeEnqueueBudgetPGFixture) stale(t *testing.T) {
	t.Helper()
	fixture.requireQueued(t, fixture.enqueue(fixture.ctx), 0)
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_range_jobs_v1 SET state='completed',version=7,attempts=3
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, fixture.doc, fixture.base); err != nil {
		t.Fatal(err)
	}
}

func (fixture docRangeEnqueueBudgetPGFixture) requireQueued(t *testing.T, result docRangeEnqueueBudgetPGResult, version int64) {
	t.Helper()
	job := result.job
	if result.err != nil || job.RepoID != fixture.repo || job.DocHash != fixture.doc || job.BaseHash != fixture.base ||
		job.State != "waiting" || job.Version != version || job.Attempts != 0 || job.Reason != "" || !job.LeaseUntil.IsZero() {
		t.Fatalf("enqueue did not recover: %+v, error=%v", job, result.err)
	}
	stored, err := scanDocRangeJob(fixture.storage.pool.QueryRow(fixture.ctx, `SELECT `+docRangeJobColumns+`
 FROM doc_range_jobs_v1 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, fixture.doc, fixture.base))
	if err != nil || !reflect.DeepEqual(stored, job) {
		t.Fatalf("receipt was not persisted: %+v, error=%v", stored, err)
	}
	var count int
	if err := fixture.storage.pool.QueryRow(fixture.ctx, `SELECT count(*) FROM doc_range_jobs_v1 WHERE repo_id=$1`, fixture.repo).Scan(&count); err != nil || count != 1 {
		t.Fatalf("unexpected queue size: %d, error=%v", count, err)
	}
}

func (fixture docRangeEnqueueBudgetPGFixture) requireFailure(t *testing.T, result docRangeEnqueueBudgetPGResult, want error, before string) {
	t.Helper()
	if !errors.Is(result.err, want) || want != domain.ErrResourceOverloaded && errors.Is(result.err, domain.ErrResourceOverloaded) {
		t.Fatalf("enqueue error=%v, want %v (elapsed %s)", result.err, want, result.elapsed)
	}
	if result.job != (domain.DocRangeIndexJob{}) {
		t.Fatalf("failed enqueue returned a receipt: %+v", result.job)
	}
	if after := fixture.queue(t); after != before {
		t.Fatalf("failed enqueue changed queue: before=%s after=%s", before, after)
	}
}

func (fixture docRangeEnqueueBudgetPGFixture) waitBlocked(t *testing.T, blocker pgx.Tx, done <-chan docRangeEnqueueBudgetPGResult) {
	t.Helper()
	poll, cancel := context.WithTimeout(fixture.ctx, time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		if err := fixture.storage.pool.QueryRow(poll, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity
 WHERE datname=current_database() AND $1::int=ANY(pg_blocking_pids(pid)))`, blocker.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case result := <-done:
			t.Fatalf("enqueue returned before waiting for blocker: %+v", result)
		case <-poll.Done():
			t.Fatal("enqueue did not wait for held lock", poll.Err())
		case <-ticker.C:
		}
	}
}

func TestDocRangeEnqueueBudgetPGLockTimeoutRollback(t *testing.T) {
	for _, kind := range []string{"index", "boundary"} {
		for _, state := range []string{"missing", "stale_completed"} {
			locks := []string{"advisory", "doc_snapshot", "doc_ownership"}
			if kind == "boundary" {
				locks = append(locks, "base_snapshot", "base_ownership")
			}
			if state == "stale_completed" {
				locks = append(locks, "queue")
			}
			for _, lock := range locks {
				t.Run(kind+"/"+state+"/"+lock, func(t *testing.T) {
					fixture := docRangeEnqueueBudgetPG(t, kind)
					version := int64(0)
					if state == "stale_completed" {
						fixture.stale(t)
						version = 8
					}
					before := fixture.queue(t)
					blocker := fixture.hold(t, lock)
					parent, cancel := context.WithTimeout(fixture.ctx, 2*time.Second)
					defer cancel()
					result := fixture.enqueue(parent)
					fixture.requireFailure(t, result, domain.ErrResourceOverloaded, before)
					if parent.Err() != nil || result.elapsed < 200*time.Millisecond || result.elapsed > 450*time.Millisecond {
						t.Fatalf("250ms lock budget not respected: elapsed=%s, parent=%v", result.elapsed, parent.Err())
					}
					if err := blocker.Rollback(fixture.ctx); err != nil {
						t.Fatal(err)
					}
					fixture.requireQueued(t, fixture.enqueue(fixture.ctx), version)
				})
			}
		}
	}
}

func TestDocRangeEnqueueBudgetPGTotalTimeoutRollback(t *testing.T) {
	for _, kind := range []string{"index", "boundary"} {
		t.Run(kind, func(t *testing.T) {
			fixture := docRangeEnqueueBudgetPG(t, kind)
			fixture.stale(t)
			before := fixture.queue(t)
			advisory := fixture.hold(t, "advisory")
			scope := fixture.hold(t, "doc_snapshot")
			queue := fixture.hold(t, "queue")
			parent, cancel := context.WithTimeout(fixture.ctx, 2*time.Second)
			defer cancel()
			done := fixture.start(parent)
			for _, blocker := range []pgx.Tx{advisory, scope} {
				fixture.waitBlocked(t, blocker, done)
				timer := time.NewTimer(200 * time.Millisecond)
				select {
				case result := <-done:
					timer.Stop()
					t.Fatalf("enqueue exhausted budget before staged lock release: %+v", result)
				case <-timer.C:
				}
				if err := blocker.Rollback(fixture.ctx); err != nil {
					t.Fatal(err)
				}
			}
			fixture.waitBlocked(t, queue, done)
			result := <-done
			fixture.requireFailure(t, result, domain.ErrResourceOverloaded, before)
			if parent.Err() != nil || result.elapsed < 450*time.Millisecond || result.elapsed > 600*time.Millisecond {
				t.Fatalf("500ms total budget not respected: elapsed=%s, parent=%v", result.elapsed, parent.Err())
			}
			if err := queue.Rollback(fixture.ctx); err != nil {
				t.Fatal(err)
			}
			fixture.requireQueued(t, fixture.enqueue(fixture.ctx), 8)
		})
	}
}

func TestDocRangeEnqueueBudgetPGExistingReceiptsBypassLocks(t *testing.T) {
	for _, kind := range []string{"index", "boundary"} {
		for _, state := range []string{"waiting", "running", "retrying", "rejected", "completed"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				fixture := docRangeEnqueueBudgetPG(t, kind)
				fixture.requireQueued(t, fixture.enqueue(fixture.ctx), 0)
				if state != "waiting" {
					claim := docRangeJobPGClaim(t, fixture.storage, fixture.ctx, fixture.repo, fixture.doc)
					switch state {
					case "retrying", "rejected":
						claim.State, claim.Reason = state, "temporary"
						if err := fixture.storage.FinishDocRangeJob(fixture.ctx, claim, time.Now().UTC()); err != nil {
							t.Fatal(err)
						}
					case "completed":
						if err := fixture.storage.BuildDocRangeJob(fixture.ctx, claim); err != nil {
							t.Fatal(err)
						}
					}
				}
				prior := fixture.enqueue(fixture.ctx)
				if prior.err != nil || prior.job.State != state {
					t.Fatalf("unexpected initial receipt: %+v", prior)
				}
				before := fixture.queue(t)
				fixture.hold(t, append(fixture.locks(), "queue")...)
				parent, cancel := context.WithTimeout(fixture.ctx, 200*time.Millisecond)
				defer cancel()
				receipt := fixture.enqueue(parent)
				if receipt.err != nil || !reflect.DeepEqual(receipt.job, prior.job) {
					t.Fatalf("existing receipt waited for locks or changed: %+v", receipt)
				}
				if after := fixture.queue(t); after != before {
					t.Fatalf("receipt polling changed queue: before=%s after=%s", before, after)
				}
			})
		}
	}
}

func TestDocRangeEnqueueBudgetPGParentCancellationDuringLockWait(t *testing.T) {
	for _, kind := range []string{"index", "boundary"} {
		for _, lock := range []string{"advisory", "doc_snapshot"} {
			for _, cause := range []string{"canceled", "deadline"} {
				t.Run(kind+"/"+lock+"/"+cause, func(t *testing.T) {
					fixture := docRangeEnqueueBudgetPG(t, kind)
					fixture.stale(t)
					before := fixture.queue(t)
					blocker := fixture.hold(t, lock)
					var parent context.Context
					var cancel context.CancelFunc
					want := context.Canceled
					if cause == "deadline" {
						parent, cancel = context.WithTimeout(fixture.ctx, 150*time.Millisecond)
						want = context.DeadlineExceeded
					} else {
						parent, cancel = context.WithCancel(fixture.ctx)
					}
					defer cancel()
					done := fixture.start(parent)
					fixture.waitBlocked(t, blocker, done)
					if cause == "canceled" {
						cancel()
					}
					result := <-done
					fixture.requireFailure(t, result, want, before)
					if parent.Err() != want || result.elapsed > 450*time.Millisecond {
						t.Fatalf("parent termination not preserved promptly: elapsed=%s, parent=%v", result.elapsed, parent.Err())
					}
					if err := blocker.Rollback(fixture.ctx); err != nil {
						t.Fatal(err)
					}
					fixture.requireQueued(t, fixture.enqueue(fixture.ctx), 8)
				})
			}
		}
	}
}

func TestDocRangeEnqueueBudgetPGParentAlreadyDone(t *testing.T) {
	for _, kind := range []string{"index", "boundary"} {
		for _, cause := range []string{"canceled", "deadline"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				fixture := docRangeEnqueueBudgetPG(t, kind)
				before := fixture.queue(t)
				blocker := fixture.hold(t, fixture.locks()...)
				var parent context.Context
				var cancel context.CancelFunc
				want := context.Canceled
				if cause == "deadline" {
					parent, cancel = context.WithDeadline(fixture.ctx, time.Now().Add(-time.Second))
					want = context.DeadlineExceeded
				} else {
					parent, cancel = context.WithCancel(fixture.ctx)
					cancel()
				}
				defer cancel()
				fixture.requireFailure(t, fixture.enqueue(parent), want, before)
				if err := blocker.Rollback(fixture.ctx); err != nil {
					t.Fatal(err)
				}
				fixture.requireQueued(t, fixture.enqueue(fixture.ctx), 0)
			})
		}
	}
}
