//go:build postgres

package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryReadWakeScopeActivationCannotLoseOlderSourceWriter(test *testing.T) {
	for _, activation := range []string{"initial-enable", "re-enable"} {
		for _, mutation := range []struct {
			name  string
			query string
		}{
			{"graph", `UPDATE snapshots SET message='older writer after scope activation' WHERE repo_id=$1`},
			{"memory", `UPDATE repository_memory_source_state SET head_seq=head_seq+1 WHERE repo_id=$1`},
		} {
			test.Run(activation+"/"+mutation.name, func(test *testing.T) {
				storage, ctx, repo := catalogPG(test)
				snapshot := initializationPGSnapshot(test, storage, repo, "scope activation race")
				target := domain.MemoryReadTarget{Version: domain.MemoryReadWorkVersion, Rules: domain.MemoryReadRules, RepoID: repo,
					Selection: domain.EffectiveMemorySelection{SnapshotID: snapshot.ID, CodeCommit: fmt.Sprintf("%040x", 1)}}
				wantedScopes := 0
				if activation == "re-enable" {
					if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
						test.Fatal(err)
					}
					memoryReadWakeRejectPG(test, storage, ctx, target)
					memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
					if err := storage.ConfigureMemoryReadWork(ctx, repo, false); err != nil {
						test.Fatal(err)
					}
					wantedScopes = 1
				}
				writer, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(writer)
				assertOldScope := func() {
					test.Helper()
					var scopes int
					var enabled bool
					if err := writer.QueryRow(ctx, `SELECT count(*),COALESCE(bool_or(enabled),false)
 FROM memory_read_work_scopes WHERE repo_id=$1`, string(repo)).Scan(&scopes, &enabled); err != nil || scopes != wantedScopes || enabled {
						test.Fatal("writer snapshot does not retain the pre-activation scope", scopes, enabled, err)
					}
				}
				assertOldScope()
				if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
					test.Fatal(err)
				}
				job := memoryReadWakeRejectPG(test, storage, ctx, target)
				if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
					test.Fatal("worker did not drain the terminal transition after activation", count)
				}
				before := memoryReadWakeReadPG(test, storage, ctx, repo)
				var consumed int64
				if err := storage.pool.QueryRow(ctx, `SELECT wake_revision FROM memory_read_work
 WHERE repo_id=$1 AND target_id=$2`, string(repo), string(job.ID)).Scan(&consumed); err != nil || consumed != before.revision || before.nextAttempt != nil {
					test.Fatal("activation notice was not fully consumed before the older write", consumed, before, err)
				}
				assertOldScope()
				mutate := func(transaction pgx.Tx) error {
					tag, err := transaction.Exec(ctx, mutation.query, string(repo))
					if err != nil {
						return err
					}
					if tag.RowsAffected() != 1 {
						return fmt.Errorf("source mutation affected %d rows", tag.RowsAffected())
					}
					return transaction.Commit(ctx)
				}
				if err := mutate(writer); err != nil {
					var databaseError *pgconn.PgError
					if !errors.As(err, &databaseError) || databaseError.Code != "40001" {
						test.Fatal("older source writer failed without a serialization retry", err)
					}
					rollbackPG(writer)
					if source := memorySourceGenerationPG(test, ctx, storage, repo); source != job.Source {
						test.Fatal("serialization failure committed a partial logical source", job.Source, source)
					}
					if actual := memoryReadWakeReadPG(test, storage, ctx, repo); !reflect.DeepEqual(actual, before) {
						test.Fatal("serialization failure changed the consumed activation notice", before, actual)
					}
					retry, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
					if err != nil {
						test.Fatal(err)
					}
					defer rollbackPG(retry)
					if err := mutate(retry); err != nil {
						test.Fatal("fresh source writer could not retry after activation", err)
					}
					test.Log("older writer received 40001; fresh writer committed its logical source change")
				}
				source := memorySourceGenerationPG(test, ctx, storage, repo)
				if source == job.Source {
					test.Fatal("committed writer did not change the logical source")
				}
				after := memoryReadWakeReadPG(test, storage, ctx, repo)
				if after.revision <= before.revision || after.objectRevision != before.objectRevision || after.nextAttempt == nil || after.nextAttempt.After(time.Now()) {
					test.Fatal("older writer committed its source change without a new due logical wake", before, after)
				}
				var notices int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM memory_read_wakes WHERE repo_id=$1`, string(repo)).Scan(&notices); err != nil || notices != 1 {
					test.Fatal("source retry did not retain one coalesced repository notice", notices, err)
				}
				if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != job {
					test.Fatal("source writer changed target work before reconciliation", job, actual)
				}
				if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
					test.Fatal("committed older source change was not reconciled", count)
				}
				queued := memoryReadWorkStatusPG(test, storage, ctx, job)
				if queued.State != "waiting" || queued.Attempts != 0 || queued.Version <= job.Version || queued.Source != job.Source {
					test.Fatal("older writer left the rejected target stranded", job, queued)
				}
				if claimed := memoryReadWorkClaimPG(test, storage, ctx, repo); claimed.ID != job.ID || claimed.Attempts != 1 || claimed.Source != source {
					test.Fatal("requeued target did not adopt the committed source", claimed, source)
				}
			})
		}
	}
}

func TestPGMemoryReadWakeDisabledScopeRecordsWithoutExecution(test *testing.T) {
	storage, ctx, target := memoryReadWorkPG(test)
	repo := target.RepoID
	job := memoryReadWorkRequestPG(test, storage, ctx, target)
	memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1)
	if err := storage.ConfigureMemoryReadWork(ctx, repo, false); err != nil {
		test.Fatal(err)
	}
	job = memoryReadWorkStatusPG(test, storage, ctx, job)
	before := memoryReadWakeReadPG(test, storage, ctx, repo)
	catalogExecPG(test, ctx, storage.pool, `UPDATE snapshots SET message='source changed while disabled' WHERE repo_id=$1`, string(repo))
	after := memoryReadWakeReadPG(test, storage, ctx, repo)
	if after.revision <= before.revision || after.objectRevision != before.objectRevision || after.nextAttempt == nil || after.nextAttempt.After(time.Now()) {
		test.Fatal("disabled scope did not retain its logical source notice", before, after)
	}
	assertNoExecution := func(repos []domain.ContentHash) {
		test.Helper()
		if count := memoryReadWakeReconcilePG(test, storage, ctx, repos, 1); count != 0 {
			test.Fatal("scope or local allowlist gate allowed reconciliation", repos, count)
		}
		if _, err := storage.ClaimMemoryReadWork(ctx, repos, time.Minute); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("scope or local allowlist gate allowed a claim", repos, err)
		}
		if actual := memoryReadWorkStatusPG(test, storage, ctx, job); actual != job {
			test.Fatal("gated work changed despite no execution", job, actual)
		}
	}
	assertNoExecution([]domain.ContentHash{repo})
	if actual := memoryReadWakeReadPG(test, storage, ctx, repo); !reflect.DeepEqual(actual, after) {
		test.Fatal("disabled reconciliation consumed the retained notice", after, actual)
	}
	if err := storage.ConfigureMemoryReadWork(ctx, repo, true); err != nil {
		test.Fatal(err)
	}
	assertNoExecution(nil)
	assertNoExecution([]domain.ContentHash{domain.HashContent([]byte("unselected scope wake"))})
	if count := memoryReadWakeReconcilePG(test, storage, ctx, []domain.ContentHash{repo}, 1); count != 1 {
		test.Fatal("enabled and locally selected scope did not resume", count)
	}
	if claimed := memoryReadWorkClaimPG(test, storage, ctx, repo); claimed.ID != job.ID || claimed.Source != memorySourceGenerationPG(test, ctx, storage, repo) || claimed.Source == job.Source {
		test.Fatal("enabled and locally selected work lost the disabled source change", claimed)
	}
}
