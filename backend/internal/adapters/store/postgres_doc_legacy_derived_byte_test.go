//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func legacyDerivedOriginalImagePG(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'source',(SELECT jsonb_build_array(hash,md5(bytes),octet_length(bytes),doc_range_source_version,xmin::text) FROM blobs WHERE hash=$1),
 'incarnation',(SELECT jsonb_build_array(incarnation::text,xmin::text) FROM blob_source_incarnations WHERE hash=$1),
 'ownership',(SELECT jsonb_agg(jsonb_build_array(repo_id,kind,hash,xmin::text) ORDER BY repo_id,kind,hash) FROM repo_blobs WHERE hash=$1),
 'snapshot',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) ORDER BY repo_id,id) FROM snapshots snapshot WHERE doc_hash=$1)
 )::text`, fixture.hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func legacyDerivedPublishedPG(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) {
	test.Helper()
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || !ready {
		test.Fatal("derived publication is not byte-ready", ready, err)
	}
	job := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash)
	var sourceVersion, completedVersion int64
	var layout int
	var generationPlan, stagePlan domain.ContentHash
	if err := storage.pool.QueryRow(ctx, `SELECT generation.range_layout_version,generation.legacy_byte_plan_hash,
	 stage.plan_hash,source.doc_range_source_version,job.byte_source_version FROM doc_range_generations_v2 generation
 JOIN doc_legacy_byte_stages_v1 stage ON stage.doc_hash=generation.doc_hash
 JOIN blobs source ON source.hash=stage.doc_hash
 JOIN doc_range_jobs_v1 job ON job.doc_hash=source.hash AND job.repo_id=$2 AND job.base_hash=''
 WHERE generation.doc_hash=$1`, fixture.hash, fixture.repo).
		Scan(&layout, &generationPlan, &stagePlan, &sourceVersion, &completedVersion); err != nil {
		test.Fatal(err)
	}
	expectedLayout := 5
	if len(fixture.envelope) > domain.MaxConversationManifestBytes {
		expectedLayout = 6
	}
	if layout != expectedLayout || generationPlan != stagePlan || job.State != "completed" || completedVersion != sourceVersion {
		test.Fatal("byte generation, retained plan and job completion disagree", layout, generationPlan, stagePlan, job, sourceVersion)
	}
}

func TestLegacyDerivedBytePGResumesPublishesReadsAndRetains(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	first, err := storage.stageLegacyByteBackfill(ctx, job, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || first.next != 8 || first.blocks != 11 || first.complete {
		test.Fatal("fixture did not persist a partial bounded copy", first, err)
	}
	prefix := legacyByteStagePGImage(test, storage, ctx, fixture.hash, first.next)
	job.State, job.Reason, job.NextAttempt = "retrying", "restart fixture", time.Now().UTC().Add(-time.Minute)
	if err := storage.FinishDocRangeJob(ctx, job, time.Now().UTC()); err != nil {
		test.Fatal(err)
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	resumed := docRangeJobPGClaim(test, peer, ctx, fixture.repo, fixture.hash)
	other, _ := docRangeJobPGRepo(test, storage, ctx)
	if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, other, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: other, DocHash: fixture.hash,
		DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	if _, err := storage.EnqueueDocByteRangeJob(ctx, other, fixture.hash, time.Now().UTC().Add(-time.Hour)); err != nil {
		test.Fatal(err)
	}
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	if err := peer.BuildDocRangeJob(ctx, resumed); err != nil {
		test.Fatal(err)
	}
	legacyDerivedPublishedPG(test, peer, ctx, fixture)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, first.next); after != prefix {
		test.Fatal("worker restart rewrote the durable prefix")
	}
	peer.ConfigureLegacyDerivedByteRangePublication(false)
	peer.ConfigureLegacyByteRangePublication(false)
	foreign, _ := docRangeJobPGRepo(test, storage, ctx)
	if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := peer.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		if generation := reader.Descriptor(); generation.Version != 5 || generation.Identity() != domain.DocumentIdentityLegacy ||
			generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.ChunkCount() != first.blocks {
			test.Fatal("reader-only peer changed the document or byte-storage identity", generation)
		}
		for _, ordinal := range []int{0, 7, 8, first.blocks - 1, 0} {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			body, err := peer.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
			if err != nil || !bytes.Equal(body, want) {
				test.Fatal("published read changed the selected canonical bytes", ordinal, err)
			}
		}
		_, descriptor := legacyByteStagePGBlock(fixture, 0)
		for _, fault := range []string{"repository", "generation", "source version", "ordinal", "descriptor"} {
			repo, binding, ordinal, selected := fixture.repo, reader.Binding(), 0, descriptor
			switch fault {
			case "repository":
				repo = foreign
			case "generation":
				binding.RangeGeneration = domain.HashContent([]byte("unrelated generation"))
			case "source version":
				binding.SourceVersion++
			case "ordinal":
				ordinal = first.blocks
			case "descriptor":
				selected.Hash = domain.HashContent([]byte("unrelated bytes"))
			}
			if body, err := peer.ReadLegacyDerivedByteChunk(read, repo, fixture.hash, binding, ordinal, selected); err == nil || len(body) != 0 {
				test.Fatal("rejected binding returned published content", fault, len(body), err)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	peer.ConfigureLegacyByteRangePublication(true)
	otherJob := docRangeJobPGClaim(test, peer, ctx, other, fixture.hash)
	if err := peer.BuildDocRangeJob(ctx, otherJob); err != nil {
		test.Fatal("reader-first worker could not adopt an already published derived generation", err)
	}
	if done := docRangeJobPGGet(test, peer, ctx, other, fixture.hash); done.State != "completed" {
		test.Fatal("shared derived generation did not complete the second repository's job", done)
	}
	if _, err := storage.pool.Exec(ctx, `DELETE FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if ready, err := peer.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("cache removal retained public readiness", ready, err)
	}
	var derivedRows int
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM doc_range_generations_v2 WHERE doc_hash=$1)
 +(SELECT count(*) FROM doc_range_objects_v2 WHERE doc_hash=$1)
 +(SELECT count(*) FROM doc_legacy_byte_stage_parts_v1 WHERE doc_hash=$1)
 +(SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1)`, fixture.hash).Scan(&derivedRows); err != nil || derivedRows != 0 {
		test.Fatal("cache removal left a dangling public generation or derived content", derivedRows, err)
	}
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("publication, adoption or cache retirement changed the original source, snapshot or ownership")
	}
	if pending, err := storage.EnqueueDocByteRangeJob(ctx, fixture.repo, fixture.hash, time.Now().UTC()); err != nil || pending.State != "waiting" {
		test.Fatal("normal demand could not rebuild the retired derived generation", pending, err)
	}
}

func TestLegacyDerivedBytePGFinalFenceRollsBackPublication(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	if stage, err := storage.stageLegacyByteSession(ctx, job, session, 8); err != nil || !stage.complete {
		test.Fatal(stage, err)
	}
	prepared, err := storage.prepareLegacyByteWork(ctx, job, session)
	if err != nil {
		test.Fatal(err)
	}
	defer prepared.Close()
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	name := pgx.Identifier{domain.NewID("derived_range_fence_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER INSERT ON doc_range_generations_v2 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	drop := `DROP TRIGGER IF EXISTS ` + name + ` ON doc_range_generations_v2; DROP FUNCTION IF EXISTS ` + name + `()`
	defer func() {
		if _, err := storage.pool.Exec(context.WithoutCancel(ctx), drop); err != nil {
			test.Error(err)
		}
	}()
	if err := prepared.Publish(ctx, job); !errors.Is(err, domain.ErrConflict) {
		test.Fatal("publication ignored the final job fence", err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("failed publication committed metadata, generation, or injected job changes")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("failed publication changed the complete resumable byte stage")
	}
	if _, err := storage.pool.Exec(ctx, drop); err != nil {
		test.Fatal(err)
	}
	if err := storage.publishLegacyDerivedByteRange(ctx, job, session); err != nil {
		test.Fatal("publication could not retry the same complete stage", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
}

func TestLegacyDerivedBytePGRejectsSwappedPrivatePart(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 2<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	session, err := storage.prepareLegacyByteBackfill(ctx, job)
	if err != nil {
		test.Fatal(err)
	}
	if stage, err := storage.stageLegacyByteSession(ctx, job, session, 8); err != nil || !stage.complete {
		test.Fatal(stage, err)
	}
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	_, first := legacyByteStagePGBlock(fixture, 0)
	_, second := legacyByteStagePGBlock(fixture, 1)
	if first.Hash == second.Hash || first.Bytes != second.Bytes {
		test.Fatal("fixture must have different equal-sized valid canonical blocks")
	}
	transaction, err := storage.pool.Begin(ctx)
	if err != nil {
		test.Fatal(err)
	}
	defer rollbackPG(transaction)
	if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_parts_v1 DISABLE TRIGGER guard_legacy_byte_stage_parts`); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `UPDATE doc_legacy_byte_stage_parts_v1 SET hash=$2 WHERE doc_hash=$1 AND ordinal=0`, fixture.hash, second.Hash); err != nil {
		test.Fatal(err)
	}
	if _, err := transaction.Exec(ctx, `ALTER TABLE doc_legacy_byte_stage_parts_v1 ENABLE TRIGGER guard_legacy_byte_stage_parts`); err != nil {
		test.Fatal(err)
	}
	if err := transaction.Commit(ctx); err != nil {
		test.Fatal(err)
	}
	if err := storage.publishLegacyDerivedByteRange(ctx, job, session); err == nil {
		test.Fatal("valid per-object hashes concealed a wrong whole-copy order")
	}
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("corrupt derived order became publicly ready", ready, err)
	}
	if pending := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash); pending.State != "running" || pending.Version != job.Version {
		test.Fatal("rejected copy completed or changed the fenced job", pending)
	}
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("rejected derived order changed the original archival state")
	}
}
