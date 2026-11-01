//go:build postgres

package store

import (
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryReadObjectWakeSharedHashRecipientProgress(test *testing.T) {
	for _, scenario := range []struct {
		name          string
		lockedOrdinal int
	}{
		{name: "healthy_before_locked", lockedOrdinal: 20},
		{name: "healthy_after_locked", lockedOrdinal: 10},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			storage, ctx, original, hash := memoryReadObjectWakePG(test)
			catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
			locked := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, scenario.lockedOrdinal)
			healthy := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 30-scenario.lockedOrdinal)
			memoryReadObjectWakeDrainPG(test, storage, ctx)
			var ownerCount int
			if err := storage.pool.QueryRow(ctx, `SELECT count(DISTINCT repo_id) FROM repo_blobs WHERE hash=$1 AND kind='memory'`, string(hash)).Scan(&ownerCount); err != nil || ownerCount != 2 {
				test.Fatal("fixture must have exactly two owners of the same memory hash", ownerCount, err)
			}
			lockedBefore := memoryReadWakeReadPG(test, storage, ctx, locked)
			healthyBefore := memoryReadWakeReadPG(test, storage, ctx, healthy)
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			queued := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
			blocker := catalogTxPG(test, ctx, storage)
			catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_wakes WHERE repo_id=$1 FOR UPDATE`, string(locked))
			probe := catalogTxPG(test, ctx, storage)
			catalogExecPG(test, ctx, probe, `SELECT 1 FROM memory_read_wakes WHERE repo_id=$1 FOR UPDATE NOWAIT`, string(healthy))
			if err := probe.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			test.Logf("shared_hash=%s locked_A=%s healthy_B=%s queued_version=%d healthy_revision=%d healthy_object_revision=%d", hash, locked, healthy, queued.version, healthyBefore.revision, healthyBefore.objectRevision)
			healthyProgress := false
			for attempt := 1; attempt <= 3; attempt++ {
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wakes SET next_attempt=clock_timestamp()-interval '1 second' WHERE hash=$1`, string(hash))
				started := time.Now()
				count, err := storage.DispatchMemoryReadObjectWakes(ctx, 1)
				elapsed := time.Since(started)
				if err != nil || count < 0 || count > 2 {
					test.Fatal("bounded object wake dispatch failed", count, err)
				}
				healthyAfter := memoryReadWakeReadPG(test, storage, ctx, healthy)
				test.Logf("locked_attempt=%d dispatched=%d elapsed=%s healthy_revision=%d->%d healthy_object_revision=%d->%d", attempt, count, elapsed, healthyBefore.revision, healthyAfter.revision, healthyBefore.objectRevision, healthyAfter.objectRevision)
				if healthyAfter.objectRevision > healthyBefore.objectRevision {
					healthyProgress = true
					break
				}
				pending := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, locked)
				test.Logf("pending_delivery_version=%d attempts=%d reason=%q next_attempt=%s", pending.version, pending.attempts, pending.reason, pending.nextAttempt.Format(time.RFC3339Nano))
			}
			if current := memoryReadWakeReadPG(test, storage, ctx, locked); current.objectRevision != lockedBefore.objectRevision {
				test.Fatal("locked recipient unexpectedly advanced before releasing its row lock", lockedBefore, current)
			}
			if err := blocker.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wakes SET next_attempt=clock_timestamp()-interval '1 second' WHERE hash=$1`, string(hash))
			catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp()-interval '1 second' WHERE hash=$1`, string(hash))
			memoryReadObjectWakeDrainPG(test, storage, ctx)
			memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
			for _, recipient := range []struct {
				repo   domain.ContentHash
				before memoryReadWakeStatePG
			}{{locked, lockedBefore}, {healthy, healthyBefore}} {
				after := memoryReadWakeReadPG(test, storage, ctx, recipient.repo)
				test.Logf("after_unlock repo=%s revision=%d->%d object_revision=%d->%d", recipient.repo, recipient.before.revision, after.revision, recipient.before.objectRevision, after.objectRevision)
				if after.objectRevision <= recipient.before.objectRevision {
					test.Fatal("recipient did not recover after releasing the row lock", recipient.repo, recipient.before, after)
				}
			}
			if !healthyProgress {
				test.Error("healthy shared-hash recipient B made no object-wake progress during three dispatcher retries while only recipient A was locked")
			}
		})
	}
}
