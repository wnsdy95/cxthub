//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestDocRangeGenerationPGSourceUpdateRollback(t *testing.T) {
	for _, visible := range []bool{true, false} {
		t.Run(fmt.Sprintf("generation-visible-%v", visible), func(t *testing.T) {
			storage, ctx := chunkReusePG(t)
			repo, _ := rootPGRepo(t, ctx, storage)
			fixture := rangeFixture(t, 129)
			seedRootPG(t, ctx, storage, repo, fixture)
			if visible {
				if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
					t.Fatal(err)
				}
			}
			mutator, err := storage.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(mutator)
			var exists bool
			if err := mutator.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 WHERE doc_hash=$1)`, fixture.hash).Scan(&exists); err != nil || exists != visible {
				t.Fatal("unexpected mutator snapshot", exists, err)
			}
			if !visible {
				if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
					t.Fatal(err)
				}
			}
			before := generationCountsPG(t, storage, ctx, fixture.hash)
			if _, err := mutator.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, []byte("rolled back source damage")); err != nil {
				t.Fatal(err)
			}
			var version int64
			if err := mutator.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&version); err != nil || version != 1 {
				t.Fatal("uncommitted source did not advance", version, err)
			}
			if err := mutator.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_range_generations_v2 WHERE doc_hash=$1)`, fixture.hash).Scan(&exists); err != nil || exists {
				t.Fatal("mutator retained visible generation", exists, err)
			}
			if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || !ready {
				t.Fatal("uncommitted source escaped transaction", ready, err)
			}
			if err := mutator.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
			var raw []byte
			if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version,bytes FROM blobs WHERE hash=$1`, fixture.hash).Scan(&version, &raw); err != nil || version != 0 || !bytes.Equal(raw, docCompress(fixture.manifestBytes)) {
				t.Fatal("rollback did not restore source and version", version, err)
			}
			if after := generationCountsPG(t, storage, ctx, fixture.hash); after != before {
				t.Fatal("rollback changed published generation", before, after)
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

func TestDocRangeGenerationPGSourceVersionCannotBeReset(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 1)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	for position, statement := range []string{
		`UPDATE blobs SET doc_range_source_version=0 WHERE hash=$1`,
		`UPDATE blobs SET bytes=bytes,doc_range_source_version=0 WHERE hash=$1`,
		`UPDATE blobs SET doc_range_source_version=-1 WHERE hash=$1`,
		`UPDATE blobs SET bytes=bytes,doc_range_source_version=9223372036854775807 WHERE hash=$1`,
		`UPDATE blobs SET hash=hash WHERE hash=$1`,
		`UPDATE blobs SET bytes=bytes WHERE hash=$1`,
	} {
		if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
			t.Fatal("ordinary source update failed", err)
		}
		var version int64
		if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&version); err != nil || version != int64(position+1) {
			t.Fatal("caller assignment replaced monotonic version", position, version, err)
		}
		if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || ready {
			t.Fatal("old generation became ready", position, ready, err)
		}
		if err := storage.WithinRepository(ctx, repo, func(bound context.Context) error {
			return storage.ensureRangeGeneration(bound, repo, fixture.hash)
		}); err != nil {
			t.Fatal(err)
		}
		var publishedVersion int64
		if err := storage.pool.QueryRow(ctx, `SELECT source_version FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&publishedVersion); err != nil || publishedVersion != version {
			t.Fatal("publisher did not capture current retained source version", publishedVersion, version, err)
		}
	}
}

func TestDocRangeGenerationPGSourceVersionOverflowFailsClosed(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 1)
	const maximumVersion = int64(1<<63 - 1)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes,doc_range_source_version) VALUES($1,$2,$3)`, fixture.hash, docCompress(fixture.manifestBytes), maximumVersion); err != nil {
		t.Fatal(err)
	}
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	before := generationCountsPG(t, storage, ctx, fixture.hash)
	for _, statement := range []string{
		`UPDATE blobs SET bytes=bytes WHERE hash=$1`,
		`UPDATE blobs SET doc_range_source_version=0 WHERE hash=$1`,
		`UPDATE blobs SET bytes=bytes,doc_range_source_version=0 WHERE hash=$1`,
	} {
		_, err := storage.pool.Exec(ctx, statement, fixture.hash)
		var overflow *pgconn.PgError
		if !errors.As(err, &overflow) || overflow.Code != "22003" {
			t.Fatal("source version overflow did not fail closed", err)
		}
		var version int64
		if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&version); err != nil || version != maximumVersion {
			t.Fatal("source version wrapped or reset", version, err)
		}
		if after := generationCountsPG(t, storage, ctx, fixture.hash); after != before {
			t.Fatal("failed source update changed generation", before, after)
		}
		if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || !ready {
			t.Fatal("failed source update invalidated unchanged generation", ready, err)
		}
	}
}

func TestDocRangeGenerationPGSourceDeleteReinsertDoesNotReuseGeneration(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 129)
	seedRootPG(t, ctx, storage, repo, fixture)
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET hash=hash WHERE hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if counts := generationCountsPG(t, storage, ctx, fixture.hash); counts[0] != 1 || counts[1] == 0 {
		t.Fatal("metadata update did not leave stale generation for FK regression", counts)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM blobs WHERE hash=$1`, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if counts := generationCountsPG(t, storage, ctx, fixture.hash); counts != [2]int{} {
		t.Fatal("source deletion did not cascade generation objects", counts)
	}
	seedRootPG(t, ctx, storage, repo, fixture)
	var version int64
	if err := storage.pool.QueryRow(ctx, `SELECT doc_range_source_version FROM blobs WHERE hash=$1`, fixture.hash).Scan(&version); err != nil || version != 0 {
		t.Fatal("reinserted source version", version, err)
	}
	if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || ready {
		t.Fatal("reinserted source revived deleted generation", ready, err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		if _, err := storage.OpenDocRangeGeneration(bound, repo, fixture.hash); !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			t.Fatal("reinserted source retained old proof", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := storage.EnsureDocRangeIndex(ctx, repo, fixture.hash); err != nil {
		t.Fatal(err)
	}
	if ready, err := storage.HasDocRangeIndex(ctx, repo, fixture.hash); err != nil || !ready {
		t.Fatal("reinserted source could not publish a fresh generation", ready, err)
	}
}
