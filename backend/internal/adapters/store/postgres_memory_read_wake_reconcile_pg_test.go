//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryReadWakeRejectPG(test *testing.T, storage *PostgresStore, ctx context.Context, target domain.MemoryReadTarget) domain.MemoryReadWork {
	test.Helper()
	memoryReadWorkRequestPG(test, storage, ctx, target)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if err := storage.FailMemoryReadWork(ctx, job, "invalid_source", false); err != nil {
		test.Fatal(err)
	}
	return memoryReadWorkStatusPG(test, storage, ctx, job)
}

func TestPGMemoryReadWakeCompletedTargetRequeuesWithoutDemand(test *testing.T) {
	for _, scenario := range []string{"graph", "memory", "physical-object", "catalog-reset", "memory-reset"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			repo := job.Target.RepoID
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
			completed := memoryReadWorkStatusPG(test, storage, ctx, job)
			switch scenario {
			case "graph":
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='source after completion' WHERE repo_id=$1`, string(repo))
			case "memory":
				digest := memoryCaptureTestDigest()
				digest.SnapshotID, digest.Summary = job.Target.Selection.SnapshotID, "new memory after completion"
				if _, err := storage.PutMemory(ctx, repo, digest); err != nil {
					test.Fatal(err)
				}
			case "physical-object":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
				memoryReadWakeDispatchPG(test, storage, ctx)
			case "catalog-reset":
				catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
			case "memory-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch=gen_random_uuid(),head_seq=0 WHERE repo_id=$1`, string(repo))
			}
			if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != completed {
				test.Fatal("business write mutated completed target before reconciliation", completed, actual)
			}
			if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
				test.Fatal("source notice did not reconcile", count)
			}
			queued := memoryReadWorkStatusPG(test, storage, ctx, job)
			if queued.State != "waiting" || queued.Attempts != 0 || queued.Version <= completed.Version || queued.QualificationID != identifier || queued.Source != completed.Source {
				test.Fatal("completed target was not refreshed with its historical pointer intact", completed, queued)
			}
			claimed := memoryReadWorkClaimPG(test, storage, ctx, repo)
			if claimed.ID != job.ID || claimed.Attempts != 1 || claimed.Version <= queued.Version || claimed.Source != memorySourceGenerationPG(test, ctx, storage, repo) {
				test.Fatal("auto-enqueued target not claimable", claimed)
			}
			if (claimed.Source == completed.Source) != (scenario == "physical-object") {
				test.Fatal("physical witness and business source clocks conflated", scenario, completed.Source, claimed.Source)
			}
			if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("old completion fence republished after wake", err)
			}
		})
	}
}

func TestPGMemoryReadWakeQuietExpiryAndMissingQualificationRecovery(test *testing.T) {
	for _, scenario := range []string{"expired-capture", "missing-qualification"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			repo := job.Target.RepoID
			memoryReadPGExpireSoon(test, storage, ctx, repo, qualification.CaptureID)
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
			var expiry time.Time
			if err := storage.pool.QueryRow(ctx, `SELECT expires_at FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(qualification.CaptureID)).Scan(&expiry); err != nil {
				test.Fatal(err)
			}
			wake := memoryReadWakeReadPG(test, storage, ctx, repo)
			if wake.nextAttempt == nil || wake.nextAttempt.After(expiry) || !wake.nextAttempt.After(time.Now()) {
				test.Fatal("completion did not schedule its capture expiry", wake, expiry)
			}
			completed := memoryReadWorkStatusPG(test, storage, ctx, job)
			for range 3 {
				if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 0 {
					test.Fatal("quiet completed target was polled before expiry", count)
				}
				if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != completed {
					test.Fatal("quiet reconciliation mutated current completion", completed, actual)
				}
			}
			if scenario == "missing-qualification" {
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_qualifications WHERE repo_id=$1 AND qualification_id=$2`, string(repo), string(identifier))
			} else {
				memoryReadPGWaitExpiry(test, storage, ctx, repo, qualification.CaptureID)
			}
			if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
				test.Fatal("quiet expiry did not wake completed target", count)
			}
			queued := memoryReadWorkStatusPG(test, storage, ctx, job)
			if queued.State != "waiting" || queued.Attempts != 0 || queued.Source != completed.Source || queued.Version <= completed.Version {
				test.Fatal("expired or missing qualification did not auto-requeue", completed, queued)
			}
			wantedPointer := identifier
			if scenario == "missing-qualification" {
				wantedPointer = ""
			}
			if queued.QualificationID != wantedPointer {
				test.Fatal("retention recovery changed historical pointer unexpectedly", queued)
			}
			if after := memoryReadWakeReadPG(test, storage, ctx, repo); scenario == "expired-capture" && after.revision != wake.revision {
				test.Fatal("quiet expiry manufactured a business source change", wake, after)
			}
			if claimed := memoryReadWorkClaimPG(test, storage, ctx, repo); claimed.ID != job.ID || claimed.Attempts != 1 {
				test.Fatal("quiet recovery not claimable", claimed)
			}
		})
	}
}

func TestPGMemoryReadWakeSchedulesMinimumCompletedCaptureExpiry(test *testing.T) {
	storage, ctx, early, earlyQualification, _ := memoryReadWorkOutputPG(test)
	repo := early.Target.RepoID
	target := early.Target
	target.Selection.CodeCommit = fmt.Sprintf("%040x", 2)
	capture, output := memoryReadPGFixture(test, storage, ctx, repo, 0)
	capture.Header.Selection = target.Selection
	capture = memoryBundlePGSeal(test, ctx, capture.Header, capture.Parts)
	output.Header.CaptureID, output.Header.View.Selection = capture.ID, target.Selection
	output = memoryReadPGSeal(test, ctx, output.Header, nil)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, output)
	memoryReadWorkRequestPG(test, storage, ctx, target)
	later := memoryReadWorkClaimPG(test, storage, ctx, repo)
	if later.Target != target {
		test.Fatal("later-expiring fixture claimed the wrong target", later)
	}
	memoryReadComparisonRecordPG(test, storage, ctx, later, output)
	memoryReadWorkFinishPG(test, storage, ctx, later, domain.MemoryReadQualification{
		Version: domain.MemoryReadQualificationVersion, ReadID: output.ID, CaptureID: capture.ID, Source: output.Header.Source,
	})
	later = memoryReadWorkStatusPG(test, storage, ctx, later)
	memoryReadPGExpireSoon(test, storage, ctx, repo, earlyQualification.CaptureID)
	memoryReadWorkFinishPG(test, storage, ctx, early, earlyQualification)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
	var earlyExpiry, laterExpiry time.Time
	if err := storage.pool.QueryRow(ctx, `SELECT first.expires_at,last.expires_at FROM memory_capture_headers first
 JOIN memory_capture_headers last ON last.repo_id=first.repo_id AND last.capture_id=$3
 WHERE first.repo_id=$1 AND first.capture_id=$2`, string(repo), string(earlyQualification.CaptureID), string(capture.ID)).Scan(&earlyExpiry, &laterExpiry); err != nil {
		test.Fatal(err)
	}
	wake := memoryReadWakeReadPG(test, storage, ctx, repo)
	if wake.nextAttempt == nil || !wake.nextAttempt.Equal(earlyExpiry) || !earlyExpiry.Before(laterExpiry) {
		test.Fatal("repository wake did not select the earliest capture expiry", wake, earlyExpiry, laterExpiry)
	}
	memoryReadPGWaitExpiry(test, storage, ctx, repo, earlyQualification.CaptureID)
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
		test.Fatal("earliest expiry did not wake repository", count)
	}
	if queued := memoryReadWorkStatusPG(test, storage, ctx, early); queued.State != "waiting" || queued.Attempts != 0 {
		test.Fatal("earliest expired target did not requeue", queued)
	}
	if actual := memoryReadWorkStatusPG(test, storage, ctx, later); actual != later {
		test.Fatal("earliest expiry requeued a still-current target", later, actual)
	}
	if next := memoryReadWakeReadPG(test, storage, ctx, repo); next.nextAttempt == nil || !next.nextAttempt.Equal(laterExpiry) {
		test.Fatal("expiry wake did not advance to the next retained capture", next, laterExpiry)
	}
}

func TestPGMemoryReadWakeRetentionChangeBetweenEligibilityAndScheduling(test *testing.T) {
	for _, scenario := range []string{"capture-expires", "capture-disappears"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			repo := job.Target.RepoID
			memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
			completed := memoryReadWorkStatusPG(test, storage, ctx, job)
			memoryReadWakeDuePG(test, storage, ctx, repo)
			before := memoryReadWakeReadPG(test, storage, ctx, repo)
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "UPDATE memory_read_work retained SET wake_revision=$3")
			defer release()
			done := make(chan error, 1)
			go func() {
				_, err := peer.ReconcileMemoryReadWork(ctx, []domain.ContentHash{repo}, 1)
				done <- err
			}()
			memoryQualificationWaitPG(test, ctx, reached, done)
			if scenario == "capture-expires" {
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET created_at=clock_timestamp()-interval '1 hour',
 expires_at=clock_timestamp()+interval '100 milliseconds' WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(qualification.CaptureID))
				memoryReadPGWaitExpiry(test, storage, ctx, repo, qualification.CaptureID)
			} else {
				memoryBundlePGFault(test, storage, ctx, `DELETE FROM memory_capture_headers WHERE repo_id=$1 AND capture_id=$2`, string(repo), string(qualification.CaptureID))
			}
			release()
			if err := <-done; err != nil {
				test.Fatal("retention race aborted repository reconciliation", err)
			}
			if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != completed {
				test.Fatal("fixture did not expire after target eligibility was checked", completed, actual)
			}
			after := memoryReadWakeReadPG(test, storage, ctx, repo)
			if after.revision != before.revision || after.nextAttempt == nil || after.nextAttempt.After(time.Now()) {
				test.Fatal("retention change between eligibility and scheduling lost its next wake", before, after)
			}
			if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
				test.Fatal("raced retention change was not immediately retryable", count)
			}
			if queued := memoryReadWorkStatusPG(test, storage, ctx, job); queued.State != "waiting" || queued.Attempts != 0 || queued.Version <= completed.Version {
				test.Fatal("raced retention change left completed target stranded", queued)
			}
		})
	}
}

func TestPGMemoryReadWakeRefreshesQueuedFailureBudgetOnlyForNewSource(test *testing.T) {
	for _, state := range []string{"waiting", "retrying", "rejected"} {
		test.Run(state, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			job := memoryReadWorkRequestPG(test, storage, ctx, target)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
			if state != "waiting" {
				job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
				if err := storage.FailMemoryReadWork(ctx, job, "temporary_failure", state == "retrying"); err != nil {
					test.Fatal(err)
				}
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			for range 3 {
				memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
				if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != before {
					test.Fatal("repeated no-op reset target attempts, backoff, or fence", before, actual)
				}
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='new queued source' WHERE repo_id=$1`, string(target.RepoID))
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
			queued := memoryReadWorkStatusPG(test, storage, ctx, job)
			wantedState := "waiting"
			if state == "retrying" {
				wantedState = "retrying"
			}
			if queued.State != wantedState || queued.Attempts != 0 || queued.Reason != "" || queued.Source != before.Source || queued.Version <= before.Version {
				test.Fatal("new source failed to reset failure budget", before, queued)
			}
			if claimed := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID); claimed.Attempts != 1 || claimed.Source == before.Source || claimed.Source != memorySourceGenerationPG(test, ctx, storage, target.RepoID) {
				test.Fatal("refreshed source inherited obsolete attempt budget", claimed)
			}
		})
	}
}

func TestPGMemoryReadWakeDoesNotPreemptRunningWork(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	repo := job.Target.RepoID
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='advance while running' WHERE repo_id=$1`, string(repo))
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
	if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != job {
		test.Fatal("reconciler preempted a running lease", job, actual)
	}
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale running work crossed source fence", err)
	}
	if err := storage.FailMemoryReadWork(ctx, job, "invalid_source", false); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
	if refreshed := memoryReadWorkClaimPG(test, storage, ctx, repo); refreshed.Attempts != 1 || refreshed.Source == job.Source || refreshed.Version <= job.Version {
		test.Fatal("non-preempted worker did not recover on claim", refreshed)
	}
}

func TestPGMemoryReadWakeScopeOptInAndLocalSelection(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	job := memoryReadWakeRejectPG(test, storage, ctx, target)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
	if err := storage.ConfigureMemoryReadWork(ctx, target.RepoID, false); err != nil {
		test.Fatal(err)
	}
	disabled := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='disabled source' WHERE repo_id=$1`, string(target.RepoID))
	pending := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	if pending.revision <= disabled.revision || pending.objectRevision != disabled.objectRevision || pending.nextAttempt == nil {
		test.Fatal("disabled scope discarded its logical source notice", disabled, pending)
	}
	for _, repos := range [][]domain.ContentHash{nil, {target.RepoID}, {domain.HashContent([]byte("locally unselected wake"))}} {
		if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 0 {
			test.Fatal("disabled or locally unselected repository reconciled", count)
		}
		if _, err := storage.ClaimMemoryReadWork(ctx, repos, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("disabled or locally unselected repository executed work", err)
		}
		if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != job {
			test.Fatal("disabled intent was refreshed", job, actual)
		}
		retained := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
		if retained.revision != pending.revision || retained.objectRevision != pending.objectRevision || retained.attempts != pending.attempts ||
			retained.nextAttempt == nil || !retained.nextAttempt.Equal(*pending.nextAttempt) {
			test.Fatal("skipped execution consumed a disabled repository notice", pending, retained)
		}
	}
	before := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	if err := storage.ConfigureMemoryReadWork(ctx, target.RepoID, true); err != nil {
		test.Fatal(err)
	}
	if after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); after.revision <= before.revision || after.nextAttempt == nil {
		test.Fatal("scope enable did not publish a durable wake", before, after)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, nil, 1); count != 0 {
		test.Fatal("empty local configuration selected enabled repository", count)
	}
	if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != job {
		test.Fatal("empty local configuration changed intent", actual)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 1 {
		test.Fatal("opted-in repository did not reconcile", count)
	}
	if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual.State != "waiting" || actual.Source != job.Source {
		test.Fatal("re-enable lost source change", actual)
	}
	if claimed := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID); claimed.Source == job.Source {
		test.Fatal("re-enabled work did not refresh source on claim", claimed)
	}
}

func TestPGMemoryReadWakeQuotaDrainsRetainedTargetsOnce(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	for ordinal := range domain.MaxMemoryReadTargets {
		next := target
		next.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal+1)
		work := memoryReadWorkRequestPG(test, storage, ctx, next)
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state='rejected',reason='invalid_source' WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(work.ID))
	}
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='wake all retained targets' WHERE repo_id=$1`, string(target.RepoID))
	seen := make(map[string]bool)
	for len(seen) < domain.MaxMemoryReadTargets {
		if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 1 {
			test.Fatal("pending targets lost their repository notice", count, len(seen))
		}
		rows, err := storage.pool.Query(ctx, `SELECT target_id,attempts,wake_revision FROM memory_read_work WHERE repo_id=$1 AND state IN ('waiting','retrying','running')`, string(target.RepoID))
		if err != nil {
			test.Fatal(err)
		}
		active := 0
		for rows.Next() {
			var identifier string
			var attempts int
			var revision int64
			if err := rows.Scan(&identifier, &attempts, &revision); err != nil {
				rows.Close()
				test.Fatal(err)
			}
			if seen[identifier] || attempts != 0 || revision == 0 {
				rows.Close()
				test.Fatal("notice requeued an acknowledged target or kept old attempts", identifier, attempts, revision)
			}
			seen[identifier] = true
			active++
		}
		err = rows.Err()
		rows.Close()
		if err != nil || active != domain.MaxPendingMemoryReadWork {
			test.Fatal("reconciliation violated active quota or stalled drain", active, err)
		}
		var retained int
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, string(target.RepoID)).Scan(&retained); err != nil || retained != domain.MaxMemoryReadTargets {
			test.Fatal("wake changed retained target cap", retained, err)
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state='rejected',reason='invalid_source' WHERE repo_id=$1 AND state='waiting'`, string(target.RepoID))
		if len(seen) < domain.MaxMemoryReadTargets {
			if wake := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); wake.nextAttempt == nil {
				test.Fatal("quota backlog was acknowledged before it drained", wake)
			}
			memoryReadWakeDuePG(test, storage, ctx, target.RepoID)
		}
	}
	memoryReadWakeDuePG(test, storage, ctx, target.RepoID)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
	var active int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1 AND state IN ('waiting','retrying','running')`, string(target.RepoID)).Scan(&active); err != nil || active != 0 {
		test.Fatal("fully acknowledged notice restarted rejected targets", active, err)
	}
}
