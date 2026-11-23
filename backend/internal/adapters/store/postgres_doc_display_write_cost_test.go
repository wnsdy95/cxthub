//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type docDisplayWriteCostTrace struct {
	queries         int
	proof           string
	arguments       []any
	insert          string
	insertArguments []any
}

func (trace *docDisplayWriteCostTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries++
	if strings.HasPrefix(query.SQL, "WITH requested AS (") {
		trace.proof = query.SQL
		trace.arguments = append([]any(nil), query.Args...)
	}
	if strings.HasPrefix(query.SQL, "INSERT INTO doc_display_objects_v1") {
		trace.insert = query.SQL
		trace.insertArguments = append([]any(nil), query.Args...)
	}
	return ctx
}

func (*docDisplayWriteCostTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
