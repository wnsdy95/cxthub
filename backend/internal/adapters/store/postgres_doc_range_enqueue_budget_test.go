//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
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
				_ = testCase.want[parentIndex]
			})
		}
	}
}

type docRangeEnqueueBudgetPGFixture struct {
	storage         *PostgresStore
	ctx             context.Context
	repo, doc, base domain.ContentHash
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
	if _, err := fixture.storage.pool.Exec(fixture.ctx, `UPDATE doc_range_jobs_v1 SET state='completed',version=7,attempts=3
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=$3`, fixture.repo, fixture.doc, fixture.base); err != nil {
		t.Fatal(err)
	}
}
