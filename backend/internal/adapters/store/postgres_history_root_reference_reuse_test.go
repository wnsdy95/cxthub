//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type historyRootReferenceReadKeyPG struct{}

type historyRootReferenceReadsPG struct {
	documents atomic.Int32
	chunks    atomic.Int32
}

func (reads *historyRootReferenceReadsPG) reset() {
	reads.documents.Store(0)
	reads.chunks.Store(0)
}

type historyRootReferenceTracePG struct{}

func (historyRootReferenceTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	reads, ok := ctx.Value(historyRootReferenceReadKeyPG{}).(*historyRootReferenceReadsPG)
	if !ok || !strings.Contains(query.SQL, "b.bytes") {
		return ctx
	}
	if strings.Contains(query.SQL, "rb.kind='chunk'") || len(query.Args) > 2 && query.Args[1] == "chunk" {
		reads.chunks.Add(1)
	} else {
		reads.documents.Add(1)
	}
	return ctx
}

func (historyRootReferenceTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
