//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadWorkNoConsumePG(context.Context, domain.MemoryReadHeader, outbound.MemoryReadChunkReader) error {
	return nil
}

func memoryReadWorkFinishPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.MemoryReadWork, qualification domain.MemoryReadQualification) domain.ContentHash {
	test.Helper()
	identifier, err := storage.FinishMemoryReadWork(ctx, job, qualification)
	if err != nil {
		test.Fatal(err)
	}
	return identifier
}

func TestPGMemoryReadWorkFinishDuplicateAndPointerLifetime(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	completed := memoryReadWorkStatusPG(test, storage, ctx, job)
	if completed.State != "completed" || completed.QualificationID != identifier || completed.Version != job.Version || !completed.LeaseUntil.IsZero() {
		test.Fatal("completion receipt", completed)
	}
	for _, duplicate := range []domain.MemoryReadWork{job, completed} {
		if got := memoryReadWorkFinishPG(test, storage, ctx, duplicate, qualification); got != identifier {
			test.Fatal("duplicate changed pointer")
		}
	}
	if got := memoryReadWorkRequestPG(test, storage, ctx, job.Target); got.Version != job.Version || got.State != "completed" {
		test.Fatal("current completion requeued", got)
	}
	if err := storage.FailMemoryReadWork(ctx, job, "temporary_failure", true); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("late failure changed completed work", err)
	}
	output.Header.Origin += "different"
	alternate := memoryReadPGSeal(test, ctx, output.Header, nil)
	memoryReadPGWrite(test, storage, ctx, alternate)
	different := qualification
	different.ReadID = alternate.ID
	if _, err := storage.FinishMemoryReadWork(ctx, job, different); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("same fence replaced completed pointer", err)
	}
	if memoryQualificationCountPG(test, storage, ctx, job.Target.RepoID) != 1 {
		test.Fatal("late overwrite leaked qualification")
	}
	memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("expired pointer current", err)
	}
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrMemoryCaptureExpired) {
		test.Fatal("expired duplicate accepted", err)
	}
	if count, err := storage.PruneMemoryCaptures(ctx, job.Target.RepoID, 1); err != nil || count != 1 {
		test.Fatal("prune", count, err)
	}
	if got := memoryReadWorkStatusPG(test, storage, ctx, job); got.State != "completed" || got.QualificationID != "" {
		test.Fatal("qualification cascade deleted work or retained pointer", got)
	}
	if got := memoryReadWorkRequestPG(test, storage, ctx, job.Target); got.State != "waiting" || got.Version <= job.Version || got.Attempts != 0 {
		test.Fatal("expired completion not requeued", got)
	}
}

func TestPGMemoryReadWorkFinishRejectsForeignStaleAndMismatchedWork(test *testing.T) {
	for _, scenario := range []string{"qualification-source", "target-selection", "target-content", "source-advance", "expired-lease", "witness", "disabled", "reenabled", "capture-expired", "capture-mismatch", "witness-missing"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			switch scenario {
			case "qualification-source":
				qualification.Source.Sequence++
			case "target-selection", "target-content":
				target := job.Target
				if scenario == "target-selection" {
					target.Selection.CodeCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				} else {
					target.Content = "prompt"
				}
				memoryReadWorkRequestPG(test, storage, ctx, target)
				job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
			case "source-advance":
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='advance before finish' WHERE repo_id=$1`, string(job.Target.RepoID))
			case "expired-lease":
				memoryReadWorkExpirePG(test, storage, ctx, job)
			case "witness":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
			case "disabled", "reenabled":
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
					test.Fatal(err)
				}
				if scenario == "reenabled" {
					if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, true); err != nil {
						test.Fatal(err)
					}
				}
			case "capture-expired":
				memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			case "capture-mismatch":
				qualification.CaptureID = domain.HashContent([]byte("foreign capture"))
			case "witness-missing":
				qualification.Witnesses = nil
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			if identifier, err := storage.FinishMemoryReadWork(ctx, job, qualification); err == nil || identifier != "" {
				test.Fatal("invalid finish accepted", identifier, err)
			}
			after := memoryReadWorkStatusPG(test, storage, ctx, job)
			if after != before || memoryQualificationCountPG(test, storage, ctx, job.Target.RepoID) != 0 {
				test.Fatal("failed finish changed durable state", before, after)
			}
		})
	}
}

func TestPGMemoryReadWorkFinishRollsBackAtFinalFence(test *testing.T) {
	for _, scenario := range []string{"cancel-after-qualification", "cancel-after-pointer", "lease-after-pointer"} {
		test.Run(scenario, func(test *testing.T) {
			storage, parent, job, qualification, _ := memoryReadWorkOutputPG(test)
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			prefix := "INSERT INTO memory_read_qualifications"
			if scenario != "cancel-after-qualification" {
				prefix = "UPDATE memory_read_work SET state='completed'"
			}
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, prefix)
			defer release()
			if scenario == "lease-after-pointer" {
				catalogExecPG(test, parent, storage.pool, `UPDATE memory_read_work SET lease_until=clock_timestamp()+interval '150 milliseconds'
 WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID))
			}
			done := make(chan error, 1)
			go func() { _, err := peer.FinishMemoryReadWork(ctx, job, qualification); done <- err }()
			memoryQualificationWaitPG(test, parent, reached, done)
			if scenario == "lease-after-pointer" {
				var remaining float64
				if err := storage.pool.QueryRow(parent, `SELECT GREATEST(0,extract(epoch FROM lease_until-clock_timestamp())) FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`,
					string(job.Target.RepoID), string(job.ID)).Scan(&remaining); err != nil {
					test.Fatal(err)
				}
				time.Sleep(time.Duration(remaining*float64(time.Second)) + 5*time.Millisecond)
			} else {
				cancel()
			}
			release()
			err := <-done
			wanted := context.Canceled
			if scenario == "lease-after-pointer" {
				wanted = domain.ErrConflict
			}
			if !errors.Is(err, wanted) {
				test.Fatal("final fence did not abort", err)
			}
			status := memoryReadWorkStatusPG(test, storage, parent, job)
			if status.State != "running" || status.QualificationID != "" || memoryQualificationCountPG(test, storage, parent, job.Target.RepoID) != 0 {
				test.Fatal("failed atomic transaction left pointer or qualification", status)
			}
		})
	}
}

func TestPGMemoryReadWorkDisableSerializesWithFinish(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "INSERT INTO memory_read_qualifications")
	defer release()
	finished := make(chan error, 1)
	go func() { _, err := peer.FinishMemoryReadWork(ctx, job, qualification); finished <- err }()
	memoryQualificationWaitPG(test, ctx, reached, finished)
	disabled := make(chan error, 1)
	go func() { disabled <- storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false) }()
	select {
	case err := <-disabled:
		test.Fatal("disable crossed completion scope lock", err)
	case <-time.After(20 * time.Millisecond):
	}
	release()
	if err := <-finished; err != nil {
		test.Fatal(err)
	}
	if err := <-disabled; err != nil {
		test.Fatal(err)
	}
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("disabled pointer current", err)
	}
}

func TestPGMemoryReadWorkRequestLockBudget(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_work_scopes WHERE repo_id=$1 FOR UPDATE`, string(target.RepoID))
	started := time.Now()
	if _, err := storage.RequestMemoryReadWork(ctx, target); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("unbounded queue wait", err)
	}
	if time.Since(started) > time.Second {
		test.Fatal("queue exceeded budget")
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memoryReadWorkRequestPG(test, storage, ctx, target)
}

func TestPGMemoryReadWorkExistingQualificationFinalExpiry(test *testing.T) {
	for _, duplicate := range []bool{false, true} {
		test.Run(map[bool]string{false: "prior-qualified", true: "duplicate-completed"}[duplicate], func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			if duplicate {
				memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			} else if _, err := storage.QualifyMemoryRead(ctx, qualification); err != nil {
				test.Fatal(err)
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			prefix := "UPDATE memory_read_work SET state='completed'"
			if duplicate {
				prefix = "UPDATE memory_read_work SET qualification_id=qualification_id"
			}
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, prefix)
			defer release()
			memoryBundlePGFault(test, storage, ctx, `UPDATE memory_capture_headers SET created_at=clock_timestamp()-interval '1 hour',
 expires_at=clock_timestamp()+interval '150 milliseconds' WHERE repo_id=$1 AND capture_id=$2`, string(job.Target.RepoID), string(qualification.CaptureID))
			done := make(chan error, 1)
			go func() { _, err := peer.FinishMemoryReadWork(ctx, job, qualification); done <- err }()
			memoryQualificationWaitPG(test, ctx, reached, done)
			memoryReadPGWaitExpiry(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			release()
			if err := <-done; !errors.Is(err, domain.ErrMemoryCaptureExpired) {
				test.Fatal("existing qualification expired before completion commit", err)
			}
			if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before {
				test.Fatal("failed finish changed pointer")
			}
		})
	}
}
