//go:build postgres

package documentquery

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type snapshotlessLegacyBasePGCosts struct {
	opens, pages, chunkLookups, manifests, chunks, whole, indexes int
}

type snapshotlessLegacyBasePGStore struct {
	*store.PostgresStore
	cost snapshotlessLegacyBasePGCosts
}

type snapshotlessLegacyBasePGRead struct {
	outbound.DocRangeGenerationRead
	storage *snapshotlessLegacyBasePGStore
}

func (storage *snapshotlessLegacyBasePGStore) OpenDocRangeGeneration(ctx context.Context, repo, hash domain.ContentHash) (outbound.DocRangeGenerationRead, error) {
	storage.cost.opens++
	read, err := storage.PostgresStore.OpenDocRangeGeneration(ctx, repo, hash)
	if err != nil {
		return nil, err
	}
	return &snapshotlessLegacyBasePGRead{DocRangeGenerationRead: read, storage: storage}, nil
}

func (read *snapshotlessLegacyBasePGRead) ReadPage(ctx context.Context, position int) ([]domain.DocRangeEntry, error) {
	read.storage.cost.pages++
	return read.DocRangeGenerationRead.ReadPage(ctx, position)
}

func (read *snapshotlessLegacyBasePGRead) Chunk(ctx context.Context, position int) (domain.ConversationManifestChunk, error) {
	read.storage.cost.chunkLookups++
	return read.DocRangeGenerationRead.Chunk(ctx, position)
}

func (storage *snapshotlessLegacyBasePGStore) DocReadIndex(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocReadIndex, error) {
	storage.cost.indexes++
	return domain.DocReadIndex{}, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) GetDoc(context.Context, domain.ContentHash, domain.ContentHash) (domain.SessionDoc, error) {
	storage.cost.whole++
	return domain.SessionDoc{}, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) GetDocManifest(context.Context, domain.ContentHash, domain.ContentHash) (domain.DocChunkManifest, error) {
	storage.cost.manifests++
	return domain.DocChunkManifest{}, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) GetChunk(context.Context, domain.ContentHash, domain.ContentHash) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func (storage *snapshotlessLegacyBasePGStore) ReadConversationChunk(context.Context, domain.ContentHash, domain.ContentHash, int64) ([]byte, error) {
	storage.cost.chunks++
	return nil, domain.ErrIntegrity
}

func TestPGDocByteUpgradeSnapshotlessLegacyBaseRetainsMetadataRead(test *testing.T) {
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		test.Skip("CXT_TEST_DSN unset")
	}
	ctx := inbound.WithSystemActor(context.Background())
	storage, err := store.NewPostgresStore(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(storage.Close)
	if _, err := storage.ApplyMigrations(ctx, "../../../../schemas/db/migrations"); err != nil {
		test.Fatal(err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	repo := domain.HashContent([]byte(fmt.Sprintf("%s-%d", test.Name(), time.Now().UnixNano())))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pool.Exec(cleanup, `DELETE FROM doc_range_jobs_v1 WHERE repo_id=$1`, repo); err != nil {
			test.Error(err)
		}
	})
	document := domain.SessionDoc{CIR: domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "1", SessionOriginID: string(repo), SourceProvider: domain.ProviderCodex, Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Seq: 0, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: "original byte upgrade source " + string(repo)}}},
			{Seq: 1, Kind: domain.EventMessage, Role: domain.RoleAssistant, Blocks: []domain.ContentBlock{{Type: "text", Text: strings.Repeat("original \"quoted\" bytes 🙂\n", 8)}}},
		},
	}}
	canonical, err := domain.CanonicalBytes(document.CIR)
	if err != nil {
		test.Fatal(err)
	}
	document.Hash = domain.HashContent(canonical)
	verified, err := domain.VerifySessionDoc(document)
	if err != nil {
		test.Fatal(err)
	}
	plan, err := verified.PlanReadIndexContext(ctx)
	if err != nil {
		test.Fatal(err)
	}
	expectedHashes := plan.EventHashes()
	if _, err := storage.PutDoc(ctx, repo, document); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: document.Hash, RepoID: repo, DocHash: document.Hash,
		DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	storage.ConfigureLegacyByteRangePublication(false)
	if _, err := storage.EnqueueDocRangeJob(ctx, repo, document.Hash, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	claimed, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), time.Minute)
	if err != nil || claimed.RepoID != repo || claimed.DocHash != document.Hash || claimed.BaseHash != "" {
		test.Fatal("could not claim snapshotless base fixture preparation", claimed, err)
	}
	if err := storage.BuildDocRangeJob(ctx, claimed); err != nil {
		test.Fatal(err)
	}
	storage.ConfigureLegacyByteRangePublication(true)
	var binding domain.DocReadGenerationBinding
	var descriptor []byte
	err = pool.QueryRow(ctx, `SELECT generation.generation_id::text,source.doc_range_source_version,generation.generation_hash,generation.descriptor
 FROM doc_range_generations_v2 generation JOIN blobs source ON source.hash=generation.doc_hash
 AND source.doc_range_source_version=generation.source_version WHERE generation.doc_hash=$1`, document.Hash).Scan(
		&binding.SourceID, &binding.SourceVersion, &binding.RangeGeneration, &descriptor)
	if err != nil {
		test.Fatal(err)
	}
	generation, err := domain.DecodeDocRangeGeneration(document.Hash, binding.RangeGeneration, descriptor)
	if err != nil || binding.Validate() != nil || generation.Version != 3 || generation.StreamBytes == 0 || generation.Identity() != domain.DocumentIdentityLegacy || generation.EventCount != int64(len(document.CIR.Events)) {
		test.Fatal("old metadata fixture lost its current legacy v3 source binding", generation, binding, err)
	}
	deleted, err := pool.Exec(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, repo, document.Hash)
	if err != nil || deleted.RowsAffected() != 1 {
		test.Fatal("fixture did not remove exactly the legacy snapshot", deleted, err)
	}
	before := snapshotlessLegacyBasePGImage(test, ctx, pool, repo)
	observed := &snapshotlessLegacyBasePGStore{PostgresStore: storage}
	queries := &Queries{metadata: observed, legacy: observed, index: observed, view: ViewDependencies{
		RangeGenerations: observed, Chunks: observed, Bytes: observed,
	}}
	err = storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		source, err := queries.openDocViewBase(read, repo, document.Hash)
		if err != nil || source.binding.Validate() != nil || source.binding != binding || source.root || source.total != len(document.CIR.Events) || source.chunk != nil || source.streamBytes != generation.StreamBytes || !reflect.DeepEqual(source.envelope, document.CIR.Envelope) {
			test.Fatal("snapshotless owned legacy base lost its bound metadata reader", source.total, source.binding, err)
		}
		entry, err := source.entry(0)
		if err != nil || entry.Index != 0 || entry.Hash == "" || entry.Hash != expectedHashes[0] {
			test.Fatal("snapshotless metadata entry unavailable or changed", entry, err)
		}
		return nil
	})
	if err != nil {
		test.Fatal(err)
	}
	if observed.cost != (snapshotlessLegacyBasePGCosts{opens: 1, pages: 1}) {
		test.Fatal("snapshotless metadata base performed a compatibility body read", observed.cost)
	}
	if after := snapshotlessLegacyBasePGImage(test, ctx, pool, repo); after != before {
		test.Fatal("snapshotless base read changed persisted state")
	}
}

func snapshotlessLegacyBasePGImage(test *testing.T, ctx context.Context, pool *pgxpool.Pool, repo domain.ContentHash) string {
	test.Helper()
	var image string
	err := pool.QueryRow(ctx, `WITH owned_documents AS (
 SELECT hash FROM repo_blobs WHERE repo_id=$1 AND kind='doc'
), owned_blocks AS (
 SELECT DISTINCT block_hash FROM doc_read_block_locations_v3 WHERE doc_hash IN (SELECT hash FROM owned_documents)
)
SELECT jsonb_build_object(
 'objects',(SELECT jsonb_agg(jsonb_build_array(owned.kind,owned.hash,encode(blob.bytes,'hex'),owned.xmin::text,blob.xmin::text) ORDER BY owned.kind,owned.hash) FROM repo_blobs owned JOIN blobs blob ON blob.hash=owned.hash WHERE owned.repo_id=$1),
 'snapshots',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) ORDER BY snapshot.id) FROM snapshots snapshot WHERE snapshot.repo_id=$1),
 'legacy_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_read_indexes header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'v2_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_read_indexes_v2 header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'v3_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_read_indexes_v3 header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'locations',(SELECT jsonb_agg(to_jsonb(location) ORDER BY location.doc_hash,location.ordinal) FROM doc_read_event_locations_current location WHERE location.doc_hash IN (SELECT hash FROM owned_documents)),
 'blocks',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(block),block.xmin::text) ORDER BY block.hash) FROM doc_read_blocks_v3 block WHERE block.hash IN (SELECT block_hash FROM owned_blocks)),
 'block_events',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(event),event.xmin::text) ORDER BY event.block_hash,event.ordinal) FROM doc_read_block_events_v3 event WHERE event.block_hash IN (SELECT block_hash FROM owned_blocks)),
 'search',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(event),event.xmin::text) ORDER BY event.hash) FROM doc_search_events_v2 event WHERE event.hash IN (SELECT event_hash FROM doc_read_event_locations_current WHERE doc_hash IN (SELECT hash FROM owned_documents))),
 'range_indexes',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(header),header.xmin::text) ORDER BY header.hash) FROM doc_range_indexes_v1 header WHERE header.hash IN (SELECT hash FROM owned_documents)),
 'range_generations',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(generation),generation.xmin::text) ORDER BY generation.doc_hash) FROM doc_range_generations_v2 generation WHERE generation.doc_hash IN (SELECT hash FROM owned_documents)),
 'range_objects',(SELECT jsonb_agg(jsonb_build_array(objects.doc_hash,objects.hash,encode(objects.bytes,'hex'),objects.xmin::text) ORDER BY objects.doc_hash,objects.hash) FROM doc_range_objects_v2 objects WHERE objects.doc_hash IN (SELECT hash FROM owned_documents)),
 'visible_generations',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(generation),generation.xmin::text) ORDER BY generation.doc_hash) FROM doc_visible_generations_v1 generation WHERE generation.doc_hash IN (SELECT hash FROM owned_documents)),
 'boundaries',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(boundary),boundary.xmin::text) ORDER BY boundary.doc_hash,boundary.base_hash) FROM doc_range_boundaries_v1 boundary WHERE boundary.repo_id=$1),
 'range_jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.doc_hash,job.base_hash) FROM doc_range_jobs_v1 job WHERE job.repo_id=$1),
 'find_jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.doc_hash) FROM doc_find_fields_jobs_v1 job WHERE job.repo_id=$1),
 'finalization_jobs',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(job),job.xmin::text) ORDER BY job.id) FROM doc_finalization_jobs job WHERE job.repo_id=$1)
)::text`, repo).Scan(&image)
	if err != nil {
		test.Fatal(err)
	}
	return image
}
