//go:build postgres

package store

import (
	"context"
	"net"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type memoryReadRangeTraceKey struct{}

type memoryReadRangeCounts struct {
	queries atomic.Int64
	rows    atomic.Int64
}

type memoryReadRangeTracer struct{}

func (memoryReadRangeTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (memoryReadRangeTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if counts, ok := ctx.Value(memoryReadRangeTraceKey{}).(*memoryReadRangeCounts); ok && data.CommandTag.Select() {
		counts.queries.Add(1)
		counts.rows.Add(data.CommandTag.RowsAffected())
	}
}

type memoryReadRangeCountingConnection struct {
	net.Conn
	bytes *atomic.Int64
}

func (connection memoryReadRangeCountingConnection) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	connection.bytes.Add(int64(count))
	return count, err
}
