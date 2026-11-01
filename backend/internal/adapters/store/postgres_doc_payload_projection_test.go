//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

func docPayloadProjectionPGSource(test *testing.T, storage *PostgresStore, ctx context.Context) (legacyByteStagePGFixture, domain.VerifiedSessionDoc) {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	object := make(map[string]any, 8194)
	array := make([]any, 8192)
	for ordinal := range array {
		object[fmt.Sprintf("entry-%05d", ordinal)] = map[string]any{
			"count":  float64(ordinal),
			"label":  "original <한🙂> \\\"",
			"values": []any{nil, ordinal%2 == 0},
		}
		array[ordinal] = []any{float64(ordinal), "original <한🙂>", ordinal%2 == 0, nil}
	}
	object["create_time"] = "payload-not-a-number"
	object["provider_metadata"] = map[string]any{"create_time": "payload-only"}
	for _, payload := range []any{object, array} {
		raw, err := json.Marshal(payload)
		if err != nil || len(raw) <= 64<<10 || len(raw) >= 2<<20 {
			test.Fatal("fixture needs bounded large generic containers, not a giant string", len(raw), err)
		}
	}
	created := json.Number("1787683260.125")
	metadata := &domain.ProviderMetadata{CreateTime: &created, TurnID: "payload-turn"}
	document := domain.CIRDocument{
		Envelope: domain.CIREnvelope{CIRVersion: "2", SourceProvider: domain.ProviderCodex,
			SourceModel: "synthetic", SessionOriginID: string(repo), Fidelity: domain.FidelityFull},
		Events: []domain.CIREvent{
			{Kind: domain.EventMessage, Seq: 2, Role: domain.RoleUser,
				Blocks: []domain.ContentBlock{{Type: "text", Text: test.Name() + ":" + string(repo)}}},
			{Kind: domain.EventToolCall, Seq: 4, CallID: "payload-call", ToolName: "synthetic",
				ProviderMetadata: metadata, Input: object},
			{Kind: domain.EventToolResult, Seq: 7, CallID: "payload-call", ProviderMetadata: metadata, Output: object},
			{Kind: domain.EventToolResult, Seq: 9, CallID: "payload-array", ProviderMetadata: metadata, Output: array},
			{Kind: domain.EventMessage, Seq: 12, Role: domain.RoleAssistant,
				Blocks: []domain.ContentBlock{{Type: "text", Text: "original completed answer"}}},
		},
	}
	proof := legacyMonolithicVerifierProof(test, document)
	raw := proof.Bytes()
	if len(raw) >= 4<<20 || len(raw) > maxLegacyReadBytes {
		test.Fatal("payload fixture exceeds one unchanged bounded copy batch or decode cap", len(raw))
	}
	var parts struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil || len(parts.Events) != len(document.Events) {
		test.Fatal("canonical fixture lost its event bodies", err)
	}
	for _, ordinal := range []int{1, 2, 3} {
		if len(parts.Events[ordinal]) <= 64<<10 || !bytes.Contains(parts.Events[ordinal], []byte(`"create_time":1787683260.125`)) {
			test.Fatal("tool event does not exercise generic payload projection beside ordinary numeric metadata", ordinal)
		}
	}
	events := make([][]byte, len(parts.Events))
	for ordinal, event := range parts.Events {
		events[ordinal] = event
	}
	fixture := legacyByteStagePGFixture{repo: repo, hash: proof.Hash(), raw: raw,
		envelope: parts.Envelope, stream: bytes.Join(events, []byte(","))}
	if len(fixture.stream) <= domain.ConversationManifestChunkBytes ||
		!bytes.Equal(domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{fixture.stream}), raw) {
		test.Fatal("fixture must preserve a canonical monolithic source spanning byte blocks")
	}
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
	return fixture, proof
}

func docPayloadProjectionPGAssertMetadata(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, proof domain.VerifiedSessionDoc, work *preparedLegacyByteWork) domain.DocRangeGeneration {
	test.Helper()
	want, objects, err := domain.BuildLegacyDerivedDocByteRangeGeneration(ctx, proof)
	if err != nil {
		test.Fatal(err)
	}
	prepared, err := work.metadata.Descriptor()
	if err != nil || !reflect.DeepEqual(prepared, want) || work.metadata.ObjectCount() != len(objects) {
		test.Fatal("prepared exact-read metadata differs from the original typed proof", err)
	}
	expected := make(map[domain.ContentHash][]byte, len(objects))
	for ordinal, object := range objects {
		expected[object.Hash] = object.Bytes
		actual, err := work.metadata.Object(ctx, ordinal, work.objects.Read)
		if err != nil || actual.Hash != object.Hash || !bytes.Equal(actual.Bytes, object.Bytes) {
			test.Fatal("prepared metadata object changed original event coordinates or hashes", ordinal, err)
		}
	}
	wantDescriptor, err := want.Encode()
	if err != nil {
		test.Fatal(err)
	}
	var descriptor []byte
	var generationHash domain.ContentHash
	if err := storage.pool.QueryRow(ctx, `SELECT generation_hash,descriptor FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).
		Scan(&generationHash, &descriptor); err != nil {
		test.Fatal(err)
	}
	if generationHash != domain.HashContent(wantDescriptor) || !bytes.Equal(descriptor, wantDescriptor) ||
		generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{1, len(expected)} {
		test.Fatal("publication changed the exact descriptor or metadata object set")
	}
	rows, err := storage.pool.Query(ctx, `SELECT hash,bytes FROM doc_range_objects_v2 WHERE doc_hash=$1`, fixture.hash)
	if err != nil {
		test.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var hash domain.ContentHash
		var body []byte
		if err := rows.Scan(&hash, &body); err != nil {
			test.Fatal(err)
		}
		if want, ok := expected[hash]; !ok || !bytes.Equal(body, want) || domain.HashContent(body) != hash {
			test.Fatal("publication stored projected or unexpected metadata", hash)
		}
		delete(expected, hash)
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	if len(expected) != 0 {
		test.Fatal("publication omitted original-proof metadata objects", len(expected))
	}
	return want
}

func docPayloadProjectionPGReadRange(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, reader outbound.DocRangeGenerationRead, offset, length int) []byte {
	test.Helper()
	if offset < 0 || length <= 0 || offset > len(fixture.stream)-length {
		test.Fatal("invalid selected canonical stream range", offset, length)
	}
	body := make([]byte, 0, length)
	for current := offset; current < offset+length; {
		ordinal := current / domain.ConversationManifestChunkBytes
		descriptor, err := reader.Chunk(ctx, ordinal)
		if err != nil {
			test.Fatal(err)
		}
		want, expected := legacyByteStagePGBlock(fixture, ordinal)
		block, err := storage.ReadLegacyDerivedByteChunk(ctx, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
		if err != nil || descriptor != expected || !bytes.Equal(block, want) {
			test.Fatal("selected byte block differs from original canonical bytes", ordinal, err)
		}
		start := current % domain.ConversationManifestChunkBytes
		count := min(len(block)-start, offset+length-current)
		if count <= 0 {
			test.Fatal("selected byte block cannot advance its requested range")
		}
		body = append(body, block[start:start+count]...)
		current += count
	}
	if !bytes.Equal(body, fixture.stream[offset:offset+length]) {
		test.Fatal("selected byte range exposes projected payload markers")
	}
	return body
}

func docPayloadProjectionPGAssertReads(test *testing.T, storage *PostgresStore, ctx context.Context, fixture legacyByteStagePGFixture, proof domain.VerifiedSessionDoc, expected domain.DocRangeGeneration) {
	test.Helper()
	_, objects, err := domain.BuildLegacyDerivedDocByteRangeGeneration(ctx, proof)
	if err != nil {
		test.Fatal(err)
	}
	metadata := make(map[domain.ContentHash][]byte, len(objects))
	for _, object := range objects {
		metadata[object.Hash] = object.Bytes
	}
	foreign, _ := docRangeJobPGRepo(test, storage, ctx)
	if err := storage.WithinReadSnapshot(ctx, func(read context.Context) error {
		if _, err := storage.OpenDocRangeGeneration(read, foreign, fixture.hash); !errors.Is(err, domain.ErrNotFound) {
			test.Fatal("payload publication granted foreign generation access", err)
		}
		reader, err := storage.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		generation := reader.Descriptor()
		if !reflect.DeepEqual(generation, expected) || generation.Version != 5 || generation.Identity() != domain.DocumentIdentityLegacy ||
			generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.EventCount != 5 || reader.Binding().Validate() != nil {
			test.Fatal("published reader changed legacy identity or exact-read metadata", generation)
		}
		var stream []byte
		for ordinal := 0; ordinal < generation.ChunkCount(); ordinal++ {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			selected, err := reader.Chunk(read, ordinal)
			if err != nil || selected != descriptor {
				test.Fatal("published chunk directory differs from the original stream", ordinal, err)
			}
			body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, selected)
			if err != nil || !bytes.Equal(body, want) {
				test.Fatal("published block changed original generic payload bytes", ordinal, err)
			}
			stream = append(stream, body...)
		}
		raw := domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{stream})
		if !bytes.Equal(raw, fixture.raw) || domain.HashContent(raw) != fixture.hash {
			test.Fatal("published blocks no longer reconstruct the original canonical source/hash")
		}
		for position := 0; position < generation.EventPages.Count; position++ {
			hash, err := domain.ResolveDocRangeLeaf(read, expected.EventPages, position, func(_ context.Context, hash domain.ContentHash) ([]byte, error) {
				body, ok := metadata[hash]
				if !ok {
					return nil, domain.ErrIntegrity
				}
				return body, nil
			})
			if err != nil {
				return err
			}
			want, err := expected.DecodePage(position, hash, metadata[hash])
			if err != nil {
				return err
			}
			pageHash, err := reader.PageHash(read, position)
			if err != nil || pageHash != hash {
				test.Fatal("selected event-page hash changed", position, err)
			}
			entries, err := reader.ReadPage(read, position)
			if err != nil || !reflect.DeepEqual(entries, want) {
				test.Fatal("selected event metadata differs from the original typed proof", position, err)
			}
			for _, entry := range entries {
				body := docPayloadProjectionPGReadRange(test, storage, read, fixture, reader, entry.Offset, entry.Length)
				if domain.HashContent(body) != entry.Hash {
					test.Fatal("selected event bytes do not match their original event hash", entry.Index)
				}
				var event domain.CIREvent
				if err := json.Unmarshal(body, &event); err != nil || event.Seq != entry.Seq || event.Kind != entry.Kind {
					test.Fatal("selected event lost its typed envelope", entry.Index, err)
				}
				if entry.Index >= 1 && entry.Index <= 3 {
					if event.ProviderMetadata == nil || event.ProviderMetadata.CreateTime == nil ||
						event.ProviderMetadata.CreateTime.String() != "1787683260.125" {
						test.Fatal("selected payload event changed ordinary numeric provider metadata", entry.Index)
					}
					if entry.Index == 1 && len(event.Input) != 8194 {
						test.Fatal("selected input was replaced by its validation marker")
					}
					if entry.Index == 2 {
						output, ok := event.Output.(map[string]any)
						if !ok || len(output) != 8194 {
							test.Fatal("selected object output was replaced by its validation marker")
						}
					}
					if entry.Index == 3 {
						output, ok := event.Output.([]any)
						if !ok || len(output) != 8192 {
							test.Fatal("selected array output was replaced by its validation marker")
						}
					}
				}
			}
		}
		for _, selected := range [][2]int{{0, 31}, {domain.ConversationManifestChunkBytes - 7, 19}, {len(fixture.stream) - 31, 31}} {
			docPayloadProjectionPGReadRange(test, storage, read, fixture, reader, selected[0], selected[1])
		}
		_, descriptor := legacyByteStagePGBlock(fixture, 0)
		if body, err := storage.ReadLegacyDerivedByteChunk(read, foreign, fixture.hash, reader.Binding(), 0, descriptor); !errors.Is(err, domain.ErrNotFound) || len(body) != 0 {
			test.Fatal("foreign byte selection returned source content", len(body), err)
		}
		binding := reader.Binding()
		binding.SourceVersion++
		if body, err := storage.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, binding, 0, descriptor); err == nil || len(body) != 0 {
			test.Fatal("stale source binding returned selected payload bytes", len(body), err)
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	stored, err := storage.readOwnedDocObject(ctx, fixture.repo, "doc", fixture.hash)
	if err != nil || !bytes.Equal(stored, fixture.raw) || domain.HashContent(stored) != fixture.hash {
		test.Fatal("prepared publication rewrote or repacked the canonical monolithic source", err)
	}
	restored, err := storage.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
	if err != nil || !restored.Valid() || restored.DocumentRef() != proof.DocumentRef() || !bytes.Equal(restored.Bytes(), fixture.raw) {
		test.Fatal("full legacy read no longer retains original bytes/hash", err)
	}
}

func TestDocPayloadProjectionPGPreparedRoundTripAndReprepare(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture, proof := docPayloadProjectionPGSource(test, storage, ctx)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	stage := work.session.stage
	if stage.blocks < 2 || stage.blocks > domain.DocLegacyByteBackfillBatchBlocks || stage.sourceID == "" {
		test.Fatal("generic payload fixture must fit the unchanged bounded step and retain its source incarnation", stage)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("preparation persisted a private stage")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("preparation changed the owned source, job lease, or public metadata")
	}
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 0)
	stage.next, stage.complete = stage.blocks, true
	stage.incarnation = work.session.stage.incarnation
	if stage.incarnation == "" {
		test.Fatal("completed work did not retain its persisted stage incarnation")
	}
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	private = legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	resumed := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, resumed)
	if resumed.session.stage.incarnation != stage.incarnation || resumed.session.stage.sourceID != stage.sourceID || resumed.session.stage.sourceVersion != stage.sourceVersion ||
		resumed.session.stage.plan != stage.plan || resumed.session.stage.blocks != stage.blocks {
		test.Fatal("reprepare changed original source incarnation, version, or backfill plan")
	}
	docRangeWorkPGFinish(test, resumed, ctx, job, len(fixture.stream), 0)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("reprepare or terminal step rewrote acknowledged private bytes")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("private copy or reprepare advertised incomplete public metadata")
	}
	if err := resumed.Publish(ctx, job); err != nil {
		test.Fatal(err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	generation := docPayloadProjectionPGAssertMetadata(test, storage, ctx, fixture, proof, resumed)
	docPayloadProjectionPGAssertReads(test, storage, ctx, fixture, proof, generation)
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("payload preparation, publication, or selected reads changed archival bytes, ownership, incarnation, or snapshots")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("publication changed complete private byte progress")
	}
	docRangeWorkPGAssertClosed(test, resumed, ctx, job)
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("payload roundtrip or reprepare leaked local spools", len(files), err)
	}
}

type docPayloadProjectionCancelKeyPG struct{}
type docPayloadProjectionInsertKeyPG struct{}

type docPayloadProjectionCancelPG struct {
	cancel    context.CancelFunc
	triggered bool
}

type docPayloadProjectionTracerPG struct{}

func (docPayloadProjectionTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if observed, ok := ctx.Value(docPayloadProjectionCancelKeyPG{}).(*docPayloadProjectionCancelPG); ok &&
		strings.HasPrefix(query.SQL, "INSERT INTO doc_range_generations_v2(") {
		return context.WithValue(ctx, docPayloadProjectionInsertKeyPG{}, observed)
	}
	return ctx
}

func (docPayloadProjectionTracerPG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	if observed, ok := ctx.Value(docPayloadProjectionInsertKeyPG{}).(*docPayloadProjectionCancelPG); ok &&
		!observed.triggered && query.Err == nil && query.CommandTag.RowsAffected() == 1 {
		observed.triggered = true
		observed.cancel()
	}
}

func TestDocPayloadProjectionPGCanceledPublicationRepreparesWithoutPartialMetadata(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	fixture, proof := docPayloadProjectionPGSource(test, storage, ctx)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	config := storage.pool.Config()
	config.ConnConfig.Tracer = docPayloadProjectionTracerPG{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	peer := &PostgresStore{pool: pool}
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	work := docRangeWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, work)
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 0)
	stage := work.session.stage
	stage.next, stage.complete = stage.blocks, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	publication, cancel := context.WithCancel(ctx)
	defer cancel()
	observed := &docPayloadProjectionCancelPG{cancel: cancel}
	err = work.Publish(context.WithValue(publication, docPayloadProjectionCancelKeyPG{}, observed), job)
	if !observed.triggered || !errors.Is(err, context.Canceled) || !errors.Is(publication.Err(), context.Canceled) {
		test.Fatal("publication did not cancel after its actual generation insert", observed.triggered, err, publication.Err())
	}
	if counts := generationCountsPG(test, storage, ctx, fixture.hash); counts != [2]int{} {
		test.Fatal("canceled publication committed partial exact-read metadata", counts)
	}
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("canceled publication advertised byte readiness", ready, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("actual cancellation changed source, ownership, lease, completion, or public generation state")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("actual cancellation discarded or rewrote complete private byte progress")
	}
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	resumed := docRangeWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, resumed)
	if resumed.session.stage.sourceID != stage.sourceID || resumed.session.stage.sourceVersion != stage.sourceVersion ||
		resumed.session.stage.plan != stage.plan || resumed.session.stage.blocks != stage.blocks {
		test.Fatal("post-cancellation reprepare changed unchanged source or byte-plan identity")
	}
	docRangeWorkPGFinish(test, resumed, ctx, job, len(fixture.stream), 0)
	if err := resumed.Publish(ctx, job); err != nil {
		test.Fatal("uncanceled reprepare could not publish original generic payload bytes", err)
	}
	legacyDerivedPublishedPG(test, peer, ctx, fixture)
	generation := docPayloadProjectionPGAssertMetadata(test, peer, ctx, fixture, proof, resumed)
	docPayloadProjectionPGAssertReads(test, peer, ctx, fixture, proof, generation)
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("cancellation or retry changed original source bytes/hash, ownership, incarnation, or snapshots")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("cancellation recovery rewrote the retained completed byte stage")
	}
	docRangeWorkPGAssertClosed(test, resumed, ctx, job)
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("canceled publication or reprepare leaked local spools", len(files), err)
	}
}
