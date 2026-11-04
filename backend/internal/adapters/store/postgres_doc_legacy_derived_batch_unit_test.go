//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
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

func TestLegacyDerivedByteVerificationBatchClosesOnEveryOutcome(tests *testing.T) {
	proof := legacyMonolithicVerifierCases(tests)[0].expected
	plan, err := domain.NewLegacyByteBackfill(context.Background(), proof)
	if err != nil {
		tests.Fatal(err)
	}
	block, err := plan.Block(context.Background(), 0)
	if err != nil {
		tests.Fatal(err)
	}
	closeFailure := errors.New("batch close failed")
	for _, scenario := range []struct {
		name     string
		scanErr  error
		closeErr error
		wantErr  error
		cancel   bool
	}{
		{name: "valid"},
		{name: "missing", scanErr: pgx.ErrNoRows, wantErr: domain.ErrPreparedDocRangeObject},
		{name: "scan-cancelled", scanErr: context.Canceled, wantErr: context.Canceled},
		{name: "close-failed", closeErr: closeFailure, wantErr: closeFailure},
		{name: "scan-and-close-failed", scanErr: context.Canceled, closeErr: closeFailure, wantErr: context.Canceled},
		{name: "cancel-after-read", cancel: true, wantErr: context.Canceled},
		{name: "wrong-length", wantErr: domain.ErrIntegrity},
		{name: "wrong-hash", wantErr: domain.ErrIntegrity},
	} {
		tests.Run(scenario.name, func(test *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			body := bytes.Clone(block.Bytes())
			if scenario.name == "wrong-length" {
				body = body[:len(body)-1]
			} else if scenario.name == "wrong-hash" {
				body[0] ^= 1
			}
			results := &legacyDerivedVerificationBatchResults{
				row: legacyRangeTestRow{values: []any{body}, err: scenario.scanErr}, closeErr: scenario.closeErr,
			}
			if scenario.cancel {
				results.cancel = cancel
			}
			transaction := &legacyDerivedVerificationBatchTransaction{results: results}
			descriptors, err := verifyLegacyDerivedByteBatch(ctx, transaction, proof.Hash(), plan, 0, 1)
			if !errors.Is(err, scenario.wantErr) || results.closes != 1 || transaction.sent != 1 || transaction.queries != 1 ||
				(scenario.closeErr != nil && !errors.Is(err, scenario.closeErr)) || (err == nil && (len(descriptors) != 1 || descriptors[0] != block.Descriptor())) {
				test.Fatal("batch verification lost integrity, cancellation, or cleanup", descriptors, err, results.closes, transaction.sent, transaction.queries)
			}
		})
	}
	for _, bounds := range [][2]int{{-1, 1}, {0, 0}, {0, domain.DocLegacyByteBackfillBatchBlocks + 1}} {
		transaction := &legacyDerivedVerificationBatchTransaction{}
		if _, err := verifyLegacyDerivedByteBatch(context.Background(), transaction, proof.Hash(), plan, bounds[0], bounds[1]); !errors.Is(err, domain.ErrValidation) || transaction.sent != 0 {
			tests.Fatal("invalid batch bounds reached PostgreSQL", bounds, err, transaction.sent)
		}
	}
}
