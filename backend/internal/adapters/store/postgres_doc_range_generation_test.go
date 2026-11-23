//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func generationCountsPG(t *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) [2]int {
	t.Helper()
	var counts [2]int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1)`, hash).Scan(&counts[0], &counts[1]); err != nil {
		t.Fatal(err)
	}
	return counts
}

type docRangeGenerationTracer struct {
	descriptors atomic.Int64
	objects     atomic.Int64
	forbidden   atomic.Int64
	selected    []domain.ContentHash
}

func (trace *docRangeGenerationTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "generation.descriptor") {
		trace.descriptors.Add(1)
	}
	if strings.Contains(query.SQL, "FROM doc_range_objects_v2") {
		trace.objects.Add(1)
		trace.selected = append(trace.selected, query.Args[1].(domain.ContentHash))
	}
	if strings.Contains(query.SQL, "doc_range_indexes_v1") || strings.Contains(query.SQL, "root.bytes") || strings.Contains(query.SQL, "b.bytes") {
		trace.forbidden.Add(1)
	}
	return ctx
}

func (*docRangeGenerationTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
