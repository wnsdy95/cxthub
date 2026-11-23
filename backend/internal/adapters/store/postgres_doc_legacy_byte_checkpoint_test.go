//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type legacyCheckpointPGCreationQueryKey struct{}

type legacyCheckpointPGCreationTrace struct {
	reads   atomic.Int32
	arrived chan struct{}
	release chan struct{}
}

func (trace *legacyCheckpointPGCreationTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	selected := strings.HasPrefix(query.SQL, "SELECT source_id::text") && strings.Contains(query.SQL, "checkpoint_sequence")
	return context.WithValue(ctx, legacyCheckpointPGCreationQueryKey{}, selected)
}

func (trace *legacyCheckpointPGCreationTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	selected, _ := ctx.Value(legacyCheckpointPGCreationQueryKey{}).(bool)
	if selected && trace.reads.Add(1) <= 2 {
		trace.arrived <- struct{}{}
		select {
		case <-trace.release:
		case <-ctx.Done():
		}
	}
}
