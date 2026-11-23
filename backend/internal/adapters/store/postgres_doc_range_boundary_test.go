//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type docRangeBoundaryReadTracer struct {
	objects   atomic.Int64
	queries   atomic.Int64
	unbounded atomic.Bool
}

func (trace *docRangeBoundaryReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	if strings.Contains(query.SQL, "FROM doc_range_objects_v2") {
		trace.objects.Add(1)
	}
	if strings.Contains(query.SQL, "doc_range_indexes_v1") || strings.Contains(query.SQL, "doc_read_indexes_v3") || strings.Contains(query.SQL, "blobs") && strings.Contains(query.SQL, "bytes") {
		trace.unbounded.Store(true)
	}
	return ctx
}

func (*docRangeBoundaryReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
