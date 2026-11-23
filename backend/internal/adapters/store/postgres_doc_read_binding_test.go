//go:build postgres

package store

import (
	"crypto/sha256"
	"fmt"
	"os"
	"testing"
)

func TestDocReadBindingPGMigrationAudit(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	var payloadColumn, generationColumn bool
	if err := storage.pool.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='blobs'::regclass AND attname='doc_range_source_id' AND NOT attisdropped),
 EXISTS(SELECT 1 FROM pg_attribute WHERE attrelid='doc_range_generations_v2'::regclass AND attname='generation_id' AND atttypid='uuid'::regtype AND attnotnull AND NOT attisdropped)`).Scan(&payloadColumn, &generationColumn); err != nil {
		test.Fatal(err)
	}
	if payloadColumn || !generationColumn {
		test.Fatal("incarnation migration altered payload schema or omitted generation UUID", payloadColumn, generationColumn)
	}
	for _, name := range []string{
		"0095_doc_range_generation.sql",
		"0096_doc_range_boundaries.sql",
		"0097_doc_visible_generations.sql",
		"0098_doc_read_source_incarnation.sql",
	} {
		raw, err := os.ReadFile("../../../../schemas/db/migrations/" + name)
		if err != nil {
			test.Fatal(err)
		}
		var checksum string
		if err := storage.pool.QueryRow(ctx, `SELECT checksum FROM schema_migrations WHERE version=$1`, name).Scan(&checksum); err != nil {
			test.Fatal(err)
		}
		if want := fmt.Sprintf("%x", sha256.Sum256(raw)); checksum != want {
			test.Fatal("migration audit checksum changed", name, checksum, want)
		}
	}
	if count, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil || count != 0 {
		test.Fatal("migration replay was not idempotent", count, err)
	}
}
