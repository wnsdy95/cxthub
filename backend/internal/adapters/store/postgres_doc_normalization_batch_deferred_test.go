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

func (*normalizationDeferredProbeTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if hook, ok := ctx.Value(normalizationDeferredProbeHookKey{}).(func()); ok {
		hook()
	}
}
