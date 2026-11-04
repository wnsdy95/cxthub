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
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
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

func TestDocVisiblePublicationRejectsUnverified(t *testing.T) {
	storage := &PostgresStore{}
	repo := domain.HashContent([]byte(t.Name()))
	ctx := context.WithValue(context.Background(), repositoryTxKey{}, &repositoryTx{owner: storage, repo: repo})
	if err := storage.putVisibleGeneration(ctx, repo, domain.VerifiedDocRangeGeneration{}); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("unverified generation published", err)
	}
	if err := storage.putVisibleGeneration(context.Background(), repo, domain.VerifiedDocRangeGeneration{}); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("unbound publication admitted", err)
	}
}

func docVisiblePGFixture(t *testing.T, events int) (*PostgresStore, context.Context, domain.ContentHash, rootReaderFixture) {
	t.Helper()
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, events)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	return storage, ctx, repo, fixture
}

func docVisiblePGRead(t *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash, want error) {
	t.Helper()
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		read, err := storage.OpenDocVisibleGeneration(bound, repo, hash)
		if err == nil && read.Descriptor().Events != 0 {
			_, _, err = read.Unit(bound, "all", 0, false)
		}
		if !errors.Is(err, want) {
			t.Fatalf("visible read: got %v, want %v", err, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocVisiblePGReaderScopeAndCopies(t *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(t, 257)
	foreign, _ := rootPGRepo(t, ctx, storage)
	if _, err := storage.OpenDocVisibleGeneration(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("unbound reader", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("writable reader", err)
		}
		_, err := storage.readVisibleGeneration(bound, repo, fixture.hash)
		return err
	}); err != nil {
		t.Fatal("write transaction decoder", err)
	}
	for _, isolation := range []pgx.TxIsoLevel{pgx.ReadCommitted, pgx.Serializable} {
		transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: isolation, AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true})
		if _, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("non-RR reader", isolation, err)
		}
		rollbackPG(transaction)
	}
	peer := &PostgresStore{pool: storage.pool}
	var retained outbound.DocVisibleGenerationRead
	var retainedContext context.Context
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := peer.OpenDocVisibleGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign store reader", err)
		}
		if _, err := storage.OpenDocVisibleGeneration(bound, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("foreign repository reader", err)
		}
		read, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		retained, retainedContext = read, bound
		original := read.Descriptor()
		changed := read.Descriptor()
		changed.Views["all"] = domain.DocVisibleRoot{}
		delete(changed.Views, "chat")
		if !reflect.DeepEqual(read.Descriptor(), original) {
			t.Fatal("descriptor exposed its views map")
		}
		for _, mode := range []string{"all", "chat", "prompts"} {
			for _, position := range []int{0, 128, 256} {
				want := domain.DocVisibleUnit{From: position, To: position + 1, EventIndex: position, TurnIndex: position, RenderedBefore: position}
				for _, byEvent := range []bool{false, true} {
					unit, ordinal, err := read.Unit(bound, mode, position, byEvent)
					if err != nil || unit != want || ordinal != position {
						t.Fatal("selected unit", mode, position, byEvent, unit, ordinal, err)
					}
				}
			}
		}
		for _, position := range []int{-1, 257} {
			if _, _, err := read.Unit(bound, "all", position, false); !errors.Is(err, domain.ErrValidation) {
				t.Fatal("invalid position", err)
			}
		}
		if _, _, err := read.Unit(bound, "invalid", 0, false); !errors.Is(err, domain.ErrValidation) {
			t.Fatal("invalid mode", err)
		}
		if _, _, err := read.Unit(ctx, "all", 0, false); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("unbound cache reuse", err)
		}
		canceled, cancel := context.WithCancel(bound)
		cancel()
		if _, _, err := read.Unit(canceled, "all", 0, false); !errors.Is(err, context.Canceled) {
			t.Fatal("canceled cache reuse", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := retained.Unit(retainedContext, "all", 0, false); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal("closed transaction cache reuse", err)
	}
	if _, err := storage.OpenDocVisibleGeneration(retainedContext, repo, fixture.hash); !errors.Is(err, pgx.ErrTxClosed) {
		t.Fatal("closed transaction reopened", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, _, err := retained.Unit(bound, "all", 0, false); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("new transaction cache reuse", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
