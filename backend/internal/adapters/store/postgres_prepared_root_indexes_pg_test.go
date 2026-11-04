//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type preparedRootIndexesExpectedPG struct {
	header     []byte
	ranges     []byte
	visible    []byte
	generation domain.DocVisibleGeneration
	objects    map[domain.ContentHash][]byte
}

func preparedRootIndexesOraclePG(test *testing.T, ctx context.Context, proof domain.VerifiedSessionDoc) preparedRootIndexesExpectedPG {
	test.Helper()
	index, err := domain.BuildDocRangeIndex(ctx, proof)
	if err != nil {
		test.Fatal(err)
	}
	_, header, err := verifiedRangeHeader(index)
	if err != nil {
		test.Fatal(err)
	}
	ranges, objects, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
	if err != nil {
		test.Fatal(err)
	}
	rangeBytes, err := ranges.Descriptor().Encode()
	if err != nil {
		test.Fatal(err)
	}
	expected := preparedRootIndexesExpectedPG{header: header, ranges: rangeBytes, objects: make(map[domain.ContentHash][]byte)}
	emit := func(ctx context.Context, object domain.DocRangeObject) error {
		if len(object.Bytes) == 0 || domain.HashContent(object.Bytes) != object.Hash {
			return domain.ErrIntegrity
		}
		if previous, exists := expected.objects[object.Hash]; exists && !bytes.Equal(previous, object.Bytes) {
			return domain.ErrIntegrity
		}
		expected.objects[object.Hash] = bytes.Clone(object.Bytes)
		return ctx.Err()
	}
	for _, object := range objects {
		if err := emit(ctx, object); err != nil {
			test.Fatal(err)
		}
	}
	expected.generation, err = domain.BuildDocVisibleGenerationWithRangePages(ctx, ranges, emit, func(ctx context.Context, position int, object domain.DocRangeObject) error {
		if position < 0 || position >= len(index.Header().Pages) {
			return domain.ErrIntegrity
		}
		return emit(ctx, object)
	})
	if err != nil {
		test.Fatal(err)
	}
	expected.visible, err = expected.generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	return expected
}

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

func assertPreparedRootIndexesPublishedPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob, fixture rootReaderFixture, expected preparedRootIndexesExpectedPG) {
	test.Helper()
	assertRootSourceWitnessPublishedPG(test, storage, ctx, job, fixture)
	var headerBytes, rangeBytes, visibleBytes []byte
	var rangeHash, visibleHash domain.ContentHash
	if err := storage.db(ctx).QueryRow(ctx, `SELECT header.header,ranges.descriptor,visible.descriptor,ranges.generation_hash,visible.descriptor_hash
 FROM blobs root
 JOIN doc_range_indexes_v1 header ON header.hash=root.hash
 JOIN doc_range_generations_v2 ranges ON ranges.doc_hash=root.hash AND ranges.source_version=root.doc_range_source_version
 JOIN doc_visible_generations_v1 visible ON visible.doc_hash=root.hash AND visible.source_version=root.doc_range_source_version
 AND visible.range_generation_hash=ranges.generation_hash
 WHERE root.hash=$1`, job.DocHash).Scan(&headerBytes, &rangeBytes, &visibleBytes, &rangeHash, &visibleHash); err != nil {
		test.Fatal("root, range, and visible generations are not atomically source-bound", err)
	}
	if !bytes.Equal(headerBytes, expected.header) || !bytes.Equal(rangeBytes, expected.ranges) || !bytes.Equal(visibleBytes, expected.visible) ||
		rangeHash != domain.HashContent(expected.ranges) || visibleHash != domain.HashContent(expected.visible) {
		test.Fatal("prepared publication changed the original builders' serialized output")
	}
	_, rangeBinding, err := storage.readRangeGenerationBinding(ctx, job.RepoID, job.DocHash)
	if err != nil {
		test.Fatal(err)
	}
	_, visibleBinding, err := storage.readVisibleGenerationBinding(ctx, job.RepoID, job.DocHash)
	if err != nil || rangeBinding != visibleBinding {
		test.Fatal("range and visible readers did not observe one current source generation", rangeBinding, visibleBinding, err)
	}
	rows, err := storage.db(ctx).Query(ctx, `SELECT hash,bytes FROM doc_range_objects_v2 WHERE doc_hash=$1 ORDER BY hash`, job.DocHash)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	objects := 0
	for rows.Next() {
		var hash domain.ContentHash
		var raw []byte
		if err := rows.Scan(&hash, &raw); err != nil {
			test.Fatal(err)
		}
		if wanted, exists := expected.objects[hash]; !exists || !bytes.Equal(raw, wanted) || domain.HashContent(raw) != hash {
			test.Fatal("persisted range/visible object differs from the original builders", hash)
		}
		objects++
	}
	if err := rows.Err(); err != nil || objects != len(expected.objects) {
		test.Fatal("prepared publication omitted or added derived objects", objects, len(expected.objects), err)
	}
}

func assertPreparedRootIndexesVisibleReadsPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob, expected preparedRootIndexesExpectedPG) {
	test.Helper()
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		reader, err := storage.OpenDocVisibleGeneration(bound, job.RepoID, job.DocHash)
		if err != nil {
			return err
		}
		encoded, err := reader.Descriptor().Encode()
		if err != nil || !bytes.Equal(encoded, expected.visible) {
			return errors.Join(domain.ErrIntegrity, err)
		}
		for _, mode := range []string{"all", "chat", "prompts"} {
			for position := 0; position < expected.generation.Events; position++ {
				wanted, wantedOrdinal, err := domain.ReadDocVisibleUnit(bound, expected.generation, mode, position, true, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
					return expected.objects[hash], ctx.Err()
				})
				if err != nil {
					return err
				}
				actual, ordinal, err := reader.Unit(bound, mode, position, true)
				if err != nil || actual != wanted || ordinal != wantedOrdinal {
					return fmt.Errorf("visible mode %s event %d differs from original builder: %w", mode, position, errors.Join(domain.ErrIntegrity, err))
				}
			}
		}
		return nil
	}); err != nil {
		test.Fatal("committed prepared visible generation is not readable", err)
	}
}

func TestPreparedRootIndexesPGAtomicPublicationDoesNotRebuildInitialProof(test *testing.T) {
	for _, events := range []int{0, domain.DocRangePageEvents + 1} {
		for _, commit := range []bool{false, true} {
			test.Run(fmt.Sprintf("events-%d/commit-%t", events, commit), func(test *testing.T) {
				directory := rangeSpoolTempDir(test)
				ownedPG, storage := authorityScopePG(test)
				ctx := rootPublicationContext(ownedPG.ctx)
				repo, namespace := rootPGRepo(test, ctx, storage)
				fixture := rangeFixture(test, events)
				job, proof := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
				observer := authorityScopePeerPG(test, storage, "", nil)
				before := rootPGImage(test, ctx, observer, repo, namespace)
				indexesBefore := docVisiblePGImage(test, observer, ctx, job.DocHash)
				publication := prepareRootStreamingPG(test, storage, ctx, job, directory).(*pgStreamingRootPublication)
				assertPreparedRootIndexesSpoolsPG(test, publication, directory, events)
				assertRootStreamingUnpublishedPG(test, observer, ctx, job)
				if rootPGImage(test, ctx, observer, repo, namespace) != before || docVisiblePGImage(test, observer, ctx, job.DocHash) != indexesBefore {
					test.Fatal("actual preparation changed durable root, indexes, accounting, or receipt")
				}
				expected := preparedRootIndexesOraclePG(test, ctx, proof)
				initialPages := publication.root.ranges.Header().Pages
				publication.root.ranges = domain.VerifiedDocRangeIndex{}
				if _, err := publication.root.ranges.Page(ctx, 0); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("initial verified-range read failpoint is not armed", err)
				}
				if _, _, err := domain.BuildVerifiedDocRangeGeneration(ctx, publication.root.ranges); !errors.Is(err, domain.ErrIntegrity) {
					test.Fatal("initial verified-range builder failpoint is not armed", err)
				}
				for position, hash := range initialPages {
					raw, err := publication.spool.Read(ctx, position)
					if err != nil || domain.HashContent(raw) != hash {
						test.Fatal("disabling the initial range proof changed readable original spool bytes", position, err)
					}
				}
				rollback := errors.New("rollback after real prepared root/index/receipt publication")
				err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
					if err := publication.Complete(bound, job, time.Now()); err != nil {
						return err
					}
					assertPreparedRootIndexesPublishedPG(test, storage, bound, job, fixture, expected)
					assertRootStreamingUnpublishedPG(test, observer, ctx, job)
					if rootPGImage(test, ctx, observer, repo, namespace) != before || docVisiblePGImage(test, observer, ctx, job.DocHash) != indexesBefore {
						test.Fatal("pending publication became externally visible before outer commit")
					}
					if !commit {
						return rollback
					}
					return nil
				})
				if commit {
					if err != nil {
						test.Fatal("completion required the initial verified-range read/build again", err)
					}
					assertPreparedRootIndexesPublishedPG(test, observer, ctx, job, fixture, expected)
					assertPreparedRootIndexesVisibleReadsPG(test, observer, ctx, job, expected)
					wantedBytes := int64(len(docCompress(fixture.manifestBytes)))
					for _, body := range fixture.bodies {
						wantedBytes += int64(len(docCompress(body)))
					}
					usage, err := observer.ReadStorageUsage(ctx, namespace, time.Now().Add(-time.Hour), time.Now())
					if err != nil || usage.CurrentBytes != wantedBytes {
						test.Fatal("prepared index objects changed unique compressed root/chunk accounting", usage.CurrentBytes, wantedBytes, err)
					}
					replay, err := observer.EnqueueDocJob(ctx, rootPublicationJob(test, repo, fixture))
					if err != nil || replay.State != "completed" || replay.Version != job.Version || replay.DocumentRef() != job.DocumentRef() {
						test.Fatal("prepared publication lost the durable idempotent completion receipt", replay, err)
					}
				} else {
					if !errors.Is(err, rollback) {
						test.Fatal("did not reach rollback with the initial range proof disabled", err)
					}
					assertRootStreamingUnpublishedPG(test, observer, ctx, job)
					if rootPGImage(test, ctx, observer, repo, namespace) != before || docVisiblePGImage(test, observer, ctx, job.DocHash) != indexesBefore {
						test.Fatal("outer rollback leaked prepared root, derived indexes, accounting, or receipt")
					}
				}
				if err := publication.Close(); err != nil {
					test.Fatal(err)
				}
				assertRangeSpoolsRemoved(test, directory)
				if _, err := publication.RetainedResources(); err == nil {
					test.Fatal("closed publication still reports usable prepared resources")
				}
			})
		}
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
