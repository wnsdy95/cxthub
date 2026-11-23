//go:build postgres

package store

import (
	"context"
	"fmt"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyRangeTestRow struct {
	values []any
	err    error
}

func (row legacyRangeTestRow) Scan(destinations ...any) error {
	if row.err != nil {
		return row.err
	}
	if len(destinations) != len(row.values) {
		return fmt.Errorf("unexpected scan columns: %d, want %d", len(destinations), len(row.values))
	}
	for position, value := range row.values {
		destination := reflect.ValueOf(destinations[position]).Elem()
		if value == nil {
			destination.SetZero()
		} else {
			destination.Set(reflect.ValueOf(value))
		}
	}
	return nil
}

type legacyRangeTestRows struct {
	pgx.Rows
	rows     []legacyRangeTestRow
	position int
	err      error
}

func (rows *legacyRangeTestRows) Next() bool {
	if rows.position >= len(rows.rows) {
		return false
	}
	rows.position++
	return true
}

func (rows *legacyRangeTestRows) Scan(destinations ...any) error {
	return rows.rows[rows.position-1].Scan(destinations...)
}

func (rows *legacyRangeTestRows) Close()     {}
func (rows *legacyRangeTestRows) Err() error { return rows.err }

type legacyRangeTestBatchResult struct {
	pgx.BatchResults
	err error
}

func (result legacyRangeTestBatchResult) Close() error { return result.err }

type legacyRangeTestTx struct {
	pgx.Tx
	test  *testing.T
	row   func(string, []any) pgx.Row
	query func(string, []any) (pgx.Rows, error)
	exec  func(string, []any) (pgconn.CommandTag, error)
	batch func(*pgx.Batch) error
}

func (transaction *legacyRangeTestTx) QueryRow(_ context.Context, statement string, arguments ...any) pgx.Row {
	transaction.test.Helper()
	if transaction.row == nil {
		transaction.test.Fatal("unexpected row query", statement)
	}
	return transaction.row(statement, arguments)
}

func (transaction *legacyRangeTestTx) Query(_ context.Context, statement string, arguments ...any) (pgx.Rows, error) {
	transaction.test.Helper()
	if transaction.query == nil {
		transaction.test.Fatal("unexpected rows query", statement)
	}
	return transaction.query(statement, arguments)
}

func (transaction *legacyRangeTestTx) Exec(_ context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	transaction.test.Helper()
	if transaction.exec == nil {
		transaction.test.Fatal("unexpected write", statement)
	}
	return transaction.exec(statement, arguments)
}

func (transaction *legacyRangeTestTx) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	transaction.test.Helper()
	if transaction.batch == nil {
		transaction.test.Fatal("unexpected batch", batch.Len())
	}
	return legacyRangeTestBatchResult{err: transaction.batch(batch)}
}

func legacyRangeTestContext(test *testing.T) (*PostgresStore, context.Context, *legacyRangeTestTx, domain.ContentHash) {
	test.Helper()
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(test.Name()))
	transaction := &legacyRangeTestTx{test: test}
	ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, repo: repo})
	return storage, ctx, transaction, repo
}
