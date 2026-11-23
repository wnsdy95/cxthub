//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type docRangeEnvelopeTracePG struct {
	headerBodies atomic.Int64
	forbidden    atomic.Int64
	unbounded    atomic.Int64
}

func (trace *docRangeEnvelopeTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "substring(part.bytes") {
		trace.headerBodies.Add(1)
		if len(query.Args) != 10 {
			trace.unbounded.Add(1)
		} else if length, ok := query.Args[9].(int); !ok || length < 1 || length > 64<<10 {
			trace.unbounded.Add(1)
		}
	}
	for _, marker := range []string{"source.bytes", "root.bytes", "b.bytes", "SELECT bytes FROM blobs"} {
		if strings.Contains(query.SQL, marker) {
			trace.forbidden.Add(1)
			break
		}
	}
	return ctx
}

func (*docRangeEnvelopeTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
