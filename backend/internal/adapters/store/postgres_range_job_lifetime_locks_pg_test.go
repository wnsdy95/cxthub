//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type rangeJobLifetimeLocksAccessTracePG struct {
	source atomic.Int64
	stage  atomic.Int64
}

func (trace *rangeJobLifetimeLocksAccessTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	statement := strings.ToLower(strings.Join(strings.Fields(query.SQL), " "))
	if strings.Contains(statement, "repo_blobs") || strings.Contains(statement, "from blobs") ||
		strings.Contains(statement, "join blobs") || strings.Contains(statement, "blob_source_incarnations") {
		trace.source.Add(1)
	}
	if strings.Contains(statement, "doc_legacy_byte_") {
		trace.stage.Add(1)
	}
	return ctx
}

func (*rangeJobLifetimeLocksAccessTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}
