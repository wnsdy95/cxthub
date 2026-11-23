//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleReadTracer struct {
	descriptors atomic.Int64
	objects     atomic.Int64
	forbidden   atomic.Int64
	unbounded   atomic.Int64
}

func (trace *docVisibleReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "visible.descriptor") {
		trace.descriptors.Add(1)
		if len(query.Args) != 5 || query.Args[4] != domain.MaxDocVisibleGenerationBytes+1 || query.Args[3] != domain.MaxDocRangeGenerationBytes+1 {
			trace.unbounded.Add(1)
		}
	}
	if strings.Contains(query.SQL, "FROM doc_range_objects_v2") {
		trace.objects.Add(1)
		if len(query.Args) != 3 || query.Args[2] != domain.MaxDocVisibleObjectBytes+1 {
			trace.unbounded.Add(1)
		}
	}
	if strings.Contains(query.SQL, "doc_range_indexes_v1") || strings.Contains(query.SQL, "doc_range_pages_v1") ||
		strings.Contains(query.SQL, "root.bytes") || strings.Contains(query.SQL, "b.bytes") || strings.Contains(query.SQL, "kind='chunk'") {
		trace.forbidden.Add(1)
	}
	return ctx
}

func (*docVisibleReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
