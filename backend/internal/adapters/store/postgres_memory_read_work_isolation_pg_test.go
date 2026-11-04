//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memoryReadWorkOtherRepoPG(test *testing.T, storage *PostgresStore, ctx context.Context, target domain.MemoryReadTarget) domain.MemoryReadTarget {
	test.Helper()
	target.RepoID = memorySourceRepoPG(test, ctx, storage, "isolated claim")
	snapshot := initializationPGSnapshot(test, storage, target.RepoID, "isolated target")
	target.Selection.SnapshotID = snapshot.ID
	if err := storage.ConfigureMemoryReadWork(ctx, target.RepoID, true); err != nil {
		test.Fatal(err)
	}
	return target
}

func TestPGMemoryReadWorkClaimIsolatesBadCandidate(test *testing.T) {
	for _, scenario := range []string{"identity", "missing-source", "graph-reset", "corrupt-target", "corrupt-source", "source-locked", "scope-locked"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, badTarget := memoryReadWorkPG(test)
			bad := memoryReadWorkRequestPG(test, storage, ctx, badTarget)
			goodTarget := memoryReadWorkOtherRepoPG(test, storage, ctx, badTarget)
			good := memoryReadWorkRequestPG(test, storage, ctx, goodTarget)
			blocker := catalogTxPG(test, ctx, storage)
			defer rollbackPG(blocker)
			state, reason := "rejected", "invalid_source"
			switch scenario {
			case "identity":
				state, reason = "retrying", "identity_upgrade_required"
				catalogExecPG(test, ctx, storage.pool, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, string(badTarget.RepoID), domain.DocumentIdentityRootV1)
			case "missing-source":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repository_memory_source_state WHERE repo_id=$1`, string(badTarget.RepoID))
			case "graph-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_graph_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(badTarget.RepoID))
			case "corrupt-target":
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET target=convert_to('{}','UTF8') WHERE repo_id=$1`, string(badTarget.RepoID))
			case "corrupt-source":
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET source=convert_to('{}','UTF8') WHERE repo_id=$1`, string(badTarget.RepoID))
			case "source-locked":
				state, reason = "retrying", "temporary_failure"
				catalogExecPG(test, ctx, blocker, `SELECT 1 FROM repository_memory_source_state WHERE repo_id=$1 FOR UPDATE`, string(badTarget.RepoID))
			case "scope-locked":
				state, reason = "waiting", ""
				catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_work_scopes WHERE repo_id=$1 FOR UPDATE`, string(badTarget.RepoID))
			}
			claimed, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{badTarget.RepoID, goodTarget.RepoID}, time.Minute)
			if err != nil || claimed.ID != good.ID {
				test.Fatal("bad oldest candidate blocked unrelated work", scenario, claimed, err)
			}
			var actualState, actualReason string
			var attempts int
			var version int64
			if err := storage.pool.QueryRow(ctx, `SELECT state,reason,attempts,version FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`,
				string(badTarget.RepoID), string(bad.ID)).Scan(&actualState, &actualReason, &attempts, &version); err != nil {
				test.Fatal(err)
			}
			if actualState != state || actualReason != reason {
				test.Fatal("failure not durably sanitized", actualState, actualReason)
			}
			if scenario != "scope-locked" && (attempts != 1 || version <= bad.Version) {
				test.Fatal("failed candidate not fenced", attempts, version)
			}
			if err := blocker.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			if scenario == "identity" {
				for attempt := 2; attempt <= domain.MaxMemoryReadWorkAttempts; attempt++ {
					catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(badTarget.RepoID))
					if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{badTarget.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
						test.Fatal("identity failure escaped candidate isolation", err)
					}
				}
				status := memoryReadWorkStatusPG(test, storage, ctx, bad)
				if status.State != "rejected" || status.Reason != "attempts_exhausted" || status.Attempts != 6 {
					test.Fatal("identity failure retries unbounded", status)
				}
			}
		})
	}
}

func TestPGMemoryReadWorkClaimStatementTimeoutCommitsProgress(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	job := memoryReadWorkRequestPG(test, storage, ctx, target)
	blocker := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, blocker, `LOCK TABLE repository_memory_source_state IN ACCESS EXCLUSIVE MODE`)
	started := time.Now()
	if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("source timeout aborted queue progression", err)
	}
	status := memoryReadWorkStatusPG(test, storage, ctx, job)
	if status.State != "retrying" || status.Reason != "temporary_failure" || status.Attempts != 1 || status.Version <= job.Version || time.Since(started) > time.Second {
		test.Fatal("source timeout not durably deferred", status)
	}
	if err := blocker.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(target.RepoID))
	if claimed := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID); claimed.Attempts != 2 {
		test.Fatal("timeout retry lost attempt budget", claimed)
	}
}

func TestPGMemoryReadWorkRetentionCleanupPreservesProjectionBudget(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	memoryReadWorkRequestPG(test, storage, ctx, target)
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if err := storage.FailMemoryReadWork(ctx, job, "retention_cleanup", false); !errors.Is(err, domain.ErrValidation) {
		test.Fatal("terminal cleanup accepted", err)
	}
	for range 8 {
		if err := storage.FailMemoryReadWork(ctx, job, "retention_cleanup", true); err != nil {
			test.Fatal(err)
		}
		status := memoryReadWorkStatusPG(test, storage, ctx, job)
		if status.State != "retrying" || status.Reason != "retention_cleanup" || status.Attempts != 0 ||
			status.NextAttempt.Sub(status.UpdatedAt) < time.Second-time.Millisecond || status.NextAttempt.Sub(status.UpdatedAt) > time.Second+time.Millisecond {
			test.Fatal("cleanup consumed projection retry", status)
		}
		if err := storage.FailMemoryReadWork(ctx, job, "retention_cleanup", true); !errors.Is(err, domain.ErrConflict) {
			test.Fatal("duplicate cleanup returned attempts twice", err)
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(target.RepoID))
		prior := job
		job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
		if job.Attempts != 1 || job.Version <= prior.Version {
			test.Fatal("cleanup failed fence", job)
		}
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='advance during cleanup' WHERE repo_id=$1`, string(target.RepoID))
	if err := storage.FailMemoryReadWork(ctx, job, "retention_cleanup", true); err != nil {
		test.Fatal(err)
	}
	if status := memoryReadWorkStatusPG(test, storage, ctx, job); status.Reason != "source_changed" || status.Attempts != 1 {
		test.Fatal("cleanup masked source change", status)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(target.RepoID))
	job = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if job.Attempts != 1 {
		test.Fatal("source refresh did not reset cleanup attempts")
	}
	memoryReadWorkExpirePG(test, storage, ctx, job)
	if err := storage.FailMemoryReadWork(ctx, job, "retention_cleanup", true); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("expired cleanup refunded attempt", err)
	}
}
