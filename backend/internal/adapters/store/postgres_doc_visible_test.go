//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleBatchTx struct {
	pgx.Tx
	testing *testing.T
	counts  []int
	fail    error
}

type docVisibleBatchResult struct {
	pgx.BatchResults
	err error
}

func (result docVisibleBatchResult) Close() error { return result.err }

func (transaction *docVisibleBatchTx) SendBatch(_ context.Context, batch *pgx.Batch) pgx.BatchResults {
	transaction.testing.Helper()
	size := 0
	for _, query := range batch.QueuedQueries {
		size += len(query.Arguments[2].([]byte))
	}
	if batch.Len() > docVisibleBatchObjects || size > docVisibleBatchBytes {
		transaction.testing.Fatal("unbounded visible batch", batch.Len(), size)
	}
	transaction.counts = append(transaction.counts, batch.Len())
	return docVisibleBatchResult{err: transaction.fail}
}

func TestDocVisiblePublicationBatchBounds(t *testing.T) {
	ctx := context.Background()
	transaction := &docVisibleBatchTx{testing: t}
	batch := postgresDocVisibleBatch{transaction: &repositoryTx{Tx: transaction}, hash: domain.HashContent([]byte(t.Name()))}
	raw := bytes.Repeat([]byte("a"), domain.MaxDocVisibleObjectBytes)
	object := domain.DocRangeObject{Hash: domain.HashContent(raw), Bytes: raw}
	for range 2*docVisibleBatchObjects + 1 {
		if err := batch.emit(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := batch.flush(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(transaction.counts, []int{128, 128, 1}) || batch.bytes != 0 || batch.batch.Len() != 0 {
		t.Fatal("batch did not release queued objects", transaction.counts, batch.bytes, batch.batch.Len())
	}
	for _, damaged := range []domain.DocRangeObject{
		{}, {Hash: object.Hash, Bytes: []byte("wrong")},
		{Hash: object.Hash, Bytes: append(bytes.Clone(raw), 'a')},
	} {
		if err := batch.emit(ctx, damaged); !errors.Is(err, domain.ErrIntegrity) {
			t.Fatal("invalid emitted object", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := batch.emit(canceled, object); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled emitter", err)
	}
	if err := batch.emit(ctx, object); err != nil {
		t.Fatal(err)
	}
	transaction.fail = errors.New("batch failed")
	if err := batch.flush(ctx); !errors.Is(err, transaction.fail) {
		t.Fatal("batch failure swallowed", err)
	}
}
