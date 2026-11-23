//go:build postgres

package store

import (
	"context"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

type docDisplayMaintenancePlanTracePG struct {
	queries   []string
	arguments [][]any
}

func (trace *docDisplayMaintenancePlanTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(query.SQL, "SELECT locked_cursor.after_source,work.source_hash,work.ready") || strings.HasPrefix(query.SQL, "SELECT generation_hash,substring(bytes") {
		trace.queries = append(trace.queries, query.SQL)
		trace.arguments = append(trace.arguments, slices.Clone(query.Args))
	}
	return ctx
}

func (*docDisplayMaintenancePlanTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}
