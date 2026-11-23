//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type memoryCapturePGPinTrace struct {
	pgx.Tx
	copied int
	denied int
	limits []int
}

func (trace *memoryCapturePGPinTrace) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	trace.limits = append(trace.limits, arguments[1].(int))
	rows, err := trace.Tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &memoryCapturePGPinRows{Rows: rows, trace: trace}, nil
}

type memoryCapturePGPinRows struct {
	pgx.Rows
	trace *memoryCapturePGPinTrace
}

func (rows *memoryCapturePGPinRows) Scan(destinations ...any) error {
	if err := rows.Rows.Scan(destinations...); err != nil {
		return err
	}
	raw := *destinations[len(destinations)-1].(*[]byte)
	if raw == nil {
		rows.trace.denied++
	}
	rows.trace.copied += len(raw)
	return nil
}
