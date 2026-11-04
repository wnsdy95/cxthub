//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type rootSourceWitnessStatePG struct {
	body        []byte
	version     int64
	incarnation string
}

func readRootSourceWitnessStatePG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) rootSourceWitnessStatePG {
	test.Helper()
	var state rootSourceWitnessStatePG
	if err := storage.db(ctx).QueryRow(ctx, `SELECT source.bytes,source.doc_range_source_version,incarnation.incarnation::text
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, hash).
		Scan(&state.body, &state.version, &state.incarnation); err != nil {
		test.Fatal("read dependency source state", hash, err)
	}
	if state.incarnation == "" {
		test.Fatal("dependency has no source incarnation", hash)
	}
	return state
}

func assertRootSourceWitnessStatePG(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash, expected rootSourceWitnessStatePG) {
	test.Helper()
	actual := readRootSourceWitnessStatePG(test, storage, ctx, hash)
	if actual.version != expected.version || actual.incarnation != expected.incarnation || !bytes.Equal(actual.body, expected.body) {
		test.Fatal("publication changed dependency bytes, generation, or incarnation", hash, actual.version, actual.incarnation)
	}
}

func assertRootSourceWitnessPublishedPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob, fixture rootReaderFixture) {
	test.Helper()
	current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil || current.State != "completed" || current.DocumentRef() != job.DocumentRef() || current.Version != job.Version || !current.LeaseUntil.IsZero() {
		test.Fatal("source-bound publication lost its completion receipt", current, err)
	}
	owned, err := storage.HasDocs(ctx, job.RepoID, []domain.ContentHash{job.DocHash})
	if err != nil || len(owned) != 1 {
		test.Fatal("source-bound publication lost root ownership", owned, err)
	}
	header := assertRangePages(test, ctx, storage, job.RepoID, fixture)
	if counts := rangeCountsPG(test, storage, ctx, job.DocHash); counts != [2]int{1, len(header.Pages)} {
		test.Fatal("source-bound publication did not commit the complete range index", counts)
	}
	verified, err := storage.ReadVerifiedDoc(ctx, job.RepoID, job.DocHash)
	if err != nil {
		test.Fatal("read source-bound root", err)
	}
	assertRootProof(test, verified, fixture)
	var stored []byte
	if err := storage.db(ctx).QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, job.DocHash).Scan(&stored); err != nil {
		test.Fatal(err)
	}
	canonical, err := docDecompress(stored)
	if err != nil || !bytes.Equal(canonical, fixture.manifestBytes) {
		test.Fatal("source-bound root body is not the canonical manifest", err)
	}
}

func TestRootSourceWitnessPGUnchangedAndEmptyPublication(test *testing.T) {
	for _, count := range []int{0, 2*domain.DocRangePageEvents + 1} {
		test.Run(fmt.Sprintf("events-%d", count), func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			fixture := rangeFixture(test, count)
			if count == 0 && (len(fixture.manifest.Chunks) != 0 || len(fixture.bodies) != 0) {
				test.Fatal("empty-root fixture unexpectedly has dependencies")
			}
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			sources := make(map[domain.ContentHash]rootSourceWitnessStatePG, len(fixture.bodies))
			for hash := range fixture.bodies {
				sources[hash] = readRootSourceWitnessStatePG(test, storage, ctx, hash)
			}
			before := rootPGImage(test, ctx, storage, repo, namespace)
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("source preparation mutated ownership, bytes, accounting, indexes, or receipt")
			}
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				test.Fatal("unchanged source did not publish", err)
			}
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
			for hash, source := range sources {
				assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
			}
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}

func TestRootSourceWitnessPGChangedGenerationRequiresFreshPreparation(test *testing.T) {
	for _, change := range []string{"update-identical-bytes", "reinsert-same-hash-version-zero"} {
		test.Run(change, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			fixture := rangeFixture(test, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			chunk := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1]
			original := readRootSourceWitnessStatePG(test, storage, ctx, chunk.Hash)
			if original.version != 0 {
				test.Fatal("fresh dependency must start at source version zero", original.version)
			}
			stale := prepareRootStreamingPG(test, storage, ctx, job, directory)
			if change == "update-identical-bytes" {
				if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, chunk.Hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("advance dependency generation without changing valid bytes", err)
				}
			} else {
				transaction, err := storage.pool.Begin(ctx)
				if err != nil {
					test.Fatal(err)
				}
				defer rollbackPG(transaction)
				if tag, err := transaction.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, chunk.Hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("remove dependency grant before source replacement", err)
				}
				if tag, err := transaction.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, chunk.Hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("delete original dependency source", err)
				}
				if _, err := transaction.Exec(ctx, `INSERT INTO blobs(hash,bytes,doc_range_source_version) VALUES($1,$2,0)`, chunk.Hash, original.body); err != nil {
					test.Fatal("reinsert identical source at its original version zero", err)
				}
				if _, err := transaction.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, repo, chunk.Hash); err != nil {
					test.Fatal(err)
				}
				if err := transaction.Commit(ctx); err != nil {
					test.Fatal(err)
				}
			}
			changed := readRootSourceWitnessStatePG(test, storage, ctx, chunk.Hash)
			if !bytes.Equal(changed.body, original.body) {
				test.Fatal("source generation fixture changed the stored bytes")
			}
			if change == "update-identical-bytes" {
				if changed.version != original.version+1 || changed.incarnation != original.incarnation {
					test.Fatal("identical-byte update did not isolate the source version", original, changed)
				}
			} else if changed.version != original.version || changed.incarnation == original.incarnation {
				test.Fatal("same-hash replacement did not isolate the new incarnation at version zero", original, changed)
			}
			decoded, err := rootChunkBytes(ctx, changed.body, chunk.Bytes)
			if err != nil || !bytes.Equal(decoded, fixture.bodies[chunk.Hash]) {
				test.Fatal("replacement source must still contain valid original logical bytes", err)
			}
			before := rootPGImage(test, ctx, storage, repo, namespace)
			if err := stale.Complete(ctx, job, time.Now()); !errors.Is(err, domain.ErrConflict) {
				test.Fatal("stale source witness must be retryable conflict, not success or invalid document", err)
			}
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("stale source publication escaped rollback into ownership, bytes, accounting, indexes, or receipt")
			}
			assertRootSourceWitnessStatePG(test, storage, ctx, chunk.Hash, changed)
			if err := stale.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
			fresh := prepareRootStreamingPG(test, storage, ctx, job, directory)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("fresh preparation mutated the unchanged running job or durable archive")
			}
			if err := fresh.Complete(ctx, job, time.Now()); err != nil {
				test.Fatal("fresh preparation did not recover valid same-hash bytes", err)
			}
			assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
			assertRootSourceWitnessStatePG(test, storage, ctx, chunk.Hash, changed)
			if err := fresh.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}

func TestRootSourceWitnessPGRevokedGrantPreservesNotFoundAndRollback(test *testing.T) {
	for _, change := range []string{"revoked-only", "revoked-and-generation-changed"} {
		test.Run(change, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx := chunkReusePG(test)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			fixture := rangeFixture(test, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
			hash := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
			if change == "revoked-and-generation-changed" {
				if tag, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal(err)
				}
			}
			if tag, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil || tag.RowsAffected() != 1 {
				test.Fatal("revoke prepared dependency grant", err)
			}
			source := readRootSourceWitnessStatePG(test, storage, ctx, hash)
			before := rootPGImage(test, ctx, storage, repo, namespace)
			if err := publication.Complete(ctx, job, time.Now()); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("source witness must not replace missing ownership with conflict or success", err)
			}
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if after := rootPGImage(test, ctx, storage, repo, namespace); after != before {
				test.Fatal("revoked publication changed grants, root bytes, indexes, accounting, or receipt")
			}
			var grants int
			if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash).Scan(&grants); err != nil || grants != 0 {
				test.Fatal("publication repaired a revoked dependency grant", grants, err)
			}
			assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}

func TestRootSourceWitnessPGFinalIncarnationLockLastsThroughOuterCommit(test *testing.T) {
	directory := rangeSpoolTempDir(test)
	storage, ctx := chunkReusePG(test)
	ctx = rootPublicationContext(ctx)
	fixture := rangeFixture(test, 40*domain.DocRangePageEvents+1)
	order := conversationChunkOrder(fixture.manifest)
	slices.Sort(order)
	order = slices.Compact(order)
	if len(order) < 2 {
		test.Fatal("incarnation retention fixture must have multiple unique dependencies")
	}
	hash := order[len(order)-1]
	job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, "")
	publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
	source := readRootSourceWitnessStatePG(test, storage, ctx, hash)
	peer, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { rollbackPG(peer) })
	observer, err := storage.pool.Acquire(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(observer.Release)
	peerPID := peer.Conn().PgConn().PID()
	var peerContext context.Context
	var peerDone <-chan error
	err = storage.WithinRepository(ctx, job.RepoID, func(bound context.Context) error {
		if err := publication.Complete(bound, job, time.Now()); err != nil {
			return err
		}
		assertRootStreamingUnpublishedPG(test, storage, ctx, job)
		current, err := storage.GetDocJob(bound, job.RepoID, job.ID)
		if err != nil || current.State != "completed" {
			test.Fatal("borrowed completion did not stage its receipt inside the outer transaction", current, err)
		}
		if counts := rangeCountsPG(test, storage, bound, job.DocHash); counts[0] != 1 || counts[1] == 0 {
			test.Fatal("borrowed completion did not stage the range index inside the outer transaction", counts)
		}
		publisherPID := bound.Value(repositoryTxKey{}).(*repositoryTx).Conn().PgConn().PID()
		if publisherPID == peerPID || publisherPID == observer.Conn().PgConn().PID() {
			test.Fatal("incarnation lock proof must use independent PostgreSQL backends")
		}
		peerContext, _, peerDone = authorityScopeRunPG(test, ctx, func(work context.Context) error {
			tag, err := peer.Exec(work, `DELETE FROM blob_source_incarnations WHERE hash=$1`, hash)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("peer deletion did not reach the final dependency incarnation: rows=%d", tag.RowsAffected())
			}
			return peer.Rollback(work)
		})
		authorityScopeWaitPG(test, peerContext, observer.Conn(), publisherPID, peerPID, "DELETE FROM blob_source_incarnations", peerDone)
		assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
		assertRootStreamingUnpublishedPG(test, storage, ctx, job)
		select {
		case err := <-peerDone:
			test.Fatal("peer invalidated the source witness before outer commit", err)
		default:
		}
		return nil
	})
	if err != nil {
		test.Fatal("outer borrowed publication did not commit", err)
	}
	authorityScopeDonePG(test, peerContext, peerDone)
	assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
	assertRootSourceWitnessStatePG(test, storage, ctx, hash, source)
	if err := publication.Close(); err != nil {
		test.Fatal(err)
	}
	assertRangeSpoolsRemoved(test, directory)
}
