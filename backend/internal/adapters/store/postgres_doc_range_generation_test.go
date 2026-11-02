//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func generationCountsPG(t *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) [2]int {
	t.Helper()
	var counts [2]int
	if err := storage.db(ctx).QueryRow(ctx, `SELECT
 (SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1),
 (SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1)`, hash).Scan(&counts[0], &counts[1]); err != nil {
		t.Fatal(err)
	}
	return counts
}

func TestDocRangeGenerationPGSourceUpdateFromOlderSnapshot(t *testing.T) {
	for _, isolation := range []pgx.TxIsoLevel{pgx.ReadCommitted, pgx.RepeatableRead, pgx.Serializable} {
		for _, mutation := range []string{"corrupt", "noop", "metadata reset"} {
			t.Run(string(isolation)+"/"+mutation, func(t *testing.T) {
				storage, ctx := chunkReusePG(t)
				repo, _ := docRangeJobPGRepo(t, storage, ctx)
				fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
				docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
				claim := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
				mutator, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: isolation})
				if err != nil {
					t.Fatal(err)
				}
				defer rollbackPG(mutator)
				var priorGenerations int
				if err := mutator.QueryRow(ctx, `SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&priorGenerations); err != nil || priorGenerations != 0 {
					t.Fatal("mutator snapshot did not precede generation", priorGenerations, err)
				}
				if err := storage.BuildDocRangeJob(ctx, claim); err != nil {
					t.Fatal(err)
				}
				var publishedHash domain.ContentHash
				if err := storage.pool.QueryRow(ctx, `SELECT generation_hash FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&publishedHash); err != nil {
					t.Fatal(err)
				}
				if mutation == "corrupt" {
					changed := rangeFixture(t, 130)
					_, err = mutator.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, docCompress(changed.manifestBytes))
				} else if mutation == "noop" {
					_, err = mutator.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
				} else {
					_, err = mutator.Exec(ctx, `UPDATE blobs SET hash=hash WHERE hash=$1`, fixture.hash)
				}
				if err != nil {
					t.Fatal("ordinary source update failed", err)
				}
				if err := mutator.Commit(ctx); err != nil {
					t.Fatal("ordinary source commit failed", err)
				}
				expectedVersion := int64(1)
				if mutation == "metadata reset" {
					if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET doc_range_source_version=0 WHERE hash=$1`, fixture.hash); err != nil {
						t.Fatal(err)
					}
					expectedVersion++
				}
				counts := generationCountsPG(t, storage, ctx, fixture.hash)
				ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash)
				t.Logf("after old %s source update: persisted generations=%d objects=%d ready=%v readiness_error=%v", isolation, counts[0], counts[1], ready, err)
				if err != nil || ready {
					t.Fatal("stale generation advertised as ready", ready, err)
				}
				if isolation == pgx.RepeatableRead && (counts[0] != 1 || counts[1] == 0) {
					t.Fatal("regression did not retain snapshot-invisible generation", counts)
				}
				var sourceVersion int64
				if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&sourceVersion); err != nil || sourceVersion != expectedVersion {
					t.Fatal("source update did not advance fence", sourceVersion, err)
				}
				if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
					if _, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
						t.Fatal("fresh reader accepted stale generation", err)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				receipt := docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
				if receipt.State != "waiting" || receipt.Version <= claim.Version {
					t.Fatal("stale completed receipt was not requeued", receipt.State, receipt.Version, claim.Version)
				}
				replacement := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
				err = storage.BuildDocRangeJob(ctx, replacement)
				if mutation == "corrupt" {
					if !errors.Is(err, domain.ErrIntegrity) {
						t.Fatal("stale generation promoted without current verification", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				var rebuiltVersion int64
				var rebuiltHash domain.ContentHash
				if err := storage.pool.QueryRow(ctx, `SELECT source_version,generation_hash FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&rebuiltVersion, &rebuiltHash); err != nil {
					t.Fatal(err)
				}
				if rebuiltVersion != sourceVersion || rebuiltHash != publishedHash {
					t.Fatal("rebuild did not bind current source without changing canonical generation", rebuiltVersion, sourceVersion, rebuiltHash, publishedHash)
				}
				if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || !ready {
					t.Fatal("rebuilt generation unavailable", ready, err)
				}
				if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
					read, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
					if err != nil {
						return err
					}
					_, err = read.ReadPage(bound, 0)
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestDocRangeGenerationPGReaderScopeAndCopies(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	foreign, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 257)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	proof, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	boundProof, objects, err := domain.BuildVerifiedDocRangeGeneration(ctx, proof)
	if err != nil {
		t.Fatal(err)
	}
	expected := boundProof.Descriptor()
	storedHashes := make(map[domain.ContentHash]bool)
	for _, object := range objects {
		storedHashes[object.Hash] = true
	}
	for _, hash := range proof.Header().Pages {
		storedHashes[hash] = true
	}
	if _, err := domain.BuildDocVisibleGeneration(ctx, boundProof, func(_ context.Context, object domain.DocRangeObject) error {
		storedHashes[object.Hash] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := generationCountsPG(t, storage, ctx, fixture.hash); got != [2]int{1, len(storedHashes)} {
		t.Fatal("incomplete persisted generation", got)
	}
	if _, err := storage.OpenDocRangeGeneration(ctx, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
		t.Fatal("unbound open", err)
	}
	if err := storage.WithinRepository(ctx, repo, func(write context.Context) error {
		if _, err := storage.OpenDocRangeGeneration(write, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("writer opened generation", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	peer := &PostgresStore{pool: storage.pool}
	var retained outbound.DocRangeGenerationRead
	var retainedContext context.Context
	check := func(read outbound.DocRangeGenerationRead, bound context.Context, expected error) {
		t.Helper()
		_, pageHashErr := read.PageHash(bound, 0)
		_, pageErr := read.ReadPage(bound, 0)
		_, chunkErr := read.Chunk(bound, 0)
		for _, err := range []error{pageHashErr, pageErr, chunkErr} {
			if !errors.Is(err, expected) {
				t.Fatal("reader context guard", expected, err)
			}
		}
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := peer.OpenDocRangeGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrConflict) {
			t.Fatal("foreign store opened generation", err)
		}
		if _, err := storage.OpenDocRangeGeneration(bound, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
			t.Fatal("foreign ownership opened generation", err)
		}
		read, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		retained, retainedContext = read, bound
		if !reflect.DeepEqual(read.Descriptor(), expected) {
			t.Fatal("descriptor differs from current proof")
		}
		changed := read.Descriptor()
		changed.Envelope[0] = '!'
		if !reflect.DeepEqual(read.Descriptor(), expected) {
			t.Fatal("descriptor exposed private bytes")
		}
		for position, expectedHash := range proof.Header().Pages {
			hash, err := read.PageHash(bound, position)
			if err != nil || hash != expectedHash {
				t.Fatal("event directory differs", position, err)
			}
			raw, err := proof.Page(bound, position)
			if err != nil {
				return err
			}
			want, err := expected.DecodePage(position, hash, raw)
			if err != nil {
				return err
			}
			entries, err := read.ReadPage(bound, position)
			if err != nil || !reflect.DeepEqual(entries, want) {
				t.Fatal("selected page differs", position, err)
			}
			entries[0].Hash = "mutated"
			again, err := read.ReadPage(bound, position)
			if err != nil || !reflect.DeepEqual(again, want) {
				t.Fatal("cached page exposed private state", position, err)
			}
		}
		for ordinal, want := range fixture.manifest.Chunks {
			chunk, err := read.Chunk(bound, ordinal)
			if err != nil || chunk != want {
				t.Fatal("selected chunk differs", ordinal, err)
			}
		}
		for _, position := range []int{-1, expected.EventPages.Count} {
			if _, err := read.ReadPage(bound, position); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("invalid page ordinal", err)
			}
		}
		for _, ordinal := range []int{-1, expected.ChunkCount()} {
			if _, err := read.Chunk(bound, ordinal); !errors.Is(err, domain.ErrIntegrity) {
				t.Fatal("invalid chunk ordinal", err)
			}
		}
		check(read, ctx, domain.ErrConflict)
		canceled, cancel := context.WithCancel(bound)
		cancel()
		check(read, canceled, context.Canceled)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	check(retained, retainedContext, pgx.ErrTxClosed)
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		check(retained, bound, domain.ErrConflict)
		check(retained, retainedContext, pgx.ErrTxClosed)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocRangeGenerationPGSnapshotInvalidationAndOwnership(t *testing.T) {
	for _, change := range []string{"root bytes", "same root bytes", "chunk bytes", "ownership", "generation", "page", "missing page", "root deletion"} {
		t.Run(change, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			other, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 129)
			seedRootPG(t, ctx, storage, repo, fixture)
			seedRootPG(t, ctx, storage, other, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			header, err := storage.DocRangeIndex(ctx, repo, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			wantError := error(nil)
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				switch change {
				case "root bytes", "same root bytes":
					raw := []byte("corrupt root")
					if change == "same root bytes" {
						raw = docCompress(fixture.manifestBytes)
					}
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, raw)
					wantError = domain.ErrDocRangeIndexUnavailable
				case "chunk bytes":
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("corrupt chunk"))
				case "ownership":
					_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash)
					wantError = domain.ErrNotFound
				case "generation":
					_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash)
					wantError = domain.ErrDocRangeIndexUnavailable
				case "page":
					_, err = storage.pool.Exec(ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, header.Pages[0], []byte("[]"))
					wantError = domain.ErrIntegrity
				case "missing page":
					_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, header.Pages[0])
					wantError = domain.ErrIntegrity
				case "root deletion":
					if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE kind='doc' AND hash=$1`, fixture.hash); err != nil {
						return err
					}
					_, err = storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, fixture.hash)
					wantError = domain.ErrNotFound
				}
				if err != nil {
					return err
				}
				_, err = read.ReadPage(bound, 0)
				return err
			}); err != nil {
				t.Fatal("snapshot changed after concurrent mutation", err)
			}
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if err == nil {
					_, err = read.ReadPage(bound, 0)
				}
				if !errors.Is(err, wantError) {
					t.Fatal("fresh reader ignored mutation", wantError, err)
				}
				if change == "ownership" {
					_, err = storage.OpenDocRangeGeneration(bound, other, fixture.hash)
					if err != nil {
						t.Fatal("grant revocation deleted shared generation", err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if change == "root bytes" || change == "same root bytes" || change == "generation" || change == "root deletion" {
				if counts := generationCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
					t.Fatal("invalidation left generation objects", counts)
				}
			}
			if change == "root bytes" || change == "same root bytes" {
				if counts := rangeCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{1, 2} {
					t.Fatal("source update invalidated v1", counts)
				}
			}
		})
	}
}

func TestDocRangeGenerationPGDescriptorIntegrity(t *testing.T) {
	for _, damage := range []string{"hash", "hash bound", "document", "noncanonical", "missing", "empty document"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 0)
			seedRootPG(t, ctx, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			generation, err := storage.readRangeGeneration(ctx, repo, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			if damage == "document" {
				generation.Hash = domain.HashContent([]byte("foreign document"))
			}
			raw, err := generation.Encode()
			if err != nil {
				t.Fatal(err)
			}
			if damage == "noncanonical" {
				raw = append(raw, '\n')
			}
			hash := domain.HashContent(raw)
			if damage == "hash" {
				hash = fixture.hash
			}
			if damage == "hash bound" {
				hash += domain.ContentHash(strings.Repeat("a", 1024))
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_generations_v2 SET generation_hash=$2,descriptor=$3 WHERE doc_hash=$1`, fixture.hash, hash, raw); err != nil {
				t.Fatal(err)
			}
			wantError := domain.ErrIntegrity
			if damage == "missing" {
				if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash); err != nil {
					t.Fatal(err)
				}
				wantError = domain.ErrDocRangeIndexUnavailable
			}
			if damage == "empty document" {
				wantError = nil
			}
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if !errors.Is(err, wantError) {
					t.Fatal("descriptor validation", damage, wantError, err)
				}
				if err == nil {
					if _, err := read.PageHash(bound, 0); !errors.Is(err, domain.ErrIntegrity) {
						t.Fatal("empty document page", err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDocRangeGenerationPGSelectedPathAuthentication(t *testing.T) {
	for _, damage := range []string{"directory hash", "directory bound", "event decode", "chunk decode", "unselected page"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 129)
			seedRootPG(t, ctx, storage, repo, fixture)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			generation, err := storage.readRangeGeneration(ctx, repo, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			header, err := storage.DocRangeIndex(ctx, repo, fixture.hash)
			if err != nil {
				t.Fatal(err)
			}
			root := &generation.EventPages
			if damage == "chunk decode" {
				root = &generation.ChunkPages
			}
			var directory []byte
			if err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, root.Hash).Scan(&directory); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "directory hash":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, root.Hash, []byte("{}"))
			case "directory bound":
				directory = bytes.Repeat([]byte(" "), domain.MaxDocRangeDirectoryBytes+1)
			case "event decode", "chunk decode":
				leaf, resolveErr := domain.ResolveDocRangeLeaf(ctx, *root, 0, func(context.Context, domain.ContentHash) ([]byte, error) { return directory, nil })
				if resolveErr != nil {
					t.Fatal(resolveErr)
				}
				raw := []byte("[]")
				hash := domain.HashContent(raw)
				_, err = storage.pool.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, hash, raw)
				directory = bytes.ReplaceAll(directory, []byte(leaf), []byte(hash))
			case "unselected page":
				_, err = storage.pool.Exec(ctx, `UPDATE doc_range_objects_v2 SET bytes=$3 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, header.Pages[1], []byte("[]"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if damage == "directory bound" || damage == "event decode" || damage == "chunk decode" {
				root.Hash = domain.HashContent(directory)
				if _, err := storage.pool.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, root.Hash, directory); err != nil {
					t.Fatal(err)
				}
				raw, err := generation.Encode()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_generations_v2 SET generation_hash=$2,descriptor=$3 WHERE doc_hash=$1`, fixture.hash, domain.HashContent(raw), raw); err != nil {
					t.Fatal(err)
				}
			}
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				if damage == "chunk decode" {
					_, err = read.Chunk(bound, 0)
				} else {
					_, err = read.ReadPage(bound, 0)
				}
				if damage == "unselected page" {
					if err != nil {
						t.Fatal("unselected corruption affected read", err)
					}
					_, err = read.ReadPage(bound, 1)
				}
				if !errors.Is(err, domain.ErrIntegrity) {
					t.Fatal("selected path corruption accepted", err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type docRangeGenerationTracer struct {
	descriptors atomic.Int64
	objects     atomic.Int64
	forbidden   atomic.Int64
	selected    []domain.ContentHash
}

func (trace *docRangeGenerationTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(query.SQL, "generation.descriptor") {
		trace.descriptors.Add(1)
	}
	if strings.Contains(query.SQL, "FROM doc_range_objects_v2") {
		trace.objects.Add(1)
		trace.selected = append(trace.selected, query.Args[1].(domain.ContentHash))
	}
	if strings.Contains(query.SQL, "doc_range_indexes_v1") || strings.Contains(query.SQL, "root.bytes") || strings.Contains(query.SQL, "b.bytes") {
		trace.forbidden.Add(1)
	}
	return ctx
}

func (*docRangeGenerationTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestDocRangeGenerationPGSelectedReadCostAndBoundedCache(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, domain.DocRangeDirectoryFanout*domain.DocRangePageEvents+1)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	trace := &docRangeGenerationTracer{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	reader := &PostgresStore{pool: pool}
	if err := reader.WithinReadSnapshot(ctx, func(bound context.Context) error {
		opened, err := reader.OpenDocRangeGeneration(bound, repo, fixture.hash)
		if err != nil {
			return err
		}
		read := opened.(*postgresDocRangeGenerationRead)
		for range 3 {
			if _, err := read.PageHash(bound, 128); err != nil {
				return err
			}
			if _, err := read.ReadPage(bound, 128); err != nil {
				return err
			}
			if _, err := read.Chunk(bound, 0); err != nil {
				return err
			}
		}
		if trace.descriptors.Load() != 1 || trace.objects.Load() != 5 || trace.forbidden.Load() != 0 {
			t.Fatal("selected read fetched unrelated data", trace.descriptors.Load(), trace.objects.Load(), trace.forbidden.Load())
		}
		for position := range 3 * docRangeGenerationCacheObjects {
			if _, err := read.ReadPage(bound, position); err != nil {
				return err
			}
			if len(read.objects) > docRangeGenerationCacheObjects {
				t.Fatal("unbounded reader cache", len(read.objects))
			}
		}
		_, err = read.ReadPage(bound, 128)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDocRangeGenerationPGV1OnlyWorkerVerifiesCurrentSource(t *testing.T) {
	for _, damage := range []string{"none", "v1 header", "chunk bytes"} {
		t.Run(damage, func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := docRangeJobPGRepo(t, storage, ctx)
			fixture := docRangeJobPGSource(t, storage, ctx, repo, 129)
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal(err)
			}
			if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash); err != nil {
				t.Fatal(err)
			}
			if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
				t.Fatal("public v1 ensure behavior changed", err)
			}
			if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || ready {
				t.Fatal("v1 presence marked generation ready", ready, err)
			}
			if damage == "v1 header" {
				if _, err := storage.pool.Exec(ctx, `UPDATE doc_range_indexes_v1 SET header=$2 WHERE hash=$1`, fixture.hash, []byte("{}")); err != nil {
					t.Fatal(err)
				}
			}
			if damage == "chunk bytes" {
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.manifest.Chunks[0].Hash, []byte("corrupt source")); err != nil {
					t.Fatal(err)
				}
			}
			docRangeJobPGEnqueue(t, storage, ctx, repo, fixture.hash)
			job := docRangeJobPGClaim(t, storage, ctx, repo, fixture.hash)
			err := storage.BuildDocRangeJob(ctx, job)
			if damage == "chunk bytes" {
				if !errors.Is(err, domain.ErrIntegrity) || generationCountsPG(t, storage, ctx, fixture.hash) != [2]int{} {
					t.Fatal("v1 proof promoted without current verification", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || !ready {
				t.Fatal("worker did not publish v2", ready, err)
			}
			assertRangePages(t, ctx, storage, repo, fixture)
		})
	}
}

func TestDocRangeGenerationPGSelectedMetadataBytesScale(t *testing.T) {
	var smallDescriptorBytes int
	for _, events := range []int{12800, 128000} {
		t.Run(fmt.Sprint(events), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			phase := time.Now()
			fixture := rangeFixture(t, events)
			t.Logf("events=%d phase=fixture elapsed=%s", events, time.Since(phase))
			phase = time.Now()
			seedRootPG(t, ctx, storage, repo, fixture)
			t.Logf("events=%d phase=seed elapsed=%s", events, time.Since(phase))
			phase = time.Now()
			prepareErr := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash)
			t.Logf("events=%d phase=prepare elapsed=%s", events, time.Since(phase))
			if prepareErr != nil {
				t.Fatal(prepareErr)
			}
			trace := &docRangeGenerationTracer{}
			config := storage.pool.Config()
			config.ConnConfig.Tracer = trace
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				t.Fatal(err)
			}
			defer pool.Close()
			reader := &PostgresStore{pool: pool}
			var descriptor domain.DocRangeGeneration
			var eventPage domain.ContentHash
			if err := reader.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := reader.OpenDocRangeGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				descriptor = read.Descriptor()
				eventPage, err = read.PageHash(bound, descriptor.EventPages.Count-1)
				if err != nil {
					return err
				}
				if _, err := read.ReadPage(bound, descriptor.EventPages.Count-1); err != nil {
					return err
				}
				_, err = read.Chunk(bound, 0)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			expectedObjectReads := int64(4)
			if events == 128000 {
				expectedObjectReads++
			}
			if trace.descriptors.Load() != 1 || trace.objects.Load() != expectedObjectReads || trace.forbidden.Load() != 0 {
				t.Fatal("read cost changed", trace.descriptors.Load(), trace.objects.Load(), trace.forbidden.Load())
			}
			chunkPage, err := domain.ResolveDocRangeLeaf(ctx, descriptor.ChunkPages, 0, func(ctx context.Context, hash domain.ContentHash) ([]byte, error) {
				var raw []byte
				err := storage.pool.QueryRow(ctx, `SELECT bytes FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, hash).Scan(&raw)
				return raw, err
			})
			if err != nil {
				t.Fatal(err)
			}
			var descriptorBytes int
			if err := storage.pool.QueryRow(ctx, `SELECT octet_length(descriptor) FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&descriptorBytes); err != nil {
				t.Fatal(err)
			}
			var directoryBytes, eventBytes, chunkBytes int
			for _, hash := range trace.selected {
				var size int
				if err := storage.pool.QueryRow(ctx, `SELECT octet_length(bytes) FROM doc_range_objects_v2 WHERE doc_hash=$1 AND hash=$2`, fixture.hash, hash).Scan(&size); err != nil {
					t.Fatal(err)
				}
				switch hash {
				case eventPage:
					eventBytes += size
				case chunkPage:
					chunkBytes += size
				default:
					directoryBytes += size
				}
			}
			if descriptorBytes > domain.MaxDocRangeGenerationBytes || directoryBytes > 3*domain.MaxDocRangeDirectoryBytes || eventBytes > domain.MaxDocRangePageBytes || chunkBytes > domain.MaxDocRangeDirectoryBytes {
				t.Fatal("selected bytes exceeded structural bounds", descriptorBytes, directoryBytes, eventBytes, chunkBytes)
			}
			if events == 12800 {
				smallDescriptorBytes = descriptorBytes
			} else if descriptorBytes > smallDescriptorBytes+64 {
				t.Fatal("descriptor grew with page directory", smallDescriptorBytes, descriptorBytes)
			}
			t.Logf("events=%d descriptor_reads=1 object_reads=%d old_root_header_reads=0 descriptor_bytes=%d selected_directory_bytes=%d selected_event_page_bytes=%d selected_chunk_page_bytes=%d", events, trace.objects.Load(), descriptorBytes, directoryBytes, eventBytes, chunkBytes)
		})
	}
}

func TestDocRangeGenerationPGMigrationBounds(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 1)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	for _, descriptor := range []bool{true, false} {
		limit := domain.MaxDocRangePageBytes
		if descriptor {
			limit = domain.MaxDocRangeGenerationBytes
		}
		for _, length := range []int{0, limit, limit + 1} {
			transaction, err := storage.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			raw := bytes.Repeat([]byte("x"), length)
			if descriptor {
				_, err = transaction.Exec(ctx, `UPDATE doc_range_generations_v2 SET descriptor=$2 WHERE doc_hash=$1`, fixture.hash, raw)
			} else {
				_, err = transaction.Exec(ctx, `INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3)`, fixture.hash, domain.HashContent(raw), raw)
			}
			rollbackPG(transaction)
			if (err == nil) != (length == limit) {
				t.Fatal("database bound differs from domain", descriptor, length, err)
			}
		}
	}
}
