//go:build postgres

package store

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDocRangeGenerationPGSourceVersionOverflowFailsClosed(t *testing.T) {
	storage, ctx := chunkReusePG(t)
	repo, _ := rootPGRepo(t, ctx, storage)
	fixture := rangeFixture(t, 1)
	const maximumVersion = int64(1<<63 - 1)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes,doc_range_source_version) VALUES($1,$2,$3)`, fixture.hash, docCompress(fixture.manifestBytes), maximumVersion); err != nil {
		t.Fatal(err)
	}
	seedRootPG(t, ctx, storage, repo, fixture)
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
	}
}
