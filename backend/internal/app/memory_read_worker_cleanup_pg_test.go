//go:build postgres

package app

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryReadCleanupProbePG struct {
	*store.PostgresStore
	remainingBatches int
	pruneCalls       int
	graphReads       int
}

func (probe *memoryReadCleanupProbePG) PruneMemoryCaptures(ctx context.Context, repo domain.ContentHash, limit int) (int, error) {
	probe.pruneCalls++
	if limit != 32 {
		return 0, domain.ErrValidation
	}
	if probe.remainingBatches > 0 {
		probe.remainingBatches--
		return 32, nil
	}
	return probe.PostgresStore.PruneMemoryCaptures(ctx, repo, limit)
}

func (probe *memoryReadCleanupProbePG) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	probe.graphReads++
	return probe.PostgresStore.RepositoryGraphSourceChanges(ctx, repo, request)
}

func TestPGMemoryReadWorkerCleanupProgressPreservesFailureBudget(test *testing.T) {
	service, storage, connection, repo, request, _ := qualifiedMemoryFixturePG(test)
	ctx := systemTestContext()
	memoryWorkConfigurePG(test, service, storage, repo)
	queued, err := service.RequestMemoryReadPreparation(ctx, repo, request)
	if err != nil {
		test.Fatal("enqueue", err)
	}
	if _, err := storage.ReconcileMemoryReadWork(ctx, []domain.ContentHash{repo}, 1); err != nil {
		test.Fatal("consume initial activation", err)
	}
	priorFailures := domain.MaxMemoryReadWorkAttempts - 1
	updated, err := connection.Exec(ctx, `UPDATE memory_read_work
 SET state='retrying',reason='temporary_failure',attempts=$3,next_attempt=clock_timestamp()
 WHERE repo_id=$1 AND target_id=$2 AND state='waiting'`, string(repo), string(queued.ID), priorFailures)
	if err != nil || updated.RowsAffected() != 1 {
		test.Fatal("seed prior preparation failures", updated.RowsAffected(), err)
	}
	const cleanupRounds = 7
	probe := &memoryReadCleanupProbePG{PostgresStore: storage, remainingBatches: cleanupRounds}
	service.meta = probe
	previousFence := queued.Version
	for round := 0; round < cleanupRounds; round++ {
		if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
			test.Fatalf("cleanup round %d reported successful progress as failure: %v", round+1, err)
		}
		status, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
		if err != nil {
			test.Fatal("read cleanup status", err)
		}
		if status.State != "retrying" || status.Reason != "retention_cleanup" || status.Attempts != priorFailures {
			test.Fatalf("cleanup round %d consumed or reset the preparation failure budget: %+v", round+1, status)
		}
		if status.ID != queued.ID || status.Source != queued.Source || status.Version <= previousFence ||
			status.QualificationID != "" || !status.LeaseUntil.IsZero() {
			test.Fatalf("cleanup round %d changed source/output or failed to release a fresh lease: %+v", round+1, status)
		}
		delay := status.NextAttempt.Sub(status.UpdatedAt)
		if delay < time.Second || delay >= 2*time.Second {
			test.Fatalf("cleanup round %d used preparation-failure backoff: %v", round+1, delay)
		}
		if probe.graphReads != 0 || probe.pruneCalls != round+1 || probe.remainingBatches != cleanupRounds-round-1 {
			test.Fatalf("cleanup round %d started preparation or exceeded one bounded cleanup batch: reads=%d prunes=%d remaining=%d",
				round+1, probe.graphReads, probe.pruneCalls, probe.remainingBatches)
		}
		previousFence = status.Version
		updated, err := connection.Exec(ctx, `UPDATE memory_read_work
 SET next_attempt=clock_timestamp()-interval '1 second'
 WHERE repo_id=$1 AND target_id=$2 AND version=$3 AND state='retrying'`, string(repo), string(queued.ID), status.Version)
		if err != nil || updated.RowsAffected() != 1 {
			test.Fatal("advance cleanup scheduler", updated.RowsAffected(), err)
		}
	}
	if err := service.ProcessMemoryReadPreparations(ctx, 1); err != nil {
		test.Fatal("prepare after cleanup drained", err)
	}
	completed, err := service.ReadMemoryPreparationStatus(ctx, repo, request)
	if err != nil {
		test.Fatal("read completion", err)
	}
	if completed.State != "completed" || completed.Reason != "" || completed.QualificationID == "" ||
		completed.Attempts != priorFailures+1 || completed.Version <= previousFence ||
		completed.Source != queued.Source || !completed.LeaseUntil.IsZero() {
		test.Fatalf("cleanup failed to preserve the final real preparation attempt: %+v", completed)
	}
	if probe.pruneCalls != cleanupRounds+1 || probe.graphReads == 0 {
		test.Fatalf("final cycle skipped real cleanup/preparation: prunes=%d graph reads=%d", probe.pruneCalls, probe.graphReads)
	}
	want, err := service.QueryEffectiveMemory(ctx, repo, request)
	if err != nil {
		test.Fatal("existing memory query", err)
	}
	got, err := service.QueryCurrentMemory(ctx, repo, request)
	if err != nil || !reflect.DeepEqual(got, want) || len(got.Items) == 0 {
		test.Fatalf("cleanup did not produce the real current page: %v\ngot=%+v\nwant=%+v", err, got, want)
	}
	source, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if err != nil || source != queued.Source {
		test.Fatal("cleanup recovery required or caused a business-source change", source, err)
	}
}
