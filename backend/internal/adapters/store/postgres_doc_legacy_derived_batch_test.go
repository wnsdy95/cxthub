//go:build postgres

package store

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
)

type legacyDerivedVerificationTraceKeyPG struct{}
type legacyDerivedVerificationBatchKeyPG struct{}

type legacyDerivedVerificationTracePG struct {
	batchWidths    []int
	batchSizes     []int
	queuedOrdinals []int
	queryResults   int
	pointQueries   int
	batchEnds      int
	failures       []error
}

type legacyDerivedVerificationTracerPG struct{}

func (legacyDerivedVerificationTracerPG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (legacyDerivedVerificationTracerPG) TraceBatchStart(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchStartData) context.Context {
	trace, ok := ctx.Value(legacyDerivedVerificationTraceKeyPG{}).(*legacyDerivedVerificationTracePG)
	if !ok {
		return ctx
	}
	width := 0
	for _, query := range batch.Batch.QueuedQueries {
		width++
		ordinal := -1
		if len(query.Arguments) == 4 {
			if selected, ok := query.Arguments[1].(int); ok {
				ordinal = selected
			}
		}
		trace.queuedOrdinals = append(trace.queuedOrdinals, ordinal)
	}
	if width == 0 {
		return ctx
	}
	trace.batchWidths = append(trace.batchWidths, width)
	trace.batchSizes = append(trace.batchSizes, len(batch.Batch.QueuedQueries))
	return context.WithValue(ctx, legacyDerivedVerificationBatchKeyPG{}, trace)
}

func (legacyDerivedVerificationTracerPG) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchEndData) {
	trace, ok := ctx.Value(legacyDerivedVerificationBatchKeyPG{}).(*legacyDerivedVerificationTracePG)
	if !ok {
		return
	}
	trace.batchEnds++
	if batch.Err != nil {
		trace.failures = append(trace.failures, batch.Err)
	}
}

func (trace *legacyDerivedVerificationTracePG) assertPublication(test *testing.T) {
	test.Helper()
	if !slices.Equal(trace.batchWidths, []int{8, 3}) || !slices.Equal(trace.batchSizes, trace.batchWidths) {
		test.Fatalf("final verification batch widths=%v total queued widths=%v, want exactly two batches of eight and three statements", trace.batchWidths, trace.batchSizes)
	}
	statements := 0
	for _, width := range trace.batchWidths {
		statements += width
	}
	if statements != 11 || len(trace.queuedOrdinals) != 11 || trace.queryResults != 11 || trace.pointQueries != 0 || trace.batchEnds != 2 || len(trace.failures) != 0 {
		test.Fatalf("final verification statements=%d ordinals=%v results=%d point queries=%d batch ends=%d failures=%v", statements, trace.queuedOrdinals, trace.queryResults, trace.pointQueries, trace.batchEnds, trace.failures)
	}
	for ordinal, queued := range trace.queuedOrdinals {
		if queued != ordinal {
			test.Fatal("final verification did not queue exactly one statement for each block in order", ordinal, queued)
		}
	}
}
