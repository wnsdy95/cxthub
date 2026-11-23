//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type legacyByteStageSourceTrace struct{ reads atomic.Int64 }

func (trace *legacyByteStageSourceTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "source.bytes") {
		trace.reads.Add(1)
	}
	return ctx
}

func (*legacyByteStageSourceTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
