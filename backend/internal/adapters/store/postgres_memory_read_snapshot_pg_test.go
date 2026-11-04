//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryReadPGWaitGate(test *testing.T, storage *PostgresStore, ctx context.Context, name string, done <-chan error) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var waiting bool
		if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE locktype='advisory' AND NOT granted
			AND objid::bigint=(hashtextextended($1,0)&4294967295))`, name).Scan(&waiting); err != nil {
			test.Fatal(err)
		}
		if waiting {
			return
		}
		select {
		case err := <-done:
			test.Fatalf("writer did not reach gate: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			test.Fatal("writer gate timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestPGMemoryReadAtomicVisibilityCancellationAndPruneLock(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 130)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	name := "memory-read-gate:" + string(repo)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, name)
	memoryReadPGGate(test, storage, ctx, repo, fmt.Sprintf("PERFORM pg_advisory_xact_lock(hashtextextended('%s',0))", name))
	writer, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- storage.WriteMemoryRead(writer, bundle) }()
	memoryReadPGWaitGate(test, storage, ctx, name, done)
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("uncommitted result visible", counts)
	}
	if _, err := storage.ReadMemoryReadHeader(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("incomplete result readable", err)
	}
	pruning, stop := context.WithTimeout(ctx, 100*time.Millisecond)
	_, err := storage.pool.Exec(pruning, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(capture.ID))
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		test.Fatal("capture was not locked against pruning", err)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		test.Fatal("canceled write committed", err)
	}
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("canceled write leaked rows", counts)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadPGWrite(test, storage, ctx, bundle)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(capture.ID))
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("prune did not cascade after publication", counts)
	}
}

func memoryReadPGExpireSoon(test *testing.T, storage *PostgresStore, ctx context.Context, repo, capture domain.ContentHash) {
	test.Helper()
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET created_at=clock_timestamp()-interval '1 hour',
		expires_at=clock_timestamp()+interval '1 second' WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(capture))
}

func memoryReadPGWaitExpiry(test *testing.T, storage *PostgresStore, ctx context.Context, repo, capture domain.ContentHash) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, `SELECT pg_sleep(GREATEST(0,extract(epoch FROM expires_at-clock_timestamp()))::double precision+0.02)
		FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(capture))
}

func TestPGMemoryReadExpiresDuringPublication(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	name := "memory-read-expiry:" + string(repo)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, name)
	memoryReadPGGate(test, storage, ctx, repo, fmt.Sprintf("PERFORM pg_advisory_xact_lock(hashtextextended('%s',0))", name))
	memoryReadPGExpireSoon(test, storage, ctx, repo, capture.ID)
	done := make(chan error, 1)
	go func() { done <- storage.WriteMemoryRead(ctx, bundle) }()
	memoryReadPGWaitGate(test, storage, ctx, name, done)
	memoryReadPGWaitExpiry(test, storage, ctx, repo, capture.ID)
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("publication ignored current database time", err)
	}
	if counts := memoryReadPGCounts(test, storage, ctx, repo); counts != [2]int{} {
		test.Fatal("expired publication survived", counts)
	}
}

type memoryReadSnapshotProbe struct {
	pgx.Tx
	afterIndex func()
	afterChunk func()
}

func (probe *memoryReadSnapshotProbe) Query(ctx context.Context, query string, arguments ...any) (pgx.Rows, error) {
	rows, err := probe.Tx.Query(ctx, query, arguments...)
	if err == nil && query == memoryReadIndexSQL && probe.afterIndex != nil {
		probe.afterIndex()
	}
	return rows, err
}

func (probe *memoryReadSnapshotProbe) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	row := probe.Tx.QueryRow(ctx, query, arguments...)
	if query == memoryReadChunkSQL && probe.afterChunk != nil {
		return memoryReadRowProbe{Row: row, afterScan: probe.afterChunk}
	}
	return row
}

type memoryReadRowProbe struct {
	pgx.Row
	afterScan func()
}

func (probe memoryReadRowProbe) Scan(destinations ...any) error {
	if err := probe.Row.Scan(destinations...); err != nil {
		return err
	}
	probe.afterScan()
	return nil
}

func TestPGMemoryReadFinalClockCheckInBorrowedSnapshot(test *testing.T) {
	for _, selected := range []bool{false, true} {
		test.Run(fmt.Sprint(selected), func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
			memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
			memoryReadPGWrite(test, storage, ctx, bundle)
			memoryReadPGExpireSoon(test, storage, ctx, repo, capture.ID)
			transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
			if err != nil {
				test.Fatal(err)
			}
			defer rollbackPG(transaction)
			called := false
			wait := func() {
				called = true
				memoryReadPGWaitExpiry(test, storage, ctx, repo, capture.ID)
			}
			probe := &memoryReadSnapshotProbe{Tx: transaction, afterIndex: wait}
			if selected {
				probe.afterIndex, probe.afterChunk = nil, wait
			}
			bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: probe, owner: storage, readOnly: true, repo: repo})
			if selected {
				if raw, err := storage.ReadMemoryReadChunk(bound, repo, bundle.ID, 0); !errors.Is(err, domain.ErrMemoryCaptureExpired) || raw != nil {
					test.Fatal("chunk used transaction-start time", err)
				}
			} else if header, err := storage.ReadMemoryReadHeader(bound, repo, bundle.ID); !errors.Is(err, domain.ErrMemoryCaptureExpired) || header.CaptureID != "" {
				test.Fatal("header used transaction-start time", err)
			}
			if !called {
				test.Fatal("read did not reach expiry gate")
			}
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal("reader closed borrowed transaction", err)
			}
		})
	}
}

func TestPGMemoryReadSnapshotPinnedAcrossIndexAndChunk(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, bundle)
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	called := false
	probe := &memoryReadSnapshotProbe{Tx: transaction, afterIndex: func() {
		called = true
		memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_chunks SET body=set_byte(body,10,120) WHERE repo_id=$1 AND read_id=$2`, string(repo), string(bundle.ID))
	}}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: probe, owner: storage, readOnly: true, repo: repo})
	if _, err := storage.ReadMemoryReadChunk(bound, repo, bundle.ID, 0); err != nil || !called {
		test.Fatal("snapshot changed between index and selected payload", err)
	}
	if _, err := storage.ReadMemoryReadChunk(ctx, repo, bundle.ID, 0); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("fresh snapshot did not verify current chunk bytes", err)
	}
}

func TestPGMemoryReadConcurrentUnindexedAppendRejected(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	capture, bundle := memoryReadPGFixture(test, storage, ctx, repo, 1)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `INSERT INTO memory_read_chunks(repo_id,read_id,ordinal,chunk_hash,start_item,item_count,body)
		VALUES($1,$2,1,$3,1,1,$4)`, string(repo), string(bundle.ID), string(bundle.Chunks[0].Hash), bundle.Chunks[0].Bytes)
	memoryReadPGWrite(test, storage, ctx, bundle)
	if err := transaction.Commit(ctx); err == nil {
		test.Fatal("concurrent unindexed append committed")
	}
	memoryReadPGAssert(test, storage, ctx, bundle)
}
