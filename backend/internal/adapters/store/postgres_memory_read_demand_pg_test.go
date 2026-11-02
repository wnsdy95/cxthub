//go:build postgres

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func memoryReadDemandRequestPG(test *testing.T, storage *PostgresStore, ctx context.Context, target domain.MemoryReadTarget, requirement domain.MemoryReadRequirement) domain.MemoryReadWork {
	test.Helper()
	work, err := storage.RequestMinimumMemoryReadWork(ctx, target, requirement)
	if err != nil {
		test.Fatal(err)
	}
	return work
}

func memoryReadDemandRowsPG(test *testing.T, storage *PostgresStore, ctx context.Context, work domain.MemoryReadWork) string {
	test.Helper()
	var rows string
	if err := storage.pool.QueryRow(ctx, `SELECT row_to_json(job)::text || ':' || job.xmin::text || ':' ||
 row_to_json(wake)::text || ':' || wake.xmin::text FROM memory_read_work job JOIN memory_read_wakes wake USING(repo_id)
 WHERE job.repo_id=$1 AND job.target_id=$2`, string(work.Target.RepoID), string(work.ID)).Scan(&rows); err != nil {
		test.Fatal(err)
	}
	return rows
}

func TestPGMemoryReadDemandActiveSourceAdvanceNoChurn(test *testing.T) {
	for _, state := range []string{"waiting", "running", "retrying"} {
		test.Run(state, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
			work := memoryReadDemandRequestPG(test, storage, ctx, target, requirement)
			if state != "waiting" {
				work = memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
			}
			if state == "retrying" {
				if err := storage.FailMemoryReadWork(ctx, work, "temporary_failure", true); err != nil {
					test.Fatal(err)
				}
				catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET next_attempt=clock_timestamp()+interval '1 hour'
 WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(work.ID))
				work = memoryReadWorkStatusPG(test, storage, ctx, work)
			}
			for range 3 {
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message=message || ' later' WHERE repo_id=$1`, string(target.RepoID))
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET head_seq=head_seq+1 WHERE repo_id=$1`, string(target.RepoID))
				newer := memoryReadMinimumObservePG(test, storage, ctx, target)
				if newer.Minimum.Graph.Sequence <= work.Source.Graph.Sequence || newer.Minimum.Sequence <= work.Source.Sequence {
					test.Fatal("fixture failed to advance both source clocks")
				}
				before := memoryReadDemandRowsPG(test, storage, ctx, work)
				for _, floor := range []domain.MemoryReadRequirement{requirement, newer} {
					if actual := memoryReadDemandRequestPG(test, storage, ctx, target, floor); actual != work {
						test.Fatal("demand changed active source, fence, lease or backoff", work, actual)
					}
				}
				if after := memoryReadDemandRowsPG(test, storage, ctx, work); after != before {
					test.Fatal("coalescing rewrote work or consumed a later wake")
				}
				if source := memorySourceGenerationPG(test, ctx, storage, target.RepoID); source != newer.Minimum {
					test.Fatal("demand changed business source")
				}
			}
			if state != "waiting" {
				if _, err := storage.ClaimMemoryReadWork(ctx, []domain.ContentHash{target.RepoID}, time.Minute); !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("demand lost the live lease or retry backoff", err)
				}
			} else if claimed := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID); claimed.Source == work.Source || claimed.Version <= work.Version {
				test.Fatal("claim failed to refresh preserved waiting work", claimed)
			}
		})
	}
}

func TestPGMemoryReadDemandFinishBetweenMissAndEnqueue(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, "", memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); !errors.Is(err, domain.ErrMemoryReadNotReady) {
		test.Fatal("fixture must start with a reader miss", err)
	}
	peer, reached, release := memoryQualificationPausePG(test, ctx, storage, "SET LOCAL lock_timeout=")
	defer release()
	done := make(chan error, 1)
	var actual domain.MemoryReadWork
	go func() {
		var err error
		actual, err = peer.RequestMinimumMemoryReadWork(ctx, job.Target, requirement)
		done <- err
	}()
	memoryQualificationWaitPG(test, ctx, reached, done)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='finish after read miss' WHERE repo_id=$1`, string(job.Target.RepoID))
	identifier, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification)
	if err != nil {
		test.Fatal(err)
	}
	completed := memoryReadWorkStatusPG(test, storage, ctx, job)
	before := memoryReadDemandRowsPG(test, storage, ctx, job)
	release()
	if err := <-done; err != nil || actual != completed {
		test.Fatal("demand replaced a completed minimum result after reader miss", actual, err)
	}
	if after := memoryReadDemandRowsPG(test, storage, ctx, job); after != before {
		test.Fatal("completed demand rewrote work or acknowledged the later source")
	}
	if err := storage.WithMinimumMemoryReadRange(ctx, job.Target, requirement, identifier, memoryQualificationAllowPG, memoryReadMinimumNoConsumePG); err != nil {
		test.Fatal("retained completed minimum no longer serves", err)
	}
	newer := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	queued := memoryReadDemandRequestPG(test, storage, ctx, job.Target, newer)
	if queued.State != "waiting" || queued.Source != newer.Minimum || queued.Version <= completed.Version || queued.QualificationID != identifier {
		test.Fatal("newer floor did not retain the result and acquire a fresh fence", queued)
	}
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("old worker crossed requeue fence", err)
	}
}

func TestPGMemoryReadDemandPreservesRunningCompletionFence(test *testing.T) {
	storage, ctx, job, qualification, _ := memoryReadWorkOutputPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='source advanced while running' WHERE repo_id=$1`, string(job.Target.RepoID))
	newer := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
	for _, floor := range []domain.MemoryReadRequirement{requirement, newer} {
		if actual := memoryReadDemandRequestPG(test, storage, ctx, job.Target, floor); actual != job {
			test.Fatal("minimum demand invalidated running work", actual)
		}
	}
	if _, err := storage.FinishMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("strict completion accepted old source", err)
	}
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); err != nil {
		test.Fatal("minimum demand broke valid in-flight completion", err)
	}
	completed := memoryReadWorkStatusPG(test, storage, ctx, job)
	var pending bool
	if err := storage.pool.QueryRow(ctx, `SELECT wake.revision>job.wake_revision AND wake.next_attempt<=clock_timestamp()
 FROM memory_read_wakes wake JOIN memory_read_work job USING(repo_id) WHERE job.repo_id=$1 AND job.target_id=$2`,
		string(job.Target.RepoID), string(job.ID)).Scan(&pending); err != nil || !pending {
		test.Fatal("newer floor demand consumed the later wake", pending, err)
	}
	if actual := memoryReadDemandRequestPG(test, storage, ctx, job.Target, requirement); actual != completed {
		test.Fatal("valid minimum completion was reset", actual)
	}
	strict := memoryReadWorkRequestPG(test, storage, ctx, job.Target)
	if strict.State != "waiting" || strict.Source != newer.Minimum || strict.Version <= completed.Version {
		test.Fatal("legacy exact-current request semantics changed", strict)
	}
	if _, err := storage.FinishMinimumMemoryReadWork(ctx, job, qualification); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("stale worker crossed exact-current requeue fence", err)
	}
}

func TestPGMemoryReadDemandMinimumValidation(test *testing.T) {
	for _, scenario := range []string{"version", "target", "repo", "negative", "future-graph", "future-memory", "stale-graph", "stale-memory"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
			work := memoryReadDemandRequestPG(test, storage, ctx, target, requirement)
			expected := domain.ErrValidation
			switch scenario {
			case "version":
				requirement.Version++
			case "target":
				requirement.TargetID = domain.HashContent([]byte("another target"))
			case "repo":
				requirement.Minimum.Graph.RepoID = domain.HashContent([]byte("another repository"))
			case "negative":
				requirement.Minimum.Sequence = -1
			case "future-graph":
				requirement.Minimum.Graph.Sequence++
			case "future-memory":
				requirement.Minimum.Sequence++
			case "stale-graph":
				catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(target.RepoID))
				expected = domain.ErrCatalogResetRequired
			case "stale-memory":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch=gen_random_uuid() WHERE repo_id=$1`, string(target.RepoID))
				expected = domain.ErrCatalogResetRequired
			}
			before := memoryReadDemandRowsPG(test, storage, ctx, work)
			if actual, err := storage.RequestMinimumMemoryReadWork(ctx, target, requirement); !errors.Is(err, expected) || actual != (domain.MemoryReadWork{}) {
				test.Fatal("invalid requirement accepted", actual, err, expected)
			}
			if after := memoryReadDemandRowsPG(test, storage, ctx, work); after != before {
				test.Fatal("invalid requirement changed work or wakes")
			}
			if expected == domain.ErrCatalogResetRequired {
				fresh := memoryReadMinimumObservePG(test, storage, ctx, target)
				queued := memoryReadDemandRequestPG(test, storage, ctx, target, fresh)
				if queued.Source != fresh.Minimum || queued.Version <= work.Version {
					test.Fatal("new epoch retained old-epoch work", queued)
				}
			}
		})
	}
}

func TestPGMemoryReadDemandScopeSnapshotAndBorrowedTransactions(test *testing.T) {
	for _, scenario := range []string{"disabled", "missing-scope", "foreign-snapshot", "missing-snapshot", "readonly", "read-transaction", "write-transaction", "identity"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
			expected := domain.ErrConflict
			switch scenario {
			case "disabled":
				if err := storage.ConfigureMemoryReadWork(ctx, target.RepoID, false); err != nil {
					test.Fatal(err)
				}
			case "missing-scope":
				catalogExecPG(test, ctx, storage.pool, `DELETE FROM memory_read_work_scopes WHERE repo_id=$1`, string(target.RepoID))
			case "foreign-snapshot":
				foreign := memoryReadWorkOtherRepoPG(test, storage, ctx, target)
				target.Selection = foreign.Selection
				expected = domain.ErrNotFound
			case "missing-snapshot":
				target.Selection.SnapshotID = domain.HashContent([]byte("missing snapshot"))
				expected = domain.ErrNotFound
			case "identity":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repos SET required_doc_identity=$2 WHERE id=$1`, string(target.RepoID), domain.DocumentIdentityRootV1)
				expected = domain.ErrDocumentIdentityUpgradeRequired
			}
			var err error
			requirement.TargetID, err = domain.MemoryReadTargetID(ctx, target)
			if err != nil {
				test.Fatal(err)
			}
			check := func(bound context.Context) error {
				for _, candidate := range []*PostgresStore{storage, {pool: storage.pool}} {
					if _, err := candidate.RequestMinimumMemoryReadWork(bound, target, requirement); !errors.Is(err, expected) {
						test.Fatal("scope or ownership boundary bypassed", err, expected)
					}
				}
				return nil
			}
			switch scenario {
			case "readonly":
				err = check(outbound.WithDocReadOnly(ctx))
			case "read-transaction":
				err = storage.WithinReadSnapshot(ctx, check)
			case "write-transaction":
				err = storage.WithinRepository(ctx, target.RepoID, check)
			default:
				err = check(ctx)
			}
			if err != nil {
				test.Fatal(err)
			}
			var count int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, string(target.RepoID)).Scan(&count); err != nil || count != 0 {
				test.Fatal("rejected demand registered work", count, err)
			}
		})
	}
}

func TestPGMemoryReadDemandDuplicatesColdSourceAndQuota(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='before cold demand' WHERE repo_id=$1`, string(target.RepoID))
	source := memorySourceGenerationPG(test, ctx, storage, target.RepoID)
	type result struct {
		work domain.MemoryReadWork
		err  error
	}
	results := make(chan result, 8)
	for range cap(results) {
		go func() {
			work, err := storage.RequestMinimumMemoryReadWork(ctx, target, requirement)
			results <- result{work, err}
		}()
	}
	var initial domain.MemoryReadWork
	for range cap(results) {
		actual := <-results
		if actual.err != nil {
			test.Fatal(actual.err)
		}
		if initial.ID == "" {
			initial = actual.work
		}
		if actual.work != initial || actual.work.Source != source || actual.work.State != "waiting" {
			test.Fatal("duplicate cold demand changed intent or missed current source", actual)
		}
	}
	for ordinal := 2; ordinal <= domain.MaxPendingMemoryReadWork; ordinal++ {
		next := target
		next.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal)
		floor := memoryReadMinimumObservePG(test, storage, ctx, next)
		memoryReadDemandRequestPG(test, storage, ctx, next, floor)
	}
	next := target
	next.Content = "claims"
	floor := memoryReadMinimumObservePG(test, storage, ctx, next)
	if _, err := storage.RequestMinimumMemoryReadWork(ctx, next, floor); !errors.Is(err, domain.ErrResourceOverloaded) {
		test.Fatal("pending quota bypassed", err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='at pending quota' WHERE repo_id=$1`, string(target.RepoID))
	newer := memoryReadMinimumObservePG(test, storage, ctx, target)
	before := memoryReadDemandRowsPG(test, storage, ctx, initial)
	if actual := memoryReadDemandRequestPG(test, storage, ctx, target, newer); actual != initial {
		test.Fatal("at-cap coalescing reset an active target", actual)
	}
	if after := memoryReadDemandRowsPG(test, storage, ctx, initial); after != before {
		test.Fatal("at-cap demand rewrote work or wakes")
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, string(target.RepoID)).Scan(&count); err != nil || count != domain.MaxPendingMemoryReadWork {
		test.Fatal("duplicates or rejected demand exceeded pending quota", count, err)
	}
}

func TestPGMemoryReadDemandRetainedQuota(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
	active := memoryReadDemandRequestPG(test, storage, ctx, target, requirement)
	var oldest domain.MemoryReadWork
	for ordinal := 2; ordinal <= domain.MaxMemoryReadTargets+1; ordinal++ {
		next := target
		next.Selection.CodeCommit = fmt.Sprintf("%040x", ordinal)
		floor := memoryReadMinimumObservePG(test, storage, ctx, next)
		work := memoryReadDemandRequestPG(test, storage, ctx, next, floor)
		if oldest.ID == "" {
			oldest = work
		}
		catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_work SET state='rejected',reason='invalid_source' WHERE repo_id=$1 AND target_id=$2`, string(target.RepoID), string(work.ID))
	}
	if _, err := storage.ReadMemoryReadWork(ctx, target.RepoID, oldest.ID); !errors.Is(err, domain.ErrNotFound) {
		test.Fatal("retained quota failed to evict the oldest terminal work", err)
	}
	if actual := memoryReadWorkStatusPG(test, storage, ctx, active); actual != active {
		test.Fatal("retention evicted or changed active work", actual)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_work WHERE repo_id=$1`, string(target.RepoID)).Scan(&count); err != nil || count != domain.MaxMemoryReadTargets {
		test.Fatal("retained target quota bypassed", count, err)
	}
}

func TestPGMemoryReadDemandRejectedSameSource(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	requirement := memoryReadMinimumObservePG(test, storage, ctx, target)
	memoryReadDemandRequestPG(test, storage, ctx, target, requirement)
	job := memoryReadWorkClaimPG(test, storage, ctx, target.RepoID)
	if err := storage.FailMemoryReadWork(ctx, job, "invalid_source", false); err != nil {
		test.Fatal(err)
	}
	rejected := memoryReadWorkStatusPG(test, storage, ctx, job)
	before := memoryReadDemandRowsPG(test, storage, ctx, job)
	if _, err := storage.RequestMinimumMemoryReadWork(ctx, target, requirement); !errors.Is(err, domain.ErrMemoryReadNeedsAttention) {
		test.Fatal("same-source rejection not surfaced", err)
	}
	if actual := memoryReadWorkRequestPG(test, storage, ctx, target); actual != rejected {
		test.Fatal("legacy rejected-work behavior changed", actual)
	}
	if after := memoryReadDemandRowsPG(test, storage, ctx, job); after != before {
		test.Fatal("same-source rejection was rewritten")
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='rejected source changed' WHERE repo_id=$1`, string(target.RepoID))
	queued := memoryReadDemandRequestPG(test, storage, ctx, target, requirement)
	if queued.State != "waiting" || queued.Attempts != 0 || queued.Source == job.Source || queued.Version <= job.Version {
		test.Fatal("changed source did not recover rejected work", queued)
	}
}

func TestPGMemoryReadDemandCompletedValidation(test *testing.T) {
	for _, scenario := range []string{"matched", "missing-comparison", "mismatch", "corrupt-comparison", "expiry", "witness"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, job, qualification, output := memoryReadWorkOutputWithoutComparisonPG(test)
			requirement := memoryReadMinimumObservePG(test, storage, ctx, job.Target)
			var expected error
			if scenario != "missing-comparison" {
				comparison := memoryReadComparisonPG(test, ctx, job, output)
				if scenario == "mismatch" {
					comparison.Expected = domain.HashContent([]byte("different reference"))
					expected = domain.ErrMemoryReadNeedsAttention
				}
				if err := storage.RecordMemoryReadComparison(ctx, job, comparison); err != nil {
					test.Fatal(err)
				}
			}
			identifier := memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
			completed := memoryReadWorkStatusPG(test, storage, ctx, job)
			switch scenario {
			case "corrupt-comparison":
				expected = domain.ErrIntegrity
				memoryBundlePGFault(test, storage, ctx, `UPDATE memory_read_comparisons SET body=convert_to('{}','UTF8') WHERE repo_id=$1 AND read_id=$2`, string(job.Target.RepoID), string(output.ID))
			case "expiry":
				memoryBundlePGExpire(test, storage, ctx, job.Target.RepoID, qualification.CaptureID)
			case "witness":
				catalogExecPG(test, ctx, storage.pool, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(qualification.Witnesses[0].Hash))
			}
			catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='later than completion' WHERE repo_id=$1`, string(job.Target.RepoID))
			before := memoryReadDemandRowsPG(test, storage, ctx, job)
			actual, err := storage.RequestMinimumMemoryReadWork(ctx, job.Target, requirement)
			if !errors.Is(err, expected) {
				test.Fatal("completed demand ignored qualification validity", err, expected)
			}
			if expected != nil || scenario == "matched" {
				if after := memoryReadDemandRowsPG(test, storage, ctx, job); after != before {
					test.Fatal("valid or invalid completed receipt was rewritten")
				}
				if expected == nil && actual != completed {
					test.Fatal("valid completed minimum was not reused", actual)
				}
			} else if actual.State != "waiting" || actual.Version <= job.Version || actual.QualificationID != identifier || actual.Source == job.Source {
				test.Fatal("unavailable completed result was not requeued with retained pointer", actual)
			}
		})
	}
}
