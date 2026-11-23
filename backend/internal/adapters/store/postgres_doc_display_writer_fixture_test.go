//go:build postgres

package store

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgconn"
)

var docDisplayOwnedFixturesPG sync.Map

func docDisplayJobCorruptionPG(ctx context.Context, storage *PostgresStore, statement string, arguments ...any) (pgconn.CommandTag, error) {
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer rollbackPG(transaction)
	var database string
	if err := transaction.QueryRow(ctx, `SELECT current_database()`).Scan(&database); err != nil {
		return pgconn.CommandTag{}, err
	}
	if _, owned := docDisplayOwnedFixturesPG.Load(database); !owned {
		return pgconn.CommandTag{}, fmt.Errorf("deliberate job corruption requires an isolated fixture database")
	}
	if _, err := transaction.Exec(ctx, `ALTER TABLE doc_display_jobs_v1 DISABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := transaction.Exec(ctx, statement, arguments...)
	if err != nil {
		return tag, err
	}
	if _, err := transaction.Exec(ctx, `ALTER TABLE doc_display_jobs_v1 ENABLE TRIGGER guard_doc_display_zz_catalog_write`); err != nil {
		return tag, err
	}
	return tag, transaction.Commit(ctx)
}
