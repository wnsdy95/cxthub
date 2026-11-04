//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func rangeCountsPG(t *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) [2]int {
	t.Helper()
	var counts [2]int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_indexes_v1 WHERE hash=$1),
 (SELECT count(*) FROM doc_range_pages_v1 WHERE doc_hash=$1)`, hash).Scan(&counts[0], &counts[1]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestDocRangePGColdRebuildAndWarmSelection(t *testing.T) {
	spoolDirectory := rangeSpoolTempDir(t)
	storage, ctx := chunkReusePG(t)
	repo, namespace := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 2*domain.DocRangePageEvents+1)
	seedRootPG(t, ctx, storage, repo, fixture)
	before := rootPGImage(t, ctx, storage, repo, namespace)
	if _, err := storage.DocRangeIndex(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatal("cold header", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if err := storage.EnsureDocRangeIndex(read, repo, fixture.hash); err == nil {
			t.Fatal("cold ensure nested a write inside a readonly transaction")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
		t.Fatal("readonly ensure published", got)
	}
	stop := errors.New("rollback range maintenance")
	err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		if err := storage.EnsureDocRangeIndex(write, repo, fixture.hash); err != nil {
			return err
		}
		assertRangePages(t, write, storage, repo, fixture)
		if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
			t.Fatal("index escaped uncommitted transaction", got)
		}
		return stop
	})
	if !errors.Is(err, stop) || rangeCountsPG(t, storage, ctx, fixture.hash) != [2]int{} {
		t.Fatal("index escaped outer rollback", err)
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
	if after := rootPGImage(t, ctx, storage, repo, namespace); after != before {
		t.Fatal("derived maintenance rewrote archive, ownership, usage or jobs")
	}
	reopened, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	header := assertRangePages(t, ctx, reopened, repo, fixture)
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("damaged source")); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_pages_v1 SET bytes=$2 WHERE doc_hash=$1 AND ordinal=1`, fixture.hash, []byte("damaged unselected page")); err != nil {
		t.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if err := reopened.EnsureDocRangeIndex(read, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign transaction was not rejected", err)
		}
		if err := storage.EnsureDocRangeIndex(read, repo, fixture.hash); err != nil {
			return err
		}
		_, err := storage.DocRangePage(read, repo, header, 0)
		return err
	}); err != nil {
		t.Fatal("warm read touched unrelated data", err)
	}
	if _, err := reopened.DocRangePage(ctx, repo, header, 1); !errors.Is(err, domain.ErrIntegrity) {
		t.Fatal("selected corruption accepted", err)
	}
	if _, err := storage.ReadVerifiedDoc(ctx, repo, fixture.hash); err == nil {
		t.Fatal("strict reader stopped auditing source")
	}
}

func TestDocRangePGManyShortEventsPublicationAndBackfill(t *testing.T) {
	spoolDirectory := rangeSpoolTempDir(t)
	storage, ctx := chunkReusePG(t)
	ctx = rootPublicationContext(ctx)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := manyShortRangeFixture(t)
	job, proof := rootPublicationClaimPG(t, storage, ctx, fixture, repo)
	publication, err := storage.PrepareDocJob(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		t.Fatal("many-short-event publication failed", err)
	}
	header, err := storage.DocRangeIndex(ctx, repo, fixture.hash)
	if err != nil {
		t.Fatal(err)
	}
	var pageBytes int64
	if err := storage.pool.QueryRow(ctx, `SELECT sum(octet_length(bytes)) FROM doc_range_pages_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&pageBytes); err != nil {
		t.Fatal(err)
	}
	if pageBytes <= domain.MaxDocRangeIndexBytes {
		t.Fatal("fixture did not exceed old aggregate cap", pageBytes)
	}
	done, err := storage.GetDocJob(ctx, repo, job.ID)
	if err != nil || done.State != "completed" {
		t.Fatal("publication lost completion", err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_range_indexes_v1 WHERE hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal("many-short-event backfill failed", err)
	}
	rebuilt, err := storage.DocRangeIndex(ctx, repo, fixture.hash)
	if err != nil || !reflect.DeepEqual(rebuilt, header) {
		t.Fatal("backfill changed verified header", err)
	}
	for _, position := range []int{0, len(header.Pages) / 2, len(header.Pages) - 1} {
		if _, err := storage.DocRangePage(ctx, repo, header, position); err != nil {
			t.Fatal("selected rebuilt page", position, err)
		}
	}
	assertRangeSpoolsRemoved(t, spoolDirectory)
}

func TestDocRangePGPageOrdinalUsesDocumentBound(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 1)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	for _, position := range []int{32768, domain.MaxDocRangePages - 1, domain.MaxDocRangePages} {
		tx, err := storage.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, `INSERT INTO doc_range_pages_v1(doc_hash,ordinal,bytes) VALUES($1,$2,$3)`, fixture.hash, position, []byte("[]"))
		rollbackPG(tx)
		if (err == nil) != (position < domain.MaxDocRangePages) {
			t.Fatal("SQL ordinal bounds differ from domain", position, err)
		}
	}
}

func TestDocRangePGGlobalDedupAndOwnership(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	foreign, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, domain.DocRangePageEvents+1)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	header := assertRangePages(t, ctx, storage, repo, fixture)
	for _, read := range []func() error{
		func() error { _, err := storage.DocRangeIndex(ctx, foreign, fixture.hash); return err },
		func() error { _, err := storage.DocRangePage(ctx, foreign, header, 0); return err },
		func() error { return storage.EnsureDocRangeIndex(ctx, foreign, fixture.hash) },
	} {
		if err := read(); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("global index leaked to foreign repository", err)
		}
	}
	seedRootPG(t, ctx, storage, foreign, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, foreign, fixture.hash); err != nil {
		t.Fatal(err)
	}
	assertRangePages(t, ctx, storage, foreign, fixture)
	if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{1, 2} {
		t.Fatal("global index was not deduplicated", got)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.DocRangeIndex(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("header ignored revoked ownership", err)
	}
	if _, err := storage.DocRangePage(ctx, repo, header, 0); !errors.Is(err, domain.ErrNotFound) {
		t.Fatal("page ignored revoked ownership", err)
	}
	assertRangePages(t, ctx, storage, foreign, fixture)
	if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, foreign, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
		t.Fatal("deleted source retained orphan derived pages", got)
	}
}

func TestDocRangePGRejectsCurrentHeaderAndPageCorruption(t *testing.T) {
	for _, damage := range []string{"root", "header", "header-manifest", "page", "missing-page", "caller-header"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 1)
			seedRootPG(t, ctx, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			header := assertRangePages(t, ctx, storage, repo, fixture)
			var err error
			switch damage {
			case "root":
				changed := rangeFixture(t, 2)
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, docCompress(changed.manifestBytes))
			case "header":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_indexes_v1 SET header=$2 WHERE hash=$1`, fixture.hash, []byte("{}"))
			case "header-manifest":
				changed := header
				changed.Manifest.EventCount++
				raw, _ := json.Marshal(changed)
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_indexes_v1 SET header=$2 WHERE hash=$1`, fixture.hash, raw)
			case "page":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_pages_v1 SET bytes=$2 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash, []byte("[]"))
			case "missing-page":
				_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_pages_v1 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash)
			case "caller-header":
				header.Pages[0] = domain.HashContent([]byte("forged page"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := storage.DocRangePage(ctx, repo, header, 0); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("current corruption accepted", err)
			}
		})
	}
}

func TestDocRangePGColdFailureDoesNotPublish(t *testing.T) {
	for _, damage := range []string{"corrupt", "revoked", "foreign-transaction", "canceled"} {
		t.Run(damage, func(t *testing.T) {
			spoolDirectory := rangeSpoolTempDir(t)
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 1)
			seedRootPG(t, ctx, storage, repo, fixture)
			var err error
			call := ctx
			switch damage {
			case "corrupt":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("corrupt"))
			case "revoked":
				_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, fixture.manifest.Chunks[0].Hash)
			case "foreign-transaction":
				foreign, _ := rootPGRepo(t, ctx, storage)
				err = storage.WithinRepository(ctx, foreign, func(write context.Context) error {
					if err := storage.EnsureDocRangeIndex(write, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
						t.Fatal("maintenance crossed repository transaction", err)
					}
					return nil
				})
			case "canceled":
				var cancel context.CancelFunc
				call, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if damage != "foreign-transaction" {
				if err := storage.EnsureDocRangeIndex(call, repo, fixture.hash); err == nil {
					t.Fatal("invalid source admitted")
				}
			}
			if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
				t.Fatal("failed verification exposed index", got)
			}
			assertRangeSpoolsRemoved(t, spoolDirectory)
		})
	}
}

func TestDocRangePGPublicationAtomicity(t *testing.T) {
	for _, mode := range []string{"empty", "pages", "corrupt", "root-corrupt", "expired", "rollback", "quota"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			ctx = rootPublicationContext(ctx)
			repo, namespace := rootPGRepo(t, ctx, storage)
			count := domain.DocRangePageEvents + 1
			if mode == "empty" || mode == "quota" {
				count = 0
			}
			fixture := rangeFixture(t, count)
			job, doc := rootPublicationClaimPG(t, storage, ctx, fixture, repo)
			publication, err := storage.PrepareDocJob(ctx, doc)
			if err != nil {
				t.Fatal(err)
			}
			if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
				t.Fatal("preparation exposed index", got)
			}
			if got := generationCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
				t.Fatal("preparation exposed generation", got)
			}
			switch mode {
			case "corrupt":
				_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("corrupt after proof"))
			case "root-corrupt":
				_, err = storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.hash, []byte("corrupt root after proof"))
			case "expired":
				old := job
				old.LeaseUntil = time.Now().Add(-time.Second)
				err = storage.writeDocJob(ctx, old)
			case "quota":
				err = storage.ConfigureStoragePolicy(ctx, namespace, "range-limit-"+namespace, 0, domain.StoragePolicy{Plan: "free", IncludedBytes: 1}, "test-range-publication", "synthetic range quota")
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "rollback" {
				stop := errors.New("rollback publication and index")
				err = storage.WithinRepository(ctx, repo, func(write context.Context) error {
					if err := publication.Complete(write, job, time.Now()); err != nil {
						return err
					}
					assertRangePages(t, write, storage, repo, fixture)
					if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
						t.Fatal("uncommitted index was visible", got)
					}
					if got := generationCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
						t.Fatal("uncommitted generation was visible", got)
					}
					return stop
				})
				if !errors.Is(err, stop) {
					t.Fatal(err)
				}
			} else {
				err = publication.Complete(ctx, job, time.Now())
			}
			if mode != "empty" && mode != "pages" {
				if err == nil || rangeCountsPG(t, storage, ctx, fixture.hash) != [2]int{} {
					t.Fatal("failed publication exposed index", err)
				}
				if got := generationCountsPG(t, storage, ctx, fixture.hash); got != [2]int{} {
					t.Fatal("failed publication exposed generation", got)
				}
				assertDocNotPublishedPG(t, storage, ctx, job)
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRangePages(t, ctx, storage, repo, fixture)
			if got := generationCountsPG(t, storage, ctx, fixture.hash); got[0] != 1 || mode == "pages" && got[1] == 0 {
				t.Fatal("publication omitted generation", got)
			}
			rootProjectionNoIndex(t, storage, ctx, fixture.hash)
			done, err := storage.GetDocJob(ctx, repo, job.ID)
			if err != nil || done.State != "completed" {
				t.Fatal("index publication lost completion", done, err)
			}
		})
	}
}

func TestDocRangePGColdBuildRetainsSourceThroughCommit(t *testing.T) {
	for _, mutation := range []string{"root-grant", "root-bytes", "chunk-grant", "chunk-bytes"} {
		t.Run(mutation, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 1)
			seedRootPG(t, ctx, storage, repo, fixture)
			var mutationDone chan error
			err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
				if err := storage.EnsureDocRangeIndex(write, repo, fixture.hash); err != nil {
					return err
				}
				mutationDone = make(chan error, 1)
				go func() {
					probe, cancel := context.WithTimeout(ctx, 2*time.Second)
					defer cancel()
					var err error
					switch mutation {
					case "root-grant":
						_, err = storage.pool.Exec(probe, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash)
					case "root-bytes":
						_, err = storage.pool.Exec(probe, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, []byte("post-commit damage"))
					case "chunk-grant":
						_, err = storage.pool.Exec(probe, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, fixture.manifest.Chunks[0].Hash)
					case "chunk-bytes":
						_, err = storage.pool.Exec(probe, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("post-commit damage"))
					}
					mutationDone <- err
				}()
				select {
				case err := <-mutationDone:
					t.Errorf("mutation crossed source retention: %v", err)
					mutationDone = nil
				case <-time.After(40 * time.Millisecond):
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if mutationDone != nil {
				if err := <-mutationDone; err != nil {
					t.Fatal(err)
				}
			}
			counts := generationCountsPG(t, storage, ctx, fixture.hash)
			if mutation == "root-bytes" {
				if counts != [2]int{} {
					t.Fatal("root update left stale generation after publisher commit", counts)
				}
			} else if counts[0] != 1 || counts[1] == 0 {
				t.Fatal("unrelated source mutation removed global generation", counts)
			}
		})
	}
}

func TestDocRangePGWarmReadsHaveConstantQueries(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := uniqueRootPublicationFixturePG(t, strings.Repeat(string(repo), 3*domain.ConversationManifestChunkBytes/len(repo)))
	seedRootPG(t, ctx, storage, repo, fixture)
	if len(fixture.manifest.Chunks) < 3 {
		t.Fatal("fixture must cross multiple chunks")
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = storedDocQueryTracer{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	reader := &PostgresStore{pool: pool}
	defer reader.Close()
	counts := &storedDocQueryCount{}
	counted := context.WithValue(ctx, storedDocQueryCountKey{}, counts)
	if err := reader.WithinReadSnapshot(counted, func(read context.Context) error {
		if err := reader.EnsureDocRangeIndex(read, repo, fixture.hash); err != nil {
			return err
		}
		header, err := reader.DocRangeIndex(read, repo, fixture.hash)
		if err != nil {
			return err
		}
		_, err = reader.DocRangePage(read, repo, header, 0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := counts.reads.Load(); got != 3 {
		t.Fatalf("warm ensure/header/page made %d reads, want three and no chunk reads", got)
	}
}

func TestDocRangePGConcurrentColdBuild(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 2*domain.DocRangePageEvents+1)
	seedRootPG(t, ctx, storage, repo, fixture)
	ready := make(chan struct{})
	results := make(chan error, 4)
	for worker := 0; worker < cap(results); worker++ {
		go func() {
			<-ready
			results <- storage.EnsureDocRangeIndex(ctx, repo, fixture.hash)
		}()
	}
	close(ready)
	for worker := 0; worker < cap(results); worker++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	assertRangePages(t, ctx, storage, repo, fixture)
	if got := rangeCountsPG(t, storage, ctx, fixture.hash); got != [2]int{1, 3} {
		t.Fatal("concurrent maintenance published partial or duplicate metadata", got)
	}
}
