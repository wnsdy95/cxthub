//go:build postgres

package store

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type legacyDerivedVerificationBatchResults struct {
	pgx.BatchResults
	row      pgx.Row
	closeErr error
	cancel   context.CancelFunc
	closes   int
}

func (results *legacyDerivedVerificationBatchResults) QueryRow() pgx.Row {
	if results.cancel != nil {
		results.cancel()
	}
	return results.row
}

func (results *legacyDerivedVerificationBatchResults) Close() error {
	results.closes++
	return results.closeErr
}

type legacyDerivedVerificationBatchTransaction struct {
	pgx.Tx
	results *legacyDerivedVerificationBatchResults
	sent    int
	queries int
}

func (transaction *legacyDerivedVerificationBatchTransaction) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	transaction.sent++
	transaction.queries += batch.Len()
	return transaction.results
}
