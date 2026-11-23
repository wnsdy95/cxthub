//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type pendingProjectionEmptyDeltaTracer struct {
	queries   atomic.Int64
	forbidden atomic.Int64
}

func (trace *pendingProjectionEmptyDeltaTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	statement := strings.ToLower(query.SQL)
	if strings.Contains(statement, "pending_projection_inputs") || strings.Contains(statement, "pending_projection_parts") {
		trace.forbidden.Add(1)
	}
	return ctx
}

func (*pendingProjectionEmptyDeltaTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}
