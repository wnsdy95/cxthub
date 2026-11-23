//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docRangeJobPGRepo(t *testing.T, storage *PostgresStore, ctx context.Context) (domain.ContentHash, string) {
	t.Helper()
	repo, namespace := rootPGRepo(t, ctx, storage)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DELETE FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo); err != nil {
			t.Error(err)
		}
	})
	return repo, namespace
}

type docRangeJobFenceTracer struct {
	paused           atomic.Bool
	statement        string
	reached, proceed chan struct{}
}

func (trace *docRangeJobFenceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	matches := strings.HasPrefix(query.SQL, "SELECT 1 FROM doc_range_jobs_v1") && strings.HasSuffix(query.SQL, "FOR UPDATE")
	if trace.statement != "" {
		matches = query.SQL == trace.statement
	}
	if matches && trace.paused.CompareAndSwap(false, true) {
		close(trace.reached)
		select {
		case <-trace.proceed:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*docRangeJobFenceTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
