//go:build postgres

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const (
	settingsCodexMigrationPG  = "0152_settings_codex.sql"
	settingsCodexCatalog152PG = "42c721b3d691c2ff3f2b6d717da9a879246fe031ae464885a317c3ec14937f5b"
)

func settingsCodexRequireCurrentPG(test *testing.T) int {
	test.Helper()
	manifest, err := migrationManifest(test.Context(), migrationVerificationBundlePG)
	if err != nil {
		test.Fatal(err)
	}
	historical := sha256.New()
	count := 0
	for _, migration := range manifest {
		if migration.name <= settingsCodexMigrationPG {
			_, _ = historical.Write([]byte(migration.name + "\x00" + migration.checksum + "\n"))
			count++
		}
	}
	if count != 152 || hex.EncodeToString(historical.Sum(nil)) != settingsCodexCatalog152PG {
		test.Fatal("current bundle must preserve all released 152 settings migration files", count)
	}
	if err := verifyMigrationCatalog(manifest); err != nil {
		test.Fatal("current public bundle does not match the compiled catalog", err)
	}
	return len(manifest)
}

func settingsCodexCurrentPG(test *testing.T) (*processAdmissionPGFixture, *PostgresStore) {
	test.Helper()
	migrations := settingsCodexRequireCurrentPG(test)
	fixture, storage := migrationVerificationStorePG(test)
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil || applied != migrations {
		test.Fatal("explicit public owner did not install the complete released bundle", applied, migrations, err)
	}
	return fixture, storage
}

func settingsCodexRepositoryPG(test *testing.T, fixture *processAdmissionPGFixture, storage *PostgresStore) domain.ContentHash {
	test.Helper()
	owner := domain.User{ID: domain.NewID("user_"), Username: domain.NewID("settings"), Name: "Settings fixture owner", Email: "settings@example.test"}
	if err := storage.UpsertUser(fixture.ctx, owner); err != nil {
		test.Fatal(err)
	}
	namespace := domain.Namespace{ID: domain.NewID("ns_"), Slug: owner.Username, Kind: domain.NamespaceUser, UserID: owner.ID, CreatedAt: time.Now().UTC()}
	if err := storage.CreateNamespace(fixture.ctx, namespace); err != nil {
		test.Fatal(err)
	}
	record := domain.Repository{ID: domain.NewID("ws_"), OwnerID: owner.ID, OwnerUsername: owner.Username, OwnerNamespaceID: namespace.ID, Name: "Settings regression", Slug: "settings", Visibility: domain.VisibilityPrivate}
	if err := storage.CreateRepository(fixture.ctx, record); err != nil {
		test.Fatal(err)
	}
	repo, err := storage.PutRepo(fixture.ctx, domain.Repo{ID: domain.HashContent([]byte(record.ID)), RepositoryID: record.ID, DefaultBranch: "main"})
	if err != nil {
		test.Fatal(err)
	}
	return repo.ID
}
