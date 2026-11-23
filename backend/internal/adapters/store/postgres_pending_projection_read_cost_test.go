//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type pendingProjectionReadCostQuery struct {
	statement string
	budget    *repositoryViewSourceBudgetQuery
}

type pendingProjectionReadCostTx struct {
	pgx.Tx
	queries    []pendingProjectionReadCostQuery
	statements []string
}

func (observed *pendingProjectionReadCostTx) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	observed.statements = append(observed.statements, statement)
	rows, err := observed.Tx.Query(ctx, statement, args...)
	if err != nil || !strings.Contains(statement, "pending_projection_parts") {
		return rows, err
	}
	budget := &repositoryViewSourceBudgetQuery{args: append([]any(nil), args...), columns: len(rows.FieldDescriptions())}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == pgtype.JSONBOID || field.DataTypeOID == pgtype.JSONOID {
			budget.jsonColumns++
		}
	}
	observed.queries = append(observed.queries, pendingProjectionReadCostQuery{statement: statement, budget: budget})
	return &repositoryViewSourceBudgetRows{Rows: rows, record: budget}, nil
}

type pendingProjectionReadCostRow struct {
	rows pgx.Rows
	err  error
}

func (observed pendingProjectionReadCostRow) Scan(destinations ...any) error {
	if observed.err != nil {
		return observed.err
	}
	defer observed.rows.Close()
	if !observed.rows.Next() {
		if err := observed.rows.Err(); err != nil {
			return err
		}
		return pgx.ErrNoRows
	}
	return observed.rows.Scan(destinations...)
}

func (observed *pendingProjectionReadCostTx) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	rows, err := observed.Query(ctx, statement, args...)
	return pendingProjectionReadCostRow{rows: rows, err: err}
}
