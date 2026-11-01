//go:build postgres

package store

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func TestPGMemoryReadWakeBatchBoundsAndOwnership(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	for _, limit := range []int{-1, 0, 33} {
		if count, err := storage.ReconcileMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, limit); !errors.Is(err, domain.ErrValidation) || count != 0 {
			test.Fatal("invalid repository batch accepted", limit, count, err)
		}
	}
	for _, scenario := range []struct {
		repos []domain.ContentHash
		err   error
	}{{[]domain.ContentHash{"invalid"}, domain.ErrIntegrity}, {make([]domain.ContentHash, domain.MaxMemoryReadWorkerRepos+1), domain.ErrValidation}} {
		if count, err := storage.ReconcileMemoryReadWork(ctx, scenario.repos, 1); !errors.Is(err, scenario.err) || count != 0 {
			test.Fatal("invalid local repository configuration accepted", count, err)
		}
	}
	check := func(bound context.Context) error {
		for _, candidate := range []*PostgresStore{storage, {pool: storage.pool}} {
			if count, err := candidate.ReconcileMemoryReadWork(bound, []domain.ContentHash{target.RepoID}, 1); !errors.Is(err, domain.ErrConflict) || count != 0 {
				test.Fatal("borrowed or read-only context reconciled work", count, err)
			}
		}
		return nil
	}
	if err := storage.WithinReadSnapshot(ctx, check); err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, target.RepoID, check); err != nil {
		test.Fatal(err)
	}
	if err := check(outbound.WithDocReadOnly(ctx)); err != nil {
		test.Fatal(err)
	}
	jobs := []domain.MemoryReadWork{memoryReadWakeRejectPG(test, storage, ctx, target)}
	for range 2 {
		jobs = append(jobs, memoryReadWakeRejectPG(test, storage, ctx, memoryReadWorkOtherRepoPG(test, storage, ctx, target)))
	}
	var repos []domain.ContentHash
	for _, job := range jobs {
		repos = append(repos, job.Target.RepoID)
	}
	memoryReadWakeReconcilePG(test, storage, ctx, repos, 32)
	for _, repo := range repos {
		catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='bounded repo batch' WHERE repo_id=$1`, string(repo))
	}
	for expected := 1; expected <= len(jobs); expected++ {
		if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 1 {
			test.Fatal("repository batch failed to make bounded progress", count)
		}
		queued := 0
		for _, job := range jobs {
			if memoryReadWorkStatusPG(test, storage, ctx, job).State == "waiting" {
				queued++
			}
		}
		if queued != expected {
			test.Fatal("limit=1 reconciled more than one repository notice", queued, expected)
		}
	}
}

func TestPGMemoryReadWakeFailureBackoffSurvivesNoops(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	memoryReadWakeRejectPG(test, storage, ctx, target)
	catalogExecPG(test, ctx, storage.pool, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, string(target.RepoID), domain.DocumentIdentityRootV1)
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 1 {
		test.Fatal("incompatible identity did not consume a notice attempt", count)
	}
	before := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	if before.attempts != 1 || before.reason != "identity_upgrade_required" || before.nextAttempt == nil {
		test.Fatal("notice failure not durably backed off", before)
	}
	for range 3 {
		catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message=message WHERE repo_id=$1`, string(target.RepoID))
		if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 0 {
			test.Fatal("no-op bypassed notice backoff", count)
		}
		after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
		if after.attempts != before.attempts || after.reason != before.reason || after.revision != before.revision || after.nextAttempt == nil || !after.nextAttempt.Equal(*before.nextAttempt) {
			test.Fatal("no-op reset notice failure budget", before, after)
		}
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='new source after failed notice' WHERE repo_id=$1`, string(target.RepoID))
	if after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); after.attempts != 0 || after.reason != "" || after.revision <= before.revision || after.nextAttempt == nil {
		test.Fatal("new source did not renew notice attempt budget", before, after)
	}
}

func TestPGMemoryReadWakeParallelReconciliationConsumesNoticeOnce(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	job := memoryReadWakeRejectPG(test, storage, ctx, target)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='one notice many reconcilers' WHERE repo_id=$1`, string(target.RepoID))
	type result struct {
		count int
		err   error
	}
	results := make(chan result, 8)
	for range 8 {
		go func() {
			count, err := storage.ReconcileMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, 1)
			results <- result{count: count, err: err}
		}()
	}
	processed := 0
	for range 8 {
		actual := <-results
		if actual.err != nil || actual.count < 0 || actual.count > 1 {
			test.Fatal("concurrent reconcile failed", actual)
		}
		processed += actual.count
	}
	if processed != 1 {
		test.Fatal("one durable notice was reconciled more than once", processed)
	}
	if queued := memoryReadWorkStatusPG(test, storage, ctx, job); queued.State != "waiting" || queued.Attempts != 0 {
		test.Fatal("concurrent acknowledgement lost target", queued)
	}
}

func TestPGMemoryReadWakeCancellationRollsBackTargetsAndAcknowledgement(test *testing.T) {
	storage, parent, target := memoryReadWorkPG(test)
	job := memoryReadWakeRejectPG(test, storage, parent, target)
	catalogExecPG(test, parent, storage.pool, `UPDATE snapshots SET message='cancel wake acknowledgement' WHERE repo_id=$1`, string(target.RepoID))
	before := memoryReadWakeReadPG(test, storage, parent, target.RepoID)
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "UPDATE memory_read_wakes SET next_attempt=$3")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := peer.ReconcileMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, 1)
		done <- err
	}()
	memoryQualificationWaitPG(test, parent, reached, done)
	cancel()
	release()
	if err := <-done; !errors.Is(err, context.Canceled) {
		test.Fatal("canceled acknowledgement committed", err)
	}
	if actual := memoryReadWorkStatusPG(test, storage, parent, job); actual != job {
		test.Fatal("canceled reconciliation left a partial target update", job, actual)
	}
	after := memoryReadWakeReadPG(test, storage, parent, target.RepoID)
	if after.revision != before.revision || after.nextAttempt == nil || !after.nextAttempt.Equal(*before.nextAttempt) || after.attempts != before.attempts {
		test.Fatal("canceled reconciliation lost its notice", before, after)
	}
	if count := memoryReadWakeReconcilePG(test, storage, parent, []domain.ContentHash{target.RepoID}, 1); count != 1 {
		test.Fatal("rolled back notice was not retryable", count)
	}
}

func TestPGMemoryReadWakeBadSourceDoesNotBlockAnotherRepository(test *testing.T) {
	for _, scenario := range []string{"missing-source", "invalid-source", "identity", "source-locked", "scope-locked", "wake-locked", "work-locked"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, badTarget := memoryReadWorkPG(test)
			bad := memoryReadWakeRejectPG(test, storage, ctx, badTarget)
			goodTarget := memoryReadWorkOtherRepoPG(test, storage, ctx, badTarget)
			good := memoryReadWakeRejectPG(test, storage, ctx, goodTarget)
			repos := []domain.ContentHash{badTarget.RepoID, goodTarget.RepoID}
			memoryReadWakeReconcilePG(test, storage, ctx, repos, 32)
			for _, repo := range repos {
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='isolate reconciliation' WHERE repo_id=$1`, string(repo))
			}
			blocker := catalogTxPG(test, ctx, storage)
			switch scenario {
			case "missing-source":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM repository_memory_source_state WHERE repo_id=$1`, string(badTarget.RepoID))
			case "invalid-source":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch='00000000-0000-0000-0000-000000000000' WHERE repo_id=$1`, string(badTarget.RepoID))
			case "identity":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, string(badTarget.RepoID), domain.DocumentIdentityRootV1)
			case "source-locked":
				catalogExecPG(test, ctx, blocker, `SELECT 1 FROM repository_memory_source_state WHERE repo_id=$1 FOR UPDATE`, string(badTarget.RepoID))
			case "scope-locked":
				catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_work_scopes WHERE repo_id=$1 FOR UPDATE`, string(badTarget.RepoID))
			case "wake-locked":
				catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_wakes WHERE repo_id=$1 FOR UPDATE`, string(badTarget.RepoID))
			case "work-locked":
				catalogExecPG(test, ctx, blocker, `SELECT 1 FROM memory_read_work WHERE repo_id=$1 AND target_id=$2 FOR UPDATE`, string(badTarget.RepoID), string(bad.ID))
			}
			memoryReadWakeReconcilePG(test, storage, ctx, repos, 32)
			if actual := memoryReadWorkStatusPG(test, storage, ctx, good); actual.State != "waiting" || actual.Attempts != 0 || actual.Version <= good.Version {
				test.Fatal("bad repository prevented healthy notice progression", actual)
			}
			if actual := memoryReadWorkStatusPG(test, storage, ctx, bad); actual != bad {
				test.Fatal("failed source check changed target state", bad, actual)
			}
			if scenario == "work-locked" {
				for attempt := 2; attempt <= domain.MaxMemoryReadWorkAttempts+1; attempt++ {
					priorGood := memoryReadWorkStatusPG(test, storage, ctx, good)
					memoryReadWakeDuePG(test, storage, ctx, badTarget.RepoID)
					catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message=message || ' retry' WHERE repo_id=$1`, string(goodTarget.RepoID))
					memoryReadWakeReconcilePG(test, storage, ctx, repos, 32)
					wake := memoryReadWakeReadPG(test, storage, ctx, badTarget.RepoID)
					if wake.attempts != min(attempt, domain.MaxMemoryReadWorkAttempts) || wake.nextAttempt == nil || !wake.nextAttempt.After(time.Now()) {
						test.Fatal("repeated target lock permanently stopped its notice", attempt, wake)
					}
					if actual := memoryReadWorkStatusPG(test, storage, ctx, good); actual.State != "waiting" || actual.Version <= priorGood.Version {
						test.Fatal("repeated target lock blocked unrelated repository progress", attempt, priorGood, actual)
					}
					if actual := memoryReadWorkStatusPG(test, storage, ctx, bad); actual != bad {
						test.Fatal("target lock failure leaked partial work changes", attempt, bad, actual)
					}
				}
			}
			if err := blocker.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			wake := memoryReadWakeReadPG(test, storage, ctx, badTarget.RepoID)
			if scenario != "scope-locked" && scenario != "wake-locked" && (wake.attempts == 0 || wake.reason == "" || !domain.MemoryReadWorkReasonValid(wake.reason)) {
				test.Fatal("bad source failure did not retain a sanitized notice outcome", wake)
			}
			if scenario == "work-locked" {
				if wake.attempts != domain.MaxMemoryReadWorkAttempts || wake.reason != "temporary_failure" || wake.nextAttempt == nil || !wake.nextAttempt.After(time.Now()) {
					test.Fatal("locked target did not retain bounded retry backoff", wake)
				}
				memoryReadWakeDuePG(test, storage, ctx, badTarget.RepoID)
				if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{badTarget.RepoID}, 1); count != 1 {
					test.Fatal("unlocked target notice was lost", count)
				}
				if actual := memoryReadWorkStatusPG(test, storage, ctx, bad); actual.State != "waiting" || actual.Attempts != 0 || actual.Version <= bad.Version {
					test.Fatal("target phase did not recover after lock release", actual)
				}
			}
		})
	}
}

func TestPGMemoryReadWakeDefersCanonicalTargetValidationToClaim(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	job := memoryReadWakeRejectPG(test, storage, ctx, target)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET target=convert_to('{}','UTF8') WHERE repo_id=$1`, string(target.RepoID))
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='do not hydrate target during wake' WHERE repo_id=$1`, string(target.RepoID))
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 1 {
		test.Fatal("malformed target blocked metadata-only reconciliation", count)
	}
	var state, reason string
	if err := storage.pool.QueryRow(ctx, `SELECT state,reason FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(job.ID)).Scan(&state, &reason); err != nil || state != "waiting" {
		test.Fatal("reconciler unexpectedly decoded canonical intent", state, reason, err)
	}
	if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("claim admitted malformed target", err)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT state,reason FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(job.ID)).Scan(&state, &reason); err != nil || state != "rejected" || reason != "invalid_source" {
		test.Fatal("claim did not durably reject malformed target", state, reason, err)
	}
}

type memoryReadWakeCostTracerPG struct {
	memoryReadRangeTracer
	hydrated atomic.Bool
	targets  atomic.Int64
}

func (tracer *memoryReadWakeCostTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	for _, forbidden := range []string{"memory_capture_parts", "memory_capture_blocks", "memory_read_chunks", "from snapshots", "from history_events", "body.bytes", "select bytes"} {
		if strings.Contains(query, forbidden) {
			tracer.hydrated.Store(true)
		}
	}
	if strings.Contains(query, "from memory_read_work ") {
		tracer.targets.Add(1)
	}
	return ctx
}

func TestPGMemoryReadWakeReconciliationDoesNotHydrateOrPollQuietTargets(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	tracer := &memoryReadWakeCostTracerPG{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = tracer
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	repos := []domain.ContentHash{job.Target.RepoID}
	memoryReadWakeReconcilePG(test, peer, ctx, repos, 1)
	if tracer.hydrated.Load() {
		test.Fatal("reconciliation hydrated raw context or materialized capture")
	}
	before := tracer.targets.Load()
	for range 4 {
		if count := memoryReadWakeReconcilePG(test, peer, ctx, repos, 1); count != 0 {
			test.Fatal("quiet repository was reconciled without a due notice", count)
		}
	}
	if tracer.targets.Load() != before {
		test.Fatal("idle reconciliation polled retained target history", before, tracer.targets.Load())
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='metadata-only wake refresh' WHERE repo_id=$1`, string(job.Target.RepoID))
	memoryReadWakeReconcilePG(test, peer, ctx, repos, 1)
	if tracer.hydrated.Load() || memoryReadWorkStatusPG(test, storage, ctx, job).State != "waiting" {
		test.Fatal("source refresh hydrated inputs or failed to queue")
	}
}

func TestPGMemoryReadWakeSourceChangeOverlapsAcknowledgement(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	job := memoryReadWakeRejectPG(test, storage, ctx, target)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='first wake revision' WHERE repo_id=$1`, string(target.RepoID))
	before := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "SELECT epoch::text,head_seq FROM repository_memory_source_state")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := peer.ReconcileMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, 1)
		done <- err
	}()
	memoryQualificationWaitPG(test, ctx, reached, done)
	writer := catalogTxPG(test, ctx, storage)
	written := make(chan error, 1)
	go func() {
		_, err := writer.Exec(ctx, `UPDATE snapshots SET message='second wake revision' WHERE repo_id=$1`, string(target.RepoID))
		written <- err
	}()
	memoryQualificationBlockedPG(test, ctx, storage, writer.Conn().PgConn().PID(), written)
	release()
	if err := <-done; err != nil {
		test.Fatal("reconciliation failed while source writer waited", err)
	}
	if err := <-written; err != nil {
		test.Fatal(err)
	}
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	if after.revision <= before.revision || after.nextAttempt == nil {
		test.Fatal("acknowledgement erased the overlapping source notice", before, after)
	}
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{target.RepoID}, 1); count != 1 {
		test.Fatal("overlapping source change was not retained", count)
	}
	claimed := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if claimed.ID != job.ID || claimed.Source != memorySourceGenerationPG(test, ctx, storage, target.RepoID) || claimed.Attempts != 1 {
		test.Fatal("overlapping source change did not reach the next claim", claimed)
	}
}

func TestPGMemoryReadWakePhysicalChangeAtFinalAttemptIsNotLost(test *testing.T) {
	for _, retry := range []bool{false, true} {
		test.Run(map[bool]string{false: "terminal", true: "exhausted"}[retry], func(test *testing.T) {
			storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
			repo := job.Target.RepoID
			memoryReadWakeDispatchPG(test, storage, ctx)
			if err := storage.FailMemoryReadWork(ctx, job, "temporary_failure", true); err != nil {
				test.Fatal(err)
			}
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
			job = memoryReadWorkClaimPG(test, storage, ctx, repo)
			catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET attempts=$2 WHERE repo_id=$1`, string(repo), domain.MaxMemoryReadWorkAttempts)
			job = memoryReadWorkStatusPG(test, storage, ctx, job)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
			var consumed int64
			if err := storage.pool.QueryRow(ctx, `SELECT wake_revision FROM memory_read_work WHERE repo_id=$1`, string(repo)).Scan(&consumed); err != nil {
				test.Fatal(err)
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
			memoryReadWakeDispatchPG(test, storage, ctx)
			memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
			var after int64
			if err := storage.pool.QueryRow(ctx, `SELECT wake_revision FROM memory_read_work WHERE repo_id=$1`, string(repo)).Scan(&after); err != nil || after != consumed {
				test.Fatal("running target consumed an unproven object notice", consumed, after, err)
			}
			if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != job {
				test.Fatal("object wake preempted final running attempt", job, actual)
			}
			if err := storage.FailMemoryReadWork(ctx, job, "temporary_failure", retry); err != nil {
				test.Fatal(err)
			}
			if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
				test.Fatal("terminal transition failed to reschedule unconsumed notice", count)
			}
			queued := memoryReadWorkStatusPG(test, storage, ctx, job)
			if queued.State != "waiting" || queued.Attempts != 0 || queued.Source != job.Source || queued.Version <= job.Version {
				test.Fatal("final-attempt object wake was lost", job, queued)
			}
			if claimed := memoryReadWorkClaimPG(test, storage, ctx, repo); claimed.Attempts != 1 || claimed.Source != job.Source {
				test.Fatal("physical-only refresh did not renew failure budget", claimed)
			}
		})
	}
}

func TestPGMemoryReadWakeSuccessfulFinishConsumesProvenLatestNotice(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	repo := job.Target.RepoID
	catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
	memoryReadWakeDispatchPG(test, storage, ctx)
	object := memoryWitnessPGCapture(test, storage, ctx, repo, qualification.Witnesses[0].Hash)
	qualification.Witnesses[0] = object.Witness
	latest := memoryReadWakeReadPG(test, storage, ctx, repo)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	var consumed int64
	if err := storage.pool.QueryRow(ctx, `SELECT wake_revision FROM memory_read_work WHERE repo_id=$1 AND target_id=$2`, string(repo), string(job.ID)).Scan(&consumed); err != nil || consumed != latest.revision {
		test.Fatal("successful finish did not atomically consume proven latest notice", consumed, latest, err)
	}
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
	if completed := memoryReadWorkStatusPG(test, storage, ctx, job); completed.State != "completed" || completed.Version != job.Version {
		test.Fatal("already-proven physical change requeued completed work", completed)
	}
}
