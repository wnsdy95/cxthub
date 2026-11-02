//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryReadObjectWakeLegacyPagePG(test *testing.T, storage *PostgresStore, parent context.Context) bool {
	test.Helper()
	return memoryReadObjectWakePreSweepPagePG(test, storage, parent, false)
}

func memoryReadObjectWakePreSweepPagePG(test *testing.T, storage *PostgresStore, parent context.Context, durable bool) bool {
	test.Helper()
	ctx, cancel := context.WithTimeout(parent, docRangeEnqueueTimeout)
	defer cancel()
	transaction := catalogTxPG(test, ctx, storage)
	defer rollbackPG(transaction)
	catalogExecPG(test, ctx, transaction, `SET LOCAL lock_timeout='50ms'; SET LOCAL statement_timeout='100ms'`)
	var hash, afterRepo string
	var version int64
	err := transaction.QueryRow(ctx, `SELECT hash,version,after_repo FROM memory_read_object_wakes
 WHERE next_attempt<=clock_timestamp() ORDER BY next_attempt,hash FOR UPDATE SKIP LOCKED LIMIT 1`).Scan(&hash, &version, &afterRepo)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		test.Fatal(err)
	}
	rows, err := transaction.Query(ctx, `SELECT DISTINCT owned.repo_id COLLATE "C" AS repo_id
 FROM repo_blobs owned JOIN memory_read_work_scopes scope ON scope.repo_id=owned.repo_id AND scope.enabled
 WHERE owned.hash=$1 AND owned.kind IN ('memory','memory_chunk') AND owned.repo_id COLLATE "C">$2
 ORDER BY repo_id LIMIT 33`, hash, afterRepo)
	if err != nil {
		test.Fatal(err)
	}
	recipients, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		test.Fatal(err)
	}
	more := len(recipients) > 32
	if more {
		recipients = recipients[:32]
	}
	if durable {
		catalogExecPG(test, ctx, transaction, memoryReadObjectWakeDeliveryUpsertSQL, hash, version, recipients)
	} else {
		for _, recipient := range recipients {
			catalogExecPG(test, ctx, transaction, `SELECT cxt_notify_memory_read_wake($1,true)`, recipient)
		}
	}
	if more {
		catalogExecPG(test, ctx, transaction, `UPDATE memory_read_object_wakes SET after_repo=$3,
 next_attempt=clock_timestamp(),attempts=0,reason='' WHERE hash=$1 AND version=$2`, hash, version, recipients[len(recipients)-1])
	} else {
		catalogExecPG(test, ctx, transaction, `DELETE FROM memory_read_object_wakes WHERE hash=$1 AND version=$2`, hash, version)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	return true
}

func memoryReadObjectWakeLegacyTriggerSQLPG(test *testing.T) string {
	test.Helper()
	raw, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", "0115_memory_read_object_wake.sql"))
	if err != nil {
		test.Fatal(err)
	}
	const declaration = "CREATE OR REPLACE FUNCTION cxt_memory_read_object_wake()"
	_, body, found := strings.Cut(string(raw), declaration)
	if !found {
		test.Fatal("legacy trigger definition missing")
	}
	body, _, found = strings.Cut(body, "$$;")
	if !found {
		test.Fatal("legacy trigger body incomplete")
	}
	return declaration + body + "$$;"
}

func memoryReadObjectWakeRestoreLegacyTriggerPG(test *testing.T, storage *PostgresStore, ctx context.Context) {
	test.Helper()
	var current string
	if err := storage.pool.QueryRow(ctx, `SELECT pg_get_functiondef('cxt_memory_read_object_wake()'::regprocedure)`).Scan(&current); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, current); err != nil {
			test.Error(err)
		}
	})
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `SET LOCAL lock_timeout='250ms'; SET LOCAL statement_timeout='5s'`)
	catalogExecPG(test, ctx, transaction, memoryReadObjectWakeLegacyTriggerSQLPG(test))
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
}

func TestPGMemoryReadObjectWakeDeliveryRollbackRequiresCompatibleDrain(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
	owners := []domain.ContentHash{
		memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 10),
		memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 20),
	}
	compatible, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	defer compatible.Close()
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	before := memoryReadWakeReadPG(test, storage, ctx, owners[1])
	memoryReadObjectWakeDispatchPG(test, compatible, ctx, 1, 2)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	job := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owners[1])
	for range 3 {
		if memoryReadObjectWakeLegacyPagePG(test, storage, ctx) {
			test.Fatal("parent-only legacy dispatcher unexpectedly found a parent")
		}
	}
	if actual := memoryReadWakeReadPG(test, storage, ctx, owners[1]); !reflect.DeepEqual(actual, before) {
		test.Fatal("legacy parent-only dispatch unexpectedly delivered an orphaned recipient obligation", before, actual)
	}
	if actual := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owners[1]); actual != job {
		test.Fatal("legacy dispatcher changed a durable delivery", job, actual)
	}
	test.Log("unsafe rollback reproduced: parent queue empty, one durable delivery stranded after three parent-only legacy dispatches")
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	if !memoryReadObjectWakeLegacyPagePG(test, storage, ctx) {
		test.Fatal("legacy dispatcher failed to process a new parent during mixed-version operation")
	}
	if actual := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owners[1]); actual != job {
		test.Fatal("legacy parent acknowledgement silently dropped a compatible worker's delivery", job, actual)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp()+interval '1 hour' WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, compatible, ctx, 1, 0)
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 1 {
		test.Fatal("a zero due-work count must not be treated as a completed drain", count)
	}
	test.Log("rollback preconditions: quiesce producers and other new expanders; retain a sweep-and-delivery-capable worker until both queues are empty, including deferred deliveries")
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp() WHERE hash=$1`, string(hash))
	before = memoryReadWakeReadPG(test, storage, ctx, owners[1])
	memoryReadObjectWakeDispatchPG(test, compatible, ctx, 1, 1)
	if actual := memoryReadWakeReadPG(test, storage, ctx, owners[1]); actual.objectRevision <= before.objectRevision {
		test.Fatal("compatible drain failed to complete the durable recipient obligation", before, actual)
	}
	var parents, deliveries int
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM memory_read_object_wakes),
 (SELECT count(*) FROM memory_read_object_wake_deliveries)`).Scan(&parents, &deliveries); err != nil || parents != 0 || deliveries != 0 {
		test.Fatal("quiesced rollback gate requires both complete queues empty, not merely no due work", parents, deliveries, err)
	}
	compatible.Close()
	memoryReadObjectWakeRestoreLegacyTriggerPG(test, storage, ctx)
	baseline := make(map[domain.ContentHash]int64)
	for _, owner := range owners {
		baseline[owner] = memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	if !memoryReadObjectWakeLegacyPagePG(test, storage, ctx) {
		test.Fatal("legacy dispatcher did not resume after the quiesced drain")
	}
	for _, owner := range owners {
		if actual := memoryReadWakeReadPG(test, storage, ctx, owner); actual.objectRevision <= baseline[owner] {
			test.Fatal("legacy operation lost a recipient after rollback", owner, actual)
		}
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
		test.Fatal("legacy dispatcher created delivery-table work after rollback", count)
	}
	test.Log("safe tested handoff: both queues drained while quiesced, last compatible worker stopped, legacy reset-cursor trigger restored before legacy writes and dispatch resumed; additive tables/column retained and no delivery rows discarded")
}

func TestPGMemoryReadObjectWakeSweepMixedOldWorkerLosesNewerPrefix(test *testing.T) {
	for _, durable := range []bool{false, true} {
		name := "parent_only"
		if durable {
			name = "delivery_0122"
		}
		test.Run(name, func(test *testing.T) {
			storage, ctx, original, hash := memoryReadObjectWakePG(test)
			owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, original, hash)
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
			prefix := memoryReadWakeReadPG(test, storage, ctx, owners[0])
			tail := memoryReadWakeReadPG(test, storage, ctx, owners[34])
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			pending := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
			if pending.version <= pending.scanVersion || pending.afterRepo == "" {
				test.Fatal("fixture lacks newer work behind an in-flight cursor", pending)
			}
			if !memoryReadObjectWakePreSweepPagePG(test, storage, ctx, durable) {
				test.Fatal("old worker did not claim the sweep parent")
			}
			if durable {
				memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 3)
			}
			memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
			memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 0)
			if actual := memoryReadWakeReadPG(test, storage, ctx, owners[0]); !reflect.DeepEqual(actual, prefix) {
				test.Fatal("unsafe mixed-worker fixture unexpectedly delivered the newer prefix", prefix, actual)
			}
			if actual := memoryReadWakeReadPG(test, storage, ctx, owners[34]); actual.objectRevision <= tail.objectRevision {
				test.Fatal("legacy tail-page control did not deliver", tail, actual)
			}
			test.Log("0123 is not rolling-compatible: this old worker deletes the parent after its tail page and loses the newer prefix obligation; stop all pre-sweep dispatchers before migration/activation")
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			memoryReadObjectWakeDrainPG(test, storage, ctx)
			if actual := memoryReadWakeReadPG(test, storage, ctx, owners[0]); actual.objectRevision <= prefix.objectRevision {
				test.Fatal("compatible recovery control failed after an explicit additional physical write", actual)
			}
		})
	}
}
