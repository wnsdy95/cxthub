//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type memoryReadWorkCostTracer struct {
	memoryReadRangeTracer
	hydrated atomic.Bool
}

func (tracer *memoryReadWorkCostTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.ToLower(data.SQL)
	for _, forbidden := range []string{"memory_capture_parts", "memory_capture_blocks", "from snapshots", "from history_events", "body.bytes"} {
		if strings.Contains(query, forbidden) {
			tracer.hydrated.Store(true)
		}
	}
	return ctx
}
