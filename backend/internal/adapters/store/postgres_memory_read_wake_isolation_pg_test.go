//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type memoryReadWakeCostTracerPG struct {
	memoryReadRangeTracer
	hydrated atomic.Bool
	targets  atomic.Int64
}

func (tracer *memoryReadWakeCostTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	for _, forbidden := range []string{"memory_capture_parts", "memory_capture_blocks", "memory_read_chunks", "from snapshots", "from history_events", "body.bytes", "select bytes"} {
		if strings.Contains(query, forbidden) {
			tracer.hydrated.Store(true)
		}
	}
	if strings.Contains(query, "from memory_read_work ") {
		tracer.targets.Add(1)
	}
	return ctx
}
