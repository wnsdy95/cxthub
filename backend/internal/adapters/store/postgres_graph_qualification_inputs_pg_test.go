//go:build postgres

package store

import (
	"github.com/jackc/pgx/v5"
)

type graphQualificationInputSizeTx struct {
	pgx.Tx
	queries, rows, transferred int
	valid                      bool
}

type graphQualificationInputSizeRows struct {
	pgx.Rows
	transaction *graphQualificationInputSizeTx
}

func (rows *graphQualificationInputSizeRows) Next() bool {
	if !rows.Rows.Next() {
		return false
	}
	rows.transaction.rows++
	for _, raw := range rows.Rows.RawValues() {
		rows.transaction.transferred += len(raw)
	}
	return true
}
