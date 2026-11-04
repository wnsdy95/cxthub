//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const docRangeWorkEmissionBatchObjects = 128
const docRangeWorkEmissionInsertSQL = "INSERT INTO doc_range_objects_v2(doc_hash,hash,bytes) VALUES($1,$2,$3) ON CONFLICT DO NOTHING"

type docRangeWorkEmissionTraceKeyPG struct{}
type docRangeWorkEmissionBatchKeyPG struct{}

type docRangeWorkEmissionTracePG struct {
	batchWidths        []int
	batchSizes         []int
	statements         int
	insertedRows       int64
	batchEnds          int
	failures           []error
	afterFirstBatch    func()
	corruptionInjected bool
}

type docRangeWorkEmissionTracerPG struct{}

var _ pgx.QueryTracer = docRangeWorkEmissionTracerPG{}
var _ pgx.BatchTracer = docRangeWorkEmissionTracerPG{}

func (docRangeWorkEmissionTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (docRangeWorkEmissionTracerPG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (docRangeWorkEmissionTracerPG) TraceBatchStart(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchStartData) context.Context {
	trace, ok := ctx.Value(docRangeWorkEmissionTraceKeyPG{}).(*docRangeWorkEmissionTracePG)
	if !ok {
		return ctx
	}
	width := 0
	for _, query := range batch.Batch.QueuedQueries {
		if query.SQL == docRangeWorkEmissionInsertSQL {
			width++
		}
	}
	if width == 0 {
		return ctx
	}
	trace.batchWidths = append(trace.batchWidths, width)
	trace.batchSizes = append(trace.batchSizes, len(batch.Batch.QueuedQueries))
	return context.WithValue(ctx, docRangeWorkEmissionBatchKeyPG{}, trace)
}

func (docRangeWorkEmissionTracerPG) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, query pgx.TraceBatchQueryData) {
	trace, ok := ctx.Value(docRangeWorkEmissionBatchKeyPG{}).(*docRangeWorkEmissionTracePG)
	if !ok || query.SQL != docRangeWorkEmissionInsertSQL {
		return
	}
	trace.statements++
	trace.insertedRows += query.CommandTag.RowsAffected()
	if query.Err != nil {
		trace.failures = append(trace.failures, query.Err)
	}
}

func (docRangeWorkEmissionTracerPG) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchEndData) {
	trace, ok := ctx.Value(docRangeWorkEmissionBatchKeyPG{}).(*docRangeWorkEmissionTracePG)
	if !ok {
		return
	}
	trace.batchEnds++
	if batch.Err != nil {
		trace.failures = append(trace.failures, batch.Err)
	}
	if trace.batchEnds == 1 && len(trace.batchWidths) == 1 && trace.batchWidths[0] == docRangeWorkEmissionBatchObjects &&
		trace.batchSizes[0] == docRangeWorkEmissionBatchObjects && trace.statements == docRangeWorkEmissionBatchObjects &&
		trace.insertedRows == docRangeWorkEmissionBatchObjects && len(trace.failures) == 0 && trace.afterFirstBatch != nil {
		trace.corruptionInjected = true
		trace.afterFirstBatch()
	}
}

func docRangeWorkEmissionPGSource(test *testing.T, storage *PostgresStore, ctx context.Context, events int) (legacyByteStagePGFixture, domain.VerifiedSessionDoc) {
	test.Helper()
	repo, _ := docRangeJobPGRepo(test, storage, ctx)
	proof := readBlockDoc(test, string(repo), "", events)
	raw := proof.Bytes()
	if len(raw) > maxLegacyReadBytes {
		test.Fatal("many-page fixture exceeds the unchanged monolithic source cap", len(raw), maxLegacyReadBytes)
	}
	var canonical struct {
		Envelope json.RawMessage   `json:"envelope"`
		Events   []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal(raw, &canonical); err != nil {
		test.Fatal(err)
	}
	if len(canonical.Events) != events {
		test.Fatal("many-page fixture changed its event count", len(canonical.Events), events)
	}
	eventBodies := make([][]byte, len(canonical.Events))
	for ordinal, event := range canonical.Events {
		eventBodies[ordinal] = event
	}
	fixture := legacyByteStagePGFixture{repo: repo, hash: proof.Hash(), raw: raw,
		envelope: canonical.Envelope, stream: bytes.Join(eventBodies, []byte(","))}
	if !bytes.Equal(domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{fixture.stream}), raw) {
		test.Fatal("many-page fixture does not preserve its canonical source bytes")
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

func TestDocRangeWorkPGMetadataEmissionLaterBatchFailureRollsBackReplacementAndReprepares(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	directory := rangeSpoolTempDir(test)
	eventCount := (docRangeWorkEmissionBatchObjects+1)*domain.DocRangePageEvents + 1
	fixture, proof := docRangeWorkEmissionPGSource(test, storage, ctx, eventCount)
	if err := storage.WithinRepository(ctx, fixture.repo, func(bound context.Context) error {
		return storage.putLegacyRangeGeneration(bound, fixture.repo, proof)
	}); err != nil {
		test.Fatal(err)
	}
	oldCounts := generationCountsPG(test, storage, ctx, fixture.hash)
	if oldCounts[0] != 1 || oldCounts[1] <= docRangeWorkEmissionBatchObjects {
		test.Fatal("replacement fixture must retain an existing generation and more than one metadata batch", oldCounts)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	config := storage.pool.Config()
	config.ConnConfig.Tracer = docRangeWorkEmissionTracerPG{}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	defer pool.Close()
	peer := &PostgresStore{pool: pool}
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	work := docRangeWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, work)
	docRangeWorkPGResources(test, work, len(fixture.stream))
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 0)
	objectCount := work.metadata.ObjectCount()
	generation, err := work.metadata.Descriptor()
	if err != nil {
		test.Fatal(err)
	}
	if objectCount <= docRangeWorkEmissionBatchObjects || len(work.objects.offsets) != objectCount+1 ||
		generation.Version != 5 || generation.EventCount != int64(eventCount) || generation.EventPages.Count <= docRangeWorkEmissionBatchObjects {
		test.Fatal("fixture does not require multiple metadata batches and event-page directories", objectCount, generation)
	}
	descriptor, err := generation.Encode()
	if err != nil {
		test.Fatal(err)
	}
	stage := work.session.stage
	stage.next, stage.complete = stage.blocks, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	trace := &docRangeWorkEmissionTracePG{afterFirstBatch: func() {
		docRangeWorkPGCorruptByte(test, work.objects.file, work.objects.offsets[docRangeWorkEmissionBatchObjects])
	}}
	publication := context.WithValue(ctx, docRangeWorkEmissionTraceKeyPG{}, trace)
	if err := work.Publish(publication, job); !errors.Is(err, domain.ErrIntegrity) || !errors.Is(err, domain.ErrPreparedDocRangeObject) {
		test.Fatal("later metadata corruption did not reject replacement after the first SQL batch", err, trace)
	}
	if !trace.corruptionInjected || len(trace.batchWidths) != 1 || trace.batchWidths[0] != docRangeWorkEmissionBatchObjects ||
		len(trace.batchSizes) != 1 || trace.batchSizes[0] != docRangeWorkEmissionBatchObjects || trace.batchEnds != 1 ||
		trace.statements != docRangeWorkEmissionBatchObjects || trace.insertedRows != docRangeWorkEmissionBatchObjects || len(trace.failures) != 0 {
		test.Fatal("corruption was not injected after exactly one successfully executed 128-object metadata batch", trace)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("later metadata failure committed descriptor replacement, old-object deletion, inserted metadata, or job completion")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("later metadata failure changed the complete durable private stage")
	}
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("failed replacement advertised byte readiness", ready, err)
	}
	docRangeWorkPGAssertClosed(test, work, ctx, job)
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("discarding corrupt work left a local spool", len(files), err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("closing corrupt local work removed its complete durable private stage")
	}
	reprepared := docRangeWorkPGPrepare(test, peer, ctx, job)
	defer docRangeWorkPGClose(test, reprepared)
	docRangeWorkPGResources(test, reprepared, len(fixture.stream))
	if reprepared.session.stage.sourceID != stage.sourceID ||
		reprepared.session.stage.sourceVersion != stage.sourceVersion || reprepared.session.stage.plan != stage.plan || reprepared.session.stage.blocks != stage.blocks {
		test.Fatal("repreparation changed the unchanged source incarnation or plan")
	}
	docRangeWorkPGFinish(test, reprepared, ctx, job, len(fixture.stream), 0)
	if reprepared.metadata.ObjectCount() != objectCount {
		test.Fatal("repreparation changed metadata cardinality")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("repreparation or terminal step rewrote the retained private stage")
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("repreparation or terminal step changed the rolled-back public state")
	}
	expectedObjects := make(map[domain.ContentHash]bool)
	for ordinal := 0; ordinal < objectCount; ordinal++ {
		object, err := reprepared.metadata.Object(ctx, ordinal, reprepared.objects.Read)
		if err != nil {
			test.Fatal("repreparation did not regenerate valid metadata", ordinal, err)
		}
		expectedObjects[object.Hash] = true
	}
	if err := reprepared.Publish(ctx, job); err != nil {
		test.Fatal("fresh metadata could not publish the existing complete private stage", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	var publishedDescriptor []byte
	if err := storage.pool.QueryRow(ctx, `SELECT descriptor FROM doc_range_generations_v2 WHERE doc_hash=$1`, fixture.hash).Scan(&publishedDescriptor); err != nil {
		test.Fatal(err)
	}
	if !bytes.Equal(publishedDescriptor, descriptor) || generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{1, len(expectedObjects)} {
		test.Fatal("successful replacement did not publish the exact prepared descriptor and complete metadata set")
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
		if !expectedObjects[hash] || len(body) == 0 || len(body) > domain.MaxDocRangePageBytes || domain.HashContent(body) != hash {
			test.Fatal("successful replacement retained unexpected or corrupt metadata", hash)
		}
		delete(expectedObjects, hash)
	}
	if err := rows.Err(); err != nil {
		test.Fatal(err)
	}
	rows.Close()
	if len(expectedObjects) != 0 {
		test.Fatal("successful replacement omitted prepared metadata objects", len(expectedObjects))
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("successful replacement rewrote the complete durable private stage")
	}
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("failed replacement, reprepare, or retry changed source bytes, incarnation, ownership, or snapshots")
	}
	docRangeWorkPGAssertClosed(test, reprepared, ctx, job)
	if files, err := os.ReadDir(directory); err != nil || len(files) != 0 {
		test.Fatal("successful reprepare and retry left a local spool", len(files), err)
	}
}
