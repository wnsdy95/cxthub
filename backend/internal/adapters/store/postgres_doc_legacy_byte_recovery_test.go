//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

const legacyByteRecoveryPGStepLimit = 4096

type legacyByteRecoveryPGVerificationContext struct {
	context.Context
	preparation atomic.Bool
	step        atomic.Bool
	event       atomic.Bool
}

func (ctx *legacyByteRecoveryPGVerificationContext) Err() error {
	if !ctx.preparation.Load() || !ctx.step.Load() || !ctx.event.Load() {
		var callers [24]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		for {
			frame, more := frames.Next()
			switch {
			case strings.HasSuffix(frame.Function, "domain.NewLegacyByteSourcePreparation"):
				ctx.preparation.Store(true)
			case strings.HasSuffix(frame.Function, "domain.(*LegacyByteSourcePreparation).Step"):
				ctx.step.Store(true)
			case strings.HasSuffix(frame.Function, "domain.verifyCanonicalEvent"):
				ctx.event.Store(true)
			}
			if !more {
				break
			}
		}
	}
	return ctx.Context.Err()
}

func legacyByteRecoveryPGReady(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) bool {
	test.Helper()
	var ready bool
	if err := storage.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM doc_legacy_byte_stages_v1
 WHERE doc_hash=$1 AND complete AND next_block=blocks AND header_version=1 AND header_complete
 AND header_next=(header_bytes+65535)/65536)`, hash).Scan(&ready); err != nil {
		test.Fatal(err)
	}
	return ready
}

func legacyByteRecoveryPGImage(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash) string {
	test.Helper()
	var headers string
	if err := storage.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(
 jsonb_build_array(ordinal,hash,encode(bytes,'hex'),xmin::text) ORDER BY ordinal),'[]'::jsonb)::text
 FROM doc_legacy_byte_stage_headers_v1 WHERE doc_hash=$1`, hash).Scan(&headers); err != nil {
		test.Fatal(err)
	}
	return legacyByteStagePGImage(test, storage, ctx, hash, -1) + "\n" + headers
}

func legacyByteRecoveryPGPublicationImage(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'source',(SELECT jsonb_build_array(hash,doc_range_source_version,xmin::text) FROM blobs WHERE hash=$2),
 'incarnation',(SELECT jsonb_build_array(incarnation::text,xmin::text) FROM blob_source_incarnations WHERE hash=$2),
 'ownership',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(owned),owned.xmin::text) ORDER BY repo_id,kind,hash)
 FROM repo_blobs owned WHERE repo_id=$1 OR hash=$2),
 'snapshot',(SELECT jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) FROM snapshots snapshot WHERE repo_id=$1 AND id=$2),
 'job',(SELECT jsonb_build_array(to_jsonb(job),job.xmin::text) FROM doc_range_jobs_v1 job WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''),
 'generation',(SELECT jsonb_build_array(to_jsonb(generation),generation.xmin::text) FROM doc_range_generations_v2 generation WHERE doc_hash=$2),
 'objects',(SELECT jsonb_agg(jsonb_build_array(hash,encode(bytes,'hex'),xmin::text) ORDER BY hash) FROM doc_range_objects_v2 WHERE doc_hash=$2)
 )::text`, fixture.repo, fixture.hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func legacyByteRecoveryPGAssertCheckpoint(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) {
	test.Helper()
	var stage legacyByteStage
	var version, size, next int
	var complete bool
	if err := storage.pool.QueryRow(ctx, `SELECT source_id::text,source_version,plan_hash,next_block,blocks,complete,
	 header_version,header_bytes,header_next,header_complete,stage_id::text FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&stage.sourceID, &stage.sourceVersion, &stage.plan, &stage.next, &stage.blocks, &stage.complete,
			&version, &size, &next, &complete, &stage.incarnation); err != nil {
		test.Fatal(err)
	}
	parts := (len(fixture.envelope) + 65535) / 65536
	if !stage.complete || stage.next != stage.blocks || version != 1 || size != len(fixture.envelope) || next != parts || !complete {
		test.Fatal("checkpoint did not complete both exact canonical extents", stage, version, size, next, complete)
	}
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	rows, err := storage.pool.Query(ctx, `SELECT ordinal,hash,bytes FROM doc_legacy_byte_stage_headers_v1
 WHERE doc_hash=$1 ORDER BY ordinal`, fixture.hash)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var envelope []byte
	count := 0
	for rows.Next() {
		var ordinal int
		var hash domain.ContentHash
		var body []byte
		if err := rows.Scan(&ordinal, &hash, &body); err != nil {
			test.Fatal(err)
		}
		if ordinal != count || ordinal >= parts {
			test.Fatal("header ordinals are not an exact contiguous prefix", ordinal, count, parts)
		}
		start := ordinal * 65536
		want := fixture.envelope[start:min(start+65536, len(fixture.envelope))]
		if len(body) == 0 || len(body) > 65536 || !bytes.Equal(body, want) || domain.HashContent(body) != hash {
			test.Fatal("header part lost its exact bounded bytes or SHA", ordinal)
		}
		envelope = append(envelope, body...)
		count++
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	if count != parts || !bytes.Equal(envelope, fixture.envelope) ||
		!bytes.Equal(domain.AssembleCanonicalDoc(envelope, [][]byte{fixture.stream}), fixture.raw) || domain.HashContent(fixture.raw) != fixture.hash {
		test.Fatal("persisted header included framing or changed the original document bytes/SHA")
	}
}

func legacyByteRecoveryPGPreservedImage(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'objects',(SELECT jsonb_agg(jsonb_build_array(owned.kind,owned.hash,encode(body.bytes,'hex'),owned.xmin::text,
 body.xmin::text,body.doc_range_source_version) ORDER BY owned.kind,owned.hash)
 FROM repo_blobs owned JOIN blobs body ON body.hash=owned.hash WHERE owned.repo_id=$1),
 'snapshots',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) ORDER BY id)
 FROM snapshots snapshot WHERE repo_id=$1),
 'memory',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(memory),memory.xmin::text) ORDER BY snapshot_id)
 FROM memories memory WHERE repo_id=$1),
 'publications',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(history),history.xmin::text) ORDER BY id)
 FROM context_history history WHERE repo_id=$1),
 'refs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(ref),ref.xmin::text) ORDER BY kind,name) FROM refs ref WHERE repo_id=$1),
 'workqueue',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY id)
 FROM doc_finalization_jobs job WHERE repo_id=$1)
 )::text`, fixture.repo).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return legacyDerivedOriginalImagePG(test, storage, ctx, fixture) + "\n" + image
}

func legacyByteRecoveryPGPreservationFixture(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) {
	test.Helper()
	digest := domain.MemoryDigest{SnapshotID: fixture.hash, Summary: "preserved memory \ud55c🙂<>& " + string(fixture.repo),
		KeyFacts: []string{"source bytes and publication identity are immutable"}, OpenTasks: []string{}, Provider: domain.ProviderCodex}
	memory, err := storage.PutMemory(ctx, fixture.repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, removal := range []struct {
			statement string
			arguments []any
		}{
			{`DELETE FROM doc_finalization_jobs WHERE repo_id=$1`, []any{fixture.repo}},
			{`DELETE FROM context_history WHERE repo_id=$1`, []any{fixture.repo}},
			{`DELETE FROM memories WHERE repo_id=$1 AND snapshot_id=$2`, []any{fixture.repo, fixture.hash}},
			{`UPDATE snapshots SET memory_hash=NULL WHERE repo_id=$1 AND id=$2`, []any{fixture.repo, fixture.hash}},
			{`DELETE FROM repo_blobs WHERE repo_id=$1 AND kind='memory' AND hash=$2`, []any{fixture.repo, memory}},
			{`DELETE FROM blobs WHERE hash=$1`, []any{memory}},
		} {
			if _, err := storage.pool.Exec(cleanup, removal.statement, removal.arguments...); err != nil {
				test.Error(err)
			}
		}
	})
	if err := storage.PutMemoryMeta(ctx, fixture.repo, digest); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `UPDATE snapshots SET memory_hash=$3 WHERE repo_id=$1 AND id=$2`, fixture.repo, fixture.hash, memory); err != nil {
		test.Fatal(err)
	}
	publication := domain.HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(fixture.repo), BranchID: "l05-recovery",
		Branch: "recovery", Kind: "publish", Source: fixture.hash, Target: fixture.hash, MemorySource: fixture.hash,
		MemoryHash: memory, MemoryPinned: true, GitAfter: strings.Repeat("b", 40), CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if err := domain.ValidateHistoryEvent(publication); err != nil {
		test.Fatal(err)
	}
	encoded, err := json.Marshal(publication)
	if err != nil {
		test.Fatal(err)
	}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,$3)`, fixture.repo, publication.ID, encoded); err != nil {
		test.Fatal(err)
	}
	queued, _, _ := docJobFixture(test, fixture.repo, "preserved workqueue "+string(fixture.repo))
	if _, err := storage.EnqueueDocJob(ctx, queued); err != nil {
		test.Fatal(err)
	}
}

func legacyByteRecoveryPGResources(test *testing.T, work outbound.PreparedDocRangeWork) {
	test.Helper()
	resources, err := work.RetainedResources()
	if err != nil || resources.HeapBytes <= 0 || resources.HeapBytes >= 2<<20 || resources.SpoolBytes < 0 || resources.SpoolBytes >= 64<<20 {
		test.Fatal("recovery exceeded bounded prepared-work resources", resources, err)
	}
}

func legacyByteRecoveryPGAssertPublished(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) {
	test.Helper()
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		generation := reader.Descriptor()
		envelope, err := reader.Envelope(read)
		if err != nil {
			return err
		}
		expectedVersion := 5
		if len(fixture.envelope) > domain.MaxConversationManifestBytes {
			expectedVersion = 6
			if generation.EnvelopeRef == nil || generation.EnvelopeRef.Hash != domain.HashContent(fixture.envelope) || generation.EnvelopeRef.Bytes != len(fixture.envelope) || generation.Envelope != nil {
				test.Fatal("large recovered header lost its exact immutable reference")
			}
		}
		var document domain.CIRDocument
		if err := json.Unmarshal(fixture.raw, &document); err != nil {
			return err
		}
		blocks := (len(fixture.stream) + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
		if generation.Hash != fixture.hash || generation.Version != expectedVersion || generation.Identity() != domain.DocumentIdentityLegacy ||
			generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.ChunkCount() != blocks ||
			generation.StreamBytes != int64(len(fixture.stream)) || generation.EventCount != int64(len(document.Events)) ||
			!bytes.Equal(envelope, fixture.envelope) {
			test.Fatal("recovered generation lost canonical identity, envelope or stream geometry", generation)
		}
		var stream []byte
		for ordinal := 0; ordinal < blocks; ordinal++ {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
			if err != nil {
				return err
			}
			if !bytes.Equal(body, want) || domain.HashContent(body) != descriptor.Hash {
				test.Fatal("recovered public stream changed original bytes/SHA", ordinal)
			}
			stream = append(stream, body...)
		}
		raw := domain.AssembleCanonicalDoc(envelope, [][]byte{stream})
		if !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash {
			test.Fatal("cold recovery publication does not reconstruct the exact original document bytes/SHA")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
}

type legacyByteRecoveryPGColdScenario struct {
	name        string
	empty       bool
	largeHeader bool
}

func TestLegacyByteRecoveryPGColdRestartPublishesExactCanonicalSource(test *testing.T) {
	legacyByteRecoveryPGColdRestart(test, []legacyByteRecoveryPGColdScenario{
		{name: "normal"},
		{name: "empty stream", empty: true},
		{name: "multi-part header", largeHeader: true},
	})
}

func legacyByteRecoveryPGColdRestart(test *testing.T, scenarios []legacyByteRecoveryPGColdScenario) {
	test.Helper()
	if len(scenarios) == 0 {
		test.Fatal("cold recovery requires a restart scenario")
	}
	for _, scenario := range scenarios {
		test.Run(scenario.name, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			directory := rangeSpoolTempDir(test)
			storage.ConfigureLegacyByteRangePublication(true)
			storage.ConfigureLegacyDerivedByteRangePublication(true)
			repo, _ := docRangeJobPGRepo(test, storage, ctx)
			document := domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
				SessionOriginID: string(repo), Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{}}
			if scenario.largeHeader {
				document.Envelope.Cwd = strings.Repeat("header-", 24<<10) + "\ud55c🙂\"\\\n<>&"
			}
			if !scenario.empty {
				document.Events = []domain.CIREvent{
					{Kind: domain.EventMessage, Seq: 9, Role: domain.RoleAssistant,
						Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("x", 768<<10) + "\ud55c🙂\"\\\n<>&"}}},
					{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
						Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo)}}},
				}
			}
			fixture := legacyByteStagePGDocument(test, storage, ctx, repo, document)
			if scenario.empty != (len(fixture.stream) == 0) || scenario.largeHeader && len(fixture.envelope) <= 2*65536 {
				test.Fatal("fixture missed its empty-stream or multi-part-header boundary")
			}
			legacyByteRecoveryPGPreservationFixture(test, storage, ctx, fixture)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			work, err := storage.PrepareDocRangeWork(ctx, job)
			if err != nil || work == nil {
				test.Fatal("prepare original source producer", err)
			}
			test.Cleanup(func() {
				if err := work.Close(); err != nil {
					test.Error(err)
				}
			})
			for step := 0; step < legacyByteRecoveryPGStepLimit && !legacyByteRecoveryPGReady(test, storage, ctx, fixture.hash); step++ {
				progress, err := work.Step(ctx, job)
				if err != nil || progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
					test.Fatal("producer did not persist a recoverable checkpoint through real work", step, progress, err)
				}
			}
			if !legacyByteRecoveryPGReady(test, storage, ctx, fixture.hash) {
				test.Fatal("producer exceeded finite checkpoint steps", legacyByteRecoveryPGStepLimit)
			}
			legacyByteRecoveryPGAssertCheckpoint(test, storage, ctx, fixture)
			checkpoint := legacyByteRecoveryPGImage(test, storage, ctx, fixture.hash)
			if err := work.Close(); err != nil {
				test.Fatal(err)
			}
			if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
				test.Fatal("closed producer retained process-local recovery state", len(files), err)
			}
			if legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public ||
				legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != preserved ||
				legacyByteRecoveryPGImage(test, storage, ctx, fixture.hash) != checkpoint {
				test.Fatal("producer/Close changed public source, workqueue, publication identity or durable checkpoint")
			}
			tag, err := storage.pool.Exec(ctx, `UPDATE doc_range_jobs_v1 SET lease_until=clock_timestamp()-interval '1 minute'
 WHERE repo_id=$1 AND doc_hash=$2 AND base_hash='' AND state='running' AND version=$3`, job.RepoID, job.DocHash, job.Version)
			if err != nil || tag.RowsAffected() != 1 {
				test.Fatal("expire the original producer lease", tag.RowsAffected(), err)
			}
			encoded, err := json.Marshal(job)
			if err != nil {
				test.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				test.Fatal(err)
			}
			command := exec.CommandContext(ctx, executable, "-test.run=^TestLegacyByteRecoveryPGColdChild$", "-test.count=1", "-test.timeout=30s")
			command.Env = append(os.Environ(), "CXT_L05_RECOVERY_CHILD=1", "CXT_L05_RECOVERY_PRIOR="+string(encoded))
			if output, err := command.CombinedOutput(); err != nil {
				test.Fatalf("cold recovery child failed: %v\n%s", err, output)
			}
			legacyByteRecoveryPGAssertPublished(test, storage, ctx, fixture)
			legacyByteRecoveryPGAssertCheckpoint(test, storage, ctx, fixture)
			if legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != preserved ||
				legacyByteRecoveryPGImage(test, storage, ctx, fixture.hash) != checkpoint {
				test.Fatal("cold reclaim/reverify/publication rewrote source, conversation, memory, publicationID, workqueue or checkpoint")
			}
			if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
				test.Fatal("cold child Close leaked local recovery spools", len(files), err)
			}
		})
	}
}

func TestLegacyByteRecoveryPGColdChild(test *testing.T) {
	if strings.TrimSpace(os.Getenv("CXT_TEST_DSN")) == "" {
		test.Fatal("cold recovery requires an explicit CXT_TEST_DSN")
	}
	if os.Getenv("CXT_L05_RECOVERY_CHILD") == "" {
		legacyByteRecoveryPGColdRestart(test, []legacyByteRecoveryPGColdScenario{{name: "normal"}})
		return
	}
	if os.Getenv("CXT_L05_RECOVERY_CHILD") != "1" {
		test.Fatal("invalid cold recovery child mode")
	}
	var prior domain.DocRangeIndexJob
	if err := json.Unmarshal([]byte(os.Getenv("CXT_L05_RECOVERY_PRIOR")), &prior); err != nil || prior.Validate() != nil {
		test.Fatal("invalid parent claim identity", err)
	}
	base, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	storage, err := NewPostgresStore(base, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		test.Fatal(err)
	}
	defer storage.Close()
	peer, trace := docCaptureWorkPGStore(test, storage, base)
	job := docRangeJobPGClaim(test, peer, base, prior.RepoID, prior.DocHash)
	if job.Version != prior.Version+1 || job.Attempts != prior.Attempts+1 || !job.CreatedAt.Equal(prior.CreatedAt) || job.BaseHash != prior.BaseHash {
		test.Fatal("cold reclaim replaced queue identity or lost its lease/version fence", prior, job)
	}
	fixture := legacyByteStagePGFixture{repo: job.RepoID, hash: job.DocHash}
	if !legacyByteRecoveryPGReady(test, peer, base, job.DocHash) || generationCountsPG(test, peer, base, job.DocHash) != [2]int{} {
		test.Fatal("cold recovery must start with a durable checkpoint and no public generation")
	}
	var nonempty bool
	if err := peer.pool.QueryRow(base, `SELECT blocks>0 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, job.DocHash).Scan(&nonempty); err != nil {
		test.Fatal(err)
	}
	checkpoint := legacyByteRecoveryPGImage(test, peer, base, job.DocHash)
	public := legacyByteRecoveryPGPublicationImage(test, peer, base, fixture)
	ctx := &legacyByteRecoveryPGVerificationContext{Context: base}
	work, err := peer.PrepareDocRangeWork(ctx, job)
	if err != nil || work == nil {
		test.Fatal("prepare cold recovery", err)
	}
	defer func() {
		if err := work.Close(); err != nil {
			test.Error(err)
		}
	}()
	if _, recovery := work.(*preparedLegacyRecoveryWork); !recovery {
		test.Fatalf("ready checkpoint repeated capture/decode/native/sort instead of recovery: %T", work)
	}
	legacyByteRecoveryPGResources(test, work)
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("durable checkpoint minted publication authority before fresh verification", err)
	}
	progress := outbound.DocRangeWorkPending
	for step := 0; step < legacyByteRecoveryPGStepLimit && progress == outbound.DocRangeWorkPending; step++ {
		progress, err = work.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending && progress != outbound.DocRangeWorkPublishable {
			test.Fatal("cold recovery did not advance through bounded prepared work", step, progress, err)
		}
		legacyByteRecoveryPGResources(test, work)
		if progress == outbound.DocRangeWorkPending {
			if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
				test.Fatal("partial recovery advertised publication", step, err)
			}
		}
	}
	if progress != outbound.DocRangeWorkPublishable || !ctx.preparation.Load() || !ctx.step.Load() || nonempty && !ctx.event.Load() {
		test.Fatal("cold recovery skipped fresh source preparation/event verification or exceeded finite steps",
			progress, ctx.preparation.Load(), ctx.step.Load(), ctx.event.Load())
	}
	if trace.bodyReads.Load() != 0 || legacyByteRecoveryPGPublicationImage(test, peer, base, fixture) != public ||
		legacyByteRecoveryPGImage(test, peer, base, job.DocHash) != checkpoint {
		test.Fatal("recovery reread original blob bodies, changed the checkpoint or published before Publish", trace.bodyReads.Load())
	}
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("publish cold reverified work", err)
	}
	if trace.bodyReads.Load() != 0 {
		test.Fatal("publication reread original blob bodies after cold recovery", trace.bodyReads.Load())
	}
	done := docRangeJobPGGet(test, peer, base, job.RepoID, job.DocHash)
	if done.State != "completed" || done.Version != job.Version || done.Attempts != job.Attempts ||
		!done.CreatedAt.Equal(job.CreatedAt) || done.BaseHash != job.BaseHash || !done.LeaseUntil.IsZero() {
		test.Fatal("publication did not atomically complete the same reclaimed workqueue claim", job, done)
	}
	legacyDerivedPublishedPG(test, peer, base, fixture)
	if err := work.Close(); err != nil {
		test.Fatal(err)
	}
	if legacyByteRecoveryPGImage(test, peer, base, job.DocHash) != checkpoint {
		test.Fatal("recovery publication/Close rewrote durable header or stream parts")
	}
}
