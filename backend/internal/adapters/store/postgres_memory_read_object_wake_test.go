//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryReadObjectWakeStatePG struct {
	version     int64
	scanVersion int64
	throughRepo pgtype.Text
	afterRepo   string
	nextAttempt time.Time
	attempts    int
	reason      string
}

func memoryReadObjectWakeDrainPG(test *testing.T, storage *PostgresStore, ctx context.Context) {
	test.Helper()
	for range 128 {
		count, err := storage.DispatchMemoryReadObjectWakes(ctx, 32)
		if err != nil {
			test.Fatal(err)
		}
		if count == 0 {
			return
		}
	}
	test.Fatal("object notice fixture did not drain")
}

func memoryReadObjectWakePG(test *testing.T) (*PostgresStore, context.Context, domain.ContentHash, domain.ContentHash) {
	test.Helper()
	storage, ctx, repo := catalogPG(test)
	if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
		test.Fatal(err)
	}
	hash := memorySourceBlobPG(test, ctx, storage, repo, "memory", "physical outbox")
	memoryReadObjectWakeDrainPG(test, storage, ctx)
	return storage, ctx, repo, hash
}

func memoryReadObjectWakeReadPG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) memoryReadObjectWakeStatePG {
	test.Helper()
	var event memoryReadObjectWakeStatePG
	if err := storage.pool.QueryRow(ctx, `SELECT version,scan_version,through_repo,after_repo,next_attempt,attempts,reason FROM memory_read_object_wakes WHERE hash=$1`, string(hash)).
		Scan(&event.version, &event.scanVersion, &event.throughRepo, &event.afterRepo, &event.nextAttempt, &event.attempts, &event.reason); err != nil {
		test.Fatal(err)
	}
	return event
}

func memoryReadObjectWakeAbsentPG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) {
	test.Helper()
	var exists bool
	if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM memory_read_object_wakes WHERE hash=$1)`, string(hash)).Scan(&exists); err != nil || exists {
		test.Fatal("physical outbox event retained unexpectedly", exists, err)
	}
}

func memoryReadObjectWakeDispatchPG(test *testing.T, storage *PostgresStore, ctx context.Context, limit, expected int) {
	test.Helper()
	if count, err := storage.DispatchMemoryReadObjectWakes(ctx, limit); err != nil || count != expected {
		test.Fatal("object dispatch count", count, expected, err)
	}
}

func memoryReadObjectWakeOwnerPG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash, ordinal int) domain.ContentHash {
	test.Helper()
	prefix := strings.TrimPrefix(string(domain.HashContent([]byte(test.Name()))), "sha256:")[:32]
	repo := domain.ContentHash(fmt.Sprintf("sha256:%s%032x", prefix, ordinal))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(repo), string(hash))
	return repo
}

func memoryReadObjectWakeOwnersPG(test *testing.T, storage *PostgresStore, ctx context.Context, original, hash domain.ContentHash) []domain.ContentHash {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
	var owners []domain.ContentHash
	for ordinal := 10; ordinal < 45; ordinal++ {
		owners = append(owners, memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, ordinal))
	}
	return owners
}

func TestPGMemoryReadObjectWakeRepeatableWriterFindsLaterOwner(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	peer := memorySourceRepoPG(test, ctx, storage, "new owner after writer snapshot")
	if err := storage.ConfigureMemoryReadWork(ctx, peer, true); err != nil {
		test.Fatal(err)
	}
	writer, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(writer)
	var owned bool
	if err := writer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND hash=$2)`, string(peer), string(hash)).Scan(&owned); err != nil || owned {
		test.Fatal("writer saw future owner", owned, err)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(peer), string(hash))
	before := map[domain.ContentHash]memoryReadWakeStatePG{}
	sources := map[domain.ContentHash]domain.RepositoryMemorySourceGeneration{}
	for _, owner := range []domain.ContentHash{repo, peer} {
		before[owner] = memoryReadWakeReadPG(test, storage, ctx, owner)
		sources[owner] = memorySourceGenerationPG(test, ctx, storage, owner)
	}
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	if err := writer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND hash=$2)`, string(peer), string(hash)).Scan(&owned); err != nil || owned {
		test.Fatal("repeatable snapshot changed", owned, err)
	}
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	for _, owner := range []domain.ContentHash{repo, peer} {
		if actual := memoryReadWakeReadPG(test, storage, ctx, owner); !reflect.DeepEqual(actual, before[owner]) {
			test.Fatal("writer fanned out to an owner", owner, actual)
		}
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 3)
	for _, owner := range []domain.ContentHash{repo, peer} {
		if after := memoryReadWakeReadPG(test, storage, ctx, owner); after.objectRevision <= before[owner].objectRevision || after.nextAttempt == nil {
			test.Fatal("fresh dispatcher omitted a current owner", owner, before[owner], after)
		}
		if actual := memorySourceGenerationPG(test, ctx, storage, owner); actual != sources[owner] {
			test.Fatal("object dispatcher changed logical source", owner)
		}
	}
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}

func TestPGMemoryReadObjectWakeAllUpdatesRollbackAndCascade(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	before := memoryReadWakeReadPG(test, storage, ctx, repo)
	writer := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	var version int64
	if err := writer.QueryRow(ctx, `SELECT version FROM memory_read_object_wakes WHERE hash=$1`, string(hash)).Scan(&version); err != nil || version <= 0 {
		test.Fatal("hash-only update omitted witness notice", version, err)
	}
	if err := writer.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	first := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if first.version <= version || first.afterRepo != "" {
		test.Fatal("rolled-back fence reused", version, first)
	}
	if actual := memoryReadWakeReadPG(test, storage, ctx, repo); !reflect.DeepEqual(actual, before) {
		test.Fatal("physical writer touched repo notice", actual)
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM blob_source_incarnations WHERE hash=$1`, string(hash))
	removed := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blob_source_incarnations(hash) VALUES($1)`, string(hash))
	inserted := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if removed.version <= first.version || inserted.version <= removed.version {
		test.Fatal("incarnation repair omitted fencing", first, removed, inserted)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
	if actual := memoryReadWakeReadPG(test, storage, ctx, repo); actual.objectRevision <= before.objectRevision {
		test.Fatal("all-update notice not dispatched", actual)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}

func TestPGMemoryReadObjectWakeWriterDoesNotReadOwnership(test *testing.T) {
	storage, ctx, _, hash := memoryReadObjectWakePG(test)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `LOCK TABLE repo_blobs IN ACCESS EXCLUSIVE MODE`)
	writer := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, writer, `SET LOCAL statement_timeout='250ms'`)
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	var definition string
	if err := storage.pool.QueryRow(ctx, `SELECT pg_get_functiondef('cxt_memory_read_object_wake()'::regprocedure)`).Scan(&definition); err != nil {
		test.Fatal(err)
	}
	for _, forbidden := range []string{"repo_blobs", "memory_read_work", "bytes"} {
		if strings.Contains(definition, forbidden) {
			test.Fatal("object writer reads bodies or recipients", forbidden)
		}
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
}

func TestPGMemoryReadObjectWakeBoundsAndOwnedWriteContext(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	for _, limit := range []int{-1, 0, 33} {
		if count, err := storage.DispatchMemoryReadObjectWakes(ctx, limit); count != 0 || !errors.Is(err, domain.ErrValidation) {
			test.Fatal("unbounded dispatch accepted", count, err)
		}
	}
	check := func(bound context.Context) error {
		for _, candidate := range []*PostgresStore{storage, {pool: storage.pool}} {
			if count, err := candidate.DispatchMemoryReadObjectWakes(bound, 1); count != 0 || !errors.Is(err, domain.ErrConflict) {
				test.Fatal("borrowed/read-only dispatch accepted", count, err)
			}
		}
		return nil
	}
	if err := storage.WithinReadSnapshot(ctx, check); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, repo, check); err != nil {
		test.Fatal(err)
	}
	if err := check(outbound.WithDocReadOnly(ctx)); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	second := memorySourceBlobPG(test, ctx, storage, repo, "memory_chunk", "second bounded event")
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(second))
	blocked := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocked, `SELECT 1 FROM memory_read_object_wakes WHERE hash=$1 FOR UPDATE`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, second)
	memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if err := blocked.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 2)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 0)
}

func TestPGMemoryReadObjectWakePagesUseCurrentOwners(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, original, hash)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(owners[0]), string(hash))
	before := make(map[domain.ContentHash]memoryReadWakeStatePG)
	for _, repo := range owners {
		before[repo] = memoryReadWakeReadPG(test, storage, ctx, repo)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	event := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	page := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if page.version != event.version || page.afterRepo != string(owners[31]) {
		test.Fatal("page did not preserve fence or cap recipients at 32", event, page)
	}
	for ordinal, repo := range owners {
		after := memoryReadWakeReadPG(test, storage, ctx, repo)
		if (after.objectRevision > before[repo].objectRevision) != (ordinal < 32) {
			test.Fatal("wrong owner notified on first page", ordinal, before[repo], after)
		}
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(owners[32]), string(hash))
	if err := storage.ConfigureMemoryReadWork(ctx, owners[33], false); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET kind='doc' WHERE repo_id=$1 AND hash=$2`, string(owners[34]), string(hash))
	behind := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 5)
	ahead := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 50)
	behindNotice := memoryReadWakeReadPG(test, storage, ctx, behind)
	aheadNotice := memoryReadWakeReadPG(test, storage, ctx, ahead)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	if after := memoryReadWakeReadPG(test, storage, ctx, ahead); !reflect.DeepEqual(after, aheadNotice) || after.nextAttempt == nil {
		test.Fatal("beyond-upper-fence owner lost its independent ownership wake", aheadNotice, after)
	}
	if after := memoryReadWakeReadPG(test, storage, ctx, behind); !reflect.DeepEqual(after, behindNotice) || after.nextAttempt == nil {
		test.Fatal("behind-cursor owner lost its ownership wake", behindNotice, after)
	}
	for _, repo := range owners[32:] {
		if after := memoryReadWakeReadPG(test, storage, ctx, repo); after.objectRevision != before[repo].objectRevision {
			test.Fatal("revoked, disabled or document-only owner notified", repo, after)
		}
	}
	if err := storage.ConfigureMemoryReadWork(ctx, owners[33], true); err != nil {
		test.Fatal(err)
	}
	if after := memoryReadWakeReadPG(test, storage, ctx, owners[33]); after.objectRevision <= before[owners[33]].objectRevision {
		test.Fatal("re-enable did not cover disabled gap", after)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDrainPG(test, storage, ctx)
	if after := memoryReadWakeReadPG(test, storage, ctx, ahead); after.objectRevision <= aheadNotice.objectRevision {
		test.Fatal("next sweep omitted an owner beyond the previous upper fence", after)
	}
	test.Log("current ownership is checked inside the frozen upper fence; additions outside it retain source wakes and join the next physical sweep")
}

func TestPGMemoryReadObjectWakeNewVersionFinishesSweepBeforeRestart(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, repo, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	prior := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	before := memoryReadWakeReadPG(test, storage, ctx, owners[0])
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	current := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if current.version <= prior.version || current.afterRepo != prior.afterRepo || current.scanVersion != prior.scanVersion || current.nextAttempt != prior.nextAttempt || current.attempts != 0 {
		test.Fatal("new object version discarded the in-flight sweep or due age", prior, current)
	}
	stale := catalogTxPG(test, ctx, storage)
	if err := dispatchMemoryReadObjectWakePage(ctx, stale, string(hash), prior.version, prior.scanVersion, prior.afterRepo); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale event cursor acknowledged newer version", err)
	}
	if err := stale.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadObjectWakeReadPG(test, storage, ctx, hash); actual != current {
		test.Fatal("stale version changed event", actual)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 4)
	if actual := memoryReadWakeReadPG(test, storage, ctx, owners[0]); !reflect.DeepEqual(actual, before) {
		test.Fatal("current sweep revisited a completed page before finishing later owners", before, actual)
	}
	if next := memoryReadObjectWakeReadPG(test, storage, ctx, hash); next.scanVersion != current.version || next.afterRepo != "" {
		test.Fatal("completed old sweep did not schedule the newer fence", current, next)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	if actual := memoryReadWakeReadPG(test, storage, ctx, owners[0]); actual.objectRevision <= before.objectRevision {
		test.Fatal("restart omitted an already traversed owner", actual)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 4)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}

func TestPGMemoryReadObjectWakeInitialUploadsDoNotQueueFanout(test *testing.T) {
	storage, ctx, repo, _ := memoryReadObjectWakePG(test)
	for ordinal := range 40 {
		hash := memorySourceBlobPG(test, ctx, storage, repo, "doc", fmt.Sprintf("new conversation upload %d", ordinal))
		memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	}
	writer := catalogTxPG(test, ctx, storage)
	body := []byte(test.Name() + string(repo))
	hash := domain.HashContent(body)
	catalogExecPG(test, ctx, writer, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), body)
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 1)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM blob_source_incarnations WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blob_source_incarnations(hash) VALUES($1)`, string(hash))
	memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
}

func TestPGMemoryReadObjectWakeFailureIsolationAndSaturatedRetry(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, repo, hash)
	goodRepo := memorySourceRepoPG(test, ctx, storage, "healthy event")
	if err := storage.ConfigureMemoryReadWork(ctx, goodRepo, true); err != nil {
		test.Fatal(err)
	}
	goodHash := memorySourceBlobPG(test, ctx, storage, goodRepo, "memory", "healthy event")
	before := memoryReadWakeReadPG(test, storage, ctx, owners[0])
	goodBefore := memoryReadWakeReadPG(test, storage, ctx, goodRepo)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(goodHash))
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_wakes WHERE repo_id=$1 FOR UPDATE`, string(owners[1]))
	started := time.Now()
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	if time.Since(started) > time.Second {
		test.Fatal("blocked recipient exceeded transaction budget")
	}
	event := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if event.attempts != 0 || event.reason != "" || event.afterRepo != string(owners[31]) {
		test.Fatal("recipient failure rolled back durable page expansion", event)
	}
	delivery := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owners[1])
	if delivery.attempts != 1 || delivery.reason != "temporary_failure" || delivery.version != event.version || !delivery.nextAttempt.After(time.Now()) {
		test.Fatal("failed recipient obligation not durably deferred", delivery)
	}
	if actual := memoryReadWakeReadPG(test, storage, ctx, owners[0]); actual.objectRevision <= before.objectRevision {
		test.Fatal("blocked recipient rolled back healthy recipient", before, actual)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 2)
	if actual := memoryReadWakeReadPG(test, storage, ctx, goodRepo); actual.objectRevision <= goodBefore.objectRevision {
		test.Fatal("blocked event starved healthy fanout", actual)
	}
	memoryReadObjectWakeAbsentPG(test, storage, ctx, goodHash)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 4)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	for attempt := 2; attempt <= 8; attempt++ {
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp()-interval '1 second' WHERE hash=$1`, string(hash))
		memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
		current := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owners[1])
		if current.attempts != min(attempt, 6) || current.version != event.version || current.reason != "temporary_failure" {
			test.Fatal("retry counter did not saturate without losing recipient obligation", current)
		}
		var delay float64
		if err := storage.pool.QueryRow(ctx, `SELECT extract(epoch FROM next_attempt-clock_timestamp()) FROM memory_read_object_wake_deliveries WHERE hash=$1 AND repo_id=$2`, string(hash), string(owners[1])).Scan(&delay); err != nil || delay <= 0 || delay > 32 {
			test.Fatal("retry delay not finite and bounded", delay, err)
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp()-interval '1 second' WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
		test.Fatal("successful retry retained its exact obligation", count)
	}
}

func TestPGMemoryReadObjectWakeCancellationRollsBackPage(test *testing.T) {
	for _, paged := range []bool{false, true} {
		test.Run(fmt.Sprint(paged), func(test *testing.T) {
			storage, parent, repo, hash := memoryReadObjectWakePG(test)
			prefix := "DELETE FROM memory_read_object_wakes"
			if paged {
				owners := memoryReadObjectWakeOwnersPG(test, storage, parent, repo, hash)
				repo = owners[0]
				prefix = "UPDATE memory_read_object_wakes SET after_repo"
			}
			catalogExecPG(test, parent, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
			before := memoryReadWakeReadPG(test, storage, parent, repo)
			event := memoryReadObjectWakeReadPG(test, storage, parent, hash)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, prefix)
			defer release()
			done := make(chan error, 1)
			go func() {
				_, err := peer.DispatchMemoryReadObjectWakes(ctx, 1)
				done <- err
			}()
			memoryQualificationWaitPG(test, parent, reached, done)
			cancel()
			release()
			if err := <-done; !errors.Is(err, context.Canceled) {
				test.Fatal("canceled fanout committed", err)
			}
			if actual := memoryReadObjectWakeReadPG(test, storage, parent, hash); actual != event {
				test.Fatal("canceled fanout changed event fence or cursor", event, actual)
			}
			if count := memoryReadObjectWakeDeliveryCountPG(test, storage, parent, hash); count != 0 {
				test.Fatal("canceled page left durable recipient obligations", count)
			}
			if actual := memoryReadWakeReadPG(test, storage, parent, repo); !reflect.DeepEqual(actual, before) {
				test.Fatal("canceled fanout published notices", before, actual)
			}
			memoryReadObjectWakeDrainPG(test, storage, parent)
		})
	}
}

func TestPGMemoryReadObjectWakeWaitingWriterPreservesSweepAfterAcknowledgement(test *testing.T) {
	for _, paged := range []bool{false, true} {
		test.Run(fmt.Sprint(paged), func(test *testing.T) {
			storage, ctx, repo, hash := memoryReadObjectWakePG(test)
			prefix := "DELETE FROM memory_read_object_wakes"
			var owners []domain.ContentHash
			if paged {
				owners = memoryReadObjectWakeOwnersPG(test, storage, ctx, repo, hash)
				prefix = "UPDATE memory_read_object_wakes SET after_repo"
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
			before := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, prefix)
			defer release()
			done := make(chan error, 1)
			go func() {
				_, err := peer.DispatchMemoryReadObjectWakes(ctx, 1)
				done <- err
			}()
			memoryQualificationWaitPG(test, ctx, reached, done)
			writer := catalogTxPG(test, ctx, storage)
			written := make(chan error, 1)
			go func() {
				_, err := writer.Exec(ctx, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
				written <- err
			}()
			memoryQualificationBlockedPG(test, ctx, storage, writer.Conn().PgConn().PID(), written)
			release()
			if err := <-done; err != nil {
				test.Fatal("dispatcher failed before releasing waiting writer", err)
			}
			if err := <-written; err != nil {
				test.Fatal(err)
			}
			if err := writer.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			current := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
			if current.version <= before.version || current.attempts != 0 {
				test.Fatal("acknowledgement erased or partially consumed later update", before, current)
			}
			if paged && (current.afterRepo != string(owners[31]) || current.scanVersion != before.version) {
				test.Fatal("waiting writer reset the committed page or sweep fence", before, current)
			}
			if !paged && (current.afterRepo != "" || current.scanVersion != 0) {
				test.Fatal("waiting writer failed to recreate an unstarted sweep", current)
			}
			memoryReadObjectWakeDrainPG(test, storage, ctx)
		})
	}
}

func TestPGMemoryReadObjectWakeParallelDispatchConsumesOnce(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	before := memoryReadWakeReadPG(test, storage, ctx, repo)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	type result struct {
		count int
		err   error
	}
	done := make(chan result, 8)
	for range 8 {
		go func() {
			count, err := storage.DispatchMemoryReadObjectWakes(ctx, 1)
			done <- result{count, err}
		}()
	}
	processed := 0
	for range 8 {
		actual := <-done
		if actual.err != nil || actual.count < 0 || actual.count > 2 {
			test.Fatal("parallel dispatch failed", actual)
		}
		processed += actual.count
	}
	if processed != 2 {
		test.Fatal("one expansion and one recipient delivery were not consumed exactly once", processed)
	}
	if actual := memoryReadWakeReadPG(test, storage, ctx, repo); actual.objectRevision <= before.objectRevision {
		test.Fatal("parallel dispatch lost notice", actual)
	}
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}

func TestPGMemoryReadObjectWakeConflictAllocatesFenceAfterRowLock(test *testing.T) {
	storage, ctx, _, hash := memoryReadObjectWakePG(test)
	function := pgx.Identifier{domain.NewID("object_wake_barrier_")}.Sanitize()
	barrier := string(hash)
	catalogExecPG(test, ctx, storage.pool, `CREATE FUNCTION `+function+`() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN
 IF current_setting('cxt.object_wake_barrier',true)='on' THEN
 PERFORM pg_advisory_xact_lock(hashtextextended(NEW.hash,115));
 END IF;
 RETURN NEW;
 END $$`)
	catalogExecPG(test, ctx, storage.pool, `CREATE TRIGGER `+function+` BEFORE INSERT ON memory_read_object_wakes
 FOR EACH ROW EXECUTE FUNCTION `+function+`() `)
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, `DROP TRIGGER `+function+` ON memory_read_object_wakes; DROP FUNCTION `+function+`()`); err != nil {
			test.Error(err)
		}
	})
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT pg_advisory_xact_lock(hashtextextended($1,115))`, barrier)
	older := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, older, `SET LOCAL cxt.object_wake_barrier='on'`)
	done := make(chan error, 1)
	go func() {
		_, err := older.Exec(ctx, `DELETE FROM blob_source_incarnations WHERE hash=$1`, string(hash))
		done <- err
	}()
	memoryQualificationBlockedPG(test, ctx, storage, older.Conn().PgConn().PID(), done)
	var allocated int64
	if err := storage.pool.QueryRow(ctx, `SELECT last_value FROM memory_read_object_wake_fence_seq`).Scan(&allocated); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET hash=hash WHERE hash=$1`, string(hash))
	newer := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if newer.version <= allocated {
		test.Fatal("barrier did not order sequence allocations", allocated, newer)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if err := <-done; err != nil {
		test.Fatal(err)
	}
	if err := older.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	actual := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if actual.version <= newer.version || actual.afterRepo != "" {
		test.Fatal("late older allocation moved fence backwards", allocated, newer, actual)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blob_source_incarnations(hash) VALUES($1)`, string(hash))
	memoryReadObjectWakeDrainPG(test, storage, ctx)
}
