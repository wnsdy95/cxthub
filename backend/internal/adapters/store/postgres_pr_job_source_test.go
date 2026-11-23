//go:build postgres

package store

import (
	"context"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type pausePRClaimKey struct{}
type pausePRClaimTracer struct {
	id                string
	paused            atomic.Bool
	selected, release chan struct{}
}

func (p *pausePRClaimTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(pausePRClaimKey{}) == true {
		close(p.selected)
		select {
		case <-p.release:
		case <-ctx.Done():
		}
	}
}
