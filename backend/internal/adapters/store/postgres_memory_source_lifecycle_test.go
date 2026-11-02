//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func memorySourceErrorPG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, expected error) {
	test.Helper()
	generation, err := storage.ReadRepositoryMemorySourceGeneration(ctx, repo)
	if !errors.Is(err, expected) || generation != (domain.RepositoryMemorySourceGeneration{}) {
		test.Fatalf("memory read returned partial generation or wrong error: generation=%+v err=%v want=%v", generation, err, expected)
	}
}

func TestPGRepositoryMemorySourceDeleteReinsertABA(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	hash := memorySourceBlobPG(test, ctx, storage, repo, "memory_chunk", "aba")
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	var body []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&body); err != nil {
		test.Fatal(err)
	}
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2`, string(repo), string(hash))
	catalogExecPG(test, ctx, transaction, `DELETE FROM blobs WHERE hash=$1`, string(hash))
	catalogExecPG(test, ctx, transaction, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, string(hash), body)
	catalogExecPG(test, ctx, transaction, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(repo), string(hash))
	memorySourceAdvancePG(test, ctx, storage, before, 0)
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	after := memorySourceAdvancePG(test, ctx, storage, before, 1)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2`, string(repo), string(hash))
	after = memorySourceAdvancePG(test, ctx, storage, after, 1)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(repo), string(hash))
	memorySourceAdvancePG(test, ctx, storage, after, 1)
}

func TestPGRepositoryMemorySourceStaleWriterCannotCertifyNewGrantBytes(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	hash := memorySourceBlobPG(test, ctx, storage, repo, "", "grant after writer snapshot")
	writer, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(writer)
	var owned bool
	if err := writer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2)`, string(repo), string(hash)).Scan(&owned); err != nil || owned {
		test.Fatalf("writer snapshot unexpectedly includes future grant: owned=%v err=%v", owned, err)
	}
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(repo), string(hash))
	granted := memorySourceAdvancePG(test, ctx, storage, before, 1)
	if err := writer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2)`, string(repo), string(hash)).Scan(&owned); err != nil || owned {
		test.Fatalf("repeatable-read writer adopted a concurrent grant: owned=%v err=%v", owned, err)
	}
	catalogExecPG(test, ctx, writer, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, string(hash), []byte("changed after new grant"))
	if err := writer.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	memorySourceAdvancePG(test, ctx, storage, granted, 0)
	var body []byte
	if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, string(hash)).Scan(&body); err != nil || string(body) != "changed after new grant" {
		test.Fatalf("stale writer did not change newly granted bytes: body=%q err=%v", body, err)
	}
}

func TestPGRepositoryMemorySourceMissingAndForeignStateFailsClosed(test *testing.T) {
	for _, testcase := range []struct {
		name  string
		query string
	}{
		{"missing_memory", `DELETE FROM repository_memory_source_state WHERE repo_id=$1`},
		{"zero_memory_epoch", `UPDATE repository_memory_source_state SET epoch='00000000-0000-0000-0000-000000000000' WHERE repo_id=$1`},
		{"missing_graph", `DELETE FROM repository_graph_source_state WHERE repo_id=$1`},
		{"foreign_graph_epoch", `UPDATE repository_graph_source_state SET epoch=(SELECT epoch FROM repository_graph_source_state WHERE repo_id=$2) WHERE repo_id=$1`},
		{"missing_view", `DELETE FROM repository_view_source_state WHERE repo_id=$1`},
		{"missing_catalog", `DELETE FROM repository_catalog_state WHERE repo_id=$1`},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			peer := memorySourceRepoPG(test, ctx, storage, "foreign")
			before := memorySourceGenerationPG(test, ctx, storage, peer)
			memorySourceGenerationPG(test, ctx, storage, repo)
			arguments := []any{string(repo)}
			if testcase.name == "foreign_graph_epoch" {
				arguments = append(arguments, string(peer))
			}
			catalogExecPG(test, ctx, storage.pool, testcase.query, arguments...)
			memorySourceErrorPG(test, ctx, storage, repo, domain.ErrIntegrity)
			if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
				memorySourceErrorPG(test, read, storage, repo, domain.ErrIntegrity)
				return nil
			}); err != nil {
				test.Fatal(err)
			}
			memorySourceAdvancePG(test, ctx, storage, before, 0)
			if testcase.name == "missing_memory" {
				var remaining int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&remaining); err != nil || remaining != 0 {
					test.Fatalf("reader repaired missing memory state: remaining=%d err=%v", remaining, err)
				}
			}
		})
	}
}

func TestPGRepositoryMemorySourceCatalogResetAndRepositoryDeletion(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	peer := memorySourceRepoPG(test, ctx, storage, "survivor")
	hash := memorySourceBlobPG(test, ctx, storage, repo, "memory", "lifecycle")
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(peer), string(hash))
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	peerBefore := memorySourceGenerationPG(test, ctx, storage, peer)
	transaction := catalogTxPG(test, ctx, storage)
	catalogExecPG(test, ctx, transaction, `SELECT cxt_reset_catalog($1)`, string(repo))
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memorySourceAdvancePG(test, ctx, storage, before, 0)
	catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
	after := memorySourceGenerationPG(test, ctx, storage, repo)
	if after.Graph.Epoch == before.Graph.Epoch || after.Graph.Sequence != 0 || after.Epoch != before.Epoch || after.Sequence != before.Sequence {
		test.Fatalf("catalog reset changed independent memory clock: before=%+v after=%+v", before, after)
	}
	catalogExecPG(test, ctx, storage.pool, `UPDATE repo_blobs SET kind=CASE kind WHEN 'memory' THEN 'memory_chunk' ELSE 'memory' END WHERE hash=$1 AND repo_id IN ($2,$3)`, string(hash), string(repo), string(peer))
	after = memorySourceAdvancePG(test, ctx, storage, after, 1)
	peerBefore = memorySourceAdvancePG(test, ctx, storage, peerBefore, 1)
	catalogExecPG(test, ctx, storage.pool, `DELETE FROM repos WHERE id=$1`, string(repo))
	memorySourceErrorPG(test, ctx, storage, repo, domain.ErrNotFound)
	var remaining int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&remaining); err != nil || remaining != 0 {
		test.Fatalf("repository deletion retained memory state: remaining=%d err=%v", remaining, err)
	}
	memorySourceAdvancePG(test, ctx, storage, peerBefore, 0)
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		test.Fatal(err)
	}
	recreated := memorySourceGenerationPG(test, ctx, storage, repo)
	if recreated.Epoch == after.Epoch || recreated.Graph.Epoch == after.Graph.Epoch || recreated.Sequence != 0 {
		test.Fatalf("repository reincarnation reused memory generation: before=%+v after=%+v", after, recreated)
	}
}

func TestPGRepositoryMemorySourceReadSnapshotStaysStable(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	hash := memorySourceBlobPG(test, ctx, storage, repo, "memory", "snapshot")
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	var committed domain.RepositoryMemorySourceGeneration
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if initial := memorySourceGenerationPG(test, read, storage, repo); initial != before {
			test.Fatalf("initial read snapshot changed: before=%+v initial=%+v", before, initial)
		}
		transaction := catalogTxPG(test, ctx, storage)
		catalogExecPG(test, ctx, transaction, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory_chunk',$2)`, string(repo), string(hash))
		catalogExecPG(test, ctx, transaction, `UPDATE repos SET default_branch='concurrent-branch' WHERE id=$1`, string(repo))
		if err := transaction.Commit(ctx); err != nil {
			return err
		}
		committed = memorySourceGenerationPG(test, ctx, storage, repo)
		if committed.Graph.Sequence != before.Graph.Sequence+1 || committed.Sequence != before.Sequence+1 || committed.Epoch != before.Epoch {
			test.Fatalf("writer did not advance both source generations: before=%+v after=%+v", before, committed)
		}
		memorySourceAdvancePG(test, read, storage, before, 0)
		catalogExecPG(test, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
		memorySourceAdvancePG(test, read, storage, before, 0)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	after := memorySourceGenerationPG(test, ctx, storage, repo)
	if after.Graph.Epoch == committed.Graph.Epoch || after.Epoch != committed.Epoch || after.Sequence != committed.Sequence {
		test.Fatalf("fresh read failed to see committed reset: committed=%+v after=%+v", committed, after)
	}
}

type memorySourceInterleavedTx struct {
	pgx.Tx
	afterGraph func()
}

type memorySourceInterleavedRow struct {
	pgx.Row
	afterScan func()
}

func (transaction *memorySourceInterleavedTx) QueryRow(ctx context.Context, query string, arguments ...any) pgx.Row {
	row := transaction.Tx.QueryRow(ctx, query, arguments...)
	if query == repositoryGraphSourceStateSQL && transaction.afterGraph != nil {
		afterScan := transaction.afterGraph
		transaction.afterGraph = nil
		return memorySourceInterleavedRow{Row: row, afterScan: afterScan}
	}
	return row
}

func (row memorySourceInterleavedRow) Scan(destinations ...any) error {
	err := row.Row.Scan(destinations...)
	if err == nil {
		row.afterScan()
	}
	return err
}

func TestPGRepositoryMemorySourceGraphAndClockShareSnapshot(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	hash := memorySourceBlobPG(test, ctx, storage, repo, "memory_chunk", "interleave")
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	transaction, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	interleaved := &memorySourceInterleavedTx{Tx: transaction, afterGraph: func() {
		writer := catalogTxPG(test, ctx, storage)
		catalogExecPG(test, ctx, writer, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'memory',$2)`, string(repo), string(hash))
		catalogExecPG(test, ctx, writer, `UPDATE repos SET default_branch='interleaved-branch' WHERE id=$1`, string(repo))
		if err := writer.Commit(ctx); err != nil {
			test.Fatal(err)
		}
	}}
	bound := context.WithValue(ctx, repositoryTxKey{}, &repositoryTx{Tx: interleaved, owner: storage, readOnly: true})
	got, err := storage.ReadRepositoryMemorySourceGeneration(bound, repo)
	if err != nil || got != before || interleaved.afterGraph != nil {
		test.Fatalf("graph and memory clock came from different snapshots: before=%+v got=%+v interleaved=%v err=%v", before, got, interleaved.afterGraph == nil, err)
	}
	after := memorySourceGenerationPG(test, ctx, storage, repo)
	if after.Graph.Sequence != before.Graph.Sequence+1 || after.Sequence != before.Sequence+1 {
		test.Fatalf("interleaved writer failed to commit both changes: before=%+v after=%+v", before, after)
	}
}

func TestPGRepositoryMemorySourceRejectsWriteForeignAndCanceledContexts(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	memorySourceErrorPG(test, ctx, storage, domain.HashContent([]byte(test.Name()+"absent")), domain.ErrNotFound)
	memorySourceErrorPG(test, ctx, storage, "malformed", domain.ErrValidation)
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		memorySourceErrorPG(test, write, storage, repo, domain.ErrConflict)
		return storage.WithinReadSnapshot(write, func(borrowed context.Context) error {
			memorySourceErrorPG(test, borrowed, storage, repo, domain.ErrConflict)
			return nil
		})
	}); err != nil {
		test.Fatal(err)
	}
	peer := &PostgresStore{pool: storage.pool}
	if err := peer.WithinReadSnapshot(ctx, func(foreign context.Context) error {
		memorySourceErrorPG(test, foreign, storage, repo, domain.ErrConflict)
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	memorySourceErrorPG(test, canceled, storage, repo, context.Canceled)
	memorySourceAdvancePG(test, ctx, storage, before, 0)
}

func TestPGRepositoryMemorySourceConcurrentWritersAndCancellation(test *testing.T) {
	for _, cancelWriter := range []bool{false, true} {
		name := "both_commit"
		if cancelWriter {
			name = "cancel_waiting_writer"
		}
		test.Run(name, func(test *testing.T) {
			storage, ctx, repo := catalogPG(test)
			firstHash := memorySourceBlobPG(test, ctx, storage, repo, "memory", "first writer")
			secondHash := memorySourceBlobPG(test, ctx, storage, repo, "memory_chunk", "second writer")
			before := memorySourceGenerationPG(test, ctx, storage, repo)
			first := catalogTxPG(test, ctx, storage)
			catalogExecPG(test, ctx, first, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, string(repo), string(firstHash))
			waiting, cancel := context.WithCancel(ctx)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				done <- func() error {
					second, err := storage.pool.Begin(waiting)
					if err != nil {
						return err
					}
					defer rollbackPG(second)
					if _, err := second.Exec(waiting, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2`, string(repo), string(secondHash)); err != nil {
						return err
					}
					return second.Commit(waiting)
				}()
			}()
			cyclePGWaitBlocked(test, ctx, storage, first.Conn().PgConn().PID())
			memorySourceAdvancePG(test, ctx, storage, before, 0)
			if cancelWriter {
				cancel()
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						test.Fatalf("waiting writer ignored cancellation: %v", err)
					}
				case <-ctx.Done():
					test.Fatal(ctx.Err())
				}
			}
			if err := first.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			if !cancelWriter {
				select {
				case err := <-done:
					if err != nil {
						test.Fatal(err)
					}
				case <-ctx.Done():
					test.Fatal(ctx.Err())
				}
				memorySourceAdvancePG(test, ctx, storage, before, 2)
			} else {
				memorySourceAdvancePG(test, ctx, storage, before, 1)
			}
			var owned bool
			if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM repo_blobs WHERE repo_id=$1 AND kind='memory_chunk' AND hash=$2)`, string(repo), string(secondHash)).Scan(&owned); err != nil || owned != cancelWriter {
				test.Fatalf("waiting writer published wrong ownership: owned=%v canceled=%v err=%v", owned, cancelWriter, err)
			}
		})
	}
}

func TestPGRepositoryMemorySourceMigrationSeedsExistingRepositories(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	emptyRepo := memorySourceRepoPG(test, ctx, storage, "empty")
	snapshot := initializationPGSnapshot(test, storage, repo, "before memory migration")
	hash := memorySourceBlobPG(test, ctx, storage, repo, "memory_chunk", "before migration")
	catalogExecPG(test, ctx, storage.pool, `INSERT INTO memories(repo_id,snapshot_id,summary,provider) VALUES($1,$2,'existing memory','claude')`, string(repo), string(snapshot.ID))
	before := memorySourceGenerationPG(test, ctx, storage, repo)
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0106_memory_source.sql")
	if err != nil {
		test.Fatal(err)
	}
	transaction := catalogTxPG(test, ctx, storage)
	schema := pgx.Identifier{domain.NewID("memory_source_seed_")}.Sanitize()
	catalogExecPG(test, ctx, transaction, `CREATE SCHEMA `+schema)
	for _, table := range []string{"repos", "blobs", "repo_blobs", "memories", "pr_contribution_memories", "git_contribution_memories"} {
		identifier := pgx.Identifier{table}.Sanitize()
		catalogExecPG(test, ctx, transaction, `CREATE TABLE `+schema+`.`+identifier+` (LIKE public.`+identifier+` INCLUDING ALL)`)
	}
	catalogExecPG(test, ctx, transaction, `INSERT INTO `+schema+`.repos SELECT * FROM public.repos WHERE id IN ($1,$2)`, string(repo), string(emptyRepo))
	catalogExecPG(test, ctx, transaction, `INSERT INTO `+schema+`.blobs SELECT * FROM public.blobs WHERE hash IN (SELECT hash FROM public.repo_blobs WHERE repo_id=$1)`, string(repo))
	for _, table := range []string{"repo_blobs", "memories"} {
		identifier := pgx.Identifier{table}.Sanitize()
		catalogExecPG(test, ctx, transaction, `INSERT INTO `+schema+`.`+identifier+` SELECT * FROM public.`+identifier+` WHERE repo_id=$1`, string(repo))
	}
	catalogExecPG(test, ctx, transaction, `SET LOCAL search_path TO `+schema+`, public`)
	catalogExecPG(test, ctx, transaction, string(migration))
	var count, epochs, initialized int
	if err := transaction.QueryRow(ctx, `SELECT count(*),count(DISTINCT epoch),count(*) FILTER (WHERE head_seq=0 AND writer_xid IS NULL AND epoch<>'00000000-0000-0000-0000-000000000000'::uuid AND epoch<>$1::uuid) FROM repository_memory_source_state`, before.Graph.Epoch).Scan(&count, &epochs, &initialized); err != nil || count != 2 || epochs != 2 || initialized != 2 {
		test.Fatalf("migration did not seed independent zero baselines: count=%d epochs=%d initialized=%d err=%v", count, epochs, initialized, err)
	}
	newRepo := domain.HashContent([]byte(test.Name() + "new after migration"))
	catalogExecPG(test, ctx, transaction, `INSERT INTO repos(id,remote_url,team) VALUES($1,'https://example.test/memory-source','fixture')`, string(newRepo))
	var sequence int64
	if err := transaction.QueryRow(ctx, `SELECT head_seq FROM repository_memory_source_state WHERE repo_id=$1`, string(newRepo)).Scan(&sequence); err != nil || sequence != 0 {
		test.Fatalf("new repository did not receive zero baseline: sequence=%d err=%v", sequence, err)
	}
	catalogExecPG(test, ctx, transaction, `ALTER TABLE repo_blobs ADD CONSTRAINT memory_source_fixture_hash_fk FOREIGN KEY(hash) REFERENCES blobs(hash) DEFERRABLE INITIALLY DEFERRED`)
	for _, testcase := range []struct {
		name  string
		query string
	}{
		{"owned_blob_hash", `UPDATE blobs SET hash='sha256:'||repeat('9',64) WHERE hash=$1`},
		{"owned_blob_delete", `DELETE FROM blobs WHERE hash=$1`},
	} {
		test.Run(testcase.name, func(test *testing.T) {
			catalogExecPG(test, ctx, transaction, `SAVEPOINT memory_source_blob_change`)
			catalogExecPG(test, ctx, transaction, testcase.query, string(hash))
			if err := transaction.QueryRow(ctx, `SELECT head_seq FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&sequence); err != nil || sequence != 0 {
				test.Fatalf("byte-only maintenance changed metadata clock: sequence=%d err=%v", sequence, err)
			}
			catalogExecPG(test, ctx, transaction, `ROLLBACK TO SAVEPOINT memory_source_blob_change`)
			if err := transaction.QueryRow(ctx, `SELECT head_seq FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&sequence); err != nil || sequence != 0 {
				test.Fatalf("rolled-back blob mutation leaked generation: sequence=%d err=%v", sequence, err)
			}
		})
	}
	catalogExecPG(test, ctx, transaction, `UPDATE memories SET summary='after migration' WHERE repo_id=$1`, string(repo))
	catalogExecPG(test, ctx, transaction, `UPDATE memories SET key_facts=ARRAY['coalesced'] WHERE repo_id=$1`, string(repo))
	if err := transaction.QueryRow(ctx, `SELECT head_seq FROM repository_memory_source_state WHERE repo_id=$1`, string(repo)).Scan(&sequence); err != nil || sequence != 1 {
		test.Fatalf("post-migration writes did not coalesce: sequence=%d err=%v", sequence, err)
	}
	if err := transaction.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	memorySourceAdvancePG(test, ctx, storage, before, 0)
}
