//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type memoryQualificationCostTracer struct {
	memoryReadRangeTracer
	fullRead atomic.Bool
}

func (tracer *memoryQualificationCostTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	if (strings.Contains(query, "blobs") && strings.Contains(query, "bytes")) || strings.Contains(query, "from snapshots") || strings.Contains(query, "from history_events") {
		tracer.fullRead.Store(true)
	}
	return ctx
}
