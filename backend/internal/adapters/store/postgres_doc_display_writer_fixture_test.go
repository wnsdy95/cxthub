//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

var docDisplayOwnedFixturesPG sync.Map

func retryDocDisplayCatalogBusyPG(ctx context.Context, operation func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for ctx.Err() == nil {
		err := operation(ctx)
		var failure *pgconn.PgError
		if !errors.As(err, &failure) || failure.Code != "55P03" || failure.Message != "document catalog admission is busy" {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	return ctx.Err()
}

func docDisplayLeaseMutationPG(ctx context.Context, storage *PostgresStore, repository domain.ContentHash, statement string, arguments ...any) (pgconn.CommandTag, error) {
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `SELECT cxthub_bind_doc_catalog_control(repo_id,doc_hash,event_index,profile,version)
 FROM doc_display_jobs_v1 WHERE repo_id=$1 AND state='running'`, repository); err != nil {
		return pgconn.CommandTag{}, err
	}
	tag, err := transaction.Exec(ctx, statement, arguments...)
	if err != nil {
		return tag, err
	}
	return tag, transaction.Commit(ctx)
}

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
