//go:build postgres

package store

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func docRangeBoundaryPGPause(t *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, statement string) (func(), context.CancelFunc, <-chan error) {
	t.Helper()
	trace := &docRangeJobFenceTracer{statement: statement, reached: make(chan struct{}), proceed: make(chan struct{})}
	peer := docRangeBoundaryPGTracedStore(t, storage, ctx, trace)
	work, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	var once sync.Once
	resume := func() { once.Do(func() { close(trace.proceed) }) }
	t.Cleanup(resume)
	done := make(chan error, 1)
	go func() { done <- peer.BuildDocRangeJob(work, job) }()
	select {
	case <-trace.reached:
	case err := <-done:
		t.Fatal("worker missed publication fence", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	return resume, cancel, done
}

func docRangeBoundaryPGRebuildGeneration(t *testing.T, storage *PostgresStore, ctx context.Context, repo, hash domain.ContentHash) {
	t.Helper()
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		return storage.ensureRangeGeneration(write, repo, hash)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocRangeBoundaryPGSourceChangePreservesCheckpointThenRestarts(t *testing.T) {
	for _, changed := range []int{0, 1} {
		t.Run([]string{"doc", "base"}[changed], func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			batchEvents := domain.DocRangePrefixBatchPages * domain.DocRangePageEvents
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, batchEvents+37, batchEvents+36)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			first := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, first); err != nil {
				t.Fatal(err)
			}
			before, _, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil {
				t.Fatal(err)
			}
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			resume, _, done := docRangeBoundaryPGPause(t, storage, ctx, claim, `SELECT hash FROM blobs WHERE hash=ANY($1::text[]) ORDER BY hash FOR UPDATE`)
			if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, pair[changed].hash); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRebuildGeneration(t, storage, ctx, repo, pair[changed].hash)
			resume()
			if err := <-done; !errors.Is(err, domain.ErrConflict) {
				t.Fatal("stale comparison published", err)
			}
			after, _, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil || before != after {
				t.Fatal("conflict changed checkpoint", after, err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrDocRangeIndexUnavailable)
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, repo); err != nil {
				t.Fatal(err)
			}
			reclaimed := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if reclaimed.Version <= claim.Version {
				t.Fatal("nonmonotonic reclaim", reclaimed)
			}
			if err := storage.BuildDocRangeJob(ctx, reclaimed); err != nil {
				t.Fatal(err)
			}
			restarted, _, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil || restarted.complete || restarted.events != batchEvents || restarted.docVersion+restarted.baseVersion != 1 {
				t.Fatal("new lifetime reused old progress", restarted, err)
			}
			last := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, last); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, batchEvents+36, nil)
		})
	}
}

func TestDocRangeBoundaryPGFinalFenceAndRetainedLocks(t *testing.T) {
	for _, action := range []string{"cancel", "expired", "renewed", "reclaimed", "duplicate"} {
		t.Run(action, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			resume, cancel, done := docRangeBoundaryPGPause(t, storage, ctx, claim, "")
			for _, fixture := range pair {
				for _, relation := range []string{"root", "owned", "snapshot", "generation"} {
					var present int
					err := storage.pool.QueryRow(ctx, `SELECT 1 FROM blobs root
 JOIN repo_blobs owned ON owned.hash=root.hash AND owned.repo_id=$1 AND owned.kind='doc'
 JOIN snapshots snapshot ON snapshot.repo_id=owned.repo_id AND snapshot.id=root.hash
 JOIN doc_range_generations_v2 generation ON generation.doc_hash=root.hash
 WHERE root.hash=$2 FOR UPDATE OF `+relation+` NOWAIT`, repo, fixture.hash).Scan(&present)
					var locked *pgconn.PgError
					if !errors.As(err, &locked) || locked.Code != "55P03" {
						t.Fatalf("%s lock missing: %v", relation, err)
					}
				}
			}
			poll, stop := context.WithTimeout(ctx, time.Second)
			receipt, err := storage.EnqueueDocRangeBoundary(poll, repo, pair[0].hash, pair[1].hash, time.Now())
			stop()
			if err != nil || !reflect.DeepEqual(receipt, docRangeJobPGReceipt(claim)) {
				t.Fatal("unfinished receipt waited for publisher", receipt, err)
			}
			var duplicate <-chan error
			want := domain.ErrConflict
			switch action {
			case "cancel":
				cancel()
				want = context.Canceled
			case "expired", "reclaimed":
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, repo); err != nil {
					t.Fatal(err)
				}
				if action == "reclaimed" {
					next := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
					if next.Version <= claim.Version {
						t.Fatal(next)
					}
				}
			case "renewed":
				if err := storage.RenewDocRangeJob(ctx, claim, time.Now(), 2*time.Minute); err != nil {
					t.Fatal(err)
				}
				want = nil
			case "duplicate":
				result := make(chan error, 1)
				duplicate = result
				go func() { result <- storage.BuildDocRangeJob(ctx, claim) }()
				want = nil
			}
			resume()
			if err := <-done; !errors.Is(err, want) {
				t.Fatalf("fence result=%v want=%v", err, want)
			}
			if duplicate != nil {
				if err := <-duplicate; !errors.Is(err, domain.ErrConflict) {
					t.Fatal("duplicate worker published", err)
				}
			}
			checkpoint, exists, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil || exists != (want == nil) {
				t.Fatal("fence atomicity", checkpoint, exists, err)
			}
		})
	}
}

func TestDocRangeBoundaryPGOlderWriterInvalidatesCompletedProof(t *testing.T) {
	for _, changed := range []int{0, 1} {
		t.Run([]string{"doc", "base"}[changed], func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
			if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, pair[changed].hash); err != nil {
				t.Fatal(err)
			}
			writer, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(writer)
			var visible bool
			if err := writer.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 WHERE doc_hash=$1)`, pair[changed].hash).Scan(&visible); err != nil || visible {
				t.Fatal("generation unexpectedly visible", visible, err)
			}
			docRangeBoundaryPGRebuildGeneration(t, storage, ctx, repo, pair[changed].hash)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 128, nil)
			if _, err := writer.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, pair[changed].hash); err != nil {
				t.Fatal(err)
			}
			if err := writer.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 WHERE doc_hash=$1)`, pair[changed].hash).Scan(&visible); err != nil || !visible {
				t.Fatal("test did not retain unseen generation", visible, err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrDocRangeIndexUnavailable)
			requeued := docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			if requeued.State != "waiting" || requeued.Version <= claim.Version || requeued.Attempts != 0 {
				t.Fatal("stale completed proof reused", requeued)
			}
			docRangeBoundaryPGRebuildGeneration(t, storage, ctx, repo, pair[changed].hash)
			newClaim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, newClaim); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 128, nil)
		})
	}
}

func TestDocRangeBoundaryPGPublicationRechecksScopesAndGeneration(t *testing.T) {
	for _, damage := range []string{"doc grant", "base grant", "snapshot", "generation", "descriptor", "generation identity"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			resume, _, done := docRangeBoundaryPGPause(t, storage, ctx, claim, `SELECT hash FROM blobs WHERE hash=ANY($1::text[]) ORDER BY hash FOR UPDATE`)
			statement, args := `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, []any{repo, pair[1].hash}
			want := domain.ErrNotFound
			switch damage {
			case "doc grant":
				args[1] = pair[0].hash
			case "snapshot":
				statement = `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`
			case "generation":
				statement, args = `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, []any{pair[1].hash}
				want = domain.ErrConflict
			case "descriptor":
				statement, args = `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, []any{pair[1].hash, []byte("corrupt")}
				want = domain.ErrIntegrity
			case "generation identity":
				var raw []byte
				if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
					generation, err := storage.readRangeGeneration(read, repo, pair[1].hash)
					if err != nil {
						return err
					}
					generation.StreamBytes++
					raw, err = generation.Encode()
					return err
				}); err != nil {
					t.Fatal(err)
				}
				statement, args = `UPDATE doc_range_generations_v2 SET descriptor=$2,generation_hash=$3 WHERE doc_hash=$1`, []any{pair[1].hash, raw, domain.HashContent(raw)}
				want = domain.ErrConflict
			}
			if _, err := storage.pool.Exec(ctx, statement, args...); err != nil {
				t.Fatal(err)
			}
			resume()
			if err := <-done; !errors.Is(err, want) {
				t.Fatal("publisher accepted changed scope/generation", err)
			}
			_, exists, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil || exists {
				t.Fatal("failed final validation left proof", exists, err)
			}
			if current := docRangeBoundaryPGGet(t, storage, ctx, repo, pair); !reflect.DeepEqual(current, docRangeJobPGReceipt(claim)) {
				t.Fatal("failed final validation changed job", current)
			}
		})
	}
}

func TestDocRangeBoundaryPGRootRollbackAndDeleteReinsert(t *testing.T) {
	for _, side := range []int{0, 1} {
		t.Run([]string{"doc", "base"}[side], func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			pair := docRangeBoundaryPGPair(t, storage, ctx, repo, 129, 128)
			docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			claim := docRangeBoundaryPGClaim(t, storage, ctx, repo, pair)
			if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
				t.Fatal(err)
			}
			writer, err := storage.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(writer)
			if _, err := writer.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, pair[side].hash); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 128, nil)
			if err := writer.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 128, nil)
			for _, statement := range []string{
				`DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`,
				`DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`,
				`DELETE FROM blobs WHERE hash=$2 AND $1::text<>''`,
			} {
				if _, err := storage.pool.Exec(ctx, statement, repo, pair[side].hash); err != nil {
					t.Fatal(err)
				}
			}
			_, exists, err := readDocRangeBoundaryCheckpoint(ctx, storage.pool, repo, pair[0].hash, pair[1].hash, false)
			if err != nil || exists {
				t.Fatal("root delete did not cascade proof", exists, err)
			}
			seedRootPG(t, ctx, storage, repo, pair[side])
			if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: pair[side].hash, RepoID: repo, DocHash: pair[side].hash, DocIdentity: domain.DocumentIdentityRootV1, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
				t.Fatal(err)
			}
			docRangeBoundaryPGRebuildGeneration(t, storage, ctx, repo, pair[side].hash)
			docRangeBoundaryPGRead(t, storage, ctx, repo, pair, 0, domain.ErrDocRangeIndexUnavailable)
			requeued := docRangeBoundaryPGEnqueue(t, storage, ctx, repo, pair)
			if requeued.State != "waiting" || requeued.Version <= claim.Version {
				t.Fatal("reinsert revived old proof", requeued)
			}
		})
	}
}
