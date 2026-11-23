//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type docDisplayReadCostTrace struct {
	settings int
	joined   int
	separate int
	bounded  bool
}

func (*docDisplayReadCostTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
