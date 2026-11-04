//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryReadObjectWakeContinuousWritesCompleteFiniteSweeps(test *testing.T) {
	for _, scenario := range []struct {
		owners int
		limit  int
		rounds int
	}{{35, 32, 8}, {65, 32, 12}, {35, 1, 80}} {
		test.Run(fmt.Sprintf("owners%d_limit%d", scenario.owners, scenario.limit), func(test *testing.T) {
			storage, ctx, original, hash := memoryReadObjectWakePG(test)
			catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(original), string(hash))
			var owners []domain.ContentHash
			previous := make(map[domain.ContentHash]int64)
			progress := make(map[domain.ContentHash]int)
			for ordinal := 10; ordinal < 10+scenario.owners; ordinal++ {
				owner := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, ordinal)
				owners = append(owners, owner)
				previous[owner] = memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
			}
			pages := (scenario.owners + 31) / 32
			sweeps := 0
			var prior memoryReadObjectWakeStatePG
			for step := 0; step < scenario.rounds; step++ {
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
				written := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
				if step > 0 && (written.version <= prior.version || written.scanVersion != prior.scanVersion || written.throughRepo != prior.throughRepo || written.afterRepo != prior.afterRepo || written.nextAttempt != prior.nextAttempt) {
					test.Fatal("continuous writer reset sweep progress or pending age", prior, written)
				}
				count, err := storage.DispatchMemoryReadObjectWakes(ctx, scenario.limit)
				if err != nil || count < 1 || count > scenario.limit+1 {
					test.Fatal("bounded continuous dispatch", count, err)
				}
				after := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
				if (step+1)%pages == 0 {
					sweeps++
					if after.afterRepo != "" || after.scanVersion != written.version || after.throughRepo.Valid {
						test.Fatal("finite sweep failed to restart at the newest pending fence", written, after)
					}
				} else {
					expectedCursor := owners[((step%pages)+1)*32-1]
					expectedScan := written.scanVersion
					if expectedScan == 0 {
						expectedScan = written.version
					}
					if after.afterRepo != string(expectedCursor) || after.scanVersion != expectedScan || !after.throughRepo.Valid || after.throughRepo.String != string(owners[len(owners)-1]) {
						test.Fatal("continuous writes interrupted the fixed-version page sequence", written, after)
					}
				}
				for _, owner := range owners {
					current := memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
					if current > previous[owner] {
						progress[owner]++
					}
					previous[owner] = current
				}
				prior = after
			}
			minimum := scenario.rounds
			for _, owner := range owners {
				minimum = min(minimum, progress[owner])
				if progress[owner] < 2 {
					test.Errorf("owner %s advanced %d times during uninterrupted writes", owner, progress[owner])
				}
			}
			test.Logf("owners=%d limit=%d uninterrupted_writes=%d completed_sweeps=%d minimum_recipient_progress=%d", scenario.owners, scenario.limit, scenario.rounds, sweeps, minimum)
			memoryReadObjectWakeDrainPG(test, storage, ctx)
			memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
			if remaining := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); remaining != 0 {
				test.Fatal("quiet-interval final drain lost or retained obligations", remaining)
			}
		})
	}
}

func TestPGMemoryReadObjectWakeSweepBoundaryCancellationRestartAndFence(test *testing.T) {
	storage, parent, original, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, parent, original, hash)
	catalogExecPG(test, parent, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, parent, hash)
	first := memoryReadObjectWakeReadPG(test, storage, parent, hash)
	catalogExecPG(test, parent, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	latest := memoryReadObjectWakeReadPG(test, storage, parent, hash)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "UPDATE memory_read_object_wakes SET after_repo=''")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := peer.dispatchMemoryReadObjectWake(ctx)
		done <- err
	}()
	memoryQualificationWaitPG(test, parent, reached, done)
	cancel()
	release()
	if err := <-done; !errors.Is(err, context.Canceled) {
		test.Fatal("canceled sweep boundary committed", err)
	}
	if actual := memoryReadObjectWakeReadPG(test, storage, parent, hash); actual != latest {
		test.Fatal("canceled sweep restart discarded the old cursor or latest obligation", latest, actual)
	}
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, parent, hash); count != 32 {
		test.Fatal("canceled final page committed partial deliveries", count)
	}
	restarted, err := NewPostgresStore(parent, storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	defer restarted.Close()
	memoryReadObjectWakeExpandPG(test, restarted, parent, hash)
	next := memoryReadObjectWakeReadPG(test, storage, parent, hash)
	if next.afterRepo != "" || next.scanVersion != latest.version || next.version != latest.version || next.throughRepo.Valid {
		test.Fatal("restart failed to retain the newer full-sweep obligation", latest, next)
	}
	for _, owner := range owners {
		if job := memoryReadObjectWakeDeliveryReadPG(test, storage, parent, hash, owner); job.version != first.scanVersion {
			test.Fatal("one sweep mixed recipient fences", first, job)
		}
	}
	stale := catalogTxPG(test, parent, storage)
	if err := dispatchMemoryReadObjectWakePage(parent, stale, string(hash), next.version, first.scanVersion, next.afterRepo); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale scan fence acknowledged a new sweep at the same latest version", err)
	}
	if err := stale.Rollback(parent); err != nil {
		test.Fatal(err)
	}
	memoryReadObjectWakeExpandPG(test, restarted, parent, hash)
	for _, owner := range owners[:32] {
		if job := memoryReadObjectWakeDeliveryReadPG(test, storage, parent, hash, owner); job.version != next.scanVersion {
			test.Fatal("next sweep did not monotonically renew earlier recipients", next, job)
		}
	}
	memoryReadObjectWakeExpandPG(test, restarted, parent, hash)
	memoryReadObjectWakeAbsentPG(test, storage, parent, hash)
	for _, owner := range owners {
		if job := memoryReadObjectWakeDeliveryReadPG(test, storage, parent, hash, owner); job.version != next.scanVersion {
			test.Fatal("final parent acknowledgement lost a newer recipient obligation", next, job)
		}
	}
	memoryReadObjectWakeDispatchPG(test, restarted, parent, 32, 32)
	memoryReadObjectWakeDispatchPG(test, restarted, parent, 32, 3)
}

func TestPGMemoryReadObjectWakeSweepUpperFenceBoundsAppendingOwners(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	test.Cleanup(func() { memoryReadObjectWakeDrainPG(test, storage, ctx) })
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, original, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	first := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if !first.throughRepo.Valid || first.throughRepo.String != string(owners[34]) {
		test.Fatal("sweep did not freeze its initial upper owner fence", first)
	}
	prefix := memoryReadWakeReadPG(test, storage, ctx, owners[0])
	var added []domain.ContentHash
	for page := 0; page < 5; page++ {
		for offset := 0; offset < 32; offset++ {
			owner := memoryReadObjectWakeOwnerPG(test, storage, ctx, hash, 100+page*32+offset)
			added = append(added, owner)
			if wake := memoryReadWakeReadPG(test, storage, ctx, owner); wake.nextAttempt == nil {
				test.Fatal("appended owner lacks an independent ownership/scope wake", owner)
			}
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
		before := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
		count, err := storage.DispatchMemoryReadObjectWakes(ctx, 32)
		if err != nil || count < 1 || count > 33 {
			test.Fatal("bounded append-page dispatch", count, err)
		}
		after := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
		if page == 0 || page == 4 {
			if after.afterRepo != "" || after.scanVersion != before.version || after.throughRepo.Valid {
				test.Fatal("appending 32 owners before each page prevented a finite sweep boundary", page, before, after)
			}
		} else {
			expectedUpper := added[63]
			if !after.throughRepo.Valid || after.throughRepo.String != string(expectedUpper) {
				test.Fatal("later additions extended an already started sweep", page, before, after)
			}
		}
		if page == 1 {
			if actual := memoryReadWakeReadPG(test, storage, ctx, owners[0]); actual.objectRevision <= prefix.objectRevision {
				test.Fatal("uninterrupted owner growth starved the older prefix's newer notification", prefix, actual)
			}
		}
		test.Logf("append_page=%d owners=%d processed=%d sweep_restarted=%t", page+1, len(owners)+len(added), count, after.afterRepo == "")
	}
	memoryReadObjectWakeDrainPG(test, storage, ctx)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}

func TestPGMemoryReadObjectWakeSweepRecreationRejectsOldParentFence(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, original, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeExpandPG(test, storage, ctx, hash)
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	prior := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	var body []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&body); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
	if count := memoryReadObjectWakeDeliveryCountPG(test, storage, ctx, hash); count != 0 {
		test.Fatal("deleted object retained old sweep deliveries", count)
	}
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), body)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(owners[34]), string(hash))
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	current := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	if current.version <= prior.version || current.scanVersion != 0 || current.throughRepo.Valid || current.afterRepo != "" {
		test.Fatal("recreated parent reused an old fence, cursor or upper boundary", prior, current)
	}
	stale := catalogTxPG(test, ctx, storage)
	if err := dispatchMemoryReadObjectWakePage(ctx, stale, string(hash), prior.version, prior.scanVersion, prior.afterRepo); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("pre-recreation sweep acknowledged the recreated parent", err)
	}
	if err := stale.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadObjectWakeReadPG(test, storage, ctx, hash); actual != current {
		test.Fatal("stale sweep changed recreated parent", current, actual)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 1, 2)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}

func TestPGMemoryReadObjectWakeContinuousWritesWithBlockedFirstPage(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, original, hash)
	before := make(map[domain.ContentHash]int64)
	for _, owner := range owners {
		before[owner] = memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
	}
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_wakes WHERE repo_id=ANY($1::text[]) ORDER BY repo_id FOR UPDATE`, strs(owners[:32]))
	for step := 0; step < 8; step++ {
		catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
		expected := 33
		if step%2 == 1 {
			expected = 4
		}
		memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, expected)
		if step%2 == 1 {
			for _, owner := range owners[32:] {
				current := memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision
				if current <= before[owner] {
					test.Fatal("continuous writes and blocked first page starved a later recipient", step, owner)
				}
				before[owner] = current
			}
		}
	}
	for _, owner := range owners[:32] {
		if current := memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision; current != before[owner] {
			test.Fatal("locked recipient was notified", owner, current)
		}
		if job := memoryReadObjectWakeDeliveryReadPG(test, storage, ctx, hash, owner); job.attempts != 1 || job.reason != "temporary_failure" {
			test.Fatal("blocked recipient lost its latest independent obligation", owner, job)
		}
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_object_wake_deliveries SET next_attempt=clock_timestamp() WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDrainPG(test, storage, ctx)
	for _, owner := range owners[:32] {
		if current := memoryReadWakeReadPG(test, storage, ctx, owner).objectRevision; current <= before[owner] {
			test.Fatal("unlock lost retained recipient obligation", owner)
		}
	}
}

func TestPGMemoryReadObjectWakeSweepBehindCursorOwnershipAndScopeWake(test *testing.T) {
	storage, ctx, original, hash := memoryReadObjectWakePG(test)
	owners := memoryReadObjectWakeOwnersPG(test, storage, ctx, original, hash)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(owners[0]), string(hash))
	if err := storage.ConfigureMemoryReadWork(ctx, owners[1], false); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 33)
	event := memoryReadObjectWakeReadPG(test, storage, ctx, hash)
	beforeSource := memorySourceGenerationPG(test, ctx, storage, owners[0])
	beforeOwner := memoryReadWakeReadPG(test, storage, ctx, owners[0])
	beforeScope := memoryReadWakeReadPG(test, storage, ctx, owners[1])
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(owners[0]), string(hash))
	if source := memorySourceGenerationPG(test, ctx, storage, owners[0]); reflect.DeepEqual(source, beforeSource) {
		test.Fatal("behind-cursor ownership addition did not advance current memory source")
	}
	if current := memoryReadWakeReadPG(test, storage, ctx, owners[0]); current.revision <= beforeOwner.revision || current.nextAttempt == nil {
		test.Fatal("behind-cursor ownership addition lost its source wake", beforeOwner, current)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, owners[1], true); err != nil {
		test.Fatal(err)
	}
	if current := memoryReadWakeReadPG(test, storage, ctx, owners[1]); current.objectRevision <= beforeScope.objectRevision || current.nextAttempt == nil {
		test.Fatal("behind-cursor scope activation lost its current-object wake", beforeScope, current)
	}
	if current := memoryReadObjectWakeReadPG(test, storage, ctx, hash); current != event {
		test.Fatal("ownership or scope wake unexpectedly reset the physical sweep", event, current)
	}
	memoryReadObjectWakeDispatchPG(test, storage, ctx, 32, 2)
	memoryReadObjectWakeAbsentPG(test, storage, ctx, hash)
}
