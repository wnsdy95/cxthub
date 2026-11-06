//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
)

type graphQueryTraceKey struct{}

type graphQueryTracer struct{}

func (graphQueryTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	query := graphdiagnostics.StartSQL(ctx)
	if query == nil {
		return ctx
	}
	return context.WithValue(ctx, graphQueryTraceKey{}, query)
}

func (graphQueryTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	query, _ := ctx.Value(graphQueryTraceKey{}).(*graphdiagnostics.SQLQuery)
	if query == nil {
		return
	}
	known := data.Err == nil && (data.CommandTag.Select() || data.CommandTag.Insert() || data.CommandTag.Update() || data.CommandTag.Delete())
	query.End(data.CommandTag.RowsAffected(), known, data.Err != nil)
}

var _ pgx.QueryTracer = graphQueryTracer{}
