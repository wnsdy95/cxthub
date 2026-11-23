//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type docToolOrdersPGTrace struct {
	pgx.Tx
	queries   []string
	arguments [][]any
	rows      int
	objects   int
}

type docToolOrdersPGRows struct {
	pgx.Rows
	trace *docToolOrdersPGTrace
}

func (rows *docToolOrdersPGRows) Next() bool {
	if rows.Rows.Next() {
		rows.trace.rows++
		return true
	}
	return false
}

func (trace *docToolOrdersPGTrace) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	trace.queries = append(trace.queries, query)
	trace.arguments = append(trace.arguments, arguments)
	rows, err := trace.Tx.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	return &docToolOrdersPGRows{Rows: rows, trace: trace}, nil
}

func (trace *docToolOrdersPGTrace) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	trace.queries = append(trace.queries, query)
	trace.arguments = append(trace.arguments, arguments)
	if strings.Contains(query, "doc_display_objects_v1") {
		trace.objects++
	}
	return trace.Tx.QueryRow(ctx, query, arguments...)
}
