//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type normalizationDeferredProbeHookKey struct{}

type normalizationDeferredProbeTrace struct {
	normalizationBatchPGTrace
	afterProbe func()
}

func (trace *normalizationDeferredProbeTrace) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	ctx = trace.normalizationBatchPGTrace.TraceQueryStart(ctx, connection, data)
	if data.SQL == legacyNormalizationDeferredBatchCurrent && trace.afterProbe != nil {
		return context.WithValue(ctx, normalizationDeferredProbeHookKey{}, trace.afterProbe)
	}
	return ctx
}

func (*normalizationDeferredProbeTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if hook, ok := ctx.Value(normalizationDeferredProbeHookKey{}).(func()); ok {
		hook()
	}
}
