//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryReadObjectWakeDeliveryStatePG struct {
	version     int64
	nextAttempt time.Time
	attempts    int
	reason      string
}

func memoryReadObjectWakeDeliveryReadPG(test *testing.T, storage *PostgresStore, ctx context.Context, hash, repo domain.ContentHash) memoryReadObjectWakeDeliveryStatePG {
	test.Helper()
	var state memoryReadObjectWakeDeliveryStatePG
	if err := storage.pool.QueryRow(ctx, `SELECT version,next_attempt,attempts,reason FROM memory_read_object_wake_deliveries WHERE hash=$1 AND repo_id=$2`, string(hash), string(repo)).
		Scan(&state.version, &state.nextAttempt, &state.attempts, &state.reason); err != nil {
		test.Fatal(err)
	}
	return state
}

func memoryReadObjectWakeDeliveryCountPG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) int {
	test.Helper()
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_object_wake_deliveries WHERE hash=$1`, string(hash)).Scan(&count); err != nil {
		test.Fatal(err)
	}
	return count
}

func memoryReadObjectWakeExpandPG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) {
	test.Helper()
	if expanded, err := storage.dispatchMemoryReadObjectWake(ctx); err != nil || expanded != string(hash) {
		test.Fatal("expand one durable recipient page", expanded, err)
	}
}

func TestPGMemoryReadObjectWakeProcessedCountContract(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, repo, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
	if event := memoryReadObjectWakeReadPG(test, storage, ctx, hash); event.afterRepo != string(owners[31]) {
		test.Fatal("a call must expand exactly one page of at most 32 recipients", event)
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 31 {
		test.Fatal("limit=1 must deliver at most one recipient after expansion", count)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 2 {
		test.Fatal("recipient attempts exceeded the per-call limit", count)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 2)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 0)
	test.Log("processed counts committed expansion/defer transactions plus committed recipient delivery/retirement/backoff transactions; maximum 1+limit, limit in [1,32]; expansion and each delivery have independent 500ms deadlines")
}

func TestPGMemoryReadObjectWakeManyBlockedRecipientsDoNotStarveLaterPage(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, repo, hash)
	before := make(map[domain.ContentHash]memoryReadWakeStatePG)
	for _, owner := range owners {
		before[owner] = memoryReadWakeReadPG(test, storage, ctx, owner)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_wakes WHERE repo_id=ANY($1::text[]) ORDER BY repo_id FOR UPDATE`, strs(owners[:32]))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	for _, owner := range owners[:32] {
		job := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owner)
		if job.attempts != 1 || job.reason != "temporary_failure" || !job.nextAttempt.After(time.Now()) {
			test.Fatal("blocked recipient lost its independent retry obligation", owner, job)
		}
		if actual := memoryReadWakeReadPG(test, storage, ctx, owner); !reflect.DeepEqual(actual, before[owner]) {
			test.Fatal("locked recipient was acknowledged", owner, before[owner], actual)
		}
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 4)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	for _, owner := range owners[32:] {
		if actual := memoryReadWakeReadPG(test, storage, ctx, owner); actual.objectRevision <= before[owner].objectRevision {
			test.Fatal("32 blocked recipients starved a healthy later-page recipient", owner, actual)
		}
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 32 {
		test.Fatal("parent acknowledgement discarded blocked recipient obligations", count)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp() WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 32)
	for _, owner := range owners {
		if actual := memoryReadWakeReadPG(test, storage, ctx, owner); actual.objectRevision <= before[owner].objectRevision {
			test.Fatal("retained recipient did not recover after unlock", owner, actual)
		}
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
		test.Fatal("recovered deliveries remained pending", count)
	}
}

func TestPGMemoryReadObjectWakeDeliveryClaimSkipsLockedJob(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
	locked := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 10)
	healthy := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 20)
	before := memoryReadWakeReadPG(test, storage, ctx, healthy)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_object_wake_deliveries WHERE hash=$1 AND repo_id=$2 FOR UPDATE`, string(hash), string(locked))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	if actual := memoryReadWakeReadPG(test, storage, ctx, healthy); actual.objectRevision <= before.objectRevision {
		test.Fatal("locked delivery claim blocked another recipient", actual)
	}
	if job := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, locked); job.attempts != 0 {
		test.Fatal("SKIP LOCKED altered another dispatcher's claim", job)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
}

func TestPGMemoryReadObjectWakeDeliveryCancellationIsAtomic(test *testing.T) {
	for _, prefix := range []string{"SELECT cxt_notify_memory_read_wake", "DELETE FROM memory_read_object_wake_deliveries"} {
		test.Run(prefix, func(test *testing.T) {
			storage, parent, repo, hash := memoryReadObjectWakePG(test)
			catalogExecPG(test, parent, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			memoryReadObjectWakeExpandPG(test, storage, parent, hash)
			before := memoryReadWakeReadPG(test, storage, parent, repo)
			job := memoryReadObjectWakeDeliveryReadPG(test, storage, parent, hash, repo)
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
				test.Fatal("canceled delivery committed", err)
			}
			if actual := memoryReadWakeReadPG(test, storage, parent, repo); !reflect.DeepEqual(actual, before) {
				test.Fatal("canceled delivery committed its notification", before, actual)
			}
			if actual := memoryReadObjectWakeDeliveryReadPG(test, storage, parent, hash, repo); actual != job {
				test.Fatal("canceled delivery changed its durable obligation", job, actual)
			}
			memoryReadObjectWakeDispatchPG(test, storage, parent, 1, 1)
			after := memoryReadWakeReadPG(test, storage, parent, repo)
			if after.objectRevision <= before.objectRevision {
				test.Fatal("retry lost canceled notification", after)
			}
			memoryReadObjectWakeDispatchPG(test, storage, parent, 1, 0)
			if duplicate := memoryReadWakeReadPG(test, storage, parent, repo); !reflect.DeepEqual(duplicate, after) {
				test.Fatal("acknowledged delivery notified twice", after, duplicate)
			}
		})
	}
}

func TestPGMemoryReadObjectWakeDeliveryRestartRetainsCursorAndJobs(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, repo, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeExpandPG(test, peer, ctx, hash)
	peer.Close()
	if event := memoryReadObjectWakeReadPG(test, storage, ctx, hash); event.afterRepo != string(owners[31]) {
		test.Fatal("restart lost expansion cursor", event)
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 32 {
		test.Fatal("restart lost durable recipient obligations", count)
	}
	restarted, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	defer restarted.Close()
	memoryReadObjectWakeDispatchPG(test, restarted, ctx, 32, 33)
	memoryReadObjectWakeDispatchPG(test, restarted, ctx, 32, 3)
	memoryReadObjectWakeDispatchPG(test, restarted, ctx, 32, 0)
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
		test.Fatal("restarted dispatcher left obligations", count)
	}
}

func TestPGMemoryReadObjectWakeDeliveryRetiresObsoleteRecipient(test *testing.T) {
	for _, change := range []string{"revoked", "document_only", "disabled", "scope_removed"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx, repo, hash := memoryReadObjectWakePG(test)
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
			switch change {
			case "revoked":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash))
			case "document_only":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET kind='doc' WHERE repo_id=$1 AND hash=$2`, string(repo), string(hash))
			case "disabled":
				if err := storage.ConfigureMemoryReadWork(ctx, repo, false); err != nil {
					test.Fatal(err)
				}
			case "scope_removed":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_work_scopes WHERE repo_id=$1`, string(repo))
			}
			before := memoryReadWakeReadPG(test, storage, ctx, repo)
			memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
			if actual := memoryReadWakeReadPG(test, storage, ctx, repo); !reflect.DeepEqual(actual, before) {
				test.Fatal("obsolete delivery notified a revoked or disabled recipient", before, actual)
			}
			if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
				test.Fatal("obsolete obligation was not retired", count)
			}
		})
	}
}

func TestPGMemoryReadObjectWakeDeliveryDoesNotValidateStaleWitness(test *testing.T) {
	storage, ctx, repo, _ := memoryReadObjectWakePG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = repo
	hash, err := storage.PutMemory(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	object := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDrainPG(test, storage, ctx)
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{object.Witness}, domain.ErrConflict)
	current := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	memoryWitnessPGCheck(test, storage, ctx, repo, []domain.MemoryObjectWitness{current.Witness}, nil)
}

func TestPGMemoryReadObjectWakeDeliveryVersionDedupAndStaleAcknowledgement(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(repo), string(hash))
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 1 {
		test.Fatal("multiple ownership kinds duplicated a recipient obligation", count)
	}
	older := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET attempts=3,reason='temporary_failure',next_attempt=clock_timestamp()+interval '10 seconds' WHERE hash=$1`, string(hash))
	deferred := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	catalogExecPG(test, ctx, storage.pool, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), older.version, []string{string(repo)})
	if actual := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo); actual != deferred {
		test.Fatal("same version reset recipient backoff", deferred, actual)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	newer := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	if newer.version <= older.version || newer.attempts != 0 || newer.reason != "" {
		test.Fatal("new global object fence did not renew the recipient obligation", older, newer)
	}
	catalogExecPG(test, ctx, storage.pool, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), older.version, []string{string(repo)})
	if actual := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo); actual != newer {
		test.Fatal("older expansion downgraded a newer recipient fence", newer, actual)
	}
	before := memoryReadWakeReadPG(test, storage, ctx, repo)
	stale := catalogTxPG(test, ctx, storage)
	if err := deliverMemoryReadObjectWakeRecipient(ctx, stale, string(hash), string(repo), older.version); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale recipient version acknowledged a newer obligation", err)
	}
	if err := stale.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadWakeReadPG(test, storage, ctx, repo); !reflect.DeepEqual(actual, before) {
		test.Fatal("stale delivery notified a recipient", actual)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 0)
}

func TestPGMemoryReadObjectWakeDeliveryWaitingExpansionSurvivesAcknowledgement(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	older := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "DELETE FROM memory_read_object_wake_deliveries")
	defer release()
	done := make(chan error, 1)
	go func() { done <- peer.deliverMemoryReadObjectWake(ctx) }()
	memoryQualificationWaitPG(test, ctx, reached, done)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	newer := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	writer := catalogTxPG(test, ctx, storage)
	written := make(chan error, 1)
	go func() {
		written <- dispatchMemoryReadObjectWakePage(ctx, writer, string(hash), newer.version, newer.scanVersion, newer.afterRepo)
	}()
	memoryQualificationBlockedPG(test, ctx, storage, writer.Conn().PgConn().PID(), written)
	release()
	if err := <-done; err != nil {
		test.Fatal(err)
	}
	if err := <-written; err != nil {
		test.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	job := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	if job.version != newer.version || job.version <= older.version {
		test.Fatal("older acknowledgement consumed the waiting newer obligation", older, newer, job)
	}
	before := memoryReadWakeReadPG(test, storage, ctx, repo)
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	if after := memoryReadWakeReadPG(test, storage, ctx, repo); after.objectRevision <= before.objectRevision {
		test.Fatal("waiting newer version was not notified", before, after)
	}
}

func TestPGMemoryReadObjectWakeDeliveryRecreationCannotReuseFence(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	older := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	var body []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&body); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
		test.Fatal("deleted object retained old delivery obligations", count)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), body)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(repo), string(hash))
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	current := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	if current.version <= older.version {
		test.Fatal("recreated object reused a recipient version", older, current)
	}
	stale := catalogTxPG(test, ctx, storage)
	if err := deliverMemoryReadObjectWakeRecipient(ctx, stale, string(hash), string(repo), older.version); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("pre-recreation delivery acknowledged the current object", err)
	}
	if err := stale.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 1)
	deleted := catalogTxPG(test, ctx, storage)
	if err := deliverMemoryReadObjectWakeRecipient(ctx, deleted, string(hash), string(repo), current.version); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("acknowledged recipient version could be delivered again", err)
	}
	if err := deleted.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		test.Fatal(err)
	}
}

type memoryReadObjectWakeSecondDeliveryPause struct {
	graphServingSizePause
	deletions atomic.Int64
}

func (pause *memoryReadObjectWakeSecondDeliveryPause) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, pause.prefix) && pause.deletions.Add(1) == 2 {
		return pause.graphServingSizePause.TraceQueryStart(ctx, connection, data)
	}
	return ctx
}

func TestPGMemoryReadObjectWakeDeliveryCancellationPreservesPriorCommits(test *testing.T) {
	storage, parent, original, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, parent, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
	first := memoryReadObjectWakeOwnerPG(test, storage, parent, hash, 10)
	second := memoryReadObjectWakeOwnerPG(test, storage, parent, hash, 20)
	firstBefore := memoryReadWakeReadPG(test, storage, parent, first)
	secondBefore := memoryReadWakeReadPG(test, storage, parent, second)
	catalogExecPG(test, parent, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	pause := &memoryReadObjectWakeSecondDeliveryPause{graphServingSizePause: graphServingSizePause{
		prefix: "DELETE FROM memory_read_object_wake_deliveries", reached: make(chan struct{}), release: make(chan struct{}),
	}}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = pause
	pool, err := pgxpool.NewWithConfig(parent, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer close(pause.release)
	processed := 0
	done := make(chan error, 1)
	go func() {
		count, err := peer.DispatchMemoryReadObjectWakes(ctx, 2)
		processed = count
		done <- err
	}()
	memoryQualificationWaitPG(test, parent, pause.reached, done)
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) || processed != 2 {
		test.Fatal("cancellation lost the committed expansion/delivery count", processed, err)
	}
	firstAfter := memoryReadWakeReadPG(test, storage, parent, first)
	if firstAfter.objectRevision <= firstBefore.objectRevision {
		test.Fatal("later cancellation rolled back an earlier recipient", firstBefore, firstAfter)
	}
	if secondAfter := memoryReadWakeReadPG(test, storage, parent, second); !reflect.DeepEqual(secondAfter, secondBefore) {
		test.Fatal("canceled recipient notification survived rollback", secondBefore, secondAfter)
	}
	if job := memoryReadObjectWakeDeliveryReadPG(test, storage, parent, hash, second); job.attempts != 0 || job.reason != "" {
		test.Fatal("canceled recipient was acknowledged or deferred", job)
	}
	memoryReadObjectWakeAbsentPG(test, storage, parent, hash)
	memoryReadObjectWakeDispatchPG(test, storage, parent, 2, 1)
	if actual := memoryReadWakeReadPG(test, storage, parent, first); !reflect.DeepEqual(actual, firstAfter) {
		test.Fatal("restart redelivered an already committed recipient", firstAfter, actual)
	}
	if actual := memoryReadWakeReadPG(test, storage, parent, second); actual.objectRevision <= secondBefore.objectRevision {
		test.Fatal("restart lost the unacknowledged recipient", secondBefore, actual)
	}
}

func TestPGMemoryReadObjectWakeDeliveryOlderUpsertWaitsWithoutDowngrade(test *testing.T) {
	storage, ctx, repo, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	older := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	newer := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	newerWriter := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, newerWriter, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), newer.version, []string{string(repo)})
	olderWriter := catalogTxPG(test, ctx, storage)
	done := make(chan error, 1)
	go func() {
		_, err := olderWriter.Exec(ctx, memoryReadObjectWakeDeliveryUpsertSQL, string(hash), older.version, []string{string(repo)})
		done <- err
	}()
	memoryQualificationBlockedPG(test, ctx, storage, olderWriter.Conn().PgConn().PID(), done)
	if err := newerWriter.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := <-done; err != nil {
		test.Fatal(err)
	}
	if err := olderWriter.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, repo); actual.version != newer.version {
		test.Fatal("waiting older upsert downgraded the committed recipient fence", older, newer, actual)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
}

func TestPGMemoryReadObjectWakeDeliveryContinuousWritesPreserveRecipientAge(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
	owners := []domain.ContentHash{
		memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 10),
		memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 20),
	}
	previous := make(map[domain.ContentHash]int64)
	progress := make(map[domain.ContentHash]int)
	for _, owner := range owners {
		previous[owner] = memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
	}
	for step := 1; step <= 8; step++ {
		catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
		memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
		for _, owner := range owners {
			current := memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
			if current > previous[owner] {
				progress[owner]++
			}
			previous[owner] = current
		}
		test.Logf("continuous_write=%d earlier_A_progress=%d later_B_progress=%d pending_deliveries=%d", step, progress[owners[0]], progress[owners[1]], memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash))
	}
	for _, owner := range owners {
		if progress[owner] < 2 {
			test.Errorf("recipient %s advanced only %d times during eight writes with no quiet interval; newer fences must retain already-due delivery age", owner, progress[owner])
		}
	}
	memoryReadObjectWakeDrainPG(test, storage, ctx)
}
