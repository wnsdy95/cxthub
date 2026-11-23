//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type memoryReadMinimumTracer struct {
	memoryReadRangeTracer
	hydrated atomic.Bool
}

func (tracer *memoryReadMinimumTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.ToLower(data.SQL)
	for _, forbidden := range []string{"memory_capture_parts", "memory_capture_blocks", "from history_events", "body.bytes", "select bytes", "select *"} {
		if strings.Contains(query, forbidden) {
			tracer.hydrated.Store(true)
		}
	}
	if strings.Contains(query, "from snapshots") && !strings.Contains(query, "select exists(select 1 from snapshots where repo_id=$1 and id=$2)") {
		tracer.hydrated.Store(true)
	}
	return ctx
}
