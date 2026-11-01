//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func prepareRootStreamingPG(t *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob, directory string) outbound.PreparedRootDocPublication {
	t.Helper()
	publication, err := storage.PrepareRootDocJob(ctx, job)
	if err != nil {
		assertRangeSpoolsRemoved(t, directory)
		t.Fatal(err)
	}
	if publication == nil {
		t.Fatal("preparation returned no publication")
	}
	t.Cleanup(func() {
		if err := publication.Close(); err != nil {
			t.Error("close streaming publication", err)
		}
		assertRangeSpoolsRemoved(t, directory)
	})
	return publication
}

func assertRootStreamingUnpublishedPG(t *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob) {
	t.Helper()
	if job.State == "running" {
		assertDocNotPublishedPG(t, storage, ctx, job)
	} else {
		current, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
		if err != nil || current.State != job.State {
			t.Fatal("unsuccessful publication changed receipt", current.State, err)
		}
		if owned, err := storage.HasDocs(ctx, job.RepoID, []domain.ContentHash{job.DocHash}); err != nil || len(owned) != 0 {
			t.Fatal("unsuccessful publication granted ownership", owned, err)
		}
		rootProjectionNoIndex(t, storage, ctx, job.DocHash)
	}
	if got := rangeCountsPG(t, storage, ctx, job.DocHash); got != [2]int{} {
		t.Fatal("range header/pages escaped unsuccessful publication", got)
	}
	var count int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, job.DocHash).Scan(&count); err != nil || count != 0 {
		t.Fatal("root body escaped unsuccessful publication", count, err)
	}
}

func TestRootStreamingPGPagesReceiptAndCleanup(t *testing.T) {
	for _, count := range []int{0, 2*domain.DocRangePageEvents + 1} {
		t.Run(fmt.Sprintf("events-%d", count), func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, count)
			job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, repo)
			before := rootPGImage(t, ctx, storage, repo, namespace)
			publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
			assertRootStreamingUnpublishedPG(t, storage, ctx, job)
			if after := rootPGImage(t, ctx, storage, repo, namespace); after != before {
				t.Fatal("preparation changed durable archive, ownership, accounting or receipt")
			}
			paths, err := filepath.Glob(filepath.Join(directory, "cxthub-doc-range-*"))
			if err != nil || len(paths) != 2 {
				t.Fatal("preparation did not retain its original and derived bounded spools", paths, err)
			}
			for _, path := range paths {
				info, err := os.Stat(path)
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0600 || count > 0 && info.Size() == 0 {
					t.Fatal("spool is empty or not private", info.Mode(), info.Size())
				}
			}
			if err := publication.Complete(ctx, job, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := publication.Close(); err != nil {
				t.Fatal(err)
			}
			assertRangeSpoolsRemoved(t, directory)
			reopened, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			header := assertRangePages(t, ctx, reopened, repo, fixture)
			if got := rangeCountsPG(t, reopened, ctx, fixture.hash); got != [2]int{1, len(header.Pages)} {
				t.Fatal("incomplete persisted range index", got)
			}
			done, err := reopened.GetDocJob(ctx, repo, job.ID)
			if err != nil || done.State != "completed" || done.DocumentRef() != job.DocumentRef() || done.Version != job.Version || !done.LeaseUntil.IsZero() {
				t.Fatal("invalid completion receipt", done, err)
			}
			replay, err := reopened.EnqueueDocJob(ctx, rootPublicationJob(t, repo, fixture))
			if err != nil || replay.State != "completed" || replay.Version != job.Version {
				t.Fatal("replay lost durable receipt", replay, err)
			}
			var raw []byte
			if err := reopened.pool.QueryRow(ctx, `SELECT bytes FROM blobs WHERE hash=$1`, fixture.hash).Scan(&raw); err != nil {
				t.Fatal(err)
			}
			raw, err = docDecompress(raw)
			if err != nil || !bytes.Equal(raw, fixture.manifestBytes) {
				t.Fatal("root body is not the canonical manifest", err)
			}
			wantBytes := int64(len(docCompress(fixture.manifestBytes)))
			for _, body := range fixture.bodies {
				wantBytes += int64(len(docCompress(body)))
			}
			usage, err := reopened.ReadStorageUsage(ctx, namespace, time.Now().Add(-time.Hour), time.Now())
			if err != nil || usage.CurrentBytes != wantBytes {
				t.Fatal("publication accounting changed unique compressed ownership", usage.CurrentBytes, wantBytes, err)
			}
			rootProjectionNoIndex(t, storage, ctx, fixture.hash)
		})
	}
}

func TestRootStreamingPGPrepareRejectsInvalidSemanticsAndCleansSpool(t *testing.T) {
	for _, damage := range []string{"event-count", "late-sequence-regression", "late-unknown-field", "late-chunk-corrupt", "revoked", "canceled"} {
		t.Run(damage, func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 40*domain.DocRangePageEvents+1)
			if len(fixture.manifest.Chunks) < 2 {
				t.Fatal("fixture must span multiple chunks")
			}
			last := len(fixture.manifest.Chunks) - 1
			chunk := fixture.manifest.Chunks[last]
			switch damage {
			case "event-count":
				fixture.manifest.EventCount++
			case "late-sequence-regression", "late-unknown-field":
				needle := []byte(fmt.Sprintf(`"seq":%d`, fixture.manifest.EventCount))
				replacement := []byte(`"seq":0`)
				if damage == "late-unknown-field" {
					replacement = append(bytes.Clone(needle), []byte(`,"unknown":true`)...)
				}
				body := bytes.Replace(fixture.bodies[chunk.Hash], needle, replacement, 1)
				if bytes.Equal(body, fixture.bodies[chunk.Hash]) {
					t.Fatal("late semantic mutation did not change the source")
				}
				delete(fixture.bodies, chunk.Hash)
				fixture.manifest.StreamBytes += int64(len(body)) - chunk.Bytes
				chunk.Hash, chunk.Bytes = domain.HashContent(body), int64(len(body))
				fixture.manifest.Chunks[last] = chunk
				fixture.bodies[chunk.Hash] = body
			}
			var err error
			fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
			if err != nil {
				t.Fatal(err)
			}
			fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
				t.Fatal(err)
			}
			if _, _, err := storage.PutChunks(ctx, repo, fixture.bodies); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.EnqueueDocJob(ctx, rootPublicationJob(t, repo, fixture)); err != nil {
				t.Fatal(err)
			}
			job, err := storage.ClaimDocJob(ctx, repo, time.Now(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			prepareCtx := ctx
			switch damage {
			case "late-chunk-corrupt":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk.Hash, docCompress(bytes.Repeat([]byte("x"), int(chunk.Bytes))))
			case "revoked":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, chunk.Hash)
			case "canceled":
				var cancel context.CancelFunc
				prepareCtx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			publication, err := storage.PrepareRootDocJob(prepareCtx, job)
			if publication != nil {
				defer publication.Close()
				t.Error("invalid preparation returned a publication")
			}
			want := domain.ErrConversationManifest
			if damage == "revoked" {
				want = domain.ErrNotFound
			} else if damage == "canceled" {
				want = context.Canceled
			}
			if !errors.Is(err, want) {
				t.Fatalf("invalid or incomplete document: got %v, want %v", err, want)
			}
			assertRangeSpoolsRemoved(t, directory)
			assertRootStreamingUnpublishedPG(t, storage, ctx, job)
		})
	}
}

func TestRootStreamingPGCompleteRechecksCurrentClosureAndFence(t *testing.T) {
	for _, damage := range []string{"corrupt", "revoked", "foreign-only", "missing", "root-winner", "expired", "reclaimed", "caller-version", "compatibility", "canceled", "recompressed"} {
		t.Run(damage, func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			fixture := rangeFixture(t, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, "")
			publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
			chunk := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1]
			candidate, completeCtx := job, ctx
			want := domain.ErrConflict
			var err error
			switch damage {
			case "corrupt":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk.Hash, docCompress(bytes.Repeat([]byte("x"), int(chunk.Bytes))))
				want = domain.ErrIntegrity
			case "revoked", "foreign-only", "missing":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, job.RepoID, chunk.Hash)
				if err != nil {
					t.Fatal(err)
				}
				if damage == "missing" {
					_, err = storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, chunk.Hash)
				} else if damage == "foreign-only" {
					foreign, _ := rootPGRepo(t, ctx, storage)
					_, err = storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'chunk',$2)`, foreign, chunk.Hash)
				}
				want = domain.ErrNotFound
			case "root-winner":
				_, err = storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.hash, []byte("corrupt root conflict winner"))
				want = domain.ErrIntegrity
			case "expired":
				expired := job
				expired.LeaseUntil = time.Now().Add(-time.Second)
				err = storage.writeDocJob(ctx, expired)
			case "reclaimed":
				_, err = storage.ClaimDocJob(ctx, job.RepoID, job.LeaseUntil.Add(time.Second), time.Minute)
			case "caller-version":
				candidate.Version++
			case "compatibility":
				completeCtx = outbound.WithDocumentIdentityCompatibility(ctx, []domain.DocumentIdentity{domain.DocumentIdentityLegacy}, []domain.DocumentIdentity{domain.DocumentIdentityLegacy})
				want = domain.ErrDocumentIdentityUpgradeRequired
			case "canceled":
				var cancel context.CancelFunc
				completeCtx, cancel = context.WithCancel(ctx)
				cancel()
				want = context.Canceled
			case "recompressed":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, chunk.Hash, fixture.bodies[chunk.Hash])
			}
			if err != nil {
				t.Fatal(err)
			}
			err = publication.Complete(completeCtx, candidate, job.UpdatedAt)
			if !errors.Is(err, want) {
				t.Fatalf("got %v, want %v", err, want)
			}
			if damage == "recompressed" {
				assertRootStreamingUnpublishedPG(t, storage, ctx, job)
				if err := publication.Close(); err != nil {
					t.Fatal(err)
				}
				fresh := prepareRootStreamingPG(t, storage, ctx, job, directory)
				if err := fresh.Complete(ctx, job, time.Now()); err != nil {
					t.Fatal("fresh source generation did not preserve recompression support", err)
				}
				assertRangePages(t, ctx, storage, job.RepoID, fixture)
				return
			}
			if damage == "root-winner" {
				assertDocNotPublishedPG(t, storage, ctx, job)
				if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
					t.Fatal("corrupt winner received range metadata", got)
				}
			} else {
				assertRootStreamingUnpublishedPG(t, storage, ctx, job)
			}
			if damage == "revoked" || damage == "foreign-only" || damage == "missing" {
				var count int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, job.RepoID, chunk.Hash).Scan(&count); err != nil || count != 0 {
					t.Fatal("publication recreated revoked chunk ownership", count, err)
				}
				if damage == "missing" {
					if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM blobs WHERE hash=$1`, chunk.Hash).Scan(&count); err != nil || count != 0 {
						t.Fatal("publication reconstructed a missing chunk", count, err)
					}
				}
			}
		})
	}
}

func TestRootStreamingPGRejectsSwappedJobAndRepository(t *testing.T) {
	for _, swap := range []string{"job", "repository", "transaction-owner"} {
		t.Run(swap, func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			fixture := rangeFixture(t, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, "")
			publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
			candidate := job
			var err error
			switch swap {
			case "repository":
				candidate, _ = rootPublicationClaimPG(t, storage, ctx, fixture, "")
				err = publication.Complete(ctx, candidate, time.Now())
			case "job":
				other := rangeFixture(t, 1)
				if _, _, err := storage.PutChunks(ctx, job.RepoID, other.bodies); err != nil {
					t.Fatal(err)
				}
				candidate = rootPublicationJob(t, job.RepoID, other)
				if _, err := storage.EnqueueDocJob(ctx, candidate); err != nil {
					t.Fatal(err)
				}
				job.State = "retrying"
				job.LeaseUntil = time.Time{}
				job.NextAttempt = time.Now().Add(time.Hour)
				if err := storage.FinishDocJob(ctx, job, time.Now()); err != nil {
					t.Fatal(err)
				}
				candidate, err = storage.ClaimDocJob(ctx, job.RepoID, time.Now(), time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				err = publication.Complete(ctx, candidate, time.Now())
			case "transaction-owner":
				peer, openErr := NewPostgresStore(ctx, storage.pool.Config().ConnString())
				if openErr != nil {
					t.Fatal(openErr)
				}
				defer peer.Close()
				err = peer.WithinRepository(ctx, job.RepoID, func(bound context.Context) error {
					return publication.Complete(bound, candidate, time.Now())
				})
			}
			if !errors.Is(err, domain.ErrConflict) {
				t.Fatal("swapped job/repository/transaction owner was not fenced", err)
			}
			assertRootStreamingUnpublishedPG(t, storage, ctx, job)
			assertRootStreamingUnpublishedPG(t, storage, ctx, candidate)
		})
	}
}

func TestRootStreamingPGRollbackAndChangedStoragePolicy(t *testing.T) {
	for _, mode := range []string{"enclosing-rollback", "quota", "spool-corrupt", "closed"} {
		t.Run(mode, func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 2*domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, repo)
			publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
			before := rootPGImage(t, ctx, storage, repo, namespace)
			var err error
			switch mode {
			case "enclosing-rollback":
				stop := errors.New("rollback completed root publication")
				err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
					if err := publication.Complete(bound, job, time.Now()); err != nil {
						return err
					}
					assertRangePages(t, bound, storage, repo, fixture)
					completed, err := storage.GetDocJob(bound, repo, job.ID)
					if err != nil || completed.State != "completed" {
						t.Fatal("receipt is not part of the publishing transaction", err)
					}
					assertRootStreamingUnpublishedPG(t, storage, ctx, job)
					return stop
				})
				if !errors.Is(err, stop) {
					t.Fatal("did not reach enclosing rollback", err)
				}
			case "quota":
				if err := storage.ConfigureStoragePolicy(ctx, namespace, "stream-limit-"+namespace, 0, domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, "streaming-root-test", "quota changed after preparation"); err != nil {
					t.Fatal(err)
				}
				before = rootPGImage(t, ctx, storage, repo, namespace)
				err = publication.Complete(ctx, job, time.Now())
				if !errors.Is(err, domain.ErrStorageLimit) {
					t.Fatal("current storage policy was not enforced", err)
				}
			case "spool-corrupt":
				paths, globErr := filepath.Glob(filepath.Join(directory, "cxthub-doc-range-*"))
				if globErr != nil || len(paths) != 2 {
					t.Fatal(paths, globErr)
				}
				sourcePath := publication.(*pgStreamingRootPublication).spool.file.Name()
				if err := os.WriteFile(sourcePath, []byte("invalid page"), 0600); err != nil {
					t.Fatal(err)
				}
				err = publication.Complete(ctx, job, time.Now())
			case "closed":
				if err := publication.Close(); err != nil {
					t.Fatal(err)
				}
				err = publication.Complete(ctx, job, time.Now())
			}
			if err == nil {
				t.Fatal("failed publication succeeded")
			}
			assertRootStreamingUnpublishedPG(t, storage, ctx, job)
			if after := rootPGImage(t, ctx, storage, repo, namespace); after != before {
				t.Fatal("failed publication changed archive, ownership, accounting or receipt")
			}
		})
	}
}

func TestRootStreamingPGCurrentRepositoryOptIn(t *testing.T) {
	for _, optIn := range []bool{false, true} {
		t.Run(fmt.Sprintf("opt-in-after-prepare-%t", optIn), func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, domain.DocRangePageEvents+1)
			if _, _, err := storage.PutChunks(ctx, repo, fixture.bodies); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.EnqueueDocJob(ctx, rootPublicationJob(t, repo, fixture)); err != nil {
				t.Fatal(err)
			}
			job, err := storage.ClaimDocJob(ctx, repo, time.Now(), time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
			if optIn {
				if err := storage.RequireDocumentIdentity(ctx, repo, domain.DocumentIdentityRootV1); err != nil {
					t.Fatal(err)
				}
			}
			err = publication.Complete(ctx, job, time.Now())
			if optIn {
				if err != nil {
					t.Fatal("completion used stale repository policy", err)
				}
				assertRangePages(t, ctx, storage, repo, fixture)
			} else {
				if !errors.Is(err, domain.ErrRootPublicationDisabled) {
					t.Fatal("root published without current repository opt-in", err)
				}
				assertRootStreamingUnpublishedPG(t, storage, ctx, job)
			}
		})
	}
}

func TestRootStreamingPGChunkShareLocksLastThroughOuterCommit(t *testing.T) {
	for _, mutation := range []string{"grant-delete", "byte-update"} {
		t.Run(mutation, func(t *testing.T) {
			directory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			fixture := rangeFixture(t, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, "")
			publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
			query := `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`
			args := []any{job.RepoID, fixture.manifest.Chunks[0].Hash}
			if mutation == "byte-update" {
				query = `UPDATE blobs SET bytes=$2 WHERE hash=$1`
				args = []any{fixture.manifest.Chunks[0].Hash, []byte("post-commit corruption")}
			}
			err := storage.WithinRepository(ctx, job.RepoID, func(bound context.Context) error {
				if err := publication.Complete(bound, job, time.Now()); err != nil {
					return err
				}
				assertRootStreamingUnpublishedPG(t, storage, ctx, job)
				probe, err := storage.pool.Begin(ctx)
				if err != nil {
					return err
				}
				defer rollbackPG(probe)
				if _, err := probe.Exec(ctx, `SET LOCAL lock_timeout='100ms'`); err != nil {
					return err
				}
				_, err = probe.Exec(ctx, query, args...)
				var postgresError *pgconn.PgError
				if !errors.As(err, &postgresError) || postgresError.Code != "55P03" {
					t.Fatalf("chunk grant/body was mutable before outer commit: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			assertRangePages(t, ctx, storage, job.RepoID, fixture)
			if tag, err := storage.pool.Exec(ctx, query, args...); err != nil || tag.RowsAffected() != 1 {
				t.Fatal("mutation did not proceed after outer commit", err)
			}
			if _, err := storage.ReadVerifiedDoc(ctx, job.RepoID, fixture.hash); err == nil {
				t.Fatal("completion receipt concealed later source damage")
			}
		})
	}
}

func TestRootStreamingPGLeaseExpiresBehindChunkLock(t *testing.T) {
	directory := rangeSpoolTempDir(t)
	storage, ctx := chunkReusePG(t)
	ctx = rootPublicationContext(ctx)
	fixture := rangeFixture(t, domain.DocRangePageEvents+1)
	job, _ := rootPublicationClaimPG(t, storage, ctx, fixture, "")
	publication := prepareRootStreamingPG(t, storage, ctx, job, directory)
	blocker, err := storage.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(blocker)
	if _, err := blocker.Exec(ctx, `SELECT hash FROM blobs WHERE hash=$1 FOR NO KEY UPDATE`, fixture.manifest.Chunks[0].Hash); err != nil {
		t.Fatal(err)
	}
	job.LeaseUntil = time.Now().Add(time.Second)
	if err := storage.writeDocJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer close(done)
		done <- publication.Complete(ctx, job, job.UpdatedAt)
	}()
	defer func() {
		rollbackPG(blocker)
		select {
		case <-done:
		case <-ctx.Done():
		}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var blocked bool
		if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE $1=ANY(pg_blocking_pids(pid)))`, blocker.Conn().PgConn().PID()).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatal("completion never waited for the chunk write lock", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("completion did not reach the chunk lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if remaining := time.Until(job.LeaseUntil.Add(30 * time.Millisecond)); remaining > 0 {
		time.Sleep(remaining)
	}
	if err := blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	err = <-done
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatal("expired lock waiter published with an old timestamp", err)
	}
	assertRootStreamingUnpublishedPG(t, storage, ctx, job)
}
