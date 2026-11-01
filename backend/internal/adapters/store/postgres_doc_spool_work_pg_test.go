//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docSpoolWorkPGTrace struct {
	sourceReads     atomic.Int64
	privateWrites   atomic.Int64
	authorityChecks atomic.Int64
	writeMu         sync.Mutex
	writeQueries    map[string]int
	writeDocument   domain.ContentHash
	unboundWrites   int
}

type docSpoolWorkPGRead struct {
	offset int
	length int
}

type docSpoolWorkPGSource struct {
	spool *legacyDocSpool
	reads []docSpoolWorkPGRead
}

func (source *docSpoolWorkPGSource) Append(ctx context.Context, body []byte) error {
	return source.spool.Append(ctx, body)
}

func (source *docSpoolWorkPGSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	source.reads = append(source.reads, docSpoolWorkPGRead{offset: offset, length: length})
	return source.spool.Read(ctx, offset, length)
}

func (trace *docSpoolWorkPGTrace) TraceQueryStart(ctx context.Context, connection *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(strings.ReplaceAll(query.SQL, "octet_length(source.bytes)", ""), "source.bytes") {
		trace.sourceReads.Add(1)
	}
	statement := strings.ToUpper(strings.Join(strings.Fields(query.SQL), " "))
	if strings.HasPrefix(statement, "WITH RETAINED_SOURCE AS MATERIALIZED") &&
		strings.Contains(statement, "RETAINED_JOB AS MATERIALIZED") && !strings.Contains(statement, "RETAINED_STAGE AS MATERIALIZED") {
		trace.authorityChecks.Add(1)
	}
	mutation := strings.HasPrefix(statement, "INSERT ") || strings.HasPrefix(statement, "UPDATE ") ||
		strings.HasPrefix(statement, "DELETE ") || strings.HasPrefix(statement, "COPY ")
	if mutation && strings.Contains(statement, "DOC_LEGACY_BYTE_STAGE") {
		trace.privateWrites.Add(1)
	}
	if strings.HasPrefix(statement, "WITH ") {
		unlocked := strings.ReplaceAll(statement, "FOR UPDATE", "FOR SHARE")
		mutation = strings.Contains(unlocked, " INSERT ") || strings.Contains(unlocked, " UPDATE ") ||
			strings.Contains(unlocked, " DELETE ") || strings.Contains(unlocked, " COPY ")
	}
	if mutation {
		trace.writeMu.Lock()
		if trace.writeQueries != nil {
			trace.writeQueries[statement]++
			if connection.PgConn().TxStatus() != 'T' || len(query.Args) == 0 || fmt.Sprint(query.Args[0]) != string(trace.writeDocument) {
				trace.unboundWrites++
			}
		}
		trace.writeMu.Unlock()
	}
	return ctx
}

func (*docSpoolWorkPGTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func docSpoolWorkPGCheckpointWrites(test *testing.T, trace *docSpoolWorkPGTrace) map[string]int {
	test.Helper()
	allowed := map[string]string{
		`INSERT INTO doc_legacy_byte_stages_v1
 (doc_hash,source_id,source_version,plan_hash,spec,blocks,next_block,complete)
 VALUES($1,$2::uuid,$3,NULL,NULL,NULL,0,false) ON CONFLICT(doc_hash) DO NOTHING RETURNING stage_id::text`: "quarantine",
		`INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1(doc_hash,stage_id,hash,bytes)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO NOTHING`: "checkpoint-object",
		`INSERT INTO doc_legacy_checkpoint_structures_v1(doc_hash,stage_id,hash,kind)
 VALUES($1,$2::uuid,$3,$4) ON CONFLICT(doc_hash,stage_id,hash) DO NOTHING`: "checkpoint-structure",
		`UPDATE doc_legacy_byte_stages_v1 SET checkpoint_sequence=checkpoint_sequence+1,
 checkpoint_hash=$2,checkpoint_bytes=$3 WHERE doc_hash=$1 AND stage_id=$4::uuid AND checkpoint_sequence=$5
 AND checkpoint_hash IS NOT DISTINCT FROM NULLIF($6,'') RETURNING checkpoint_sequence`: "checkpoint-cas",
		`WITH verified AS MATERIALIZED (
 SELECT octet_length(bytes) AS stored_bytes,CASE WHEN octet_length(bytes)=$4
 THEN 'sha256:'||encode(sha256(bytes),'hex') ELSE '' END AS stored_hash
 FROM doc_legacy_byte_stage_checkpoint_objects_v1 WHERE doc_hash=$1 AND stage_id=$2::uuid AND hash=$3
 ), touched AS (
 INSERT INTO doc_legacy_checkpoint_staging_v1(doc_hash,stage_id,hash,last_sequence)
 SELECT $1,$2::uuid,$3,$5 FROM verified WHERE stored_bytes=$4 AND stored_hash=$3
 ON CONFLICT(doc_hash,stage_id,hash) DO UPDATE SET last_sequence=GREATEST(doc_legacy_checkpoint_staging_v1.last_sequence,EXCLUDED.last_sequence)
 RETURNING 1
 ) SELECT stored_bytes,stored_hash,EXISTS(SELECT 1 FROM touched) FROM verified`: "checkpoint-touch",
	}
	classes := make(map[string]string, len(allowed))
	for statement, class := range allowed {
		classes[strings.ToUpper(strings.Join(strings.Fields(statement), " "))] = class
	}
	trace.writeMu.Lock()
	defer trace.writeMu.Unlock()
	if trace.unboundWrites != 0 {
		test.Fatal("provisional checkpoint write escaped its document or transaction", trace.unboundWrites)
	}
	counts := make(map[string]int)
	for statement, count := range trace.writeQueries {
		class, permitted := classes[statement]
		if !permitted {
			test.Fatal("unverified source attempted a non-checkpoint mutation", count, statement)
		}
		counts[class] += count
	}
	return counts
}

func docSpoolWorkPGQuarantineOnly(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) {
	test.Helper()
	var provisional bool
	err := storage.pool.QueryRow(ctx, `SELECT
 NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1 stage
 WHERE stage.doc_hash=$1 AND (stage.plan_hash IS NOT NULL OR stage.spec IS NOT NULL OR stage.blocks IS NOT NULL
 OR stage.next_block<>0 OR stage.complete OR stage.header_version<>0 OR stage.header_bytes IS NOT NULL
 OR stage.header_next<>0 OR stage.header_complete
 OR NOT EXISTS(SELECT 1 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash
 WHERE source.hash=stage.doc_hash AND source.doc_range_source_version=stage.source_version AND incarnation.incarnation=stage.source_id)))
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)
 AND NOT EXISTS(SELECT 1 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1)`, fixture.hash).Scan(&provisional)
	if err != nil || !provisional {
		test.Fatal("unverified source acquired a verified plan, byte/header data, or a stale source binding", provisional, err)
	}
}

func docSpoolWorkPGSourceQuanta(storedBytes int) int64 {
	return int64((storedBytes + (512 << 10) - 1) / (512 << 10))
}

func docSpoolWorkPGStore(test *testing.T, storage *PostgresStore, ctx context.Context) (*PostgresStore, *docSpoolWorkPGTrace) {
	test.Helper()
	trace := &docSpoolWorkPGTrace{}
	config := storage.pool.Config()
	config.ConnConfig.Tracer = trace
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	peer := &PostgresStore{pool: pool}
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	return peer, trace
}

func docRangeDirectSpoolWorkPGPrepare(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) *preparedLegacyByteWork {
	test.Helper()
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal("prepare direct eager compatibility session", err)
	}
	work, err := storage.newLegacyByteWork(ctx, job, session)
	if err != nil || work == nil {
		if work != nil {
			defer work.Close()
		}
		test.Fatal("prepare direct compatibility local spool work", err)
	}
	test.Cleanup(func() {
		if err := work.Close(); err != nil {
			test.Error("close direct compatibility local spool work", err)
		}
	})
	if work.spooling == nil || work.preparation != nil || work.metadata.Valid() || work.session.proof.Valid() ||
		work.header.Reference() != (domain.DocumentRef{Hash: job.DocHash, Identity: domain.DocumentIdentityLegacy}) ||
		work.header.PlanKey() != work.session.stage.plan || work.spooling.Reference() != work.header.Reference() ||
		work.spooling.PlanKey() != work.header.PlanKey() || work.spooling.SourceBytes() != 0 || work.data.size != 0 {
		defer work.Close()
		test.Fatal("direct compatibility constructor spooled local source bytes, retained a duplicate proof, or prepared metadata")
	}
	spec, err := work.session.plan.Spec()
	if err != nil {
		defer work.Close()
		test.Fatal(err)
	}
	resources := docRangeWorkPGResources(test, work, int(spec.StreamBytes))
	if resources.SpoolBytes != 0 || len(work.objects.offsets) != 1 || work.objects.offsets[0] != 0 {
		defer work.Close()
		test.Fatal("direct compatibility constructor did not leave two empty private spools", resources, work.objects.offsets)
	}
	docSpoolWorkPGAssertPending(test, work, ctx, job)
	return work
}

func docSpoolWorkPGAssertPending(test *testing.T, work *preparedLegacyByteWork, ctx context.Context, job domain.DocRangeIndexJob) {
	test.Helper()
	if work.spooling == nil || work.preparation != nil || work.metadata.Valid() {
		test.Fatal("local spool verification exposed metadata preparation")
	}
	if plan, err := work.spooling.Plan(); !errors.Is(err, domain.ErrDocRangeIndexPending) || plan.Valid() {
		test.Fatal("unverified local spool exposed a sealed source plan", plan.Valid(), err)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("unverified local spool accepted publication", err)
	}
}

func docSpoolWorkPGStep(test *testing.T, work *preparedLegacyByteWork, ctx context.Context, job domain.DocRangeIndexJob, streamBytes, step int) outbound.RootPublicationResources {
	test.Helper()
	blocks := (streamBytes + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
	quanta := max(2*blocks, 1)
	docSpoolWorkPGAssertPending(test, work, ctx, job)
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("local spool quantum completed public work or failed", step, quanta, complete, err)
	}
	data, err := work.data.file.Stat()
	if err != nil {
		test.Fatal(err)
	}
	expectedBytes := int64(min(min(step, blocks)*domain.ConversationManifestChunkBytes, streamBytes))
	if data.Size() != expectedBytes || int64(work.data.size) != expectedBytes || work.metadata.Valid() ||
		len(work.objects.offsets) != 1 || work.objects.offsets[0] != 0 || work.objects.writer.Buffered() != 0 {
		test.Fatal("local spool quantum wrote beyond one block or emitted metadata", step, expectedBytes, data.Size())
	}
	if step < quanta {
		docSpoolWorkPGAssertPending(test, work, ctx, job)
		if work.spooling.SourceBytes() != expectedBytes {
			test.Fatal("spooling did not account for its partial source extent", step, work.spooling.SourceBytes(), expectedBytes)
		}
	} else {
		if work.spooling != nil || work.preparation == nil || work.recoveryReady ||
			work.header.Reference() != work.preparation.Reference() || work.header.PlanKey() != work.preparation.PlanKey() ||
			work.preparation.ObjectCount() != 0 || work.session.proof.Valid() {
			test.Fatal("last local spool quantum did not release raw proof state and create unstepped metadata")
		}
		docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
	}
	resources := docRangeWorkPGResources(test, work, streamBytes)
	if resources.SpoolBytes != expectedBytes {
		test.Fatal("local spool resources did not match the actual partial data file", step, resources, expectedBytes)
	}
	return resources
}

func TestDocSpoolWorkPGDirectCompatibilityPreparationAndTwoPassQuantaStayPrivate(test *testing.T) {
	for _, fixtureCase := range []struct {
		name    string
		payload int
	}{
		{name: "empty", payload: -1},
		{name: "single-block", payload: 1024},
		{name: "multiple-blocks", payload: 5 << 20},
	} {
		test.Run(fixtureCase.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			directory := rangeSpoolTempDir(test)
			var fixture legacyByteStagePGFixture
			if fixtureCase.payload < 0 {
				fixture, _ = docRangeWorkEmissionPGSource(test, storage, ctx, 0)
			} else {
				fixture = legacyByteStagePGSource(test, storage, ctx, fixtureCase.payload)
			}
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			peer, trace := docSpoolWorkPGStore(test, storage, ctx)
			work := docRangeDirectSpoolWorkPGPrepare(test, peer, ctx, job)
			defer docRangeWorkPGClose(test, work)
			stage := work.session.stage
			spec, err := work.session.plan.Spec()
			if err != nil {
				test.Fatal(err)
			}
			initialPlanBytes, err := work.session.plan.RetainedBytes()
			if err != nil || (len(fixture.stream) > 0 && initialPlanBytes < int64(len(fixture.raw))) {
				test.Fatal("raw plan hid the retained complete canonical proof", initialPlanBytes, len(fixture.raw), err)
			}
			headerBytes, err := work.header.RetainedBytes()
			if err != nil || headerBytes > int64(len(fixture.envelope))+4096 {
				test.Fatal("metadata seed retained more than its bound envelope", headerBytes, len(fixture.envelope), err)
			}
			if files, err := os.ReadDir(directory); err != nil || len(files) != 2 {
				test.Fatal("direct compatibility constructor did not create exactly two empty local-spool files", files, err)
			}
			for step := 1; step <= max(2*spec.Blocks, 1); step++ {
				docSpoolWorkPGStep(test, work, ctx, job, len(fixture.stream), step)
				if step <= spec.Blocks {
					want, _ := legacyByteStagePGBlock(fixture, step-1)
					body := make([]byte, len(want))
					if _, err := work.data.file.ReadAt(body, int64(step-1)*domain.ConversationManifestChunkBytes); err != nil || !bytes.Equal(body, want) {
						test.Fatal("write quantum did not append its exact next canonical source block", step, err)
					}
				}
				if work.spooling != nil {
					if retained, err := work.session.plan.RetainedBytes(); err != nil || retained != initialPlanBytes {
						test.Fatal("raw plan was released before the final readback and extent checks", step, initialPlanBytes, retained, err)
					}
				}
				if work.session.stage != stage || trace.sourceReads.Load() != 1 || trace.privateWrites.Load() != 0 {
					test.Fatal("local spool quantum reread the original or attempted private byte copying", step, work.session.stage, stage, trace.sourceReads.Load(), trace.privateWrites.Load())
				}
				if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
					test.Fatal("local spool quantum changed a private header, CAS object, part, or checkpoint", step)
				}
				if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
					test.Fatal("local spool quantum changed source, incarnation, ownership, snapshot, job, or public ranges", step)
				}
				if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
					test.Fatal("local spool quantum advertised public readiness", step, ready, err)
				}
			}
			if after, err := work.session.plan.Spec(); err != nil || after != spec {
				test.Fatal("spool transition changed the sealed source plan", spec, after, err)
			}
			if key, err := work.session.plan.Key(); err != nil || key != stage.plan {
				test.Fatal("spool transition changed the source plan key", key, stage.plan, err)
			}
			if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
				test.Fatal("first post-spool step did not leave metadata finalization pending", complete, err)
			}
			stage.next, stage.complete = min(spec.Blocks, domain.DocLegacyByteBackfillBatchBlocks), spec.Blocks <= domain.DocLegacyByteBackfillBatchBlocks
			stage.incarnation = work.session.stage.incarnation
			legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
			if trace.privateWrites.Load() == 0 || trace.sourceReads.Load() != 1 {
				test.Fatal("private copying did not begin exclusively after local verification", trace.privateWrites.Load(), trace.sourceReads.Load())
			}
			if !work.recoveryReady || !reflect.ValueOf(work.header).IsZero() {
				test.Fatal("persisted recovery header retained its metadata seed")
			}
			docRangeWorkPGAssertMetadataPending(test, work, ctx, job)
			docRangeWorkPGAssertClosed(test, work, ctx, job)
			if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
				test.Fatal("closing phased work left local spool files", files, err)
			}
		})
	}
}

func TestDocSpoolWorkPGDirectCompatibilityNearDecodeCapReportsRawHeapUntilVerification(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, maxLegacyReadBytes-(16<<10))
	if len(fixture.raw) > maxLegacyReadBytes || len(fixture.raw) < maxLegacyReadBytes-(16<<10) {
		test.Fatal("fixture did not retain the unchanged decoded-source cap", len(fixture.raw), maxLegacyReadBytes)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docSpoolWorkPGStore(test, storage, ctx)
	work := docRangeDirectSpoolWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, work)
	initial := docRangeWorkPGResources(test, work, len(fixture.stream))
	if initial.SpoolBytes != 0 || initial.HeapBytes <= 16<<20 {
		test.Fatal("unspooled near-cap proof was incorrectly accounted as parkable", initial)
	}
	for step := 1; step <= 2*work.session.stage.blocks; step++ {
		resources := docSpoolWorkPGStep(test, work, ctx, job, len(fixture.stream), step)
		if work.spooling != nil && resources.HeapBytes <= 16<<20 {
			test.Fatal("raw near-cap source was omitted before the last readback", step, resources)
		}
		if trace.privateWrites.Load() != 0 || trace.sourceReads.Load() != 1 {
			test.Fatal("near-cap spooling performed private copying or reread the original", step, trace.privateWrites.Load(), trace.sourceReads.Load())
		}
	}
	resources := docRangeWorkPGResources(test, work, len(fixture.stream))
	if resources.HeapBytes >= 1<<20 || resources.SpoolBytes != int64(len(fixture.stream)) || work.preparation == nil {
		test.Fatal("final readback did not release the entire raw proof before metadata scanning", initial, resources)
	}
}

func TestDocSpoolWorkPGDirectCompatibilityReadbackUsesOneBlockAndFinalExtentChecks(test *testing.T) {
	for _, empty := range []bool{false, true} {
		test.Run(fmt.Sprintf("empty=%t", empty), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			rangeSpoolTempDir(test)
			storage.ConfigureLegacyDerivedByteRangePublication(true)
			var fixture legacyByteStagePGFixture
			if empty {
				fixture, _ = docRangeWorkEmissionPGSource(test, storage, ctx, 0)
			} else {
				fixture = legacyByteStagePGSource(test, storage, ctx, domain.ConversationManifestChunkBytes)
			}
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			work := docRangeDirectSpoolWorkPGPrepare(test, storage, ctx, job)
			defer docRangeWorkPGClose(test, work)
			source := &docSpoolWorkPGSource{spool: work.data}
			var err error
			work.spooling, err = domain.NewLegacyByteSpoolPreparation(ctx, work.session.plan, source)
			if err != nil || len(source.reads) != 0 {
				test.Fatal("observing an empty local spool performed source reads", source.reads, err)
			}
			blocks := work.session.stage.blocks
			quanta := max(2*blocks, 1)
			for step := 1; step <= quanta; step++ {
				before := len(source.reads)
				docSpoolWorkPGStep(test, work, ctx, job, len(fixture.stream), step)
				var expected []docSpoolWorkPGRead
				if step > blocks && blocks > 0 {
					offset := (step - blocks - 1) * domain.ConversationManifestChunkBytes
					expected = append(expected, docSpoolWorkPGRead{offset: offset, length: min(domain.ConversationManifestChunkBytes, len(fixture.stream)-offset)})
				}
				if step == quanta {
					expected = append(expected, docSpoolWorkPGRead{offset: len(fixture.stream)}, docSpoolWorkPGRead{offset: len(fixture.stream), length: 1})
				}
				actual := source.reads[before:]
				if len(actual) != len(expected) || (len(expected) > 0 && !reflect.DeepEqual(actual, expected)) {
					test.Fatal("spool quantum read beyond its next block or checked EOF outside the final quantum", step, actual, expected)
				}
			}
		})
	}
}

func TestDocSpoolWorkPGDirectCompatibilityEverySpoolQuantumRechecksJobLeaseAndSource(test *testing.T) {
	for targetStep := 1; targetStep <= 4; targetStep++ {
		for _, change := range []string{"argument-cas", "stored-cas", "lease", "source-version", "source-incarnation", "ownership", "snapshot", "read-only", "canceled"} {
			test.Run(fmt.Sprintf("step-%d/%s", targetStep, change), func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				rangeSpoolTempDir(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, domain.ConversationManifestChunkBytes)
				job := legacyByteStagePGClaim(test, storage, ctx, fixture)
				peer, trace := docSpoolWorkPGStore(test, storage, ctx)
				work := docRangeDirectSpoolWorkPGPrepare(test, peer, ctx, job)
				defer docRangeWorkPGClose(test, work)
				if work.session.stage.blocks != 2 {
					test.Fatal("fence fixture must have two write and two readback quanta", work.session.stage.blocks)
				}
				for step := 1; step < targetStep; step++ {
					docSpoolWorkPGStep(test, work, ctx, job, len(fixture.stream), step)
				}
				candidate, stepContext := job, ctx
				want := domain.ErrConflict
				switch change {
				case "argument-cas":
					candidate.Version++
				case "stored-cas":
					_, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET version=version+1,state='waiting',lease_until=NULL WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
					if err != nil {
						test.Fatal(err)
					}
				case "lease":
					_, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute' WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''`, fixture.repo, fixture.hash)
					if err != nil {
						test.Fatal(err)
					}
				case "source-version":
					if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=bytes WHERE hash=$1`, fixture.hash); err != nil {
						test.Fatal(err)
					}
				case "source-incarnation":
					for _, statement := range []string{`DELETE FROM snapshots WHERE doc_hash=$1`, `DELETE FROM repo_blobs WHERE hash=$1`, `DELETE FROM blobs WHERE hash=$1`} {
						if _, err := storage.pool.Exec(ctx, statement, fixture.hash); err != nil {
							test.Fatal(err)
						}
					}
					if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.hash, fixture.raw); err != nil {
						test.Fatal(err)
					}
					if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, fixture.repo, fixture.hash); err != nil {
						test.Fatal(err)
					}
					if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: fixture.repo, DocHash: fixture.hash,
						DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
						test.Fatal(err)
					}
					var sourceID string
					var version int64
					if err := storage.pool.QueryRow(ctx, `SELECT incarnation.incarnation::text,source.doc_range_source_version
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, fixture.hash).Scan(&sourceID, &version); err != nil ||
						sourceID == work.session.stage.sourceID || version != work.session.stage.sourceVersion {
						test.Fatal("reinserted source did not isolate incarnation from the unchanged hash and reset version", sourceID, version, err)
					}
				case "ownership":
					if _, err := storage.pool.Exec(ctx, `DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='doc' AND hash=$2`, fixture.repo, fixture.hash); err != nil {
						test.Fatal(err)
					}
					want = domain.ErrNotFound
				case "snapshot":
					if _, err := storage.pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.hash); err != nil {
						test.Fatal(err)
					}
					want = domain.ErrNotFound
				case "read-only":
					stepContext = outbound.WithDocReadOnly(ctx)
				case "canceled":
					canceled, cancel := context.WithCancel(ctx)
					cancel()
					stepContext, want = canceled, context.Canceled
				}
				private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				resources := docRangeWorkPGResources(test, work, len(fixture.stream))
				spooling, header, stage := work.spooling, work.header, work.session.stage
				if complete, err := work.Step(stepContext, candidate); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, want) {
					test.Fatal("invalid job, lease, or source advanced the local spool quantum", targetStep, change, complete, err, want)
				}
				if after := docRangeWorkPGResources(test, work, len(fixture.stream)); after != resources || work.spooling != spooling ||
					!reflect.DeepEqual(work.header, header) || work.session.stage != stage {
					test.Fatal("rejected spool quantum advanced local source or metadata state", targetStep, resources, after)
				}
				if trace.privateWrites.Load() != 0 || trace.sourceReads.Load() != 1 ||
					legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
					test.Fatal("rejected spool quantum copied bytes, reverified the original, or changed persisted state", targetStep, change)
				}
				docSpoolWorkPGAssertPending(test, work, ctx, job)
			})
		}
	}
}

func TestDocSpoolWorkPGDirectCompatibilityReadbackRejectsOnlyItsNextCorruptBlock(test *testing.T) {
	for _, corruptOrdinal := range []int{0, 1, 2} {
		test.Run(fmt.Sprintf("block-%d", corruptOrdinal), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			rangeSpoolTempDir(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, trace := docSpoolWorkPGStore(test, storage, ctx)
			work := docRangeDirectSpoolWorkPGPrepare(test, peer, ctx, job)
			defer docRangeWorkPGClose(test, work)
			blocks := work.session.stage.blocks
			if blocks != 3 {
				test.Fatal("readback corruption fixture must cross three blocks", blocks)
			}
			for step := 1; step <= blocks; step++ {
				docSpoolWorkPGStep(test, work, ctx, job, len(fixture.stream), step)
			}
			docRangeWorkPGCorruptByte(test, work.data.file, int64(corruptOrdinal)*domain.ConversationManifestChunkBytes)
			for ordinal := 0; ordinal < corruptOrdinal; ordinal++ {
				docSpoolWorkPGStep(test, work, ctx, job, len(fixture.stream), blocks+ordinal+1)
			}
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			if complete, err := work.Step(ctx, job); complete == outbound.DocRangeWorkPublishable || !errors.Is(err, domain.ErrIntegrity) || !errors.Is(err, domain.ErrPreparedDocRangeObject) {
				test.Fatal("readback quantum accepted its next corrupt local source block", corruptOrdinal, complete, err)
			}
			if work.spooling == nil || work.preparation != nil || work.metadata.Valid() || work.session.proof.Valid() ||
				trace.privateWrites.Load() != 0 || trace.sourceReads.Load() != 1 ||
				legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("failed readback released its proof early, created metadata, or changed persisted state", corruptOrdinal)
			}
			docRangeWorkPGAssertClosed(test, work, ctx, job)
		})
	}
}

func TestDocSpoolWorkPGPublicPrepareStillRejectsUnverifiedSource(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1024)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	damaged := bytes.Replace(fixture.raw, []byte(`"seq":2`), []byte(`"seq":3`), 1)
	if bytes.Equal(damaged, fixture.raw) || domain.HashContent(damaged) == fixture.hash {
		test.Fatal("source corruption did not change the canonical document identity")
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, damaged); err != nil {
		test.Fatal(err)
	}
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	peer, trace := docSpoolWorkPGStore(test, storage, ctx)
	trace.writeQueries = make(map[string]int)
	trace.writeDocument = fixture.hash
	prepared, err := peer.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		test.Fatal("public preparation did not defer full-source verification", prepared, err)
	}
	test.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			test.Error(err)
		}
	})
	capture, ok := prepared.(*preparedLegacyCaptureWork)
	if !ok || capture.work != nil || trace.sourceReads.Load() != 0 {
		test.Fatal("public preparation consumed the unverified source", prepared, trace.sourceReads.Load())
	}
	var failure error
	for step := 0; step < docCaptureWorkPGPreparationStepLimit; step++ {
		progress, stepErr := capture.Step(ctx, job)
		if stepErr == nil && progress != outbound.DocRangeWorkPending || capture.work != nil || capture.canonicalProof.Valid() ||
			capture.original || trace.sourceReads.Load() > docSpoolWorkPGSourceQuanta(len(damaged)) ||
			reflect.ValueOf(&peer.docProofs.rootVerifier).Elem().FieldByName("events").Len() != 0 {
			test.Fatal("unverified source exposed proof authority, fallback, or excess source reads", step, progress, stepErr)
		}
		docSpoolWorkPGCheckpointWrites(test, trace)
		docSpoolWorkPGQuarantineOnly(test, storage, ctx, fixture)
		if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
			test.Fatal("provisional normalization changed public data or authority", step)
		}
		if err := capture.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("unverified checkpoint accepted publication", step, err)
		}
		if stepErr != nil {
			failure = stepErr
			break
		}
	}
	test.Log("provisional mutation classes", docSpoolWorkPGCheckpointWrites(test, trace), "legacy stage writes", trace.privateWrites.Load())
	if !errors.Is(failure, domain.ErrIntegrity) || capture.work != nil ||
		trace.sourceReads.Load() != docSpoolWorkPGSourceQuanta(len(damaged)) {
		test.Fatal("capture skipped original full-source verification", capture.work, failure, trace.sourceReads.Load(), trace.privateWrites.Load())
	}
	if err := prepared.Close(); err != nil {
		test.Fatal(err)
	}
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("failed original verification retained local spool files", files, err)
	}
	docSpoolWorkPGQuarantineOnly(test, storage, ctx, fixture)
	if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("failed original verification changed public state")
	}
}
