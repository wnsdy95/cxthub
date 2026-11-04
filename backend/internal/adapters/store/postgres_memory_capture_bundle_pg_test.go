//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryBundlePGFixture(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) domain.MemoryCaptureBundle {
	test.Helper()
	generation := memorySourceGenerationPG(test, ctx, storage, repo)
	header := domain.MemoryCaptureHeader{Version: domain.MemoryCaptureVersion, Rules: domain.MemoryCaptureRules,
		Generation: generation, Selection: domain.EffectiveMemorySelection{SnapshotID: repo, CodeCommit: strings.Repeat("a", 40)}}
	body := bytes.Repeat([]byte("shared memory bytes"), 8000)
	identity := domain.HashContent(body)
	parts := []domain.MemoryCapturePart{
		{Kind: "graph:repository", Key: string(repo), Sequence: generation.Graph.Sequence, Value: []byte(fmt.Sprintf(`{"id":%q}`, repo))},
		{Kind: "absent", Key: string(domain.HashContent([]byte("unavailable memory")))},
	}
	for offset := 0; offset < len(body); offset += domain.MemoryCaptureChunkBytes {
		parts = append(parts, domain.MemoryCapturePart{Kind: "memory", Key: fmt.Sprintf("%s:%08d", identity, offset/domain.MemoryCaptureChunkBytes),
			Value: body[offset:min(offset+domain.MemoryCaptureChunkBytes, len(body))]})
	}
	return memoryBundlePGSeal(test, ctx, header, parts)
}

func memoryBundlePGSeal(test *testing.T, ctx context.Context, header domain.MemoryCaptureHeader, parts []domain.MemoryCapturePart) domain.MemoryCaptureBundle {
	test.Helper()
	bundle, err := domain.SealMemoryCapture(ctx, header, parts)
	if err != nil {
		test.Fatal(err)
	}
	return bundle
}

func memoryBundlePGWrite(test *testing.T, storage *PostgresStore, ctx context.Context, bundle domain.MemoryCaptureBundle, expires time.Time) {
	test.Helper()
	if err := storage.WriteMemoryCapture(ctx, bundle, expires); err != nil {
		test.Fatal(err)
	}
}

func memoryBundlePGRead(test *testing.T, storage *PostgresStore, ctx context.Context, expected domain.MemoryCaptureBundle) {
	test.Helper()
	actual, err := storage.ReadMemoryCapture(ctx, expected.Header.Generation.Graph.RepoID, expected.ID)
	if err != nil || actual.ID != expected.ID || actual.Header != expected.Header || len(actual.Parts) != len(expected.Parts) {
		test.Fatalf("bundle read: id=%s err=%v", actual.ID, err)
	}
	for index, part := range expected.Parts {
		got := actual.Parts[index]
		if part.Kind != got.Kind || part.Key != got.Key || part.Sequence != got.Sequence || !bytes.Equal(part.Value, got.Value) {
			test.Fatalf("part %d changed", index)
		}
	}
}

func memoryBundlePGCounts(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) [3]int {
	test.Helper()
	var counts [3]int
	if err := storage.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM memory_capture_headers WHERE repo_id=$1),
		(SELECT count(*) FROM memory_capture_parts WHERE repo_id=$1),
		(SELECT count(*) FROM memory_capture_blocks WHERE repo_id=$1)`, string(repo)).Scan(&counts[0], &counts[1], &counts[2]); err != nil {
		test.Fatal(err)
	}
	return counts
}

func memoryBundlePGFault(test *testing.T, storage *PostgresStore, ctx context.Context, query string, args ...any) {
	test.Helper()
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	catalogExecPG(test, ctx, transaction, `SET LOCAL session_replication_role=replica`)
	catalogExecPG(test, ctx, transaction, query, args...)
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}

func memoryBundlePGExpire(test *testing.T, storage *PostgresStore, ctx context.Context, repo, identifier domain.ContentHash) {
	test.Helper()
	memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET created_at=clock_timestamp()-interval '2 hours',
		expires_at=clock_timestamp()-interval '1 hour' WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(identifier))
}

func TestPGMemoryCaptureBundleRestartReuseAndIsolation(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
	original := memorySourceBlobPG(test, ctx, storage, repo, "memory", "original")
	beforeGeneration := memorySourceGenerationPG(test, ctx, storage, repo)
	memoryBundlePGWrite(test, storage, ctx, bundle, expires)
	counts := memoryBundlePGCounts(test, storage, ctx, repo)
	for _, retryExpiry := range []time.Time{expires.Add(time.Hour), expires.Add(-30 * time.Minute)} {
		memoryBundlePGWrite(test, storage, ctx, bundle, retryExpiry)
	}
	var retained time.Time
	if err := storage.pool.QueryRow(ctx, `SELECT expires_at FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(bundle.ID)).Scan(&retained); err != nil || !retained.Equal(expires) {
		test.Fatalf("retry changed expiry: %s err=%v", retained, err)
	}
	if actual := memoryBundlePGCounts(test, storage, ctx, repo); actual != counts {
		test.Fatalf("duplicate rows: %v vs %v", actual, counts)
	}
	header := bundle.Header
	header.Generation.Sequence++
	next := memoryBundlePGSeal(test, ctx, header, bundle.Parts)
	memoryBundlePGWrite(test, storage, ctx, next, expires)
	if actual := memoryBundlePGCounts(test, storage, ctx, repo); actual != [3]int{2, counts[1] * 2, counts[2]} {
		test.Fatalf("capture did not share blocks: %v", actual)
	}
	if after := memorySourceGenerationPG(test, ctx, storage, repo); after != beforeGeneration {
		test.Fatal("capture store advanced original source state")
	}
	foreign := memorySourceRepoPG(test, ctx, storage, "foreign")
	if _, err := storage.ReadMemoryCapture(ctx, foreign, bundle.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("foreign repository read", err)
	}
	peer, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	storage.Close()
	test.Cleanup(peer.Close)
	memoryBundlePGRead(test, peer, ctx, bundle)
	memoryBundlePGRead(test, peer, ctx, next)
	var originalCount int
	if err := peer.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, string(original)).Scan(&originalCount); err != nil || originalCount != 1 {
		test.Fatal("original blob changed", err)
	}
	if applied, err := peer.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil || applied != 0 {
		test.Fatal("migration retry", applied, err)
	}
}

func TestPGMemoryCaptureBundleBorrowedTransactions(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	expires := time.Now().Add(time.Hour)
	memoryBundlePGWrite(test, storage, ctx, bundle, expires)
	for _, readonly := range []bool{false, true} {
		check := func(bound context.Context) error {
			peer := &PostgresStore{pool: storage.pool}
			for _, candidate := range []*PostgresStore{storage, peer} {
				if err := candidate.WriteMemoryCapture(bound, bundle, expires); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("borrowed write", err)
				}
				if count, err := candidate.PruneMemoryCaptures(bound, repo, 1); !errors.Is(err, domain.ErrConflict) || count != 0 {
					test.Fatal("borrowed prune", count, err)
				}
				if readonly && candidate == storage {
					memoryBundlePGRead(test, candidate, bound, bundle)
				} else if _, err := candidate.ReadMemoryCapture(bound, repo, bundle.ID); !errors.Is(err, domain.ErrConflict) {
					test.Fatal("borrowed write/foreign read", err)
				}
			}
			return nil
		}
		var err error
		if readonly {
			err = storage.WithinReadSnapshot(ctx, check)
		} else {
			err = storage.WithinRepository(ctx, repo, check)
		}
		if err != nil {
			test.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := storage.WriteMemoryCapture(canceled, bundle, expires); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if _, err := storage.ReadMemoryCapture(canceled, repo, bundle.ID); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
	if _, err := storage.PruneMemoryCaptures(canceled, repo, 1); !errors.Is(err, context.Canceled) {
		test.Fatal(err)
	}
}

func TestPGMemoryCaptureBundleConcurrentIdempotence(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	expires := time.Now().Add(time.Hour)
	var workers sync.WaitGroup
	failures := make(chan error, 12)
	for range 12 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			failures <- storage.WriteMemoryCapture(ctx, bundle, expires)
		}()
	}
	workers.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			test.Fatal(err)
		}
	}
	counts := memoryBundlePGCounts(test, storage, ctx, repo)
	if counts[0] != 1 || counts[1] != len(bundle.Parts) || counts[2] > len(bundle.Parts) {
		test.Fatal("concurrent duplicates", counts)
	}
	memoryBundlePGRead(test, storage, ctx, bundle)
}

func TestPGMemoryCaptureBundleExpirySharedGCAndRace(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	foreign := memorySourceRepoPG(test, ctx, storage, "retained")
	foreignBundle := memoryBundlePGFixture(test, storage, ctx, foreign)
	expires := time.Now().Add(time.Hour)
	memoryBundlePGWrite(test, storage, ctx, foreignBundle, expires)
	original := memorySourceBlobPG(test, ctx, storage, repo, "memory", "gc-original")
	for index := range 35 {
		header := bundle.Header
		header.Generation.Sequence += int64(index)
		expired := memoryBundlePGSeal(test, ctx, header, bundle.Parts)
		memoryBundlePGWrite(test, storage, ctx, expired, expires)
		memoryBundlePGExpire(test, storage, ctx, repo, expired.ID)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("expired read", err)
	}
	if err := storage.WriteMemoryCapture(ctx, bundle, expires); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("retry resurrected capture", err)
	}
	header := bundle.Header
	header.Generation.Sequence += 40
	active := memoryBundlePGSeal(test, ctx, header, bundle.Parts)
	failures := make(chan error, 2)
	go func() { failures <- storage.WriteMemoryCapture(ctx, active, expires) }()
	go func() {
		count, err := storage.PruneMemoryCaptures(ctx, repo, 32)
		if err == nil && count != 32 {
			err = fmt.Errorf("pruned %d, want 32", count)
		}
		failures <- err
	}()
	for range 2 {
		if err := <-failures; err != nil {
			test.Fatal(err)
		}
	}
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 32); err != nil || count != 3 {
		test.Fatal(count, err)
	}
	memoryBundlePGRead(test, storage, ctx, active)
	memoryBundlePGRead(test, storage, ctx, foreignBundle)
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 32); err != nil || count != 0 {
		test.Fatal("active capture pruned", count, err)
	}
	memoryBundlePGExpire(test, storage, ctx, repo, active.ID)
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err != nil || count != 1 {
		test.Fatal(count, err)
	}
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{} {
		test.Fatal("unreferenced blocks survived", counts)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, active.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("pruned capture read", err)
	}
	var originalCount int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(repo), string(original)).Scan(&originalCount); err != nil || originalCount != 1 {
		test.Fatal("capture GC touched original ownership", err)
	}
	memoryBundlePGRead(test, storage, ctx, foreignBundle)
}

func TestPGMemoryCaptureBundleWallClockExpiry(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	expires := time.Now().Add(500 * time.Millisecond)
	memoryBundlePGWrite(test, storage, ctx, bundle, expires)
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		memoryBundlePGRead(test, storage, bound, bundle)
		time.Sleep(time.Until(expires) + 10*time.Millisecond)
		if _, err := storage.ReadMemoryCapture(bound, repo, bundle.ID); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
			test.Fatal("snapshot ignored wall-clock expiry", err)
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	if count, err := storage.PruneMemoryCaptures(ctx, repo, 1); err != nil || count != 1 {
		test.Fatal("expired capture not pruned", count, err)
	}
}

func TestPGMemoryCaptureBundleValidationBeforeTransaction(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	lock := catalogTxPG(test, ctx, storage)
	if err := lockMemoryCaptureRepo(ctx, lock, repo); err != nil {
		test.Fatal(err)
	}
	bounded, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for _, expiry := range []time.Time{{}, time.Now().Add(-time.Hour), time.Now().Add(25 * time.Hour), time.Date(100000, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if err := storage.WriteMemoryCapture(bounded, bundle, expiry); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("retention envelope", err)
		}
	}
	bad := bundle
	bad.ID = domain.HashContent([]byte("incorrect root"))
	if err := storage.WriteMemoryCapture(bounded, bad, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("root validation entered transaction", err)
	}
	bad = bundle
	bad.Parts = append([]domain.MemoryCapturePart(nil), bundle.Parts...)
	bad.Parts[0].Value = make([]byte, domain.MaxMemoryCapturePartBytes+1)
	if err := storage.WriteMemoryCapture(bounded, bad, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrRepositoryViewSourceLimit) {
		test.Fatal("payload limit entered transaction", err)
	}
	for _, limit := range []int{-1, 0, 33} {
		if count, err := storage.PruneMemoryCaptures(bounded, repo, limit); !errors.Is(err, domain.ErrValidation) || count != 0 {
			test.Fatal("prune limit entered transaction", count, err)
		}
	}
	if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{} {
		test.Fatal("invalid capture persisted", counts)
	}
}

func TestPGMemoryCaptureBundleSnapshotNotReopened(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	memoryBundlePGWrite(test, storage, ctx, bundle, time.Now().Add(time.Hour))
	err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		memoryBundlePGRead(test, storage, bound, bundle)
		memoryBundlePGFault(test, storage, ctx, `DELETE FROM memory_capture_parts WHERE repo_id=$1 AND capture_id=$2 AND ordinal=0`, string(repo), string(bundle.ID))
		memoryBundlePGRead(test, storage, bound, bundle)
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ReadMemoryCapture(ctx, repo, bundle.ID); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("new snapshot failed to see missing part", err)
	}
}

func TestPGMemoryCaptureBundleRejectsZeroParts(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	bundle := memoryBundlePGFixture(test, storage, ctx, repo)
	bundle.Parts = nil
	if err := storage.WriteMemoryCapture(ctx, bundle, time.Now().Add(time.Hour)); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("empty capture accepted", err)
	}
}

func TestPGMemoryCaptureBundleBatchBudgets(test *testing.T) {
	for _, size := range []int{1, domain.MemoryCaptureChunkBytes, domain.MaxMemoryCapturePartBytes} {
		parts := make([]domain.MemoryCapturePart, 200)
		body := make([]byte, size)
		for index := range parts {
			parts[index] = domain.MemoryCapturePart{Kind: "memory", Key: strings.Repeat("k", memoryCaptureKeyBytes), Value: body}
		}
		for start := 0; start < len(parts); {
			end := memoryCaptureBatchEnd(parts, start)
			cost := 0
			for _, part := range parts[start:end] {
				cost += len(part.Value) + len(part.Key) + len(part.Kind) + memoryCaptureBatchOverhead
			}
			if end <= start || end-start > memoryCaptureBatchParts || cost > memoryCaptureBatchBytes {
				test.Fatalf("unbounded batch %d..%d = %d bytes", start, end, cost)
			}
			start = end
		}
	}
}
