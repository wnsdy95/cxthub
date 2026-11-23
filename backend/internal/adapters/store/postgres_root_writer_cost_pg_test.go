//go:build postgres

package store

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type rootWriterCostSamplePG struct {
	trace          *rootPublicationCostPG
	allocatedBytes uint64
	mallocs        uint64
	collections    uint32
}

func (sample rootWriterCostSamplePG) report(test *testing.T, phase string) {
	test.Helper()
	var transaction time.Duration
	if !sample.trace.committed.IsZero() {
		transaction = sample.trace.committed.Sub(sample.trace.transactionStarted)
	}
	test.Logf("RF13-LP1 phase=%s wall_ns=%d final_transaction_ns=%d allocated_bytes=%d mallocs=%d gc_cycles=%d allocation_scope=process_wide_whole_method trace_overhead_included=true",
		phase, sample.trace.elapsed, transaction, sample.allocatedBytes, sample.mallocs, sample.collections)
	sample.trace.report(test, phase)
}

type rootWriterPreparationBarrierKeyPG struct{}

type rootWriterPreparationBarrierPG struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (barrier *rootWriterPreparationBarrierPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if rootPublicationCostSQLPhasePG(data.SQL) == "chunk_payload" {
		return context.WithValue(ctx, rootWriterPreparationBarrierKeyPG{}, barrier)
	}
	return ctx
}

func (barrier *rootWriterPreparationBarrierPG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err != nil || ctx.Value(rootWriterPreparationBarrierKeyPG{}) != barrier {
		return
	}
	barrier.once.Do(func() {
		close(barrier.reached)
		select {
		case <-barrier.release:
		case <-ctx.Done():
		}
	})
}
