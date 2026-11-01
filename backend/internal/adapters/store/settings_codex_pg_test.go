//go:build postgres

package store

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const (
	settingsCodexMigrationPG   = "0152_settings_codex.sql"
	settingsCodexPredecessorPG = "0151_legacy_byte_header_accounting.sql"
	settingsCodexCatalog151PG  = "5ccf1e9041446edf410205a978616aad20acbd3e07dac491fe80185d3a561994"
	settingsCodexCatalog152PG  = "42c721b3d691c2ff3f2b6d717da9a879246fe031ae464885a317c3ec14937f5b"
)

type settingsCodexImagePG struct {
	catalog [3]string
	rows    string
}

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

func settingsCodexHistoricalPG(test *testing.T) (*processAdmissionPGFixture, *PostgresStore, string) {
	test.Helper()
	settingsCodexRequireCurrentPG(test)
	directory := test.TempDir()
	manifest, err := migrationManifest(test.Context(), migrationVerificationBundlePG)
	if err != nil {
		test.Fatal(err)
	}
	hasher := sha256.New()
	copied := 0
	for _, migration := range manifest {
		if migration.name > settingsCodexPredecessorPG {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(migrationVerificationBundlePG, migration.name))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, migration.name), contents, 0600); err != nil {
			test.Fatal(err)
		}
		_, _ = hasher.Write([]byte(migration.name + "\x00" + migration.checksum + "\n"))
		copied++
	}
	if copied != 151 || hex.EncodeToString(hasher.Sum(nil)) != settingsCodexCatalog151PG {
		test.Fatal("historical fixture must copy the unchanged released 151 SQL files, never synthesized predecessor DDL", copied)
	}
	fixture, storage := migrationVerificationStorePG(test)
	if applied, err := storage.ApplyMigrations(fixture.ctx, directory); err != nil || applied != 151 {
		test.Fatal("private historical fixture installation did not apply exactly 151 migrations", applied, err)
	}
	return fixture, storage, directory
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

func settingsCodexBundlePG(kind, content string) domain.SettingsBundle {
	return domain.SettingsBundle{
		Kind: kind,
		Files: []domain.SettingsFile{
			{Path: "config.toml", ContentB64: base64.StdEncoding.EncodeToString([]byte(content))},
			{Path: "skills/local.txt", ContentB64: base64.StdEncoding.EncodeToString([]byte("retained\x00bytes\r\n\ud55c\uae00\n"))},
		},
		UpdatedAt: time.Date(2026, time.October, 28, 3, 4, 5, 0, time.UTC),
		UpdatedBy: "settings-fixture-owner",
	}
}

func settingsCodexHashPG(test *testing.T, bundle domain.SettingsBundle) domain.ContentHash {
	test.Helper()
	hash, err := domain.SettingsObjectHash(bundle)
	if err != nil {
		test.Fatal(err)
	}
	return hash
}

func settingsCodexLegacyPG(test *testing.T, fixture *processAdmissionPGFixture, storage *PostgresStore, repo domain.ContentHash) []domain.SettingsBundle {
	test.Helper()
	bundles := []domain.SettingsBundle{
		settingsCodexBundlePG("claude", "historical claude bytes\n"),
		settingsCodexBundlePG("agents", "historical agents bytes\n"),
	}
	for _, bundle := range bundles {
		if err := storage.PutSettingsBundle(fixture.ctx, repo, bundle); err != nil {
			test.Fatal(err)
		}
		if err := storage.PutSettingsObject(fixture.ctx, repo, settingsCodexHashPG(test, bundle), bundle); err != nil {
			test.Fatal(err)
		}
	}
	var metered bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT EXISTS(SELECT 1 FROM public.storage_accounts account
 JOIN public.repositories repository ON repository.owner_namespace_id=account.namespace_id
 JOIN public.repos replica ON replica.repository_id=repository.id
 WHERE replica.id=$1 AND account.current_bytes>0
 AND account.current_bytes=(SELECT sum(object.bytes) FROM public.storage_usage_objects object
 WHERE object.namespace_id=account.namespace_id))`, repo).Scan(&metered); err != nil || !metered {
		test.Fatal("owned legacy settings must retain their real accounting triggers and metering", err)
	}
	return bundles
}

func settingsCodexReadPG(test *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, bundle domain.SettingsBundle, object bool) {
	test.Helper()
	var actual domain.SettingsBundle
	var err error
	if object {
		actual, err = storage.GetSettingsObject(ctx, repo, settingsCodexHashPG(test, bundle))
	} else {
		actual, err = storage.GetSettingsBundle(ctx, repo, bundle.Kind)
	}
	if err != nil || !reflect.DeepEqual(actual, bundle) {
		test.Fatal("settings bytes, metadata, kind or repository ownership changed", object, actual, err)
	}
	if settingsCodexHashPG(test, actual) != settingsCodexHashPG(test, bundle) {
		test.Fatal("settings read did not preserve the content-addressed hash")
	}
}

func settingsCodexImageReadPG(test *testing.T, fixture *processAdmissionPGFixture) settingsCodexImagePG {
	test.Helper()
	image := settingsCodexImagePG{catalog: migrationVerificationStatePG(test, fixture)}
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT jsonb_build_object(
 'repos',(SELECT jsonb_agg(to_jsonb(replica) ORDER BY id) FROM public.repos replica),
 'repositories',(SELECT jsonb_agg(to_jsonb(repository) ORDER BY id) FROM public.repositories repository),
 'defaults',(SELECT jsonb_agg(jsonb_build_array(repo_id,kind,encode(convert_to(data::text,'UTF8'),'hex'))
 ORDER BY repo_id,kind) FROM public.repo_settings),
 'objects',(SELECT jsonb_agg(jsonb_build_array(repo_id,hash,encode(convert_to(data::text,'UTF8'),'hex'))
 ORDER BY repo_id,hash) FROM public.settings_objects),
 'snapshots',(SELECT jsonb_agg(to_jsonb(snapshot) ORDER BY repo_id,id) FROM public.snapshots snapshot),
 'blobs',(SELECT jsonb_agg(jsonb_build_array(hash,encode(bytes,'hex')) ORDER BY hash) FROM public.blobs),
 'ownership',(SELECT jsonb_agg(to_jsonb(owned) ORDER BY repo_id,kind,hash) FROM public.repo_blobs owned),
 'accounts',(SELECT jsonb_agg(to_jsonb(account) ORDER BY namespace_id) FROM public.storage_accounts account),
 'usage',(SELECT jsonb_agg(to_jsonb(object) ORDER BY namespace_id,object_key) FROM public.storage_usage_objects object),
 'ledger',(SELECT jsonb_agg(to_jsonb(receipt) ORDER BY seq) FROM public.storage_usage_ledger receipt),
 'policy',(SELECT jsonb_agg(to_jsonb(operation) ORDER BY id) FROM public.storage_policy_operations operation)
 )::text`).Scan(&image.rows); err != nil {
		test.Fatal("could not capture settings, ownership and accounting effects", err)
	}
	return image
}

func settingsCodexUnchangedPG(test *testing.T, fixture *processAdmissionPGFixture, storage *PostgresStore, before settingsCodexImagePG) {
	test.Helper()
	if after := settingsCodexImageReadPG(test, fixture); after != before {
		test.Fatal("operation changed catalog, settings bytes, ownership, snapshots, migration receipts or accounting")
	}
	if storage.pool.Stat().AcquiredConns() != 0 {
		test.Fatal("settings operation retained a pool connection")
	}
}

func settingsCodexConstraintPG(test *testing.T, fixture *processAdmissionPGFixture) (string, string) {
	test.Helper()
	var count int
	var name, definition string
	var validated bool
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT count(*),coalesce(min(conname::text),''),
 coalesce(min(pg_get_constraintdef(oid)),''),coalesce(bool_and(convalidated AND conislocal),false)
 FROM pg_constraint WHERE conrelid='public.repo_settings'::regclass AND contype='c'
 AND conkey=ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid='public.repo_settings'::regclass
 AND attname='kind' AND NOT attisdropped)]`).Scan(&count, &name, &definition, &validated); err != nil {
		test.Fatal(err)
	}
	if count != 1 || !validated {
		test.Fatal("positive fixture must have one validated local kind CHECK", count, validated)
	}
	return name, definition
}

func settingsCodexCheckErrorPG(test *testing.T, err error, constraint string) {
	test.Helper()
	var failure *pgconn.PgError
	if !errors.As(err, &failure) || failure.Code != "23514" || failure.SchemaName != "public" || failure.TableName != "repo_settings" || failure.ConstraintName != constraint {
		test.Fatal("expected the actual PostgreSQL settings kind CHECK cause, not a generic validation failure", err)
	}
}

func settingsCodexVerifyPG(test *testing.T, fixture *processAdmissionPGFixture, reader *PostgresStore, trace *migrationVerificationTracePG, mismatch bool) {
	test.Helper()
	before := settingsCodexImageReadPG(test, fixture)
	trace.readOnlyTransaction, trace.sharedLockInTransaction = false, false
	err := reader.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG)
	if mismatch {
		if !errors.Is(err, ErrSchemaMismatch) {
			test.Fatal("settings constraint drift did not fail closed with ErrSchemaMismatch", err)
		}
	} else if err != nil {
		test.Fatal("current settings schema rejected read-only verification", err)
	}
	if !trace.readOnlyTransaction || !trace.sharedLockInTransaction {
		test.Fatal("settings verification did not use its read-only transaction and shared migration lock")
	}
	settingsCodexUnchangedPG(test, fixture, reader, before)
}

func TestPGSettingsCodexHistorical151PublicUpgrade(test *testing.T) {
	migrations := settingsCodexRequireCurrentPG(test)
	fixture, storage, historical := settingsCodexHistoricalPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	legacy := settingsCodexLegacyPG(test, fixture, storage, repo)
	constraint, _ := settingsCodexConstraintPG(test, fixture)
	codex := settingsCodexBundlePG("codex", "model = \"codex\"\n")
	before := settingsCodexImageReadPG(test, fixture)
	settingsCodexCheckErrorPG(test, storage.PutSettingsBundle(fixture.ctx, repo, codex), constraint)
	settingsCodexUnchangedPG(test, fixture, storage, before)
	if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); !errors.Is(err, ErrSchemaMismatch) {
		test.Fatal("current runtime accepted unapplied migration 152", err)
	}
	if err := storage.VerifyMigrations(fixture.ctx, historical); !errors.Is(err, ErrSchemaMismatch) {
		test.Fatal("current runtime accepted an internally consistent historical 151 bundle", err)
	}
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, historical); applied != 0 || !errors.Is(err, ErrSchemaMismatch) {
		test.Fatal("public apply bypassed the current compiled catalog for a historical fixture", applied, err)
	}
	settingsCodexUnchangedPG(test, fixture, storage, before)
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil || applied != migrations-151 {
		test.Fatal("public upgrade must apply exactly the pending released migrations", applied, migrations-151, err)
	}
	after := settingsCodexImageReadPG(test, fixture)
	if after.rows != before.rows || after.catalog[2] != before.catalog[2] {
		test.Fatal("settings upgrade changed old settings bytes, object hashes, ownership or accounting receipts")
	}
	var historicalReceipts, checksum string
	var receiptCount int
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT jsonb_agg(to_jsonb(receipt) ORDER BY version)::text
 FROM public.schema_migrations receipt WHERE version<=$1`, settingsCodexPredecessorPG).Scan(&historicalReceipts); err != nil {
		test.Fatal(err)
	}
	if historicalReceipts != before.catalog[1] {
		test.Fatal("upgrade rewrote historical migration checksums or timestamps")
	}
	if err := fixture.observer.QueryRow(fixture.ctx, `SELECT (SELECT count(*) FROM public.schema_migrations),checksum
 FROM public.schema_migrations WHERE version=$1`, settingsCodexMigrationPG).Scan(&receiptCount, &checksum); err != nil {
		test.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(migrationVerificationBundlePG, settingsCodexMigrationPG))
	if err != nil {
		test.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	if receiptCount != migrations || checksum != hex.EncodeToString(digest[:]) {
		test.Fatal("upgrade did not record the complete bundle and exact migration 152 bytes", receiptCount, migrations, checksum)
	}
	for _, bundle := range legacy {
		settingsCodexReadPG(test, fixture.ctx, storage, repo, bundle, false)
		settingsCodexReadPG(test, fixture.ctx, storage, repo, bundle, true)
	}
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil || applied != 0 {
		test.Fatal("repeated full public apply was not idempotent", applied, err)
	}
	settingsCodexUnchangedPG(test, fixture, storage, after)
	trace := &migrationVerificationTracePG{}
	reader := migrationVerificationReaderPG(test, fixture, trace)
	settingsCodexVerifyPG(test, fixture, reader, trace, false)
	settingsCodexVerifyPG(test, fixture, reader, trace, false)
	if err := storage.PutSettingsBundle(fixture.ctx, repo, codex); err != nil {
		test.Fatal("explicit upgrade did not unlock Codex mutable settings", err)
	}
	if err := storage.PutSettingsObject(fixture.ctx, repo, settingsCodexHashPG(test, codex), codex); err != nil {
		test.Fatal(err)
	}
	settingsCodexReadPG(test, fixture.ctx, storage, repo, codex, false)
	settingsCodexReadPG(test, fixture.ctx, storage, repo, codex, true)
}

func TestPGSettingsCodexSaveReadReopenAndRepositoryScope(test *testing.T) {
	fixture, storage := settingsCodexCurrentPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	other := settingsCodexRepositoryPG(test, fixture, storage)
	original := settingsCodexBundlePG("codex", "model = \"original\"\n")
	hash := settingsCodexHashPG(test, original)
	if err := storage.PutSettingsBundle(fixture.ctx, repo, original); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSettingsObject(fixture.ctx, repo, hash, original); err != nil {
		test.Fatal(err)
	}
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events:   []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "settings snapshot ownership"}}}},
	}}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	for _, owner := range []domain.ContentHash{repo, other} {
		if _, err := storage.PutDoc(fixture.ctx, owner, document); err != nil {
			test.Fatal(err)
		}
	}
	snapshot := domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash, CodexSettings: hash, Branch: "main", Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}
	if err := storage.PutSnapshot(fixture.ctx, snapshot); err != nil {
		test.Fatal("could not save a real Codex snapshot with its owned settings hash", err)
	}
	beforeReplay := settingsCodexImageReadPG(test, fixture)
	replay := original
	replay.UpdatedAt = replay.UpdatedAt.Add(time.Hour)
	replay.UpdatedBy = "replayed-metadata-must-not-replace-object"
	if settingsCodexHashPG(test, replay) != hash {
		test.Fatal("metadata-only replay changed the canonical settings hash")
	}
	if err := storage.PutSettingsObject(fixture.ctx, repo, hash, replay); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSettingsBundle(fixture.ctx, repo, original); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(fixture.ctx, snapshot); err != nil {
		test.Fatal(err)
	}
	settingsCodexUnchangedPG(test, fixture, storage, beforeReplay)
	changed := settingsCodexBundlePG("codex", "model = \"changed\"\n")
	if settingsCodexHashPG(test, changed) == hash {
		test.Fatal("different Codex file bytes unexpectedly share a hash")
	}
	if err := storage.PutSettingsBundle(fixture.ctx, repo, changed); err != nil {
		test.Fatal(err)
	}
	settingsCodexReadPG(test, fixture.ctx, storage, repo, changed, false)
	settingsCodexReadPG(test, fixture.ctx, storage, repo, original, true)
	missingHash := domain.HashContent([]byte("not a stored settings object"))
	unknown := settingsCodexBundlePG("unknown", "must not be stored\n")
	constraint, _ := settingsCodexConstraintPG(test, fixture)
	foreignSnapshot := snapshot
	foreignSnapshot.RepoID = other
	for _, rejection := range []struct {
		name string
		run  func(*testing.T)
	}{
		{"foreign-repository", func(test *testing.T) {
			if _, err := storage.GetSettingsBundle(fixture.ctx, other, "codex"); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("foreign repository read mutable settings", err)
			}
			if _, err := storage.GetSettingsObject(fixture.ctx, other, hash); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("global hash leaked another repository's settings object", err)
			}
			if err := storage.PutSnapshot(fixture.ctx, foreignSnapshot); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("snapshot borrowed a foreign repository's settings hash", err)
			}
		}},
		{"wrong-hash", func(test *testing.T) {
			if err := storage.PutSettingsObject(fixture.ctx, repo, missingHash, original); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("settings object accepted a hash unrelated to its actual bytes", err)
			}
			if _, err := storage.GetSettingsObject(fixture.ctx, repo, missingHash); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("rejected object hash became readable", err)
			}
			invalid := snapshot
			invalid.ID = domain.HashContent([]byte("wrong settings snapshot"))
			invalid.CodexSettings = missingHash
			if err := storage.PutSnapshot(fixture.ctx, invalid); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("snapshot accepted an absent settings object hash", err)
			}
		}},
		{"unknown-kind", func(test *testing.T) {
			if err := storage.PutSettingsBundle(fixture.ctx, repo, unknown); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("mutable settings accepted an unknown kind", err)
			}
			if err := storage.PutSettingsObject(fixture.ctx, repo, settingsCodexHashPG(test, unknown), unknown); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("content-addressed settings accepted an unknown kind", err)
			}
			if _, err := storage.GetSettingsBundle(fixture.ctx, repo, "unknown"); !errors.Is(err, domain.ErrValidation) {
				test.Fatal("mutable settings read accepted an unknown kind", err)
			}
			_, err := fixture.observer.Exec(fixture.ctx, `INSERT INTO public.repo_settings(repo_id,kind,data) VALUES($1,'unknown','{}'::jsonb)`, repo)
			settingsCodexCheckErrorPG(test, err, constraint)
		}},
	} {
		test.Run(rejection.name, func(test *testing.T) {
			before := settingsCodexImageReadPG(test, fixture)
			rejection.run(test)
			settingsCodexUnchangedPG(test, fixture, storage, before)
		})
	}
	if err := storage.PutSettingsObject(fixture.ctx, other, hash, original); err != nil {
		test.Fatal("explicit ownership of the same hash in a second repository was rejected", err)
	}
	if err := storage.PutSettingsBundle(fixture.ctx, other, original); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(fixture.ctx, foreignSnapshot); err != nil {
		test.Fatal("explicitly owned settings did not unlock the second repository's save", err)
	}
	beforeReopen := settingsCodexImageReadPG(test, fixture)
	configuration := storage.pool.Config().Copy()
	storage.Close()
	pool, err := pgxpool.NewWithConfig(fixture.ctx, configuration)
	if err != nil {
		test.Fatal(err)
	}
	reopened := &PostgresStore{pool: pool}
	test.Cleanup(reopened.Close)
	if err := reopened.Ping(fixture.ctx); err != nil {
		test.Fatal("could not reopen the same isolated settings database", err)
	}
	settingsCodexReadPG(test, fixture.ctx, reopened, repo, changed, false)
	settingsCodexReadPG(test, fixture.ctx, reopened, other, original, false)
	for _, owner := range []domain.ContentHash{repo, other} {
		settingsCodexReadPG(test, fixture.ctx, reopened, owner, original, true)
		actual, err := reopened.GetSnapshot(fixture.ctx, owner, snapshot.ID)
		if err != nil || actual.RepoID != owner || actual.CodexSettings != hash || actual.DocHash != document.Hash {
			test.Fatal("saved Codex settings reference changed after reopen", actual, err)
		}
	}
	if err := reopened.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("reopened settings store rejected the complete current schema", err)
	}
	settingsCodexUnchangedPG(test, fixture, reopened, beforeReopen)
}

func TestPGSettingsCodexCancelledWritesAndRepositoryRollback(test *testing.T) {
	fixture, storage := settingsCodexCurrentPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	settingsCodexLegacyPG(test, fixture, storage, repo)
	original := settingsCodexBundlePG("codex", "model = \"committed\"\n")
	changed := settingsCodexBundlePG("codex", "model = \"never committed\"\n")
	if err := storage.PutSettingsBundle(fixture.ctx, repo, original); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSettingsObject(fixture.ctx, repo, settingsCodexHashPG(test, original), original); err != nil {
		test.Fatal(err)
	}
	before := settingsCodexImageReadPG(test, fixture)
	cancelled, cancel := context.WithCancel(fixture.ctx)
	cancel()
	if err := storage.PutSettingsBundle(cancelled, repo, changed); !errors.Is(err, context.Canceled) {
		test.Fatal("cancelled mutable settings write lost its cancellation cause", err)
	}
	if err := storage.PutSettingsObject(cancelled, repo, settingsCodexHashPG(test, changed), changed); !errors.Is(err, context.Canceled) {
		test.Fatal("cancelled settings object write lost its cancellation cause", err)
	}
	if applied, err := storage.ApplyPublicMigrations(cancelled, migrationVerificationBundlePG); applied != 0 || !errors.Is(err, context.Canceled) {
		test.Fatal("cancelled public apply reported a committed migration", applied, err)
	}
	settingsCodexUnchangedPG(test, fixture, storage, before)
	constraint, _ := settingsCodexConstraintPG(test, fixture)
	abort := errors.New("owned settings rollback sentinel")
	for _, outcome := range []string{"callback-error", "cancel-after-writes", "postgres-check-error"} {
		test.Run(outcome, func(test *testing.T) {
			work, stop := context.WithCancel(fixture.ctx)
			defer stop()
			before := settingsCodexImageReadPG(test, fixture)
			staged := false
			err := storage.WithinRepository(work, repo, func(bound context.Context) error {
				if err := storage.PutSettingsBundle(bound, repo, changed); err != nil {
					return err
				}
				if err := storage.PutSettingsObject(bound, repo, settingsCodexHashPG(test, changed), changed); err != nil {
					return err
				}
				settingsCodexReadPG(test, bound, storage, repo, changed, false)
				settingsCodexReadPG(test, bound, storage, repo, changed, true)
				if observed := settingsCodexImageReadPG(test, fixture); observed != before {
					test.Fatal("settings write committed before the outer repository transaction")
				}
				staged = true
				switch outcome {
				case "cancel-after-writes":
					stop()
					return work.Err()
				case "postgres-check-error":
					_, err := storage.db(bound).Exec(bound, `INSERT INTO public.repo_settings(repo_id,kind,data) VALUES($1,'unknown','{}'::jsonb)`, repo)
					return err
				default:
					return abort
				}
			})
			if !staged {
				test.Fatal("rollback control did not stage both real settings writes", err)
			}
			switch outcome {
			case "cancel-after-writes":
				if !errors.Is(err, context.Canceled) {
					test.Fatal("outer settings cancellation lost its actual cause", err)
				}
			case "postgres-check-error":
				settingsCodexCheckErrorPG(test, err, constraint)
			default:
				if !errors.Is(err, abort) {
					test.Fatal("outer settings rollback lost the callback error", err)
				}
			}
			settingsCodexUnchangedPG(test, fixture, storage, before)
			settingsCodexReadPG(test, fixture.ctx, storage, repo, original, false)
			if _, err := storage.GetSettingsObject(fixture.ctx, repo, settingsCodexHashPG(test, changed)); !errors.Is(err, domain.ErrNotFound) {
				test.Fatal("rolled-back settings object remained readable", err)
			}
		})
	}
}

func TestPGSettingsCodexMigrationCancellationAndReceiptRollback(test *testing.T) {
	migrations := settingsCodexRequireCurrentPG(test)
	fixture, storage, _ := settingsCodexHistoricalPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	settingsCodexLegacyPG(test, fixture, storage, repo)
	constraint, _ := settingsCodexConstraintPG(test, fixture)
	codex := settingsCodexBundlePG("codex", "must remain rejected until migration commits\n")
	test.Run("cancel-blocked-ddl", func(test *testing.T) {
		migrationStore := authorityScopePeerPG(test, storage, "", nil)
		migrationPID := authorityScopePIDPG(test, fixture.ctx, migrationStore)
		blocker, err := pgx.ConnectConfig(fixture.ctx, fixture.observer.Config().Copy())
		if err != nil {
			test.Fatal(err)
		}
		defer blocker.Close(fixture.ctx)
		locker, err := blocker.Begin(fixture.ctx)
		if err != nil {
			test.Fatal(err)
		}
		defer rollbackPG(locker)
		if _, err := locker.Exec(fixture.ctx, `LOCK TABLE public.repo_settings IN ACCESS SHARE MODE`); err != nil {
			test.Fatal(err)
		}
		before := settingsCodexImageReadPG(test, fixture)
		applied := -1
		work, cancel, completed := authorityScopeRunPG(test, fixture.ctx, func(work context.Context) error {
			var err error
			applied, err = migrationStore.ApplyPublicMigrations(work, migrationVerificationBundlePG)
			return err
		})
		authorityScopeWaitPG(test, work, fixture.observer, blocker.PgConn().PID(), migrationPID, "repo_settings", completed)
		cancel()
		select {
		case err := <-completed:
			if applied != 0 || !errors.Is(err, context.Canceled) {
				test.Fatal("cancelled real settings DDL wait reported a committed migration or lost its cause", applied, err)
			}
		case <-time.After(5 * time.Second):
			test.Fatal("cancelled settings migration did not finish")
		}
		if err := locker.Rollback(fixture.ctx); err != nil {
			test.Fatal(err)
		}
		settingsCodexUnchangedPG(test, fixture, storage, before)
		settingsCodexCheckErrorPG(test, storage.PutSettingsBundle(fixture.ctx, repo, codex), constraint)
		settingsCodexUnchangedPG(test, fixture, storage, before)
	})
	test.Run("receipt-failure-rolls-back-ddl", func(test *testing.T) {
		if _, err := fixture.observer.Exec(fixture.ctx, `CREATE FUNCTION public.settings_codex_reject_receipt_fixture()
 RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
 RAISE EXCEPTION USING ERRCODE='23514',SCHEMA='public',TABLE='schema_migrations',
 CONSTRAINT='settings_codex_receipt_fixture',MESSAGE='owned settings migration receipt rejection';
 END $$;
 CREATE TRIGGER settings_codex_receipt_fixture BEFORE INSERT ON public.schema_migrations
 FOR EACH ROW WHEN (NEW.version='0152_settings_codex.sql')
 EXECUTE FUNCTION public.settings_codex_reject_receipt_fixture()`); err != nil {
			test.Fatal(err)
		}
		test.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err := fixture.observer.Exec(cleanup, `DROP TRIGGER IF EXISTS settings_codex_receipt_fixture ON public.schema_migrations;
 DROP FUNCTION IF EXISTS public.settings_codex_reject_receipt_fixture()`); err != nil {
				test.Error("could not restore the owned receipt-failure fixture", err)
			}
		})
		before := settingsCodexImageReadPG(test, fixture)
		applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG)
		var failure *pgconn.PgError
		if applied != 0 || !errors.As(err, &failure) || failure.Code != "23514" || failure.TableName != "schema_migrations" || failure.ConstraintName != "settings_codex_receipt_fixture" {
			test.Fatal("migration did not reach the actual injected receipt failure after its DDL", applied, err)
		}
		settingsCodexUnchangedPG(test, fixture, storage, before)
		settingsCodexCheckErrorPG(test, storage.PutSettingsBundle(fixture.ctx, repo, codex), constraint)
		settingsCodexUnchangedPG(test, fixture, storage, before)
	})
	if applied, err := storage.ApplyPublicMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil || applied != migrations-151 {
		test.Fatal("restored fixture did not apply each pending migration exactly once", applied, migrations-151, err)
	}
	if err := storage.PutSettingsBundle(fixture.ctx, repo, codex); err != nil {
		test.Fatal("committed retry did not unlock Codex settings", err)
	}
	if err := storage.VerifyMigrations(fixture.ctx, migrationVerificationBundlePG); err != nil {
		test.Fatal("successful retry left a partial settings migration", err)
	}
}

func TestPGSettingsCodexVerifyConstraintDriftReadOnly(test *testing.T) {
	fixture, storage := settingsCodexCurrentPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	settingsCodexLegacyPG(test, fixture, storage, repo)
	constraint, definition := settingsCodexConstraintPG(test, fixture)
	quoted := pgx.Identifier{constraint}.Sanitize()
	trace := &migrationVerificationTracePG{}
	reader := migrationVerificationReaderPG(test, fixture, trace)
	settingsCodexVerifyPG(test, fixture, reader, trace, false)
	baseline := settingsCodexImageReadPG(test, fixture)
	for _, drift := range []struct {
		name        string
		replacement string
	}{
		{"receipt-present-ddl-not-applied", "CHECK (kind IN ('claude','agents'))"},
		{"missing-check", ""},
		{"not-valid-check", definition + " NOT VALID"},
		{"widened-check", "CHECK (kind IN ('claude','agents','codex','unknown'))"},
	} {
		test.Run(drift.name, func(test *testing.T) {
			test.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if _, err := fixture.observer.Exec(cleanup, "ALTER TABLE public.repo_settings DROP CONSTRAINT IF EXISTS "+quoted+
					"; ALTER TABLE public.repo_settings ADD CONSTRAINT "+quoted+" "+definition); err != nil {
					test.Error("could not restore the owned settings CHECK fixture", err)
				}
			})
			statement := "ALTER TABLE public.repo_settings DROP CONSTRAINT " + quoted
			if drift.replacement != "" {
				statement += "; ALTER TABLE public.repo_settings ADD CONSTRAINT " + quoted + " " + drift.replacement
			}
			if _, err := fixture.observer.Exec(fixture.ctx, statement); err != nil {
				test.Fatal("could not install the owned constraint drift fixture", err)
			}
			before := settingsCodexImageReadPG(test, fixture)
			if before.catalog[1] != baseline.catalog[1] || before.catalog[2] != baseline.catalog[2] || before.rows != baseline.rows {
				test.Fatal("constraint drift fixture changed actual application data or migration receipts")
			}
			settingsCodexVerifyPG(test, fixture, reader, trace, true)
			settingsCodexVerifyPG(test, fixture, reader, trace, true)
		})
		settingsCodexVerifyPG(test, fixture, reader, trace, false)
	}
}

func TestPGSettingsCodexStoredPayloadReadValidation(test *testing.T) {
	fixture, storage := settingsCodexCurrentPG(test)
	repo := settingsCodexRepositoryPG(test, fixture, storage)
	original := settingsCodexBundlePG("codex", "model = \"retained\"\n")
	hash := settingsCodexHashPG(test, original)
	if err := storage.PutSettingsBundle(fixture.ctx, repo, original); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSettingsObject(fixture.ctx, repo, hash, original); err != nil {
		test.Fatal(err)
	}
	for _, corruption := range []struct {
		name   string
		object bool
		bundle domain.SettingsBundle
	}{
		{"object-bytes-do-not-match-address", true, settingsCodexBundlePG("codex", "model = \"corrupt\"\n")},
		{"object-unknown-kind", true, settingsCodexBundlePG("unknown", "model = \"retained\"\n")},
		{"mutable-kind-disagrees-with-key", false, settingsCodexBundlePG("agents", "model = \"retained\"\n")},
	} {
		test.Run(corruption.name, func(test *testing.T) {
			statement := `UPDATE public.repo_settings SET data=$3::jsonb WHERE repo_id=$1 AND kind=$2`
			key := "codex"
			if corruption.object {
				statement = `UPDATE public.settings_objects SET data=$3::jsonb WHERE repo_id=$1 AND hash=$2`
				key = string(hash)
			}
			originalJSON, err := json.Marshal(original)
			if err != nil {
				test.Fatal(err)
			}
			test.Cleanup(func() {
				cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				if restored, err := fixture.observer.Exec(cleanup, statement, repo, key, string(originalJSON)); err != nil || restored.RowsAffected() != 1 {
					test.Error("could not restore the owned corrupt-payload fixture with accounting enabled", err)
				}
			})
			corruptJSON, err := json.Marshal(corruption.bundle)
			if err != nil {
				test.Fatal(err)
			}
			if changed, err := fixture.observer.Exec(fixture.ctx, statement, repo, key, string(corruptJSON)); err != nil || changed.RowsAffected() != 1 {
				test.Fatal("could not install the owned corrupt-payload fixture", err)
			}
			before := settingsCodexImageReadPG(test, fixture)
			if corruption.object {
				_, err = storage.GetSettingsObject(fixture.ctx, repo, hash)
			} else {
				_, err = storage.GetSettingsBundle(fixture.ctx, repo, "codex")
			}
			if !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("stored settings corruption was returned as trusted content", err)
			}
			settingsCodexUnchangedPG(test, fixture, storage, before)
		})
		settingsCodexReadPG(test, fixture.ctx, storage, repo, original, false)
		settingsCodexReadPG(test, fixture.ctx, storage, repo, original, true)
	}
}
