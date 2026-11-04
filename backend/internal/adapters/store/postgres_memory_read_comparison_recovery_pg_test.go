//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryReadComparisonRecoverPG(test *testing.T, storage *PostgresStore, ctx context.Context, prior domain.MemoryReadWork, qualification domain.MemoryReadQualification, output domain.MemoryReadBundle) {
	test.Helper()
	queued := memoryReadWorkStatusPG(test, storage, ctx, prior)
	raw, err := domain.MemoryReadQualificationBytes(ctx, qualification)
	if err != nil {
		test.Fatal(err)
	}
	identifier := domain.HashContent(raw)
	if queued.State != "waiting" || queued.Version <= prior.Version || queued.Source != prior.Source || queued.Attempts != 0 || queued.QualificationID != identifier {
		test.Fatal("comparison repair did not preserve pointer and acquire fresh fence", queued)
	}
	if memoryReadComparisonCountPG(test, storage, ctx, prior.Target.RepoID) != 0 {
		test.Fatal("queue fabricated a comparison receipt")
	}
	if _, err := storage.FinishMemoryReadWork(ctx, prior, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("legacy completion crossed new fence", err)
	}
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, prior, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("minimum completion crossed new fence", err)
	}
	if err := storage.RecordMemoryReadComparison(ctx, prior, memoryReadComparisonPG(test, ctx, prior, output)); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old worker recorded comparison after requeue", err)
	}
	if err := storage.WithCurrentMemoryReadRange(ctx, prior.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); err != nil {
		test.Fatal("repair changed strict current-reader contract", err)
	}
	next := memoryReadWorkClaimPG(test, storage, ctx, prior.Target.RepoID)
	if next.ID != prior.ID || next.Source != prior.Source || next.Version <= queued.Version {
		test.Fatal("repair required source advance or reused fence", next)
	}
	memoryReadComparisonRecordPG(test, storage, ctx, next, output)
	if actual, err := storage.FinishMinimumMemoryReadWork(ctx, next, qualification); err != nil || actual != identifier {
		test.Fatal("new comparison failed to qualify retained output", actual, err)
	}
	requirement := domain.MemoryReadRequirement{Version: domain.MemoryReadRequirementVersion, TargetID: next.ID, Minimum: prior.Source}
	for _, generation := range []domain.ContentHash{"", identifier} {
		if err := storage.WithMinimumMemoryReadRange(ctx, prior.Target, requirement, generation, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); err != nil {
			test.Fatal("matched repaired result unavailable", err)
		}
	}
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{prior.Target.RepoID}, 1)
	completed := memoryReadWorkStatusPG(test, storage, ctx, next)
	wake := memoryReadWakeReadPG(test, storage, ctx, next.Target.RepoID)
	var beforeWork, beforeWake string
	if err := storage.pool.QueryRow(ctx, `SELECT job.xmin::text,wake.xmin::text FROM memory_read_work job
 JOIN memory_read_wakes wake USING(repo_id) WHERE job.repo_id=$1 AND job.target_id=$2`, string(next.Target.RepoID), string(next.ID)).Scan(&beforeWork, &beforeWake); err != nil {
		test.Fatal(err)
	}
	for range 3 {
		if actual := memoryReadWorkRequestPG(test, storage, ctx, prior.Target); actual != completed {
			test.Fatal("matching completed target requeued", actual)
		}
		if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{prior.Target.RepoID}, 1); count != 0 {
			test.Fatal("quiet matching target repeatedly reconciled", count)
		}
	}
	var afterWork, afterWake string
	if err := storage.pool.QueryRow(ctx, `SELECT job.xmin::text,wake.xmin::text FROM memory_read_work job
 JOIN memory_read_wakes wake USING(repo_id) WHERE job.repo_id=$1 AND job.target_id=$2`, string(next.Target.RepoID), string(next.ID)).Scan(&afterWork, &afterWake); err != nil {
		test.Fatal(err)
	}
	if beforeWork != afterWork || beforeWake != afterWake || !reflect.DeepEqual(wake, memoryReadWakeReadPG(test, storage, ctx, next.Target.RepoID)) {
		test.Fatal("matching quiet target caused repeated writes")
	}
	if actual := memorySourceGenerationPG(test, ctx, storage, next.Target.RepoID); actual != prior.Source {
		test.Fatal("comparison recovery advanced logical source")
	}
}

func TestPGMemoryReadComparisonRequestRequeuesLegacyCompletion(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	queued := memoryReadWorkRequestPG(test, storage, ctx, job.Target)
	if queued.State != "waiting" {
		test.Fatal("explicit demand retained receiptless completion", queued)
	}
	memoryReadComparisonRecoverPG(test, storage, ctx, job, qualification, output)
}

func TestPGMemoryReadComparisonQuietCompletionAutomaticallyRequeues(test *testing.T) {
	for _, scenario := range []string{"strict-finish", "startup-future", "startup-unscheduled", "duplicate-strict-finish"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1)
			if wake := memoryReadWakeReadPG(test, storage, ctx, job.Target.RepoID); wake.nextAttempt != nil {
				test.Fatal("fixture retained a due wake before strict completion", wake)
			}
			memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			completed := memoryReadWorkStatusPG(test, storage, ctx, job)
			if scenario != "strict-finish" {
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_wakes SET next_attempt=clock_timestamp()+interval '1 hour' WHERE repo_id=$1`, string(job.Target.RepoID))
				if scenario == "startup-unscheduled" {
					catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_wakes SET next_attempt=NULL WHERE repo_id=$1`, string(job.Target.RepoID))
				}
				if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1); count != 0 {
					test.Fatal("idle reconciler polled targets before a scheduled wake", count)
				}
				if scenario == "duplicate-strict-finish" {
					memoryReadWorkFinishPG(test, storage, ctx, completed, qualification)
				} else if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, true); err != nil {
					test.Fatal(err)
				}
			}
			if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != completed {
				test.Fatal("scheduling mutated completion before reconciliation", actual)
			}
			wake := memoryReadWakeReadPG(test, storage, ctx, job.Target.RepoID)
			if wake.nextAttempt == nil || wake.nextAttempt.After(time.Now()) {
				test.Fatal("quiet legacy completion lost comparison wake", wake)
			}
			if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1); count != 1 {
				test.Fatal("quiet legacy result was not automatically requeued", count)
			}
			memoryReadComparisonRecoverPG(test, storage, ctx, job, qualification, output)
		})
	}
}

func memoryReadComparisonLegacyTargetPG(test *testing.T, storage *PostgresStore, ctx context.Context, target domain.MemoryReadTarget) domain.MemoryReadWork {
	test.Helper()
	capture, output := memoryReadPGFixture(test, storage, ctx, target.RepoID, 0)
	capture.Header.Selection = target.Selection
	capture = memoryBundlePGSeal(test, ctx, capture.Header, capture.Parts)
	output.Header.CaptureID, output.Header.View.Selection = capture.ID, target.Selection
	output = memoryReadPGSeal(test, ctx, output.Header, nil)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, output)
	memoryReadWorkRequestPG(test, storage, ctx, target)
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	memoryReadWorkFinishPG(test, storage, ctx, job, domain.MemoryReadQualification{
		Version: domain.MemoryReadQualificationVersion, ReadID: output.ID, CaptureID: capture.ID, Source: output.Header.Source,
	})
	return memoryReadWorkStatusPG(test, storage, ctx, job)
}

func TestPGMemoryReadComparisonRecoveryCapsAndRepositoryFairness(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	var retained []domain.MemoryReadWork
	for ordinal := range domain.MaxPendingMemoryReadWork + 2 {
		next := target
		next.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal+1)
		retained = append(retained, memoryReadComparisonLegacyTargetPG(test, storage, ctx, next))
	}
	other := memoryReadComparisonLegacyTargetPG(test, storage, ctx, memoryReadWorkOtherRepoPG(test, storage, ctx, target))
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_wakes SET next_attempt=clock_timestamp()-interval '2 seconds' WHERE repo_id=$1`, string(target.RepoID))
	memoryReadWakeDuePG(test, storage, ctx, other.Target.RepoID)
	repos := []domain.ContentHash{target.RepoID, other.Target.RepoID}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 1 {
		test.Fatal("bounded recovery did not process first repository", count)
	}
	var active, completed, pointers int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='waiting'),count(*) FILTER(WHERE state='completed'),
 count(*) FILTER(WHERE qualification_id IS NOT NULL) FROM memory_read_work WHERE repo_id=$1`, string(target.RepoID)).Scan(&active, &completed, &pointers); err != nil {
		test.Fatal(err)
	}
	if active != domain.MaxPendingMemoryReadWork || completed != 2 || pointers != len(retained) {
		test.Fatal("comparison recovery exceeded cap or discarded pointers", active, completed, pointers)
	}
	if _, err := storage.RequestMemoryReadWork(ctx, retained[len(retained)-1].Target); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("explicit recovery bypassed active-work cap", err)
	}
	wake := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	if wake.nextAttempt == nil || !wake.nextAttempt.After(time.Now()) {
		test.Fatal("capped remainder lost bounded retry", wake)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 1 {
		test.Fatal("capped first repository blocked another repository", count)
	}
	if current := memoryReadWorkStatusPG(test, storage, ctx, other); current.State != "waiting" || current.QualificationID != other.QualificationID {
		test.Fatal("second repository failed to recover", current)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 0 {
		test.Fatal("missing receipt bypassed capacity backoff", count)
	}
	if after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); !reflect.DeepEqual(after, wake) {
		test.Fatal("idle recovery rewrote capped wake", wake, after)
	}
	claimed := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if err := storage.FailMemoryReadWork(ctx, claimed, "invalid_source", false); err != nil {
		test.Fatal(err)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 1 {
		test.Fatal("released capacity failed to drain legacy remainder", count)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE state='waiting'),count(*) FILTER(WHERE state='completed')
 FROM memory_read_work WHERE repo_id=$1`, string(target.RepoID)).Scan(&active, &completed); err != nil {
		test.Fatal(err)
	}
	if active != domain.MaxPendingMemoryReadWork || completed != 1 || memoryReadComparisonCountPG(test, storage, ctx, target.RepoID) != 0 {
		test.Fatal("recovery lost bounded progress or fabricated proof", active, completed)
	}
}

func TestPGMemoryReadComparisonRecoveryScopeAndFailureBackoff(test *testing.T) {
	for _, scenario := range []string{"disabled", "unconfigured", "backoff"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputWithoutComparisonPG(test)
			memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			repos := []domain.ContentHash{job.Target.RepoID}
			switch scenario {
			case "disabled":
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
					test.Fatal(err)
				}
			case "unconfigured":
				repos = []domain.ContentHash{domain.HashContent([]byte("unconfigured repository"))}
			case "backoff":
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_wakes SET attempts=1,reason='temporary_failure',
 next_attempt=clock_timestamp()+interval '30 seconds' WHERE repo_id=$1`, string(job.Target.RepoID))
			}
			wake := memoryReadWakeReadPG(test, storage, ctx, job.Target.RepoID)
			for range 3 {
				if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 0 {
					test.Fatal("comparison recovery bypassed scope or backoff", count)
				}
			}
			if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != before || !reflect.DeepEqual(wake, memoryReadWakeReadPG(test, storage, ctx, job.Target.RepoID)) {
				test.Fatal("ineligible comparison recovery mutated work or wake", actual)
			}
			if scenario == "disabled" {
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, true); err != nil {
					test.Fatal(err)
				}
			} else if scenario == "backoff" {
				memoryReadWakeDuePG(test, storage, ctx, job.Target.RepoID)
			}
			if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1); count != 1 {
				test.Fatal("eligible recovery failed to resume", count)
			}
			if queued := memoryReadWorkStatusPG(test, storage, ctx, job); queued.State != "waiting" || queued.Version <= before.Version {
				test.Fatal("resumed comparison recovery did not requeue", queued)
			}
		})
	}
}

func TestPGMemoryReadComparisonRecoveryDoesNotOverwriteReceipts(test *testing.T) {
	for _, scenario := range []string{"matched", "mismatch", "corrupt"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
			comparison := memoryReadComparisonPG(test, ctx, job, output)
			var expected error
			if scenario == "mismatch" {
				comparison.Expected = domain.HashContent([]byte("different reference"))
				expected = domain.ErrMemoryReadNeedsAttention
			}
			if err := storage.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
				test.Fatal(err)
			}
			memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			if scenario == "corrupt" {
				expected = domain.ErrIntegrity
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_comparisons SET body=convert_to('{}','UTF8')
 WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(output.ID))
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			if _, err := storage.RequestMemoryReadWork(ctx, job.Target); !errors.Is(err, expected) {
				test.Fatal("request ignored persisted comparison state", err)
			}
			if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, true); err != nil {
				test.Fatal(err)
			}
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1)
			if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before {
				test.Fatal("existing receipt caused automatic re-comparison", after)
			}
			if memoryReadComparisonCountPG(test, storage, ctx, job.Target.RepoID) != 1 {
				test.Fatal("existing receipt discarded by automatic recovery")
			}
		})
	}
}

func TestPGMemoryReadComparisonRecoveryDeletionDuringScheduling(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1)
	completed := memoryReadWorkStatusPG(test, storage, ctx, job)
	memoryReadWakeDuePG(test, storage, ctx, job.Target.RepoID)
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "UPDATE memory_read_work retained SET wake_revision=$3")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := peer.ReconcileMemoryReadWork(ctx, []domain.ContentHash{job.Target.RepoID}, 1)
		done <- err
	}()
	memoryQualificationWaitPG(test, ctx, reached, done)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_comparisons WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(output.ID))
	release()
	if err := <-done; err != nil {
		test.Fatal(err)
	}
	if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != completed {
		test.Fatal("receipt disappeared before eligibility barrier", actual)
	}
	if wake := memoryReadWakeReadPG(test, storage, ctx, job.Target.RepoID); wake.nextAttempt == nil || wake.nextAttempt.After(time.Now()) {
		test.Fatal("receipt deletion between eligibility and scheduling lost its wake", wake)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{job.Target.RepoID}, 1); count != 1 {
		test.Fatal("raced receipt deletion was not recoverable", count)
	}
	memoryReadComparisonRecoverPG(test, storage, ctx, job, qualification, output)
}
