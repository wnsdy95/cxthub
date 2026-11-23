//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

const docRangeWorkEmissionBatchObjects = 128
const docRangeWorkEmissionInsertSQL = "INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING"

type docRangeWorkEmissionTraceKeyPG struct{}
type docRangeWorkEmissionBatchKeyPG struct{}

type docRangeWorkEmissionTracePG struct {
	batchWidths        []int
	batchSizes         []int
	statements         int
	insertedRows       int64
	batchEnds          int
	failures           []error
	afterFirstBatch    func()
	corruptionInjected bool
}

type docRangeWorkEmissionTracerPG struct{}

var _ pgx.QueryTracer = docRangeWorkEmissionTracerPG{}
var _ pgx.BatchTracer = docRangeWorkEmissionTracerPG{}

func (docRangeWorkEmissionTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (docRangeWorkEmissionTracerPG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (docRangeWorkEmissionTracerPG) TraceBatchStart(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchStartData) context.Context {
	trace, ok := ctx.Value(docRangeWorkEmissionTraceKeyPG{}).(*docRangeWorkEmissionTracePG)
	if !ok {
		return ctx
	}
	width := 0
	for _, query := range batch.Batch.QueuedQueries {
		if query.SQL == docRangeWorkEmissionInsertSQL {
			width++
		}
	}
	if width == 0 {
		return ctx
	}
	trace.batchWidths = append(trace.batchWidths, width)
	trace.batchSizes = append(trace.batchSizes, len(batch.Batch.QueuedQueries))
	return context.WithValue(ctx, docRangeWorkEmissionBatchKeyPG{}, trace)
}

func (docRangeWorkEmissionTracerPG) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, query pgx.TraceBatchQueryData) {
	trace, ok := ctx.Value(docRangeWorkEmissionBatchKeyPG{}).(*docRangeWorkEmissionTracePG)
	if !ok || query.SQL != docRangeWorkEmissionInsertSQL {
		return
	}
	trace.statements++
	trace.insertedRows += query.CommandTag.RowsAffected()
	if query.Err != nil {
		trace.failures = append(trace.failures, query.Err)
	}
}

func (docRangeWorkEmissionTracerPG) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchEndData) {
	trace, ok := ctx.Value(docRangeWorkEmissionBatchKeyPG{}).(*docRangeWorkEmissionTracePG)
	if !ok {
		return
	}
	trace.batchEnds++
	if batch.Err != nil {
		trace.failures = append(trace.failures, batch.Err)
	}
	if trace.batchEnds == 1 && len(trace.batchWidths) == 1 && trace.batchWidths[0] == docRangeWorkEmissionBatchObjects &&
		trace.batchSizes[0] == docRangeWorkEmissionBatchObjects && trace.statements == docRangeWorkEmissionBatchObjects &&
		trace.insertedRows == docRangeWorkEmissionBatchObjects && len(trace.failures) == 0 && trace.afterFirstBatch != nil {
		trace.corruptionInjected = true
		trace.afterFirstBatch()
	}
}
