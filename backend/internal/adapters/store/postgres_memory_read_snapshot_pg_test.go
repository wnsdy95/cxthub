//go:build postgres

package store

import (
	"github.com/jackc/pgx/v5"
)

type memoryReadRowProbe struct {
	pgx.Row
	afterScan func()
}

func (probe memoryReadRowProbe) Scan(destinations ...any) error {
	if err := probe.Row.Scan(destinations...); err != nil {
		return err
	}
	probe.afterScan()
	return nil
}
