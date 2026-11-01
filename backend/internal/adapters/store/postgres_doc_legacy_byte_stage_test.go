//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type legacyByteStagePGFixture struct {
	repo, hash domain.ContentHash
	raw        []byte
	envelope   json.RawMessage
	stream     []byte
}

func legacyByteStagePGSource(test *testing.T, storage *PostgresStore, ctx context.Context, payloadBytes int) legacyByteStagePGFixture {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Kind: domain.EventMessage, Seq: 9, Role: domain.RoleAssistant,
				Blocks: []domain.ContentBlock{{Type: "text", Text: string(repo) + strings.Repeat("x", payloadBytes) + "\ud55c🙂\"\\\n<>&"}}},
			{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: test.Name() + ":" + string(repo)}}},
		},
	}
	return legacyByteStagePGDocument(test, storage, ctx, repo, document)
}

func legacyByteStagePGDocument(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, document domain.CIRDocument) legacyByteStagePGFixture {
	test.Helper()
	raw, err := domain.CanonicalBytes(document)
	if err != nil {
		test.Fatal(err)
	}
	var parts struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		test.Fatal(err)
	}
	events := make([][]byte, len(parts.Events))
	for position, event := range parts.Events {
		events[position] = event
	}
	stream := bytes.Join(events, []byte(","))
	if !bytes.Equal(domain.AssembleCanonicalDoc(parts.Envelope, [][]byte{stream}), raw) {
		test.Fatal("fixture does not preserve the exact canonical events-array interior")
	}
	fixture := legacyByteStagePGFixture{repo: repo, hash: domain.HashContent(raw), raw: raw,
		envelope: parts.Envelope, stream: stream}
	if _, err := storage.pool.Exec(ctx, `INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, fixture.hash, raw); err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, statement := range []string{
			`DELETE FROM snapshots WHERE id=$1 OR doc_hash=$1`,
			`DELETE FROM repo_blobs WHERE hash=$1`,
			`DELETE FROM blobs WHERE hash=$1`,
		} {
			if _, err := storage.pool.Exec(cleanup, statement, fixture.hash); err != nil {
				test.Error(err)
			}
		}
	})
	if _, err := storage.pool.Exec(ctx, `INSERT INTO repo_blobs(repo_id,kind,hash) VALUES($1,'doc',$2)`, repo, fixture.hash); err != nil {
		test.Fatal(err)
	}
	if err := storage.PutSnapshot(ctx, domain.Snapshot{ID: fixture.hash, RepoID: repo, DocHash: fixture.hash,
		DocIdentity: domain.DocumentIdentityLegacy, Provider: domain.ProviderCodex, Fidelity: domain.FidelityFull}); err != nil {
		test.Fatal(err)
	}
	return fixture
}

func legacyByteStagePGClaim(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) domain.DocRangeIndexJob {
	test.Helper()
	storage.ConfigureLegacyByteRangePublication(true)
	if _, err := storage.EnqueueDocByteRangeJob(ctx, fixture.repo, fixture.hash, time.Now().UTC().Add(-time.Hour)); err != nil {
		test.Fatal(err)
	}
	job, err := storage.ClaimDocRangeJob(ctx, time.Now().UTC(), 2*time.Minute)
	if err != nil || job.RepoID != fixture.repo || job.DocHash != fixture.hash || job.BaseHash != "" || job.State != "running" {
		test.Fatalf("claim byte staging job: %+v, %v", job, err)
	}
	return job
}

func legacyByteStagePGPublicImage(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'source',(SELECT jsonb_build_array(hash,md5(bytes),octet_length(bytes),doc_range_source_version,xmin::text) FROM blobs WHERE hash=$2),
 'incarnation',(SELECT jsonb_build_array(incarnation::text,xmin::text) FROM blob_source_incarnations WHERE hash=$2),
 'ownership',(SELECT jsonb_agg(jsonb_build_array(repo_id,kind,hash,xmin::text) ORDER BY repo_id,kind,hash) FROM repo_blobs WHERE repo_id=$1 OR hash=$2),
 'snapshot',(SELECT jsonb_build_array(to_jsonb(snapshot),snapshot.xmin::text) FROM snapshots snapshot WHERE repo_id=$1 AND id=$2),
 'job',(SELECT jsonb_build_array(to_jsonb(job),job.xmin::text) FROM doc_range_jobs_v1 job WHERE repo_id=$1 AND doc_hash=$2 AND base_hash=''),
 'generation',(SELECT jsonb_build_array(to_jsonb(generation),generation.xmin::text) FROM doc_range_generations_v2 generation WHERE doc_hash=$2),
 'objects',(SELECT jsonb_agg(jsonb_build_array(hash,md5(bytes),xmin::text) ORDER BY hash) FROM doc_range_objects_v2 WHERE doc_hash=$2)
 )::text`, fixture.repo, fixture.hash).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func legacyByteStagePGImage(test *testing.T, storage *PostgresStore, ctx context.Context, hash domain.ContentHash, prefix int) string {
	test.Helper()
	var image string
	if err := storage.pool.QueryRow(ctx, `SELECT jsonb_build_object(
 'header',(SELECT jsonb_build_array(to_jsonb(stage),stage.xmin::text) FROM doc_legacy_byte_stages_v1 stage WHERE doc_hash=$1 AND $2::integer<0),
 'objects',(SELECT jsonb_agg(jsonb_build_array(object.hash,md5(object.bytes),octet_length(object.bytes),object.xmin::text) ORDER BY object.hash)
 FROM doc_legacy_byte_stage_objects_v1 object WHERE object.doc_hash=$1 AND ($2::integer<0 OR EXISTS(
 SELECT 1 FROM doc_legacy_byte_stage_parts_v1 part WHERE part.doc_hash=object.doc_hash AND part.hash=object.hash AND part.ordinal<$2))),
 'parts',(SELECT jsonb_agg(jsonb_build_array(to_jsonb(part),part.xmin::text) ORDER BY part.ordinal)
 FROM doc_legacy_byte_stage_parts_v1 part WHERE doc_hash=$1 AND ($2::integer<0 OR ordinal<$2))
 )::text`, hash, prefix).Scan(&image); err != nil {
		test.Fatal(err)
	}
	return image
}

func legacyByteStagePGBlock(fixture legacyByteStagePGFixture, ordinal int) ([]byte, domain.ConversationManifestChunk) {
	start := ordinal * domain.ConversationManifestChunkBytes
	body := fixture.stream[start:min(start+domain.ConversationManifestChunkBytes, len(fixture.stream))]
	return body, domain.ConversationManifestChunk{Hash: domain.HashContent(body), Bytes: int64(len(body))}
}

func legacyByteStagePGAssertStored(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, expected legacyByteStage) {
	test.Helper()
	var actual legacyByteStage
	var encoded []byte
	if err := storage.pool.QueryRow(ctx, `SELECT source_id::text,source_version,plan_hash,next_block,blocks,complete,spec,stage_id::text
 FROM doc_legacy_byte_stages_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&actual.sourceID, &actual.sourceVersion, &actual.plan, &actual.next, &actual.blocks, &actual.complete, &encoded, &actual.incarnation); err != nil {
		test.Fatal(err)
	}
	spec, err := domain.DecodeDocLegacyByteBackfillSpec(actual.plan, encoded)
	if err != nil || actual != expected || actual.incarnation == "" || spec.Reference != (domain.DocumentRef{Hash: fixture.hash, Identity: domain.DocumentIdentityLegacy}) ||
		spec.StreamBytes != int64(len(fixture.stream)) || spec.BlockBytes != domain.ConversationManifestChunkBytes || spec.Blocks != actual.blocks {
		test.Fatalf("persisted stage or canonical plan differs: %+v, %+v, %v", actual, spec, err)
	}
	var sourceID string
	var sourceVersion int64
	if err := storage.pool.QueryRow(ctx, `SELECT incarnation.incarnation::text,source.doc_range_source_version
 FROM blobs source JOIN blob_source_incarnations incarnation ON incarnation.hash=source.hash WHERE source.hash=$1`, fixture.hash).
		Scan(&sourceID, &sourceVersion); err != nil || actual.sourceID != sourceID || actual.sourceVersion != sourceVersion {
		test.Fatal("stage did not retain the current source incarnation and version", err)
	}
	rows, err := storage.pool.Query(ctx, `SELECT part.ordinal,part.hash,part.byte_count,object.bytes
 FROM doc_legacy_byte_stage_parts_v1 part JOIN doc_legacy_byte_stage_objects_v1 object
 ON object.doc_hash=part.doc_hash AND object.hash=part.hash WHERE part.doc_hash=$1 ORDER BY part.ordinal`, fixture.hash)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	var joined []byte
	objects := make(map[domain.ContentHash]bool)
	count := 0
	for rows.Next() {
		var ordinal, size int
		var hash domain.ContentHash
		var body []byte
		if err := rows.Scan(&ordinal, &hash, &size, &body); err != nil {
			test.Fatal(err)
		}
		if ordinal != count || ordinal >= actual.next {
			test.Fatal("staged parts are missing, duplicated, or beyond the checkpoint", ordinal, count, actual.next)
		}
		want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
		if size != len(want) || int64(size) != descriptor.Bytes || hash != descriptor.Hash || domain.HashContent(body) != hash || !bytes.Equal(body, want) {
			test.Fatal("staged block differs from its exact canonical stream range", ordinal)
		}
		objects[hash] = true
		joined = append(joined, body...)
		count++
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	rows.Close()
	if count != actual.next || !bytes.Equal(joined, fixture.stream[:min(actual.next*domain.ConversationManifestChunkBytes, len(fixture.stream))]) {
		test.Fatal("checkpoint and reconstructed canonical stream disagree", count, actual.next)
	}
	var objectCount int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM doc_legacy_byte_stage_objects_v1 WHERE doc_hash=$1`, fixture.hash).
		Scan(&objectCount); err != nil || objectCount != len(objects) {
		test.Fatal("private CAS contains missing, duplicate, or unreferenced objects", objectCount, len(objects), err)
	}
	if actual.complete && !bytes.Equal(domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{joined}), fixture.raw) {
		test.Fatal("completed private stage does not reconstruct the original canonical document")
	}
}

func TestLegacyByteStagePGBatchesRestartAndPrivateReads(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	var verifier domain.CanonicalDocVerifier
	proof, err := verifier.Verify(ctx, fixture.hash, fixture.raw)
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
		return storage.putLegacyRangeGeneration(bound, fixture.repo, proof)
	}); err != nil {
		test.Fatal(err)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	before := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	first, err := storage.stageLegacyByteBackfill(ctx, job, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || first.next != 8 || first.blocks != 11 || first.complete {
		test.Fatalf("first bounded batch: %+v, %v", first, err)
	}
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, first)
	prefix := legacyByteStagePGImage(test, storage, ctx, fixture.hash, first.next)
	_, descriptor := legacyByteStagePGBlock(fixture, 0)
	for _, forged := range []bool{false, true} {
		candidate := first
		if forged {
			candidate.complete, candidate.next = true, candidate.blocks
		}
		err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
			body, err := storage.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, candidate, 0, descriptor)
			if len(body) != 0 {
				test.Fatal("partial persisted stage exposed private bytes", forged)
			}
			return err
		})
		if err == nil || !forged && !errors.Is(err, domain.ErrDocRangeIndexUnavailable) {
			test.Fatal("partial stage or forged local completion was accepted", forged, err)
		}
	}
	peer, err := NewPostgresStore(ctx, storage.pool.Config().ConnString())
	if err != nil {
		test.Fatal(err)
	}
	defer peer.Close()
	peer.ConfigureLegacyByteRangePublication(true)
	completed, err := peer.stageLegacyByteBackfill(ctx, job, domain.DocLegacyByteBackfillBatchBlocks)
	if err != nil || completed.next != 11 || completed.blocks != first.blocks || !completed.complete ||
		completed.plan != first.plan || completed.sourceID != first.sourceID || completed.sourceVersion != first.sourceVersion {
		test.Fatalf("restart did not resume the same unexpired job and plan: %+v, %v", completed, err)
	}
	legacyByteStagePGAssertStored(test, peer, ctx, fixture, completed)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, first.next); after != prefix {
		test.Fatal("resuming rewrote previously copied parts or CAS objects")
	}
	terminal := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	for _, owner := range []*PostgresStore{storage, peer} {
		repeated, err := owner.stageLegacyByteBackfill(ctx, job, 1)
		if err != nil || repeated != completed {
			test.Fatal("terminal staging is not idempotent", repeated, err)
		}
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != terminal {
		test.Fatal("terminal staging updated checkpoint, part, or object xmin")
	}
	if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
		for _, ordinal := range []int{0, 7, 8, completed.blocks - 1, 0} {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			body, err := peer.readLegacyByteStageBlock(read, fixture.repo, fixture.hash, completed, ordinal, descriptor)
			if err != nil {
				return err
			}
			if len(body) > domain.ConversationManifestChunkBytes || !bytes.Equal(body, want) {
				test.Fatal("private read changed the selected canonical stream range", ordinal)
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	foreign, _ := docRangeJobPGRepo(test, storage, ctx)
	wrongPlan, wrongEpoch := completed, completed
	wrongPlan.plan = domain.HashContent([]byte("wrong private stage plan"))
	wrongEpoch.sourceID = "00000000-0000-0000-0000-000000000001"
	_, tail := legacyByteStagePGBlock(fixture, completed.blocks-1)
	for _, rejected := range []struct {
		name       string
		repo       domain.ContentHash
		stage      legacyByteStage
		descriptor domain.ConversationManifestChunk
	}{
		{"foreign ownership", foreign, completed, descriptor},
		{"wrong plan", fixture.repo, wrongPlan, descriptor},
		{"wrong epoch", fixture.repo, wrongEpoch, descriptor},
		{"wrong membership", fixture.repo, completed, tail},
	} {
		err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
			body, err := peer.readLegacyByteStageBlock(read, rejected.repo, fixture.hash, rejected.stage, 0, rejected.descriptor)
			if len(body) != 0 {
				test.Fatal("rejected private read exposed content", rejected.name)
			}
			return err
		})
		if err == nil {
			test.Fatal("invalid private read was accepted", rejected.name)
		}
	}
	if body, err := peer.readLegacyByteStageBlock(ctx, fixture.repo, fixture.hash, completed, 0, descriptor); !errors.Is(err, domain.ErrConflict) || len(body) != 0 {
		test.Fatal("private reader accepted a non-snapshot context", err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != before {
		test.Fatal("private staging or reads changed source bytes, ownership, snapshot, job, or published v3 generation")
	}
	for _, owner := range []*PostgresStore{storage, peer} {
		if ready, err := owner.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
			test.Fatal("private staging advertised public byte readiness", ready, err)
		}
	}
}

func TestLegacyByteStagePGNestedManifestRejectedByStageAndWorker(test *testing.T) {
	for _, format := range []string{domain.ChunkFormatV1, domain.ChunkFormatV2} {
		test.Run(format, func(test *testing.T) {
			defer func() {
				if recovered := recover(); recovered != nil {
					test.Fatalf("nested compressed manifest panicked instead of rejecting integrity: %v", recovered)
				}
			}()
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 0)
			plan, chunked := domain.PlanDocChunks(fixture.raw)
			if format == domain.ChunkFormatV1 {
				plan, chunked = domain.PlanDocChunksV1(fixture.raw)
			}
			if !chunked || plan.Manifest.Format != format {
				test.Fatal("fixture is not a valid requested legacy manifest", format)
			}
			manifest, err := json.Marshal(plan.Manifest)
			if err != nil {
				test.Fatal(err)
			}
			if parsed, ok := domain.ParseDocChunkManifest(manifest); !ok || parsed.Format != format {
				test.Fatal("nested fixture does not contain a recognized chunk manifest")
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, docCompress(docCompress(manifest))); err != nil {
				test.Fatal(err)
			}
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			before := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			stage, err := storage.stageLegacyByteBackfill(ctx, job, 1)
			if !errors.Is(err, domain.ErrIntegrity) || stage != (legacyByteStage{}) {
				test.Fatal("private stage decoded a nested compressed manifest", stage, err)
			}
			if err := storage.BuildDocRangeJob(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("actual range worker decoded a nested compressed manifest", err)
			}
			if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
				test.Fatal("nested compressed source created a stage header, object, part, or checkpoint")
			}
			if counts := generationCountsPG(test, storage, ctx, fixture.hash); counts != [2]int{} {
				test.Fatal("nested compressed source published a range generation", counts)
			}
			if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != before {
				test.Fatal("failed stage or worker rewrote nested source, ownership, snapshot, or running job")
			}
			if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
				test.Fatal("rejected nested source became publicly byte-ready", ready, err)
			}
		})
	}
}
