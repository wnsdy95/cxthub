//go:build postgres

package store

import (
	"github.com/jackc/pgx/v5"
)

type memorySourceInterleavedRow struct {
	pgx.Row
	afterScan func()
}

func (row memorySourceInterleavedRow) Scan(destinations ...any) error {
	err := row.Row.Scan(destinations...)
	if err == nil {
		row.afterScan()
	}
	return err
}
