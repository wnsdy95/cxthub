//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type cyclePGCountTx struct {
	pgx.Tx
	queries int
}

func (tx *cyclePGCountTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.queries++
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *cyclePGCountTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.queries++
	return tx.Tx.QueryRow(ctx, sql, args...)
}
