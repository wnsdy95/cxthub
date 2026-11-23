//go:build postgres

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type memoryGraphBaseHeaderProbe struct {
	pgx.Tx
	test    *testing.T
	headers int
}

func (probe *memoryGraphBaseHeaderProbe) check(query string) {
	probe.test.Helper()
	if strings.Contains(query, "memory_capture_parts") || strings.Contains(query, "memory_capture_blocks") || strings.Contains(query, "graph_projection_inputs") {
		probe.test.Fatal("page read hydrated capture or mutable graph inputs", query)
	}
	if strings.Contains(query, "FROM memory_capture_headers WHERE") {
		probe.headers++
	}
}

func (probe *memoryGraphBaseHeaderProbe) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	probe.check(query)
	return probe.Tx.QueryRow(ctx, query, arguments...)
}

func (probe *memoryGraphBaseHeaderProbe) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	probe.check(query)
	return probe.Tx.Query(ctx, query, arguments...)
}
