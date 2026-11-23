//go:build postgres

package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func assertPreparedRootIndexesSpoolsPG(test *testing.T, publication *pgStreamingRootPublication, directory string, events int) {
	test.Helper()
	if publication == nil || publication.preparedStreamingRootDoc == nil || publication.spool == nil || publication.indexes == nil || publication.indexes.spool == nil {
		test.Fatal("actual preparation did not retain the original and derived index spools")
	}
	if publication.spool == publication.indexes.spool || publication.spool.file == publication.indexes.spool.file {
		test.Fatal("derived index spool aliases the original verified-range spool")
	}
	var spoolBytes int64
	for _, spool := range []*docRangeSpool{publication.spool, publication.indexes.spool} {
		if spool.file == nil || spool.writer == nil || len(spool.offsets) == 0 {
			test.Fatal("preparation retained an incomplete spool")
		}
		info, err := spool.file.Stat()
		if err != nil {
			test.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || filepath.Dir(spool.file.Name()) != directory ||
			spool.writer.Buffered() != 0 || spool.offsets[0] != 0 || info.Size() != spool.offsets[len(spool.offsets)-1] || events > 0 && info.Size() == 0 {
			test.Fatal("prepared spool is not private, flushed, or exactly bounded", spool.file.Name(), info.Mode(), info.Size())
		}
		spoolBytes += info.Size()
	}
	paths, err := filepath.Glob(filepath.Join(directory, "cxthub-doc-range-*"))
	if err != nil || len(paths) != 2 {
		test.Fatal("preparation must own exactly the original and derived spools", paths, err)
	}
	resources, err := publication.RetainedResources()
	if err != nil || resources.HeapBytes <= 0 || resources.HeapBytes > 16<<20 || resources.SpoolBytes != spoolBytes || resources.SpoolBytes > 64<<20 {
		test.Fatal("prepared indexes escaped the combined publication resource budget", resources, spoolBytes, err)
	}
}

func damagePreparedRootIndexSpoolPG(test *testing.T, spool *docRangeSpool, truncate bool) {
	test.Helper()
	info, err := spool.file.Stat()
	if err != nil || info.Size() == 0 {
		test.Fatal("local corruption fixture requires a nonempty prepared spool", err)
	}
	if truncate {
		if err := spool.file.Truncate(info.Size() - 1); err != nil {
			test.Fatal(err)
		}
		return
	}
	raw := make([]byte, 1)
	if _, err := spool.file.ReadAt(raw, info.Size()-1); err != nil {
		test.Fatal(err)
	}
	raw[0] ^= 1
	if written, err := spool.file.WriteAt(raw, info.Size()-1); err != nil || written != len(raw) {
		test.Fatal("could not corrupt a prepared spool without changing its size", written, err)
	}
}

func TestPreparedRootIndexesPGAfterPrepareFailuresLeaveNoReceipt(test *testing.T) {
	for _, damage := range []string{
		"original-spool-truncated", "original-spool-same-size", "derived-spool-truncated", "derived-spool-same-size", "original-spool-closed", "derived-spool-closed",
		"publication-closed", "canceled", "revoked-grant", "lease-expired", "lease-reclaimed", "caller-version", "caller-epoch",
	} {
		test.Run(damage, func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			ownedPG, storage := authorityScopePG(test)
			ctx := rootPublicationContext(ownedPG.ctx)
			repo, namespace := rootPGRepo(test, ctx, storage)
			fixture := rangeFixture(test, domain.DocRangePageEvents+1)
			job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
			publication := prepareRootStreamingPG(test, storage, ctx, job, directory).(*pgStreamingRootPublication)
			assertPreparedRootIndexesSpoolsPG(test, publication, directory, len(fixture.cir.Events))
			candidate, completeCtx := job, ctx
			var wanted error
			switch damage {
			case "original-spool-truncated", "original-spool-same-size":
				damagePreparedRootIndexSpoolPG(test, publication.spool, damage == "original-spool-truncated")
				if damage == "original-spool-same-size" {
					wanted = domain.ErrIntegrity
				}
			case "derived-spool-truncated", "derived-spool-same-size":
				damagePreparedRootIndexSpoolPG(test, publication.indexes.spool, damage == "derived-spool-truncated")
				if damage == "derived-spool-same-size" {
					wanted = domain.ErrIntegrity
				}
			case "original-spool-closed":
				if err := publication.spool.Close(); err != nil {
					test.Fatal(err)
				}
			case "derived-spool-closed":
				if err := publication.indexes.spool.Close(); err != nil {
					test.Fatal(err)
				}
			case "publication-closed":
				if err := publication.Close(); err != nil {
					test.Fatal(err)
				}
				assertRangeSpoolsRemoved(test, directory)
			case "canceled":
				var cancel context.CancelFunc
				completeCtx, cancel = context.WithCancel(ctx)
				cancel()
				wanted = context.Canceled
			case "revoked-grant":
				hash := fixture.manifest.Chunks[len(fixture.manifest.Chunks)-1].Hash
				if tag, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='chunk' AND hash=$2`, repo, hash); err != nil || tag.RowsAffected() != 1 {
					test.Fatal("revoke only this owned local fixture's prepared dependency grant", err)
				}
				wanted = domain.ErrNotFound
			case "lease-expired":
				expired := job
				expired.LeaseUntil = time.Now().Add(-time.Second)
				if err := storage.writeDocJob(ctx, expired); err != nil {
					test.Fatal(err)
				}
				wanted = domain.ErrConflict
			case "lease-reclaimed":
				if _, err := storage.ClaimDocJob(ctx, repo, job.LeaseUntil.Add(time.Second), time.Minute); err != nil {
					test.Fatal(err)
				}
				wanted = domain.ErrConflict
			case "caller-version":
				candidate.Version++
				wanted = domain.ErrConflict
			case "caller-epoch":
				candidate.RepositoryEpoch = domain.NewID("stale_epoch_")
				wanted = domain.ErrConflict
			}
			before := rootPGImage(test, ctx, storage, repo, namespace)
			indexesBefore := docVisiblePGImage(test, storage, ctx, job.DocHash)
			err := publication.Complete(completeCtx, candidate, time.Now())
			if err == nil || wanted != nil && !errors.Is(err, wanted) {
				test.Fatal("invalid prepared publication was accepted or changed its fence error", err, wanted)
			}
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			if rootPGImage(test, ctx, storage, repo, namespace) != before || docVisiblePGImage(test, storage, ctx, job.DocHash) != indexesBefore {
				test.Fatal("failed prepared publication changed root, ownership, derived indexes, accounting, or receipt")
			}
			if err := publication.Close(); err != nil {
				test.Fatal(err)
			}
			assertRangeSpoolsRemoved(test, directory)
		})
	}
}

type preparedRootIndexesCancelTxPG struct {
	pgx.Tx
	cancel   context.CancelFunc
	observed bool
}

func (transaction *preparedRootIndexesCancelTxPG) Exec(ctx context.Context, statement string, arguments ...any) (pgconn.CommandTag, error) {
	tag, err := transaction.Tx.Exec(ctx, statement, arguments...)
	if err == nil && strings.Contains(statement, "INSERT INTO doc_visible_generations_v1") && tag.RowsAffected() == 1 {
		transaction.observed = true
		transaction.cancel()
	}
	return tag, err
}

func TestPreparedRootIndexesPGCancellationAfterActualDerivedWriteRollsBack(test *testing.T) {
	directory := rangeSpoolTempDir(test)
	ownedPG, storage := authorityScopePG(test)
	ctx := rootPublicationContext(ownedPG.ctx)
	repo, namespace := rootPGRepo(test, ctx, storage)
	fixture := rangeFixture(test, domain.DocRangePageEvents+1)
	job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
	publication := prepareRootStreamingPG(test, storage, ctx, job, directory).(*pgStreamingRootPublication)
	assertPreparedRootIndexesSpoolsPG(test, publication, directory, len(fixture.cir.Events))
	before := rootPGImage(test, ctx, storage, repo, namespace)
	indexesBefore := docVisiblePGImage(test, storage, ctx, job.DocHash)
	var trace *preparedRootIndexesCancelTxPG
	err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		work, cancel := context.WithCancel(bound)
		defer cancel()
		original := bound.Value(repositoryTxKey{}).(*repositoryTx)
		trace = &preparedRootIndexesCancelTxPG{Tx: original.Tx, cancel: cancel}
		transaction := *original
		transaction.Tx = trace
		return publication.Complete(context.WithValue(work, repositoryTxKey{}, &transaction), job, time.Now())
	})
	if trace == nil || !trace.observed || !errors.Is(err, context.Canceled) {
		test.Fatal("cancellation did not follow the actual successful PostgreSQL visible-generation insert", err)
	}
	assertRootStreamingUnpublishedPG(test, storage, ctx, job)
	if rootPGImage(test, ctx, storage, repo, namespace) != before || docVisiblePGImage(test, storage, ctx, job.DocHash) != indexesBefore {
		test.Fatal("cancellation after derived writes committed root, indexes, accounting, or receipt")
	}
	if err := publication.Close(); err != nil {
		test.Fatal(err)
	}
	assertRangeSpoolsRemoved(test, directory)
}
