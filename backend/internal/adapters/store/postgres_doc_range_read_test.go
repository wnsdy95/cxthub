//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type docRangeReadTraceKey struct{}
type docRangeReadCounts struct {
	headers atomic.Int64
	pages   atomic.Int64
}
type docRangeReadTracer struct{}

func (docRangeReadTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if counts, ok := ctx.Value(docRangeReadTraceKey{}).(*docRangeReadCounts); ok {
		if strings.Contains(query.SQL, "i.header") {
			counts.headers.Add(1)
		} else if strings.Contains(query.SQL, "substring(p.bytes") {
			counts.pages.Add(1)
		}
	}
	return ctx
}

func (docRangeReadTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
