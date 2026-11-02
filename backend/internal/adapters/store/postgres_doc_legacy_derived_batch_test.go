//go:build postgres

package store

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type legacyDerivedVerificationTraceKeyPG struct{}
type legacyDerivedVerificationBatchKeyPG struct{}

type legacyDerivedVerificationTracePG struct {
	batchWidths    []int
	batchSizes     []int
	queuedOrdinals []int
	queryResults   int
	pointQueries   int
	batchEnds      int
	failures       []error
}

type legacyDerivedVerificationTracerPG struct{}

var _ pgx.QueryTracer = legacyDerivedVerificationTracerPG{}
var _ pgx.BatchTracer = legacyDerivedVerificationTracerPG{}

func (legacyDerivedVerificationTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if trace, ok := ctx.Value(legacyDerivedVerificationTraceKeyPG{}).(*legacyDerivedVerificationTracePG); ok && query.SQL == legacyDerivedByteVerificationSQL {
		trace.pointQueries++
	}
	return ctx
}

func (legacyDerivedVerificationTracerPG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (legacyDerivedVerificationTracerPG) TraceBatchStart(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchStartData) context.Context {
	trace, ok := ctx.Value(legacyDerivedVerificationTraceKeyPG{}).(*legacyDerivedVerificationTracePG)
	if !ok {
		return ctx
	}
	width := 0
	for _, query := range batch.Batch.QueuedQueries {
		if query.SQL != legacyDerivedByteVerificationSQL {
			continue
		}
		width++
		ordinal := -1
		if len(query.Arguments) == 4 {
			if selected, ok := query.Arguments[1].(int); ok {
				ordinal = selected
			}
		}
		trace.queuedOrdinals = append(trace.queuedOrdinals, ordinal)
	}
	if width == 0 {
		return ctx
	}
	trace.batchWidths = append(trace.batchWidths, width)
	trace.batchSizes = append(trace.batchSizes, len(batch.Batch.QueuedQueries))
	return context.WithValue(ctx, legacyDerivedVerificationBatchKeyPG{}, trace)
}

func (legacyDerivedVerificationTracerPG) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, query pgx.TraceBatchQueryData) {
	trace, ok := ctx.Value(legacyDerivedVerificationBatchKeyPG{}).(*legacyDerivedVerificationTracePG)
	if !ok || query.SQL != legacyDerivedByteVerificationSQL {
		return
	}
	trace.queryResults++
	if query.Err != nil {
		trace.failures = append(trace.failures, query.Err)
	}
}

func (legacyDerivedVerificationTracerPG) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, batch pgx.TraceBatchEndData) {
	trace, ok := ctx.Value(legacyDerivedVerificationBatchKeyPG{}).(*legacyDerivedVerificationTracePG)
	if !ok {
		return
	}
	trace.batchEnds++
	if batch.Err != nil {
		trace.failures = append(trace.failures, batch.Err)
	}
}

func (trace *legacyDerivedVerificationTracePG) assertPublication(test *testing.T) {
	test.Helper()
	if !slices.Equal(trace.batchWidths, []int{8, 3}) || !slices.Equal(trace.batchSizes, trace.batchWidths) {
		test.Fatalf("final verification batch widths=%v total queued widths=%v, want exactly two batches of eight and three statements", trace.batchWidths, trace.batchSizes)
	}
	statements := 0
	for _, width := range trace.batchWidths {
		if width <= 0 || width > domain.DocLegacyByteBackfillBatchBlocks {
			test.Fatal("final verification exceeded the bounded protocol batch width", width)
		}
		statements += width
	}
	if statements != 11 || len(trace.queuedOrdinals) != 11 || trace.queryResults != 11 || trace.pointQueries != 0 || trace.batchEnds != 2 || len(trace.failures) != 0 {
		test.Fatalf("final verification statements=%d ordinals=%v results=%d point queries=%d batch ends=%d failures=%v", statements, trace.queuedOrdinals, trace.queryResults, trace.pointQueries, trace.batchEnds, trace.failures)
	}
	for ordinal, queued := range trace.queuedOrdinals {
		if queued != ordinal {
			test.Fatal("final verification did not queue exactly one statement for each block in order", ordinal, queued)
		}
	}
}

func TestLegacyDerivedBytePGPublicationVerificationBatches(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
	config := storage.pool.Config()
	config.ConnConfig.Tracer = legacyDerivedVerificationTracerPG{}
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
	if work.session.stage.blocks != 11 {
		test.Fatal("verification fixture must contain eleven byte blocks", work.session.stage)
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("first private step did not stop after eight blocks", complete, err)
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("second private step must finish byte copying but leave metadata pending", complete, err)
	}
	stage := work.session.stage
	stage.next, stage.complete = stage.blocks, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 2)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("metadata quanta rewrote the complete private byte stage")
	}
	trace := &legacyDerivedVerificationTracePG{}
	publication := context.WithValue(ctx, legacyDerivedVerificationTraceKeyPG{}, trace)
	if err := work.Publish(publication, job); err != nil {
		test.Fatal("publish fully staged derived bytes", err)
	}
	trace.assertPublication(test)
	legacyDerivedPublishedPG(test, peer, ctx, fixture)
	if err := peer.WithinReadSnapshot(ctx, func(read context.Context) error {
		reader, err := peer.OpenDocRangeGeneration(read, fixture.repo, fixture.hash)
		if err != nil {
			return err
		}
		generation := reader.Descriptor()
		if generation.Version != 5 || generation.Identity() != domain.DocumentIdentityLegacy ||
			generation.ByteStorage() != domain.DocRangeByteStorageDerived || generation.ChunkCount() != 11 {
			test.Fatal("batched verification changed the published generation", generation)
		}
		var stream []byte
		for ordinal := 0; ordinal < generation.ChunkCount(); ordinal++ {
			want, descriptor := legacyByteStagePGBlock(fixture, ordinal)
			body, err := peer.ReadLegacyDerivedByteChunk(read, fixture.repo, fixture.hash, reader.Binding(), ordinal, descriptor)
			if err != nil {
				return err
			}
			if !bytes.Equal(body, want) {
				test.Fatal("published block differs from its canonical source bytes", ordinal)
			}
			stream = append(stream, body...)
		}
		if !bytes.Equal(domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{stream}), fixture.raw) {
			test.Fatal("published blocks do not reconstruct the original canonical document")
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("batched publication rewrote the complete private stage")
	}
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("batched publication changed the raw source, incarnation, ownership, or snapshot")
	}
}

func TestLegacyDerivedBytePGFinalDataSpoolCorruptionRetainsCompleteStage(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	storage.ConfigureLegacyDerivedByteRangePublication(true)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	original := legacyDerivedOriginalImagePG(test, storage, ctx, fixture)
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
	if counts := generationCountsPG(test, storage, ctx, fixture.hash); counts[0] != 1 || counts[1] == 0 {
		test.Fatal("corruption fixture must retain existing public generation and metadata objects", counts)
	}
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	work := docRangeWorkPGPrepare(test, storage, ctx, job)
	defer docRangeWorkPGClose(test, work)
	if work.session.stage.blocks != 11 {
		test.Fatal("corruption fixture must contain eleven byte blocks", work.session.stage)
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("first private step did not stop after eight blocks", complete, err)
	}
	if complete, err := work.Step(ctx, job); err != nil || complete != outbound.DocRangeWorkPending {
		test.Fatal("second private step must finish byte copying but leave metadata pending", complete, err)
	}
	stage := work.session.stage
	stage.next, stage.complete = stage.blocks, true
	legacyByteStagePGAssertStored(test, storage, ctx, fixture, stage)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	docRangeWorkPGFinish(test, work, ctx, job, len(fixture.stream), 2)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("metadata quanta rewrote the complete private byte stage")
	}
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	lastOrdinal := stage.blocks - 1
	repair := docRangeWorkPGCorruptByte(test, work.data.file, int64(lastOrdinal)*domain.ConversationManifestChunkBytes)
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrIntegrity) {
		test.Fatal("final publication accepted corruption in the last local byte block", lastOrdinal, err)
	}
	if after := legacyByteStagePGPublicImage(test, storage, ctx, fixture); after != public {
		test.Fatal("failed final verification committed public metadata replacement, archival changes, or job completion")
	}
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("failed final verification changed the complete private checkpoint, parts, or objects")
	}
	if ready, err := storage.HasDocByteRangeIndex(ctx, fixture.repo, fixture.hash); err != nil || ready {
		test.Fatal("failed final verification advertised public byte readiness", ready, err)
	}
	if pending := docRangeJobPGGet(test, storage, ctx, fixture.repo, fixture.hash); pending.State != "running" || pending.Version != job.Version {
		test.Fatal("failed final verification completed or changed the claimed job", pending)
	}
	repair()
	if err := work.Publish(ctx, job); err != nil {
		test.Fatal("repaired data spool could not publish the same complete private stage", err)
	}
	legacyDerivedPublishedPG(test, storage, ctx, fixture)
	if after := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1); after != private {
		test.Fatal("successful publication retry lost or rewrote the retained private stage")
	}
	if after := legacyDerivedOriginalImagePG(test, storage, ctx, fixture); after != original {
		test.Fatal("failed final verification or repaired publication changed the raw source or archival state")
	}
}
