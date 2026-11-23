//go:build postgres

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type docCatalogAdmissionPGDB interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func docCatalogAdmissionExecPG(test *testing.T, ctx context.Context, database docCatalogAdmissionPGDB, statement string, arguments ...any) {
	test.Helper()
	tag, err := database.Exec(ctx, statement, arguments...)
	if err != nil || tag.RowsAffected() != 1 {
		test.Fatal("expected exactly one catalog fixture mutation", tag, err)
	}
}
