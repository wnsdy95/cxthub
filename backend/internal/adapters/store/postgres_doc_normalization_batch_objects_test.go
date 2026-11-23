//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type normalizationBatchPGTrace struct {
	legacyNormalizationParserReusePGTrace
	maximum int
}

func (trace *normalizationBatchPGTrace) TraceBatchStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	trace.mutex.Lock()
	trace.maximum = max(trace.maximum, data.Batch.Len())
	trace.mutex.Unlock()
	for _, query := range data.Batch.QueuedQueries {
		trace.TraceQueryStart(ctx, connection, pgx.TraceQueryStartData{SQL: query.SQL, Args: query.Arguments})
	}
	return ctx
}

func (*normalizationBatchPGTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (*normalizationBatchPGTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}
