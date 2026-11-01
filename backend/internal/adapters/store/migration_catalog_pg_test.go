//go:build postgres

package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMigrationCatalogRejectsConsistentHistoricalBundle(test *testing.T) {
	fixture, storage := migrationVerificationStorePG(test)
	const predecessor = "0148_legacy_byte_accounting_authority.sql"
	directory := test.TempDir()
	entries, err := os.ReadDir(migrationVerificationBundlePG)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") || entry.Name() > predecessor {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(migrationVerificationBundlePG, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, entry.Name()), contents, 0600); err != nil {
			test.Fatal(err)
		}
	}
	if applied, err := storage.ApplyMigrations(fixture.ctx, directory); err != nil || applied != 148 {
		test.Fatal("trusted low-level helper did not install the exact historical predecessor", applied, err)
	}
	repo, err := storage.PutRepo(fixture.ctx, domain.Repo{ID: domain.HashContent([]byte("catalog-predecessor-repo")), DefaultBranch: "main"})
	if err != nil {
		test.Fatal(err)
	}
	body := []byte("historical chunk bytes\x00must survive the catalog upgrade\n")
	hash := domain.HashContent(body)
	if stored, deduped, err := storage.PutChunks(fixture.ctx, repo.ID, map[domain.ContentHash][]byte{hash: body}); err != nil || stored != 1 || deduped != 0 {
		test.Fatal("could not populate historical application data", stored, deduped, err)
	}
	assertData := func(test *testing.T) {
		test.Helper()
		if actual, err := storage.GetRepo(fixture.ctx, repo.ID); err != nil || !reflect.DeepEqual(actual, repo) {
			test.Fatal("catalog validation or upgrade changed the historical repository", actual, err)
		}
		if actual, err := storage.GetChunk(fixture.ctx, repo.ID, hash); err != nil || !bytes.Equal(actual, body) {
			test.Fatal("catalog validation or upgrade changed historical bytes or their ownership", err)
		}
	}
	before := migrationVerificationStatePG(test, fixture)
	test.Run("verify-rejects-consistent-predecessor", func(test *testing.T) {
		if err := storage.VerifyMigrations(fixture.ctx, directory); !errors.Is(err, ErrSchemaMismatch) {
			test.Error("current image accepted the internally consistent historical database and bundle", err)
		}
		if after := migrationVerificationStatePG(test, fixture); after != before {
			test.Error("rejected historical verification changed the catalog or migration receipts")
		}
		assertData(test)
	})
	test.Run("public-apply-rejects-consistent-predecessor", func(test *testing.T) {
		if applied, err := storage.ApplyPublicMigrations(fixture.ctx, directory); applied != 0 || !errors.Is(err, ErrSchemaMismatch) {
			test.Error("public operator accepted the internally consistent historical database and bundle", applied, err)
		}
		if after := migrationVerificationStatePG(test, fixture); after != before {
			test.Error("rejected historical public apply changed the catalog or migration receipts")
		}
		assertData(test)
	})
	manifest, err := migrationManifest(fixture.ctx, migrationVerificationBundlePG)
	if err != nil {
		test.Fatal(err)
	}
	pending := 0
	for _, migration := range manifest {
		if migration.name > predecessor {
			pending++
		}
	}
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil || applied != pending {
		test.Fatal("current complete bundle did not apply exactly its pending migrations", applied, pending, err)
	}
	current := migrationVerificationStatePG(test, fixture)
	if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("upgraded database rejected the current complete bundle", err)
	}
	if after := migrationVerificationStatePG(test, fixture); after != current {
		test.Fatal("current-bundle verification changed the upgraded catalog or receipts")
	}
	var historicalReceipts string
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT coalesce(jsonb_agg(to_jsonb(history) ORDER BY version),'[]'::jsonb)::text
 FROM public.schema_migrations history WHERE version<=$1`, predecessor).Scan(&historicalReceipts); err != nil {
		test.Fatal(err)
	}
	if historicalReceipts != before[1] || current[2] != before[2] {
		test.Fatal("current upgrade rewrote historical checksums, timestamps or ownership receipts")
	}
	assertData(test)
}
