//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type normalizationParserRepeatPGTrace struct {
	docCaptureWorkPGTrace
	input        map[domain.ContentHash]struct{}
	objects      atomic.Int64
	inputUploads atomic.Int64
	inputReads   atomic.Int64
}

func (trace *normalizationParserRepeatPGTrace) TraceQueryStart(ctx context.Context, connection *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	ctx = trace.docCaptureWorkPGTrace.TraceQueryStart(ctx, connection, query)
	statement := strings.ToLower(strings.TrimSpace(query.SQL))
	if !strings.Contains(statement, "doc_legacy_byte_stage_checkpoint_objects_v1") {
		return ctx
	}
	if strings.HasPrefix(statement, "insert into doc_legacy_byte_stage_checkpoint_objects_v1") {
		trace.objects.Add(1)
	}
	if len(query.Args) < 3 {
		return ctx
	}
	var hash domain.ContentHash
	switch value := query.Args[2].(type) {
	case domain.ContentHash:
		hash = value
	case string:
		hash = domain.ContentHash(value)
	}
	if _, found := trace.input[hash]; !found {
		return ctx
	}
	if len(query.Args) > 3 {
		if body, ok := query.Args[3].([]byte); ok && len(body) != 0 {
			trace.inputUploads.Add(1)
		}
	}
	if strings.HasPrefix(statement, "select substring(bytes") {
		trace.inputReads.Add(1)
	}
	return ctx
}

func normalizationParserRepeatPGWatch(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, input legacyNormalizationPrefix, stored []byte) *normalizationParserRepeatPGTrace {
	test.Helper()
	if input.ChunkBytes != domain.LegacyNormalizationPartBytes || input.Tree.DataBytes != len(stored) || work.checkpoint.busy() {
		test.Fatal("repeat trace requires an acknowledged complete input prefix", input)
	}
	trace := &normalizationParserRepeatPGTrace{input: make(map[domain.ContentHash]struct{})}
	for offset := 0; offset < len(stored); offset += input.ChunkBytes {
		trace.input[domain.HashContent(stored[offset:min(offset+input.ChunkBytes, len(stored))])] = struct{}{}
	}
	config := work.store.pool.Config()
	config.ConnConfig.Tracer = trace
	config.MaxConns, config.MinConns = 1, 0
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	peer := &PostgresStore{pool: pool}
	peer.ConfigureLegacyByteRangePublication(true)
	peer.ConfigureLegacyDerivedByteRangePublication(true)
	work.store, work.checkpoint.store = peer, peer
	return trace
}

func normalizationParserRepeatPGPosition(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob, state legacyByteCheckpoint, reference legacyNormalizationBufferRef) (int, uint8) {
	test.Helper()
	body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, reference.Hash, reference.Bytes)
	if err != nil {
		test.Fatal("could not read the persisted decoder control", err)
	}
	var control struct {
		Kind        string `json:"kind"`
		Position    *int   `json:"position"`
		Phase       *uint8 `json:"phase"`
		SourceBytes int    `json:"source_bytes"`
	}
	if err := json.Unmarshal(body, &control); err != nil || control.Kind != "legacy-normalization-decoder" ||
		control.Position == nil || control.Phase == nil || *control.Position < 0 || *control.Position > control.SourceBytes {
		test.Fatal("parser reference does not identify a decoder resume position", control, err)
	}
	return *control.Position, *control.Phase
}

func normalizationParserRepeatPGPendingSecond(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob, first legacyByteCheckpoint, parser legacyNormalizationParserCheckpoint) {
	test.Helper()
	normalizationCheckpointPGCut(test, work, ctx, job, func(checkpoint *legacyNormalizationCheckpointWork) bool {
		if checkpoint.state != first || checkpoint.root.Parser == nil || *checkpoint.root.Parser != parser ||
			work.normalization == nil || work.normalization.canonical != nil || work.canonicalProof.Valid() || work.decoding == nil {
			test.Fatal("repeat capture replaced its acknowledged root or discarded live parser authority before ACK")
		}
		if checkpoint.capture == nil {
			consumed, err := work.normalization.inputConsumed()
			if err != nil || consumed {
				test.Fatal("repeat capture waited until its input was consumed", consumed, err)
			}
			return false
		}
		if checkpoint.pending.hash == "" {
			return false
		}
		if checkpoint.next.Parser == nil || !checkpoint.next.State.Parsing() || checkpoint.next.State.Version != 2 ||
			checkpoint.next.State.Count != 0 || checkpoint.next.Parser.Input != parser.Input ||
			checkpoint.next.Parser.Ref == parser.Ref || checkpoint.pending.hash == first.hash {
			test.Fatal("second candidate did not preserve the decoded input and advance only parser state", checkpoint.next)
		}
		return true
	})
	actual, found, err := work.store.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || actual != first {
		test.Fatal("a staged second root replaced the first root before commit acknowledgement", actual, found, err)
	}
	if _, err := work.normalization.preparation.Step(ctx); !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
		test.Fatal("the second candidate did not freeze semantic processing until ACK", err)
	}
}

func normalizationParserRepeatPGAcknowledged(test *testing.T, storage *PostgresStore, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob, first legacyByteCheckpoint, parser legacyNormalizationParserCheckpoint, position int, phase uint8) {
	test.Helper()
	if work.checkpoint == nil || work.checkpoint.busy() || work.checkpoint.root.Parser == nil ||
		!work.checkpoint.root.State.Parsing() || work.checkpoint.root.State.Version != 2 || work.checkpoint.root.State.Count != 0 ||
		work.checkpoint.state.sequence <= first.sequence || work.checkpoint.state.hash == first.hash ||
		work.checkpoint.root.Parser.Ref == parser.Ref || work.checkpoint.root.Parser.Input != parser.Input ||
		work.decoding == nil || work.normalization == nil || work.normalization.prepared ||
		work.normalization.canonical != nil || work.canonicalProof.Valid() || work.work != nil {
		test.Fatal("second ACK did not retain a distinct live decoder root and the exact original input prefix")
	}
	state, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || state != work.checkpoint.state || state.stage.plan != "" || state.stage.complete {
		test.Fatal("second ACK does not match the durable processing root", state, found, err)
	}
	body, err := storage.readLegacyNormalizationCheckpointObject(ctx, job, state, state.hash, state.bytes)
	if err != nil {
		test.Fatal(err)
	}
	root, err := decodeLegacyNormalizationCheckpoint(body, state)
	if err != nil || root.Parser == nil || *root.Parser != *work.checkpoint.root.Parser || !root.State.Parsing() {
		test.Fatal("durable second root changed its parser or input reference", err)
	}
	nextPosition, nextPhase := normalizationParserRepeatPGPosition(test, storage, ctx, job, state, root.Parser.Ref)
	if nextPosition <= position && nextPhase == phase {
		test.Fatal("second root changed its hash without advancing its persisted decoder position or logical phase", position, nextPosition, phase, nextPhase)
	}
	consumed, err := work.normalization.inputConsumed()
	if err != nil || consumed {
		test.Fatal("second ACK consumed or released the live parser input", consumed, err)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("second parser checkpoint acquired premature publication authority", err)
	}
}

func TestNormalizationCheckpointPGParserRepeatedDecodeColdRestart(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	directory := rangeSpoolTempDir(test)
	fixture, work, job, stored := normalizationParserRestartPGStart(test, storage, ctx, false)
	defer work.Close()
	preserved := legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture)
	normalizationParserRestartPGAwaitRoot(test, work, ctx, job, 0)
	first, parser := work.checkpoint.state, *work.checkpoint.root.Parser
	if first.sequence != 1 {
		test.Fatal("repeat fixture missed the first acknowledged parser root", first)
	}
	position, phase := normalizationParserRepeatPGPosition(test, storage, ctx, job, first, parser.Ref)
	trace := normalizationParserRepeatPGWatch(test, work, ctx, parser.Input, stored)
	normalizationParserRepeatPGPendingSecond(test, work, ctx, job, first, parser)
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
		test.Fatal("real worker did not acknowledge its second parser root", progress, err)
	}
	normalizationParserRepeatPGAcknowledged(test, storage, work, ctx, job, first, parser, position, phase)
	if trace.objects.Load() == 0 || trace.bodyReads.Load() != 0 || trace.inputUploads.Load() != 0 || trace.inputReads.Load() != 0 {
		test.Fatal("second capture repeated decoded input payload IO or bypassed the SQL spy", trace.objects.Load(),
			trace.bodyReads.Load(), trace.inputUploads.Load(), trace.inputReads.Load())
	}
	normalizationCheckpointPGColdReclaim(test, storage, ctx, fixture, job, work)
	if work.checkpoint != nil || work.decoding != nil || work.normalization != nil || work.source != nil {
		test.Fatal("cold continuation retained a process-local parser or source")
	}
	if counts := generationCountsPG(test, storage, ctx, fixture.hash); counts[0] != 1 || counts[1] == 0 ||
		legacyByteRecoveryPGPreservedImage(test, storage, ctx, fixture) != preserved {
		test.Fatal("latest-root cold continuation duplicated publication or changed original context", counts)
	}
	if entries, err := os.ReadDir(directory); err != nil || len(entries) != 0 {
		test.Fatal("repeated capture or cold continuation leaked local spools", entries, err)
	}
}

func TestNormalizationCheckpointPGParserSecondRootCommitFailure(test *testing.T) {
	storage, ctx := rootProjectionIsolatedPG(test)
	rangeSpoolTempDir(test)
	fixture, work, job, _ := normalizationParserRestartPGStart(test, storage, ctx, false)
	defer work.Close()
	normalizationParserRestartPGAwaitRoot(test, work, ctx, job, 0)
	first, parser := work.checkpoint.state, *work.checkpoint.root.Parser
	position, phase := normalizationParserRepeatPGPosition(test, storage, ctx, job, first, parser.Ref)
	normalizationParserRepeatPGPendingSecond(test, work, ctx, job, first, parser)
	before := legacyNormalizationGCPGImage(test, storage, ctx, job)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	name := pgx.Identifier{domain.NewID("parser_repeat_ack_fence_")}.Sanitize()
	statement := fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
 BEGIN IF NEW.doc_hash='%s' AND NEW.checkpoint_sequence>OLD.checkpoint_sequence THEN
 UPDATE doc_range_jobs_v1 SET state='waiting',version=version+1,lease_until=NULL WHERE repo_id='%s' AND doc_hash=NEW.doc_hash;
 END IF; RETURN NEW; END; $$;
 CREATE TRIGGER %s AFTER UPDATE ON doc_legacy_byte_stages_v1 FOR EACH ROW EXECUTE FUNCTION %s()`, name, fixture.hash, fixture.repo, name, name)
	if _, err := storage.pool.Exec(ctx, statement); err != nil {
		test.Fatal(err)
	}
	progress, failure := work.Step(ctx, job)
	if _, err := storage.pool.Exec(ctx, `DROP TRIGGER `+name+` ON doc_legacy_byte_stages_v1; DROP FUNCTION `+name+`() `); err != nil {
		test.Fatal(err)
	}
	if progress != 0 || !errors.Is(failure, domain.ErrConflict) || work.checkpoint.state != first ||
		work.checkpoint.root.Parser == nil || *work.checkpoint.root.Parser != parser || work.checkpoint.capture == nil ||
		legacyNormalizationGCPGImage(test, storage, ctx, job) != before || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public ||
		generationCountsPG(test, storage, ctx, fixture.hash) != [2]int{} {
		test.Fatal("failed second-root transaction replaced the first ACK or changed references, accounting or publication", progress, failure)
	}
	actual, found, err := storage.loadLegacyNormalizationCheckpoint(ctx, job)
	if err != nil || !found || actual != first {
		test.Fatal("failed second commit lost the durable previous root", actual, found, err)
	}
	if _, err := work.normalization.preparation.Step(ctx); !errors.Is(err, domain.ErrLegacyNormalizationBusy) {
		test.Fatal("failed second commit released the unacknowledged parser freeze", err)
	}
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
		test.Fatal("retry did not acknowledge the same second-root candidate", progress, err)
	}
	normalizationParserRepeatPGAcknowledged(test, storage, work, ctx, job, first, parser, position, phase)
}
