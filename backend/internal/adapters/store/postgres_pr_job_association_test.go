//go:build postgres

package store

import (
	"github.com/jackc/pgx/v5"
)

type prAssociationRows struct {
	pgx.Rows
	payload []byte
	count   int
	read    int
	closed  bool
}

func (rows *prAssociationRows) Next() bool { rows.read++; return rows.read <= rows.count }
func (rows *prAssociationRows) Err() error { return nil }
func (rows *prAssociationRows) Close()     { rows.closed = true }
func (rows *prAssociationRows) Scan(targets ...any) error {
	*targets[0].(*[]byte) = rows.payload
	return nil
}
