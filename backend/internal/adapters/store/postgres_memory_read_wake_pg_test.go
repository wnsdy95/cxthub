//go:build postgres

package store

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryReadWakeStatePG struct {
	revision       int64
	objectRevision int64
	nextAttempt    *time.Time
	attempts       int
	reason         string
}

func memoryReadWakeReadPG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) memoryReadWakeStatePG {
	test.Helper()
	var wake memoryReadWakeStatePG
	if err := storage.pool.QueryRow(ctx, `SELECT revision,object_revision,next_attempt,attempts,reason
 FROM memory_read_wakes WHERE repo_id=$1`, string(repo)).Scan(&wake.revision, &wake.objectRevision, &wake.nextAttempt, &wake.attempts, &wake.reason); err != nil {
		test.Fatal(err)
	}
	return wake
}

func memoryReadWakeReconcilePG(test *testing.T, storage *PostgresStore, ctx context.Context, repos []domain.ContentHash, limit int) int {
	test.Helper()
	count, err := storage.ReconcileMemoryReadWork(ctx, repos, limit)
	if err != nil || count < 0 || count > limit {
		test.Fatal("bounded repository reconciliation", count, err)
	}
	return count
}

func memoryReadWakeDispatchPG(test *testing.T, storage *PostgresStore, ctx context.Context) {
	test.Helper()
	for range 256 {
		count, err := storage.DispatchMemoryReadObjectWakes(ctx, 32)
		if err != nil || count < 0 || count > 32 {
			test.Fatal("bounded physical object dispatch", count, err)
		}
		if count == 0 {
			return
		}
	}
	test.Fatal("physical object dispatch did not drain fixture notices")
}

func memoryReadWakeDuePG(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash) {
	test.Helper()
	catalogExecPG(test, ctx, storage.pool, `UPDATE memory_read_wakes SET next_attempt=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
}

func TestPGMemoryReadWakeTransactionCoalescingAndRollback(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	hash := memorySourceBlobPG(test, ctx, storage, target.RepoID, "memory", "wake transaction")
	memoryReadWakeDispatchPG(test, storage, ctx)
	work := memoryReadWorkRequestPG(test, storage, ctx, target)
	before := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
	for _, commit := range []bool{false, true} {
		transaction := catalogTxPG(test, ctx, storage)
		var firstRevision int64
		for ordinal := range 3 {
			catalogExecPG(test, ctx, transaction, `UPDATE snapshots SET message=message || $2 WHERE repo_id=$1`, string(target.RepoID), string(rune('a'+ordinal)))
			catalogExecPG(test, ctx, transaction, `UPDATE repository_memory_source_state SET head_seq=head_seq+1 WHERE repo_id=$1`, string(target.RepoID))
			catalogExecPG(test, ctx, transaction, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			var revision int64
			if err := transaction.QueryRow(ctx, `SELECT revision FROM memory_read_wakes WHERE repo_id=$1`, string(target.RepoID)).Scan(&revision); err != nil {
				test.Fatal(err)
			}
			if ordinal == 0 {
				firstRevision = revision
			} else if revision != firstRevision {
				test.Fatal("one transaction advanced a repository notice more than once", firstRevision, revision)
			}
		}
		var revision, objectRevision int64
		if err := transaction.QueryRow(ctx, `SELECT revision,object_revision FROM memory_read_wakes WHERE repo_id=$1`, string(target.RepoID)).Scan(&revision, &objectRevision); err != nil {
			test.Fatal(err)
		}
		if revision <= before.revision || objectRevision != before.objectRevision {
			test.Fatal("source transaction failed to coalesce or dispatched physical notice inline", before, revision, objectRevision)
		}
		if visible := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); !reflect.DeepEqual(visible, before) {
			test.Fatal("uncommitted notice escaped", before, visible)
		}
		var objectEvents int
		if err := transaction.QueryRow(ctx, `SELECT count(*) FROM memory_read_object_wakes WHERE hash=$1`, string(hash)).Scan(&objectEvents); err != nil || objectEvents != 1 {
			test.Fatal("physical updates did not coalesce into one transactional object event", objectEvents, err)
		}
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_object_wakes WHERE hash=$1`, string(hash)).Scan(&objectEvents); err != nil || objectEvents != 0 {
			test.Fatal("uncommitted physical object event escaped", objectEvents, err)
		}
		if commit {
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
			if after.revision != firstRevision || after.objectRevision != objectRevision || after.nextAttempt == nil {
				test.Fatal("commit lost coalesced notice", before, after)
			}
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_object_wakes WHERE hash=$1`, string(hash)).Scan(&objectEvents); err != nil || objectEvents != 1 {
				test.Fatal("committed physical object event was not durable", objectEvents, err)
			}
			memoryReadWakeDispatchPG(test, storage, ctx)
			if dispatched := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); dispatched.revision <= after.revision || dispatched.objectRevision <= objectRevision || dispatched.nextAttempt == nil {
				test.Fatal("committed physical event did not dispatch a repository notice", after, dispatched)
			}
		} else {
			if err := transaction.Rollback(ctx); err != nil {
				test.Fatal(err)
			}
			memoryReadWakeDispatchPG(test, storage, ctx)
			if after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID); !reflect.DeepEqual(after, before) {
				test.Fatal("rolled back notice persisted", before, after)
			}
		}
		if actual := memoryReadWorkStatusPG(test, storage, ctx, work); actual != work {
			test.Fatal("business transaction fanned out into work rows", work, actual)
		}
	}
	var rows int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_wakes WHERE repo_id=$1`, string(target.RepoID)).Scan(&rows); err != nil || rows != 1 {
		test.Fatal("notice is not one durable row per repository", rows, err)
	}
}

func TestPGMemoryReadWakeBusinessSourcesAndResets(test *testing.T) {
	for _, scenario := range []string{"graph", "memory", "memory-chunk", "catalog-reset", "memory-reset"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			before := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
			switch scenario {
			case "graph":
				catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='wake graph source' WHERE repo_id=$1`, string(target.RepoID))
			case "memory":
				digest := memoryCaptureTestDigest()
				digest.SnapshotID = target.Selection.SnapshotID
				if _, err := storage.PutMemory(ctx, target.RepoID, digest); err != nil {
					test.Fatal(err)
				}
			case "memory-chunk":
				memorySourceBlobPG(test, ctx, storage, target.RepoID, "memory_chunk", "wake ownership")
			case "catalog-reset":
				catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(target.RepoID))
			case "memory-reset":
				catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET epoch=gen_random_uuid(),head_seq=0 WHERE repo_id=$1`, string(target.RepoID))
			}
			after := memoryReadWakeReadPG(test, storage, ctx, target.RepoID)
			if after.revision <= before.revision || after.nextAttempt == nil {
				test.Fatal("business source mutation did not publish notice", before, after)
			}
		})
	}
}

func TestPGMemoryReadWakePhysicalObjectsNotifyOnlyMemoryOwners(test *testing.T) {
	for _, kind := range []string{"memory", "memory_chunk"} {
		test.Run(kind, func(test *testing.T) {
			storage, ctx, target := memoryReadWorkPG(test)
			peer := memoryReadWorkOtherRepoPG(test, storage, ctx, target)
			outsider := memoryReadWorkOtherRepoPG(test, storage, ctx, target)
			hash := memorySourceBlobPG(test, ctx, storage, target.RepoID, kind, "shared physical notice")
			catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$3),($1,'memory_chunk',$3),($2,'doc',$3)`, string(peer.RepoID), string(outsider.RepoID), string(hash))
			memoryReadWakeDispatchPG(test, storage, ctx)
			before := make(map[domain.ContentHash]memoryReadWakeStatePG)
			sources := make(map[domain.ContentHash]domain.RepositoryMemorySourceGeneration)
			for _, repo := range []domain.ContentHash{target.RepoID, peer.RepoID, outsider.RepoID} {
				before[repo] = memoryReadWakeReadPG(test, storage, ctx, repo)
				sources[repo] = memorySourceGenerationPG(test, ctx, storage, repo)
			}
			transaction := catalogTxPG(test, ctx, storage)
			for range 3 {
				catalogExecPG(test, ctx, transaction, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, string(hash))
			}
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			for _, repo := range []domain.ContentHash{target.RepoID, peer.RepoID, outsider.RepoID} {
				if after := memoryReadWakeReadPG(test, storage, ctx, repo); !reflect.DeepEqual(after, before[repo]) {
					test.Fatal("physical writer resolved repository owners before fresh dispatch", repo, before[repo], after)
				}
			}
			memoryReadWakeDispatchPG(test, storage, ctx)
			for _, repo := range []domain.ContentHash{target.RepoID, peer.RepoID} {
				after := memoryReadWakeReadPG(test, storage, ctx, repo)
				if after.revision <= before[repo].revision || after.objectRevision <= before[repo].objectRevision || after.nextAttempt == nil {
					test.Fatal("shared physical witness change lost or duplicated notice", repo, before[repo], after)
				}
				if source := memorySourceGenerationPG(test, ctx, storage, repo); source != sources[repo] {
					test.Fatal("physical witness notice changed business generation", source, sources[repo])
				}
			}
			if after := memoryReadWakeReadPG(test, storage, ctx, outsider.RepoID); !reflect.DeepEqual(after, before[outsider.RepoID]) {
				test.Fatal("document-only owner received memory notice", before[outsider.RepoID], after)
			}
		})
	}
}

func TestPGMemoryReadWakeReadsAndDerivedWritesDoNotAdvanceBusinessNotice(test *testing.T) {
	storage, ctx, job, qualification, output := memoryReadWorkOutputPG(test)
	repo := job.Target.RepoID
	before := memoryReadWakeReadPG(test, storage, ctx, repo)
	source := memorySourceGenerationPG(test, ctx, storage, repo)
	memoryReadWorkFinishPG(test, storage, ctx, job, qualification)
	memoryReadWorkStatusPG(test, storage, ctx, job)
	memoryReadWorkRequestPG(test, storage, ctx, job.Target)
	if err := storage.WithCurrentMemoryReadRange(ctx, job.Target, memoryQualificationAllowPG, memoryReadWorkNoConsumePG); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.ReadMemoryReadHeader(ctx, repo, output.ID); err != nil {
		test.Fatal(err)
	}
	output.Header.Origin += " derived wake test"
	memoryReadPGWrite(test, storage, ctx, memoryReadPGSeal(test, ctx, output.Header, nil))
	if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
		test.Fatal(err)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message=message WHERE repo_id=$1`, string(repo))
	catalogExecPG(test, ctx, storage.pool, `UPDATE repository_memory_source_state SET head_seq=head_seq WHERE repo_id=$1`, string(repo))
	catalogExecPG(test, ctx, storage.pool, `UPDATE repository_catalog_state SET head_seq=head_seq WHERE repo_id=$1`, string(repo))
	after := memoryReadWakeReadPG(test, storage, ctx, repo)
	if after.revision != before.revision || after.objectRevision != before.objectRevision {
		test.Fatal("read or derived write created a business wake feedback loop", before, after)
	}
	if current := memorySourceGenerationPG(test, ctx, storage, repo); current != source {
		test.Fatal("derived write changed business source", source, current)
	}
}
