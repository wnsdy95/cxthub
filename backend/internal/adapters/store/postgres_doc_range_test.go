//go:build postgres

package store

import (
	"context"
	"errors"
	"testing"
	"time"

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
