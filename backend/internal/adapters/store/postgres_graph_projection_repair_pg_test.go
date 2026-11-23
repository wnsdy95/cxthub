//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type graphProjectionRepairFaultTx struct {
	pgx.Tx
	needle  string
	failure error
}

func (transaction graphProjectionRepairFaultTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	if strings.Contains(query, transaction.needle) {
		return graphProjectionRepairFaultRow{transaction.failure}
	}
	return transaction.Tx.QueryRow(ctx, query, args...)
}

type graphProjectionRepairFaultRow struct{ failure error }

func (row graphProjectionRepairFaultRow) Scan(...any) error { return row.failure }
