//go:build postgres

package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGMemoryReadRangeByteCapAndBoundedSQL(test *testing.T) {
	storage, ctx, bundle := memoryReadRangePGFixture(test, 1)
	repo := bundle.Header.Source.Graph.RepoID
	items, err := domain.DecodeMemoryReadChunk(ctx, bundle.Header, 0, bundle.Chunks[0].Bytes)
	if err != nil {
		test.Fatal(err)
	}
	items[0].Text = strings.Repeat("x", domain.MaxMemoryReadChunkBytes-2-len(bundle.Chunks[0].Bytes)+len(items[0].Text))
	bundle = memoryReadPGSeal(test, ctx, bundle.Header, items)
	if len(bundle.Chunks[0].Bytes) != domain.MaxMemoryReadChunkBytes-2 {
		test.Fatal("large chunk fixture", len(bundle.Chunks[0].Bytes))
	}
	memoryReadPGWrite(test, storage, ctx, bundle)
	err = storage.WithMemoryReadRange(ctx, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
		for range 3 {
			if _, err := reader.ReadChunk(bound, 0); err != nil {
				test.Fatal("premature byte cap", err)
			}
		}
		if _, err := reader.ReadChunk(bound, 0); !errors.Is(err, domain.ErrMemoryProjectionLimit) {
			test.Fatal("byte cap missing", err)
		}
		if reader.(*memoryReadRange).reads != 3 {
			test.Fatal("excess payload attempted SQL")
		}
		return nil
	})
	if !errors.Is(err, domain.ErrMemoryProjectionLimit) {
		test.Fatal("byte cap suppressible", err)
	}
	var digest *string
	var start, count, size int64
	var raw []byte
	if err := storage.pool.QueryRow(ctx, memoryReadRangeChunkSQL, string(repo), string(bundle.ID), 0, domain.MaxMemoryReadChunkBytes-3).
		Scan(&digest, &start, &count, &size, &raw); err != nil {
		test.Fatal(err)
	}
	if size != int64(len(bundle.Chunks[0].Bytes)) || raw != nil {
		test.Fatal("payload exceeded SQL transfer bound", size, len(raw))
	}
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `ALTER TABLE memory_read_chunks DROP CONSTRAINT memory_read_chunks_body_check`)
	catalogExecPG(test, ctx, transaction, `SET LOCAL session_replication_role=replica`)
	catalogExecPG(test, ctx, transaction, `UPDATE memory_read_chunks SET body=decode(repeat('00',262147),'hex') WHERE repo_id=$1 AND read_id=$2`, string(repo), string(bundle.ID))
	verifier, err := domain.NewMemoryReadChunkVerifier(ctx, repo, bundle.ID, bundle.Header)
	if err != nil {
		test.Fatal(err)
	}
	scope := &memoryReadRange{active: true, context: ctx, transaction: transaction, repo: repo, identifier: bundle.ID, references: bundle.Header.Chunks, verifier: verifier}
	if items, err := scope.ReadChunk(context.WithValue(ctx, memoryReadRangeKey{}, scope), 0); !errors.Is(err, domain.ErrMemoryProjectionLimit) || items != nil {
		test.Fatal("corrupt oversized body accepted", err)
	}
}

func memoryReadRangeWaitLock(test *testing.T, storage *PostgresStore, ctx context.Context, identifier uint32, done <-chan error) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE pid=$1 AND NOT granted)`, identifier).Scan(&waiting); err != nil {
			test.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			test.Fatal("read ended before reaching SQL lock", err)
		default:
		}
		if time.Now().After(deadline) {
			test.Fatal("selected read never reached SQL lock")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPGMemoryReadRangeDetachedContextCannotEscapeCancellation(test *testing.T) {
	for _, scenario := range []string{"parent", "callback-end"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, bundle := memoryReadRangePGFixture(test, 1)
			repo := bundle.Header.Source.Graph.RepoID
			blocker := catalogTxPG(test, ctx, storage)
			defer rollbackPG(blocker)
			parent, cancel := context.WithCancel(ctx)
			defer cancel()
			finished := make(chan error, 1)
			err := storage.WithMemoryReadRange(parent, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, reader outbound.MemoryReadChunkReader) error {
				catalogExecPG(test, ctx, blocker, `LOCK TABLE memory_read_chunks IN ACCESS EXCLUSIVE MODE`)
				identifier := reader.(*memoryReadRange).transaction.Conn().PgConn().PID()
				go func() {
					_, err := reader.ReadChunk(context.WithoutCancel(bound), 0)
					finished <- err
				}()
				memoryReadRangeWaitLock(test, storage, ctx, identifier, finished)
				if scenario == "parent" {
					cancel()
				}
				return nil
			})
			if !errors.Is(err, context.Canceled) {
				test.Fatal("detached child survived end of operation", err)
			}
			select {
			case err := <-finished:
				if !errors.Is(err, context.Canceled) {
					test.Fatal("blocked read ignored cancellation", err)
				}
			case <-time.After(time.Second):
				test.Fatal("blocked reader leaked beyond callback")
			}
		})
	}
}

func TestPGMemoryReadRangeBorrowedFinalExpiry(test *testing.T) {
	storage, ctx, bundle := memoryReadRangePGFixture(test, 0)
	repo := bundle.Header.Source.Graph.RepoID
	memoryReadPGExpireSoon(test, storage, ctx, repo, bundle.Header.CaptureID)
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: transaction, owner: storage, readOnly: true, repo: repo})
	err = storage.WithMemoryReadRange(bound, repo, bundle.ID, func(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
		memoryReadPGWaitExpiry(test, storage, ctx, repo, bundle.Header.CaptureID)
		return nil
	})
	if !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("borrowed snapshot ignored wall clock expiry", err)
	}
	if _, err := transaction.Exec(ctx, `SELECT 1`); err != nil {
		test.Fatal("range closed caller-owned transaction", err)
	}
}
