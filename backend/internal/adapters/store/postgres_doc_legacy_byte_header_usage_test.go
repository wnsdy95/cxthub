//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyByteHeaderUsagePGSource(test *testing.T, storage *PostgresStore, ctx context.Context) legacyByteStagePGFixture {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	return legacyByteStagePGDocument(test, storage, ctx, repo, domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SessionOriginID: strings.Repeat("h", 3*65536) + string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{{Kind: domain.EventMessage, Seq: 1, Role: domain.RoleUser,
			Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + strings.Repeat("x", 2<<20)}}}},
	})
}

func legacyByteHeaderUsagePGTransaction(test *testing.T, storage *PostgresStore, ctx context.Context) pgx.Tx {
	test.Helper()
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { rollbackPG(transaction) })
	if _, err := transaction.Exec(ctx, `SET LOCAL cxt.legacy_byte_stage_protocol='1'; SET LOCAL cxt.legacy_byte_header_protocol='1'`); err != nil {
		test.Fatal(err)
	}
	return transaction
}

func legacyByteHeaderUsagePGDeclare(test *testing.T, ctx context.Context, transaction pgx.Tx, fixture legacyByteStagePGFixture) {
	test.Helper()
	changed, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1
 SET header_version=1,header_bytes=$2 WHERE doc_hash=$1`, fixture.hash, len(fixture.envelope))
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("declare header extent", changed, err)
	}
}

func legacyByteHeaderUsagePGAppend(test *testing.T, ctx context.Context, transaction pgx.Tx, fixture legacyByteStagePGFixture, ordinal int) {
	test.Helper()
	start := ordinal * 65536
	body := fixture.envelope[start:min(start+65536, len(fixture.envelope))]
	if _, err := transaction.Exec(ctx, `INSERT INTO doc_legacy_byte_stage_headers_v1(doc_hash,ordinal,hash,bytes)
 VALUES($1,$2,$3,$4)`, fixture.hash, ordinal, domain.HashContent(body), []byte(body)); err != nil {
		test.Fatal(err)
	}
	changed, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stages_v1
 SET header_next=$2,header_complete=$3 WHERE doc_hash=$1`, fixture.hash, ordinal+1, start+len(body) == len(fixture.envelope))
	if err != nil || changed.RowsAffected() != 1 {
		test.Fatal("advance header checkpoint", changed, err)
	}
}

func legacyByteHeaderUsagePGAssertDelta(test *testing.T, storage *PostgresStore, ctx context.Context, initial, expected domain.LegacyDerivedByteStorageUsage) {
	test.Helper()
	actual := legacyByteUsagePGRead(test, storage, ctx)
	actual.Sources -= initial.Sources
	actual.CompleteSources -= initial.CompleteSources
	actual.Objects -= initial.Objects
	actual.Parts -= initial.Parts
	actual.PayloadBytes -= initial.PayloadBytes
	actual.HeaderBytes -= initial.HeaderBytes
	actual.HeaderParts -= initial.HeaderParts
	actual.SpecBytes -= initial.SpecBytes
	if actual != expected {
		test.Fatalf("header storage usage delta: got=%+v want=%+v", actual, expected)
	}
}

func TestLegacyByteHeaderUsageValidation(test *testing.T) {
	for _, usage := range []domain.LegacyDerivedByteStorageUsage{
		{HeaderBytes: -1}, {HeaderParts: -1}, {CheckpointBytes: -1}, {CheckpointObjects: -1},
	} {
		if err := usage.Validate(); !errors.Is(err, domain.ErrValidation) {
			test.Fatal("negative private byte usage accepted", usage, err)
		}
	}
	for _, usage := range []domain.LegacyDerivedByteStorageUsage{
		{}, {HeaderBytes: 17, HeaderParts: 2}, {CheckpointBytes: 17, CheckpointObjects: 2},
		{Sources: 1, CompleteSources: 1, Objects: 2, Parts: 5, PayloadBytes: 99, HeaderBytes: 131073, HeaderParts: 3},
	} {
		if err := usage.Validate(); err != nil {
			test.Fatal("independent header usage rejected", usage, err)
		}
	}
}

func TestLegacyByteHeaderUsagePGActualPartsRollbackSnapshotAndCascade(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	initial := legacyByteUsagePGRead(test, storage, ctx)
	fixture := legacyByteHeaderUsagePGSource(test, storage, ctx)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil || stage.complete {
		test.Fatal("fixture must leave the stream incomplete", stage, err)
	}
	expected := legacyByteUsagePGExpected(fixture, stage, len(session.encoded))
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	declaration := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
	legacyByteHeaderUsagePGDeclare(test, ctx, declaration, fixture)
	if err := declaration.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	rolledBack := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
	legacyByteHeaderUsagePGAppend(test, ctx, rolledBack, fixture, 0)
	var headerBytes, headerParts, objectBytes, objectCount, streamParts int64
	if err := rolledBack.QueryRow(ctx, `SELECT header_bytes,header_parts,object_bytes,object_count,part_count
 FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&headerBytes, &headerParts, &objectBytes, &objectCount, &streamParts); err != nil ||
		headerBytes != 65536 || headerParts != 1 || objectBytes != expected.PayloadBytes ||
		objectCount != expected.Objects || streamParts != expected.Parts {
		test.Fatal("header insert did not meter its own actual bytes independently", headerBytes, headerParts, objectBytes, objectCount, streamParts, err)
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	if err := rolledBack.Rollback(ctx); err != nil {
		test.Fatal(err)
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		before := legacyByteUsagePGRead(test, storage, read)
		first := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
		legacyByteHeaderUsagePGAppend(test, ctx, first, fixture, 0)
		if err := first.Commit(ctx); err != nil {
			return err
		}
		expected.HeaderBytes, expected.HeaderParts = 65536, 1
		legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
		if after := legacyByteUsagePGRead(test, storage, read); after != before {
			test.Fatal("header commit escaped a repeatable-read usage snapshot", before, after)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	parts := (len(fixture.envelope) + 65535) / 65536
	for ordinal := 1; ordinal < parts; ordinal++ {
		transaction := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
		legacyByteHeaderUsagePGAppend(test, ctx, transaction, fixture, ordinal)
		if err := transaction.Commit(ctx); err != nil {
			test.Fatal(err)
		}
		expected.HeaderBytes = int64(min((ordinal+1)*65536, len(fixture.envelope)))
		expected.HeaderParts = int64(ordinal + 1)
		legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	}
	var distinctHashes, storedParts, storedBytes int64
	if err := storage.pool.QueryRow(ctx, `SELECT count(DISTINCT hash),count(*),sum(octet_length(bytes))::bigint
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&distinctHashes, &storedParts, &storedBytes); err != nil || distinctHashes >= storedParts ||
		storedParts != expected.HeaderParts || storedBytes != expected.HeaderBytes || storedBytes != int64(len(fixture.envelope)) {
		test.Fatal("header usage deduplicated identical payloads or counted framing instead of stored bytes", distinctHashes, storedParts, storedBytes, expected, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("header accounting changed source, ownership, snapshot, job or publication")
	}
	stage, err = storage.stageLegacyByteSession(ctx, job, session, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || !stage.complete {
		test.Fatal("existing stream writer could not finish beside a completed header", stage, err)
	}
	expected = legacyByteUsagePGExpected(fixture, stage, len(session.encoded))
	expected.HeaderBytes, expected.HeaderParts = int64(len(fixture.envelope)), int64(parts)
	if expected.Objects >= expected.Parts {
		test.Fatal("fixture must retain deduplicated stream objects", expected)
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	usage, err := storage.ReadLegacyDerivedByteStorageUsage(ctx)
	if err != nil {
		test.Fatal(err)
	}
	var tableBytes, indexBytes int64
	if err := storage.pool.QueryRow(ctx, `SELECT sum(pg_table_size(relation))::bigint,sum(pg_indexes_size(relation))::bigint
 FROM unnest(ARRAY['doc_legacy_byte_stages_v1'::regclass,'doc_legacy_byte_stage_objects_v1'::regclass,
 'doc_legacy_byte_stage_parts_v1'::regclass,'doc_legacy_byte_stage_headers_v1'::regclass,
 'doc_legacy_byte_usage_v1'::regclass,'doc_legacy_byte_stage_checkpoint_objects_v1'::regclass,
 'doc_legacy_checkpoint_staging_v1'::regclass,'doc_legacy_checkpoint_structures_v1'::regclass,'doc_legacy_checkpoint_edges_v1'::regclass]) relation`).Scan(&tableBytes, &indexBytes); err != nil ||
		usage.TableBytes != tableBytes || usage.IndexBytes != indexBytes {
		test.Fatal("physical allocation omits header storage", usage, tableBytes, indexBytes, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if actual, err := storage.ReadLegacyDerivedByteStorageUsage(canceled); !errors.Is(err, context.Canceled) || actual != (domain.LegacyDerivedByteStorageUsage{}) {
		test.Fatal("canceled accounting read returned usage", actual, err)
	}
	if err := storage.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
		_, err := storage.ReadLegacyDerivedByteStorageUsage(bound)
		return err
	}); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("accounting read accepted a mutable repository transaction", err)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, domain.LegacyDerivedByteStorageUsage{})
	var retained int64
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&retained); err != nil || retained != 0 {
		test.Fatal("stage cascade retained header payloads", retained, err)
	}
}

func TestLegacyByteHeaderUsagePGTrustedAccountingGuards(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	initial := legacyByteUsagePGRead(test, storage, ctx)
	fixture := legacyByteHeaderUsagePGSource(test, storage, ctx)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil {
		test.Fatal(err)
	}
	expected := legacyByteUsagePGExpected(fixture, stage, len(session.encoded))
	declaration := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
	legacyByteHeaderUsagePGDeclare(test, ctx, declaration, fixture)
	if err := declaration.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	var definer, trustedOwner, privateExecution, guardInvoker, privateUsage bool
	var settings []string
	if err := storage.pool.QueryRow(ctx, `SELECT accounting.prosecdef,accounting.proowner=usage.relowner,accounting.proconfig,
 NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(accounting.proacl,acldefault('f',accounting.proowner))) permission
 WHERE permission.grantee=0 AND permission.privilege_type='EXECUTE'),NOT guard.prosecdef,
 NOT EXISTS(SELECT 1 FROM aclexplode(coalesce(usage.relacl,acldefault('r',usage.relowner))) permission WHERE permission.grantee=0)
 FROM pg_proc accounting CROSS JOIN pg_class usage CROSS JOIN pg_proc guard
 WHERE accounting.oid='public.cxt_account_legacy_byte_storage()'::regprocedure
 AND usage.oid='public.doc_legacy_byte_usage_v1'::regclass
 AND guard.oid='public.cxt_guard_legacy_byte_usage()'::regprocedure`).
		Scan(&definer, &trustedOwner, &settings, &privateExecution, &guardInvoker, &privateUsage); err != nil ||
		!definer || !trustedOwner || !privateExecution || !guardInvoker || !privateUsage ||
		!strings.Contains(strings.ReplaceAll(strings.Join(settings, ";"), " ", ""), "search_path=pg_catalog,public,pg_temp") {
		test.Fatal("header accounting weakened its owner, namespace, execution or usage guard", definer, trustedOwner, settings, privateExecution, guardInvoker, privateUsage, err)
	}
	for _, statement := range []string{
		`INSERT INTO doc_legacy_byte_usage_v1(doc_hash,header_bytes,header_parts) VALUES($1,1,1) ON CONFLICT DO NOTHING`,
		`UPDATE doc_legacy_byte_usage_v1 SET header_bytes=header_bytes+1 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_usage_v1 SET header_parts=header_parts+1 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_usage_v1 SET header_bytes=0,header_parts=0 WHERE doc_hash=$1`,
		`UPDATE doc_legacy_byte_usage_v1 SET object_bytes=object_bytes+1 WHERE doc_hash=$1`,
		`DELETE FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`,
	} {
		attempt := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
		_, err := attempt.Exec(ctx, statement, fixture.hash)
		rollbackPG(attempt)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "23514" {
			test.Fatal("protocol flags authorized a direct usage mutation", statement, err)
		}
	}
	for _, setup := range []string{
		`CREATE TEMP TABLE legacy_header_usage_unrelated(doc_hash text,ordinal integer,hash text,bytes bytea);
 CREATE TRIGGER account_legacy_byte_headers AFTER INSERT ON legacy_header_usage_unrelated
 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`,
		`CREATE TRIGGER wrong_header_accounting AFTER INSERT ON public.doc_legacy_byte_stage_headers_v1
 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`,
		`CREATE TRIGGER wrong_header_accounting BEFORE INSERT ON public.doc_legacy_byte_stage_headers_v1
 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`,
		`CREATE TRIGGER wrong_header_accounting AFTER INSERT ON public.doc_legacy_byte_stage_headers_v1
 FOR EACH STATEMENT EXECUTE FUNCTION public.cxt_account_legacy_byte_storage()`,
		`CREATE TRIGGER wrong_header_accounting AFTER INSERT ON public.doc_legacy_byte_stage_headers_v1
 FOR EACH ROW EXECUTE FUNCTION public.cxt_account_legacy_byte_storage('untrusted')`,
	} {
		attempt := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
		if _, err := attempt.Exec(ctx, setup); err != nil {
			test.Fatal(err)
		}
		table := "public.doc_legacy_byte_stage_headers_v1"
		if strings.Contains(setup, "CREATE TEMP TABLE") {
			table = "legacy_header_usage_unrelated"
		}
		body := fixture.envelope[:65536]
		_, err := attempt.Exec(ctx, `INSERT INTO `+table+`(doc_hash,ordinal,hash,bytes) VALUES($1,0,$2,$3)`,
			fixture.hash, domain.HashContent(body), []byte(body))
		rollbackPG(attempt)
		var rejected *pgconn.PgError
		if !errors.As(err, &rejected) || rejected.Code != "23514" {
			test.Fatal("unrelated relation, trigger, phase, level or arguments invoked trusted accounting", setup, err)
		}
		legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, initial, expected)
	}
}

func TestLegacyByteHeaderUsagePGMigrationBackfillsOnlyStoredPayload(test *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	storage, err := NewPostgresStore(ctx, rootProjectionIsolatedDSN(test, ctx))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	directory := "../../../../schemas/db/migrations"
	predecessor := test.TempDir()
	upgrade := test.TempDir()
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() > "0151_legacy_byte_header_accounting.sql" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			test.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(upgrade, entry.Name()), raw, 0600); err != nil {
			test.Fatal(err)
		}
		if entry.Name() < "0151" {
			if err := os.WriteFile(filepath.Join(predecessor, entry.Name()), raw, 0600); err != nil {
				test.Fatal(err)
			}
		}
	}
	if _, err := storage.ApplyMigrations(ctx, predecessor); err != nil {
		test.Fatal(err)
	}
	fixture := legacyByteHeaderUsagePGSource(test, storage, ctx)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	stage, err := storage.stageLegacyByteSession(ctx, job, session, 1)
	if err != nil {
		test.Fatal(err)
	}
	header := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
	legacyByteHeaderUsagePGDeclare(test, ctx, header, fixture)
	legacyByteHeaderUsagePGAppend(test, ctx, header, fixture, 0)
	legacyByteHeaderUsagePGAppend(test, ctx, header, fixture, 1)
	if err := header.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	undeclared := legacyByteStagePGSource(test, storage, ctx, 1024)
	undeclaredJob := legacyByteStagePGClaim(test, storage, ctx, undeclared)
	undeclaredSession, err := storage.prepareLegacyByteBackfill(ctx, undeclaredJob)
	if err != nil {
		test.Fatal(err)
	}
	undeclaredStage, err := storage.stageLegacyByteSession(ctx, undeclaredJob, undeclaredSession, 1)
	if err != nil {
		test.Fatal(err)
	}
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	undeclaredPrivate := legacyByteStagePGImage(test, storage, ctx, undeclared.hash, -1)
	const preservedSQL = `SELECT jsonb_build_object(
 'counters',(SELECT jsonb_agg(jsonb_build_array(doc_hash,object_bytes,object_count,part_count) ORDER BY doc_hash) FROM doc_legacy_byte_usage_v1),
 'headers',(SELECT jsonb_agg(jsonb_build_array(doc_hash,ordinal,hash,md5(bytes),octet_length(bytes),xmin::text) ORDER BY doc_hash,ordinal)
 FROM doc_legacy_byte_stage_headers_v1),
 'authority',(SELECT jsonb_build_array(usage.relowner,usage.relacl,accounting.proowner,guard.proowner,guard.proacl,pg_get_functiondef(guard.oid))
 FROM pg_class usage CROSS JOIN pg_proc accounting CROSS JOIN pg_proc guard
 WHERE usage.oid='public.doc_legacy_byte_usage_v1'::regclass AND accounting.oid='public.cxt_account_legacy_byte_storage()'::regprocedure
 AND guard.oid='public.cxt_guard_legacy_byte_usage()'::regprocedure))::text`
	var preserved string
	if err := storage.pool.QueryRow(ctx, preservedSQL).Scan(&preserved); err != nil {
		test.Fatal(err)
	}
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 1 {
		test.Fatal("header accounting did not upgrade exactly its populated predecessor", applied, err)
	}
	var after string
	if err := storage.pool.QueryRow(ctx, preservedSQL).Scan(&after); err != nil || after != preserved {
		test.Fatal("header backfill changed stream counters, header payloads or trusted authority", preserved, after, err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("header accounting backfill rewrote a resumable stage")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("header accounting backfill changed original or published state")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, undeclared.hash, -1); after != undeclaredPrivate {
		test.Fatal("header accounting initialized a legacy stage header")
	}
	expected := legacyByteUsagePGExpected(fixture, stage, len(session.encoded))
	other := legacyByteUsagePGExpected(undeclared, undeclaredStage, len(undeclaredSession.encoded))
	expected.Sources += other.Sources
	expected.CompleteSources += other.CompleteSources
	expected.Objects += other.Objects
	expected.Parts += other.Parts
	expected.PayloadBytes += other.PayloadBytes
	expected.SpecBytes += other.SpecBytes
	expected.HeaderBytes, expected.HeaderParts = 2*65536, 2
	var historicalBytes, historicalParts int64
	if err := storage.pool.QueryRow(ctx, `SELECT sum(header_bytes)::bigint,sum(header_parts)::bigint FROM doc_legacy_byte_usage_v1`).Scan(&historicalBytes, &historicalParts); err != nil || historicalBytes != expected.HeaderBytes || historicalParts != expected.HeaderParts {
		test.Fatal("historical header migration did not backfill exactly its stored payload", historicalBytes, historicalParts, err)
	}
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 0 {
		test.Fatal("header accounting migration is not replay-safe", applied, err)
	}
	if _, err := storage.ApplyMigrations(ctx, directory); err != nil {
		test.Fatal("populated header accounting could not upgrade to the current reader schema", err)
	}
	afterUpgrade := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	var historicalStage string
	if err := storage.pool.QueryRow(ctx, `SELECT ($1::jsonb #- '{header,0,checkpoint_sequence}'
 #- '{header,0,checkpoint_hash}' #- '{header,0,checkpoint_bytes}'
 #- '{header,0,checkpoint_gc_sequence}' #- '{header,0,checkpoint_gc_last_sequence}' #- '{header,0,checkpoint_gc_last_hash}'
 #- '{header,0,checkpoint_gc_changed}' #- '{header,0,checkpoint_gc_complete}')::text`, afterUpgrade).Scan(&historicalStage); err != nil || historicalStage != private {
		test.Fatal("current-schema upgrade changed the historical canonical stage", err)
	}
	var checkpointsEmpty bool
	if err := storage.pool.QueryRow(ctx, `SELECT NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1
 WHERE checkpoint_sequence<>0 OR checkpoint_hash IS NOT NULL OR checkpoint_bytes<>0 OR checkpoint_gc_sequence<>0 OR checkpoint_gc_last_sequence<>-1
 OR checkpoint_gc_last_hash<>'' OR checkpoint_gc_changed OR checkpoint_gc_complete)
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_checkpoint_objects_v1)`).Scan(&checkpointsEmpty); err != nil || !checkpointsEmpty {
		test.Fatal("current-schema upgrade fabricated normalization checkpoints", checkpointsEmpty, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("current-schema upgrade changed archival or published state")
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, domain.LegacyDerivedByteStorageUsage{}, expected)
	var zeroBytes, zeroParts int64
	if err := storage.pool.QueryRow(ctx, `SELECT header_bytes,header_parts FROM doc_legacy_byte_usage_v1 WHERE doc_hash=$1`, undeclared.hash).
		Scan(&zeroBytes, &zeroParts); err != nil || zeroBytes != 0 || zeroParts != 0 {
		test.Fatal("undeclared header acquired nonzero usage", zeroBytes, zeroParts, err)
	}
	var bootstrapRemains bool
	if err := storage.pool.QueryRow(ctx, `SELECT to_regprocedure('public.cxt_backfill_legacy_byte_header_usage()') IS NOT NULL
 OR EXISTS(SELECT 1 FROM pg_trigger WHERE tgrelid='public.doc_legacy_byte_stage_headers_v1'::regclass
 AND tgname='backfill_legacy_byte_header_usage')`).Scan(&bootstrapRemains); err != nil || bootstrapRemains {
		test.Fatal("migration left a bootstrap accounting entry point", bootstrapRemains, err)
	}
	for ordinal := 2; ordinal < (len(fixture.envelope)+65535)/65536; ordinal++ {
		transaction := legacyByteHeaderUsagePGTransaction(test, storage, ctx)
		legacyByteHeaderUsagePGAppend(test, ctx, transaction, fixture, ordinal)
		if err := transaction.Commit(ctx); err != nil {
			test.Fatal(err)
		}
	}
	expected.HeaderBytes = int64(len(fixture.envelope))
	expected.HeaderParts = int64((len(fixture.envelope) + 65535) / 65536)
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, domain.LegacyDerivedByteStorageUsage{}, expected)
	for _, statement := range []string{
		`DELETE FROM snapshots WHERE doc_hash=$1`,
		`DELETE FROM repo_blobs WHERE hash=$1`,
		`DELETE FROM blobs WHERE hash=$1`,
	} {
		if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
			test.Fatal(err)
		}
	}
	legacyByteHeaderUsagePGAssertDelta(test, storage, ctx, domain.LegacyDerivedByteStorageUsage{}, other)
}
