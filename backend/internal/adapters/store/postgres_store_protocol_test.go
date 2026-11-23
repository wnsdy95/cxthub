//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type contextWritePGCounts struct {
	protocol, history, refs atomic.Int32
}
type contextWritePGCountKey struct{}
type contextWritePGTracer struct{}

func (contextWritePGTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	counts, ok := ctx.Value(contextWritePGCountKey{}).(*contextWritePGCounts)
	if !ok {
		return ctx
	}
	sql := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	if strings.HasPrefix(sql, "select ") {
		switch {
		case strings.Contains(sql, " from repos "):
			counts.protocol.Add(1)
		case strings.Contains(sql, " from context_history "):
			counts.history.Add(1)
		case strings.Contains(sql, " from refs "):
			counts.refs.Add(1)
		}
	}
	return ctx
}

func (contextWritePGTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
