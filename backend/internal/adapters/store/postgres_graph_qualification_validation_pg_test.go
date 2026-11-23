//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type graphQualificationCorruptReadTx struct {
	pgx.Tx
	corrupt func([]any)
}

func (transaction graphQualificationCorruptReadTx) QueryRow(ctx context.Context, query string, args ...any) pgx.Row {
	row := transaction.Tx.QueryRow(ctx, query, args...)
	if strings.Contains(query, "FROM graph_qualification qualification") {
		return graphQualificationCorruptReadRow{Row: row, corrupt: transaction.corrupt}
	}
	return row
}

type graphQualificationCorruptReadRow struct {
	pgx.Row
	corrupt func([]any)
}

func (row graphQualificationCorruptReadRow) Scan(destinations ...any) error {
	if err := row.Row.Scan(destinations...); err != nil {
		return err
	}
	row.corrupt(destinations)
	return nil
}
