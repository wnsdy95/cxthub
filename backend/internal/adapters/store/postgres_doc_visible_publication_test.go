//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docVisibleFailTx struct {
	pgx.Tx
	point   string
	err     error
	batches int
}

func (transaction *docVisibleFailTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if transaction.point == "descriptor" && strings.Contains(sql, "INSERT INTO doc_visible_generations_v1") {
		return pgconn.CommandTag{}, transaction.err
	}
	return transaction.Tx.Exec(ctx, sql, args...)
}

func (transaction *docVisibleFailTx) SendBatch(ctx context.Context, batch *pgx.Batch) pgx.BatchResults {
	for _, query := range batch.QueuedQueries {
		if len(query.Arguments) == 3 {
			if raw, ok := query.Arguments[2].([]byte); ok && bytes.Contains(raw, []byte(`"units":[`)) {
				transaction.batches++
				if transaction.point == "objects" && transaction.batches == 2 {
					return docVisibleBatchResult{err: transaction.err}
				}
				break
			}
		}
	}
	return transaction.Tx.SendBatch(ctx, batch)
}

func docVisiblePGImage(t *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) string {
	t.Helper()
	var image string
	if err := storage.db(ctx).QueryRow(ctx, `SELECT jsonb_build_array(
 (SELECT to_jsonb(visible) FROM doc_visible_generations_v1 visible WHERE doc_hash=$1),
 (SELECT to_jsonb(ranges) FROM doc_range_generations_v2 ranges WHERE doc_hash=$1),
 (SELECT jsonb_agg(to_jsonb(object) ORDER BY hash) FROM doc_range_objects_v2 object WHERE doc_hash=$1),
 (SELECT to_jsonb(header) FROM doc_range_indexes_v1 header WHERE hash=$1),
 (SELECT jsonb_agg(to_jsonb(page) ORDER BY ordinal) FROM doc_range_pages_v1 page WHERE doc_hash=$1)
 )::text`, hash).Scan(&image); err != nil {
		t.Fatal(err)
	}
	return image
}

func TestDocVisiblePGAtomicPublicationFailure(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, point := range []string{"descriptor", "objects", "caller rollback"} {
			t.Run(fmt.Sprintf("existing-%v/%s", existing, point), func(t *testing.T) {
				storage, ctx := chunkReusePG(t)
				repo, _ := rootPGRepo(t, ctx, storage)
				events := 129
				if point == "objects" {
					events = 10000
				}
				fixture := rangeFixture(t, events)
				seedRootPG(t, ctx, storage, repo, fixture)
				if existing {
					if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
						t.Fatal(err)
					}
				}
				proof, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(t, fixture))
				if err != nil {
					t.Fatal(err)
				}
				before := docVisiblePGImage(t, storage, ctx, fixture.hash)
				failure := errors.New("injected visible publication failure")
				err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
					transaction := *bound.Value(repositoryTxKey{}).(*repositoryTx)
					fault := &docVisibleFailTx{Tx: transaction.Tx, point: point, err: failure}
					transaction.Tx = fault
					write := context.WithValue(bound, repositoryTxKey{}, &transaction)
					if err := storage.putRangeIndex(write, repo, proof); err != nil {
						if point == "objects" && fault.batches != 2 {
							t.Fatal("failed before partial visible flush", fault.batches, err)
						}
						return err
					}
					if point != "caller rollback" {
						t.Fatal("publication missed fault")
					}
					if _, err := storage.readVisibleGeneration(write, repo, fixture.hash); err != nil {
						return err
					}
					if after := docVisiblePGImage(t, storage, ctx, fixture.hash); after != before {
						t.Fatal("uncommitted publication escaped transaction")
					}
					return failure
				})
				if !errors.Is(err, failure) {
					t.Fatal("publication error", err)
				}
				if after := docVisiblePGImage(t, storage, ctx, fixture.hash); after != before {
					t.Fatal("failed publication changed committed objects")
				}
				want := domain.ErrDocRangeIndexUnavailable
				if existing {
					want = nil
				}
				docVisiblePGRead(t, storage, ctx, repo, fixture.hash, want)
			})
		}
	}
}

func TestDocVisiblePGPublicationUsesBoundProof(t *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(t, 129)
	index, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(t, fixture))
	if err != nil {
		t.Fatal(err)
	}
	proof, _, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := proof.Descriptor().Encode()
	if err != nil {
		t.Fatal(err)
	}
	foreign := domain.HashContent([]byte("foreign generation"))
	changed := proof.Descriptor()
	changed.Hash = foreign
	changed.Envelope[0] = '!'
	changed.EventPages.Hash = foreign
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.putVisibleGeneration(bound, repo, proof)
	}); err != nil {
		t.Fatal("descriptor copy changed publication proof", err)
	}
	generation, err := storage.readVisibleGeneration(ctx, repo, fixture.hash)
	if err != nil || generation.Hash != fixture.hash || generation.RangeGeneration != domain.HashContent(descriptor) {
		t.Fatal("publication lost verified descriptor binding", generation, err)
	}
	before := docVisiblePGImage(t, storage, ctx, fixture.hash)
	err = storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `UPDATE doc_range_generations_v2 SET generation_hash=$2 WHERE doc_hash=$1`, fixture.hash, foreign); err != nil {
			return err
		}
		return storage.putVisibleGeneration(bound, repo, proof)
	})
	if !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
		t.Fatal("visible proof rebound to another stored range generation", err)
	}
	if after := docVisiblePGImage(t, storage, ctx, fixture.hash); after != before {
		t.Fatal("rejected binding changed published generation")
	}
}

func TestDocVisiblePGOlderSourceSnapshotAndRebuild(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 129)
	seedRootPG(t, ctx, storage, repo, fixture)
	mutator, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(mutator)
	var count int
	if err := mutator.QueryRow(ctx, `SELECT count(*) FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&count); err != nil || count != 0 {
		t.Fatal("older snapshot", count, err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := mutator.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if err := mutator.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&count); err != nil || count != 1 {
		t.Fatal("regression requires snapshot-invisible retained generation", count, err)
	}
	docVisiblePGRead(t, storage, ctx, repo, fixture.hash, domain.ErrDocRangeIndexUnavailable)
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		return storage.ensureRangeGeneration(bound, repo, fixture.hash)
	}); err != nil {
		t.Fatal(err)
	}
	var current bool
	if err := storage.pool.QueryRow(ctx, `SELECT root.doc_range_source_version=1 AND visible.source_version=root.doc_range_source_version
 AND ranges.source_version=root.doc_range_source_version AND visible.range_generation_hash=ranges.generation_hash
 FROM blobs root JOIN doc_range_generations_v2 ranges ON ranges.doc_hash=root.hash
 JOIN doc_visible_generations_v1 visible ON visible.doc_hash=root.hash WHERE root.hash=$1`, fixture.hash).Scan(&current); err != nil || !current {
		t.Fatal("rebuilt visible generation not bound to current source", current, err)
	}
	docVisiblePGRead(t, storage, ctx, repo, fixture.hash, nil)
}

func TestDocVisiblePGPinnedInvalidationAndDeleteReinsert(t *testing.T) {
	for _, mutation := range []string{"root update", "root metadata", "range delete", "ownership", "delete reinsert"} {
		t.Run(mutation, func(t *testing.T) {
			storage, ctx, repo, fixture := docVisiblePGFixture(t, 257)
			other, _ := rootPGRepo(t, ctx, storage)
			seedRootPG(t, ctx, storage, other, fixture)
			want := domain.ErrDocRangeIndexUnavailable
			if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
				read, err := storage.OpenDocVisibleGeneration(bound, repo, fixture.hash)
				if err != nil {
					return err
				}
				if _, _, err := read.Unit(bound, "all", 0, false); err != nil {
					return err
				}
				switch mutation {
				case "root update":
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash)
				case "root metadata":
					_, err = storage.pool.Exec(ctx, `UPDATE blobs SET doc_range_source_version=0 WHERE hash=$1`, fixture.hash)
				case "range delete":
					_, err = storage.pool.Exec(ctx, `DELETE FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash)
				case "ownership":
					_, err = storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash)
					want = domain.ErrNotFound
				case "delete reinsert":
					if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE kind='doc' AND hash=$1`, fixture.hash); err != nil {
						return err
					}
					if _, err := storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, fixture.hash); err != nil {
						return err
					}
					seedRootPG(t, ctx, storage, repo, fixture)
				}
				if err != nil {
					return err
				}
				unit, ordinal, err := read.Unit(bound, "all", 256, false)
				if err == nil && (unit.From != 256 || ordinal != 256) {
					t.Fatal("pinned snapshot changed unit", unit, ordinal)
				}
				return err
			}); err != nil {
				t.Fatal("pinned reader changed after mutation", err)
			}
			docVisiblePGRead(t, storage, ctx, repo, fixture.hash, want)
			if mutation == "ownership" {
				docVisiblePGRead(t, storage, ctx, other, fixture.hash, nil)
			} else if mutation != "root metadata" {
				var count int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_visible_generations_v1 WHERE doc_hash=$1`, fixture.hash).Scan(&count); err != nil || count != 0 {
					t.Fatal("cascade retained visible descriptor", count, err)
				}
			}
		})
	}
}

func TestDocVisiblePGSourceUpdateRollback(t *testing.T) {
	storage, ctx, repo, fixture := docVisiblePGFixture(t, 129)
	before := docVisiblePGImage(t, storage, ctx, fixture.hash)
	failure := errors.New("rollback source update")
	if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
		if _, err := storage.db(bound).Exec(bound, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
			return err
		}
		if _, err := storage.readVisibleGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			t.Fatal("source update retained visible descriptor", err)
		}
		docVisiblePGRead(t, storage, ctx, repo, fixture.hash, nil)
		return failure
	}); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if after := docVisiblePGImage(t, storage, ctx, fixture.hash); after != before {
		t.Fatal("rollback changed visible generation")
	}
	docVisiblePGRead(t, storage, ctx, repo, fixture.hash, nil)
}
