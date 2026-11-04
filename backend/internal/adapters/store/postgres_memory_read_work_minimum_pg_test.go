//go:build postgres

package store

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGMemoryReadWorkMinimumFinishKeepsLaterWakeAndDuplicate(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='after captured input' WHERE repo_id=$1`, string(job.Target.RepoID))
	if identifier, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) || identifier != "" {
		test.Fatal("strict completion accepted old source", identifier, err)
	}
	identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification)
	if err != nil {
		test.Fatal(err)
	}
	completed := memoryReadWorkStatusPG(test, storage, ctx, job)
	if completed.State != "completed" || completed.Source != qualification.Source || completed.QualificationID != identifier {
		test.Fatal("bounded completion did not publish captured source", completed)
	}
	var pending bool
	if err := storage.pool.QueryRow(ctx, `SELECT wake.revision>job.wake_revision AND wake.next_attempt<=clock_timestamp()
 FROM memory_read_wakes wake JOIN memory_read_work job USING(repo_id)
 WHERE job.repo_id=$1 AND job.target_id=$2`, string(job.Target.RepoID), string(job.ID)).Scan(&pending); err != nil || !pending {
		test.Fatal("later wake was consumed", pending, err)
	}
	for _, duplicate := range []domain.MemoryReadWork{job, completed} {
		if actual, err := storage.FinishMinimumMemoryReadWork(ctx, duplicate, qualification); err != nil || actual != identifier {
			test.Fatal("bounded duplicate failed", actual, err)
		}
	}
	if count := memoryQualificationCountPG(test, storage, ctx, job.Target.RepoID); count != 1 {
		test.Fatal("duplicate completion duplicated qualification", count)
	}
	requirement := domain.MemoryReadRequirement{Version: domain.MemoryReadRequirementVersion, TargetID: job.ID, Minimum: job.Source}
	read := func() {
		test.Helper()
		if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, "", memoryQualificationAllowPG,
			func(_ context.Context, status domain.MemoryReadStatus, _ domain.MemoryReadHeader, _ outbound.MemoryReadChunkReader) error {
				if status.Generation != identifier || status.Applied != qualification.Source || status.Freshness != "catching_up" {
					test.Fatal("work lifecycle changed retained qualified result", status)
				}
				return nil
			}); err != nil {
			test.Fatal("work lifecycle hid retained qualified result", err)
		}
	}
	if count, err := storage.ReconcileMemoryReadWork(ctx, []domain.ContentHash{job.Target.RepoID}, 1); err != nil || count != 1 {
		test.Fatal("later wake did not reconcile", count, err)
	}
	if current := memoryReadWorkStatusPG(test, storage, ctx, job); current.State != "waiting" || current.Version <= job.Version || current.QualificationID != identifier {
		test.Fatal("later source not scheduled", current)
	}
	read()
	next := memoryReadWorkClaimPG(test, storage, ctx, job.Target.RepoID)
	if next.Source == qualification.Source {
		test.Fatal("new claim did not capture later source floor", next)
	}
	read()
	if err := storage.FailMemoryReadWork(ctx, next, "temporary_failure", true); err != nil {
		test.Fatal(err)
	}
	read()
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old claim replaced requeued work", err)
	}
}

func TestPGMemoryReadWorkMinimumFinishNewerCaptureThanClaim(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='before next capture' WHERE repo_id=$1`, string(job.Target.RepoID))
	capture, output := memoryReadPGFixture(test, storage, ctx, job.Target.RepoID, 0)
	capture.Header.Selection = job.Target.Selection
	capture = memoryBundlePGSeal(test, ctx, capture.Header, capture.Parts)
	header := output.Header
	header.CaptureID, header.View.Selection = capture.ID, job.Target.Selection
	header.Dependencies = []domain.MemoryReadDependency{{Hash: qualification.Witnesses[0].Hash, Available: true}}
	output = memoryReadPGSeal(test, ctx, header, nil)
	memoryBundlePGWrite(test, storage, ctx, capture, time.Now().Add(time.Hour))
	memoryReadPGWrite(test, storage, ctx, output)
	qualification.ReadID, qualification.CaptureID, qualification.Source = output.ID, capture.ID, output.Header.Source
	memoryReadComparisonRecordPG(test, storage, ctx, job, output)
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("strict finish changed exact-source contract", err)
	}
	identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification)
	if err != nil {
		test.Fatal(err)
	}
	completed := memoryReadWorkStatusPG(test, storage, ctx, job)
	if completed.Source != qualification.Source || completed.Source == job.Source {
		test.Fatal("work receipt retained claim floor instead of applied source", completed)
	}
	for _, duplicate := range []domain.MemoryReadWork{job, completed} {
		if actual, err := storage.FinishMinimumMemoryReadWork(ctx, duplicate, qualification); err != nil || actual != identifier {
			test.Fatal("original and completed receipts disagree", actual, err)
		}
	}
	var caughtUp bool
	if err := storage.pool.QueryRow(ctx, `SELECT wake.revision=job.wake_revision FROM memory_read_wakes wake
 JOIN memory_read_work job USING(repo_id) WHERE job.repo_id=$1 AND job.target_id=$2`, string(job.Target.RepoID), string(job.ID)).Scan(&caughtUp); err != nil || !caughtUp {
		test.Fatal("current completion failed to acknowledge captured source", caughtUp, err)
	}
}

func TestPGMemoryReadWorkMinimumFinishRejectsBrokenFences(test *testing.T) {
	for _, scenario := range []string{"future-source", "graph-reset", "memory-reset", "expired-lease", "witness-repair", "ownership-revoked", "disabled", "capture-expired", "target", "readonly"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			switch scenario {
			case "future-source":
				qualification.Source.Sequence++
			case "graph-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_graph_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(job.Target.RepoID))
			case "memory-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(job.Target.RepoID))
			case "expired-lease":
				memoryReadWorkExpirePG(test, storage, ctx, job)
			case "witness-repair":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
			case "ownership-revoked":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND hash=$2`, string(job.Target.RepoID), string(qualification.Witnesses[0].Hash))
			case "disabled":
				if err := storage.ConfigureMemoryReadWork(ctx, job.Target.RepoID, false); err != nil {
					test.Fatal(err)
				}
			case "capture-expired":
				memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			case "target":
				job.Target.Selection.CodeCommit = strings.Repeat("b", 40)
			case "readonly":
				ctx = outbound.WithDocReadOnly(ctx)
			}
			before := memoryReadWorkStatusPG(test, storage, ctx, job)
			if identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); err == nil || identifier != "" {
				test.Fatal("broken fence accepted", identifier, err)
			}
			if after := memoryReadWorkStatusPG(test, storage, ctx, job); after != before || memoryQualificationCountPG(test, storage, ctx, job.Target.RepoID) != 0 {
				test.Fatal("rejected finish mutated persistent state", before, after)
			}
		})
	}
}

func TestPGMemoryReadWorkMinimumUnavailableArrivalBeforeFinish(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	body := []byte("later arrival " + string(job.Target.RepoID))
	absent := domain.HashContent(body)
	header := output.Header
	header.Dependencies = append(header.Dependencies, domain.MemoryReadDependency{Hash: absent})
	slices.SortFunc(header.Dependencies, func(left, right domain.MemoryReadDependency) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	output = memoryReadPGSeal(test, ctx, header, nil)
	memoryReadPGWrite(test, storage, ctx, output)
	qualification.ReadID = output.ID
	memoryReadComparisonRecordPG(test, storage, ctx, job, output)
	qualification.Witnesses = append(qualification.Witnesses, domain.MemoryObjectWitness{RepoID: job.Target.RepoID, Hash: absent})
	slices.SortFunc(qualification.Witnesses, func(left, right domain.MemoryObjectWitness) int {
		return strings.Compare(string(left.Hash), string(right.Hash))
	})
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(absent), body)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(job.Target.RepoID), string(absent))
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("strict publication accepted late arrival", err)
	}
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); err != nil {
		test.Fatal("arrival invalidated prior complete result", err)
	}
}

func TestPGMemoryReadWorkMinimumFinalFenceRollsBackPublication(test *testing.T) {
	for _, scenario := range []string{"cancel", "lease", "capture"} {
		test.Run(scenario, func(test *testing.T) {
			storage, parent, job, qualification, _ := memoryReadWorkOutputPG(test)
			catalogExecPG(test, parent, storage.pool, `UPDATE snapshots SET message='later source' WHERE repo_id=$1`, string(job.Target.RepoID))
			ctx, cancel := context.WithCancel(parent)
			defer cancel()
			peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "UPDATE memory_read_work SET state='completed'")
			defer release()
			if scenario == "lease" {
				catalogExecPG(test, parent, storage.pool, `UPDATE memory_read_work SET lease_until=clock_timestamp()+interval '150 milliseconds'
 WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID))
			}
			if scenario == "capture" {
				memoryBundlePGFault(test, storage, parent, `UPDATE memory_capture_headers SET created_at=clock_timestamp()-interval '1 hour',
 expires_at=clock_timestamp()+interval '150 milliseconds' WHERE repo_id=$1 AND capture_id=$2`, string(job.Target.RepoID), string(qualification.CaptureID))
			}
			before := memoryReadWorkStatusPG(test, storage, parent, job)
			done := make(chan error, 1)
			go func() { _, err := peer.FinishMinimumMemoryReadWork(ctx, job, qualification); done <- err }()
			memoryQualificationWaitPG(test, parent, reached, done)
			wanted := context.Canceled
			switch scenario {
			case "cancel":
				cancel()
			case "lease":
				wanted = domain.ErrConflict
				var remaining float64
				if err := storage.pool.QueryRow(parent, `SELECT GREATEST(0,extract(epoch FROM lease_until-clock_timestamp()))
 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(job.Target.RepoID), string(job.ID)).Scan(&remaining); err != nil {
					test.Fatal(err)
				}
				time.Sleep(time.Duration(remaining*float64(time.Second)) + 5*time.Millisecond)
			case "capture":
				wanted = domain.ErrMemoryCaptureExpired
				memoryReadPGWaitExpiry(test, storage, parent, job.Target.RepoID, qualification.CaptureID)
			}
			release()
			if err := <-done; !errors.Is(err, wanted) {
				test.Fatal("final fence did not abort", err)
			}
			if after := memoryReadWorkStatusPG(test, storage, parent, job); after != before || memoryQualificationCountPG(test, storage, parent, job.Target.RepoID) != 0 {
				test.Fatal("aborted finish left a receipt or qualification", before, after)
			}
		})
	}
}
