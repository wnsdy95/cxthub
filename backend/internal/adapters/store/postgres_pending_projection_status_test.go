//go:build postgres

package store

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type pendingProjectionStatusTx struct {
	pgx.Tx
	queries []*repositoryViewSourceBudgetQuery
}

func (observed *pendingProjectionStatusTx) Query(ctx context.Context, statement string, args ...any) (pgx.Rows, error) {
	for _, forbidden := range []string{
		"pending_projection_parts", "pending_projection_inputs", "repository_view_source_images",
		"repository_view_source_changes", "repository_catalog_changes", "snapshots", "pending_contexts",
	} {
		if strings.Contains(strings.ToLower(statement), forbidden) {
			return nil, fmt.Errorf("status read accessed payload relation %s: %s", forbidden, statement)
		}
	}
	rows, err := observed.Tx.Query(ctx, statement, args...)
	if err != nil {
		return rows, err
	}
	budget := &repositoryViewSourceBudgetQuery{columns: len(rows.FieldDescriptions())}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == pgtype.JSONBOID || field.DataTypeOID == pgtype.JSONOID {
			budget.jsonColumns++
		}
	}
	observed.queries = append(observed.queries, budget)
	return &repositoryViewSourceBudgetRows{Rows: rows, record: budget}, nil
}

func (observed *pendingProjectionStatusTx) QueryRow(ctx context.Context, statement string, args ...any) pgx.Row {
	rows, err := observed.Query(ctx, statement, args...)
	return pendingProjectionReadCostRow{rows: rows, err: err}
}
