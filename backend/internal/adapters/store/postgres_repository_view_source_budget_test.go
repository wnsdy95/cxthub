//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

type repositoryViewSourceBudgetQuery struct {
	args        []any
	columns     int
	jsonColumns int
	rows        int
	images      int
	imageBytes  int
	valueBytes  int
	firstKey    string
}

type repositoryViewSourceBudgetTx struct {
	pgx.Tx
	queries []*repositoryViewSourceBudgetQuery
}

func (observed *repositoryViewSourceBudgetTx) Query(ctx context.Context, query string, args ...any) (pgx.Rows, error) {
	rows, err := observed.Tx.Query(ctx, query, args...)
	if err != nil || (!strings.Contains(query, "repository_view_source_images") && !strings.Contains(query, "repository_graph_source_images")) {
		return rows, err
	}
	record := &repositoryViewSourceBudgetQuery{args: append([]any(nil), args...), columns: len(rows.FieldDescriptions())}
	for _, field := range rows.FieldDescriptions() {
		if field.DataTypeOID == pgtype.JSONBOID || field.DataTypeOID == pgtype.JSONOID {
			record.jsonColumns++
		}
	}
	observed.queries = append(observed.queries, record)
	return &repositoryViewSourceBudgetRows{Rows: rows, record: record}, nil
}

type repositoryViewSourceBudgetRows struct {
	pgx.Rows
	record *repositoryViewSourceBudgetQuery
	closed bool
}

func (observed *repositoryViewSourceBudgetRows) Next() bool {
	if !observed.Rows.Next() {
		return false
	}
	observed.record.rows++
	fields := observed.Rows.FieldDescriptions()
	for index, raw := range observed.Rows.RawValues() {
		observed.record.valueBytes += len(raw)
		if raw != nil && (fields[index].DataTypeOID == pgtype.JSONBOID || fields[index].DataTypeOID == pgtype.JSONOID) {
			observed.record.images++
			observed.record.imageBytes += len(raw)
			if fields[index].DataTypeOID == pgtype.JSONBOID && fields[index].Format == pgx.BinaryFormatCode {
				observed.record.imageBytes--
			}
		}
		if observed.record.rows == 1 && observed.record.jsonColumns == 0 && index == 2 {
			observed.record.firstKey = string(raw)
		}
	}
	return true
}

func (observed *repositoryViewSourceBudgetRows) Close() {
	if observed.closed {
		return
	}
	observed.closed = true
	for observed.Next() {
	}
	observed.Rows.Close()
}
