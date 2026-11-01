//go:build postgres

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type docCaptureWorkPGSpan struct{ from, bytes int }

type docCaptureCommitTraceKey struct{}

type docCaptureCommitHook struct{ run func() }

const docCaptureWorkPGPreparationStepLimit = 6*maxLegacyReadBytes/domain.ConversationManifestChunkBytes + 8

type docCaptureWorkPGTrace struct {
	bodyReads      atomic.Int64
	queries        atomic.Int64
	mu             sync.Mutex
	spans          []docCaptureWorkPGSpan
	commit         atomic.Pointer[docCaptureCommitHook]
	captureWitness atomic.Bool
}

func (trace *docCaptureWorkPGTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries.Add(1)
	if strings.HasPrefix(query.SQL, "SELECT source.doc_range_source_version,incarnation.incarnation::text,octet_length(source.bytes)") {
		trace.captureWitness.Store(true)
	}
	if strings.Contains(strings.ReplaceAll(query.SQL, "octet_length(source.bytes)", ""), "source.bytes") {
		trace.bodyReads.Add(1)
		if len(query.Args) == 4 {
			from, fromOK := query.Args[2].(int)
			length, lengthOK := query.Args[3].(int)
			if fromOK && lengthOK {
				trace.mu.Lock()
				trace.spans = append(trace.spans, docCaptureWorkPGSpan{from: from, bytes: length})
				trace.mu.Unlock()
			}
		}
	}
	if hook := trace.commit.Load(); hook != nil && strings.EqualFold(strings.TrimSpace(query.SQL), "commit") {
		return context.WithValue(ctx, docCaptureCommitTraceKey{}, hook)
	}
	return ctx
}

func docCaptureWorkPGPrepare(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocRangeIndexJob) *preparedLegacyCaptureWork {
	test.Helper()
	prepared, err := storage.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		test.Fatal("prepare capture", err)
	}
	test.Cleanup(func() {
		if err := prepared.Close(); err != nil {
			test.Error(err)
		}
	})
	capture, ok := prepared.(*preparedLegacyCaptureWork)
	if !ok || capture.work != nil || capture.source == nil {
		test.Fatalf("public preparation skipped provisional capture: %T", prepared)
	}
	return capture
}

func docCaptureWorkPGCopy(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob) {
	test.Helper()
	limit := (work.witness.bytes + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
	for ordinal := 0; ordinal < limit; ordinal++ {
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending ||
			work.work != nil || work.source == nil || work.source.size != min((ordinal+1)*domain.ConversationManifestChunkBytes, work.witness.bytes) || len(work.parts) != ordinal+1 {
			test.Fatal("capture did not copy exactly one provisional stored span", ordinal, progress, err)
		}
	}
	if work.source.size != work.witness.bytes || work.work != nil {
		test.Fatal("capture did not reach its exact stored extent")
	}
}

func docCaptureWorkPGDecode(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob) {
	test.Helper()
	if work.source == nil || work.source.size != work.witness.bytes || work.work != nil || work.decoding != nil {
		test.Fatal("decode preparation did not start after the exact stored capture")
	}
	limit := maxLegacyReadBytes/domain.ConversationManifestChunkBytes + 2
	for step := 0; step < limit; step++ {
		if work.decoding != nil && work.decoding.complete {
			if step != work.decoding.decoded.size/domain.ConversationManifestChunkBytes+1 {
				test.Fatal("decode did not yield for exactly floor(decoded bytes / quantum) plus one steps", step, work.decoding.decoded.size)
			}
			return
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil ||
			work.canonical != nil || work.canonicalProof.Valid() || work.original || work.decoding == nil {
			test.Fatal("decode did not remain provisional before original proof", step, progress, err)
		}
		if !work.decoding.complete && work.decoding.decoded.size != (step+1)*domain.ConversationManifestChunkBytes {
			test.Fatal("decode did not append exactly one bounded output span", step, work.decoding.decoded.size)
		}
	}
	test.Fatal("bounded captured source did not finish decoding", limit)
}

func docCaptureWorkPGDrainPreparation(work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob) (outbound.DocRangeWorkProgress, error) {
	limit := docCaptureWorkPGPreparationStepLimit
	for step := 0; step < limit; step++ {
		if work.work != nil {
			return outbound.DocRangeWorkPending, nil
		}
		progress, err := work.Step(ctx, job)
		if err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			return progress, err
		}
	}
	return 0, fmt.Errorf("capture preparation exceeded %d bounded steps", limit)
}

func docCaptureWorkPGBeforeHandoff(test *testing.T, work *preparedLegacyCaptureWork, ctx context.Context, job domain.DocRangeIndexJob) {
	test.Helper()
	for step := 0; step < docCaptureWorkPGPreparationStepLimit; step++ {
		if work.work != nil {
			test.Fatal("provisional proof helper constructed a delegate before the handoff step")
		}
		if work.canonicalProof.Valid() || work.original {
			return
		}
		if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
			test.Fatal("capture proof did not remain pending before its separate handoff", step, progress, err)
		}
	}
	test.Fatal("bounded captured source did not reach its proof or original-classifier handoff", docCaptureWorkPGPreparationStepLimit)
}

func (*docCaptureWorkPGTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if hook, ok := ctx.Value(docCaptureCommitTraceKey{}).(*docCaptureCommitHook); ok {
		hook.run()
	}
}

func TestDocCaptureWorkPayloadTraceDistinguishesMetadataAndWholeReads(test *testing.T) {
	trace := &docCaptureWorkPGTrace{}
	spoolTrace := &docSpoolWorkPGTrace{}
	for ordinal, query := range []string{
		`SELECT octet_length(source.bytes),source.doc_range_source_version`,
		`SELECT substring(source.bytes FROM $3 FOR $4),octet_length(source.bytes)`,
		`SELECT source.bytes`,
	} {
		trace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: query})
		spoolTrace.TraceQueryStart(context.Background(), nil, pgx.TraceQueryStartData{SQL: query})
		if trace.bodyReads.Load() != int64(ordinal) || spoolTrace.sourceReads.Load() != int64(ordinal) {
			test.Fatal("payload measurement hid a whole source read or counted metadata as body", ordinal, trace.bodyReads.Load(), spoolTrace.sourceReads.Load())
		}
	}
}

func docCaptureWorkPGStore(test *testing.T, storage *PostgresStore, ctx context.Context) (*PostgresStore, *docCaptureWorkPGTrace) {
	test.Helper()
	trace := &docCaptureWorkPGTrace{}
	config := storage.pool.Config()
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
	return peer, trace
}

func TestDocCaptureWorkPGPrepareDoesNotReadStoredBody(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	work, err := peer.PrepareDocRangeWork(ctx, job)
	if work != nil {
		defer work.Close()
	}
	if err != nil || work == nil {
		test.Fatal("prepare provisional legacy source capture", err)
	}
	if trace.bodyReads.Load() != 0 {
		test.Fatalf("Prepare consumed the stored body before returning work: payload_queries=%d", trace.bodyReads.Load())
	}
	resources, err := work.RetainedResources()
	if err != nil || resources.SpoolBytes != 0 || resources.HeapBytes > 1<<20 {
		test.Fatal("provisional capture retained a whole source or invalid resources", resources, err)
	}
}

func TestDocCaptureWorkPGEachStepCopiesOneSpanBeforeProof(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 5<<20)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
	public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	path := work.source.file.Name()
	blocks := (len(fixture.raw) + domain.ConversationManifestChunkBytes - 1) / domain.ConversationManifestChunkBytes
	for ordinal := 0; ordinal < blocks; ordinal++ {
		progress, err := work.Step(ctx, job)
		captured := min((ordinal+1)*domain.ConversationManifestChunkBytes, len(fixture.raw))
		if err != nil || progress != outbound.DocRangeWorkPending || trace.bodyReads.Load() != int64(ordinal+1) ||
			work.work != nil || work.source.size != captured || len(work.parts) != ordinal+1 {
			test.Fatal("capture step exceeded a single source span or exposed proof", ordinal, progress, trace.bodyReads.Load(), err)
		}
		resources, err := work.RetainedResources()
		if err != nil || resources.SpoolBytes != int64(captured) || resources.HeapBytes >= 1<<20 {
			test.Fatal("provisional resources omitted captured storage or retained body bytes", resources, err)
		}
		if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
			test.Fatal("capture advertised publication", err)
		}
	}
	trace.mu.Lock()
	spans := append([]docCaptureWorkPGSpan(nil), trace.spans...)
	trace.mu.Unlock()
	if len(spans) != blocks {
		test.Fatal("capture SQL did not expose every bounded requested range", len(spans), blocks)
	}
	for ordinal, span := range spans {
		if span.from != ordinal*domain.ConversationManifestChunkBytes+1 || span.bytes != min(domain.ConversationManifestChunkBytes, len(fixture.raw)-ordinal*domain.ConversationManifestChunkBytes) {
			test.Fatal("source span order, bound or final length changed", ordinal, span)
		}
	}
	if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
		test.Fatal("provisional capture changed persisted source, staging or public state")
	}
	docCaptureWorkPGDecode(test, work, ctx, job)
	docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
	if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work == nil || work.source != nil || trace.bodyReads.Load() != int64(blocks) {
		test.Fatal("post-capture proof phase reread SQL or did not transfer to source-backed work", progress, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		test.Fatal("handoff retained its captured representation", err)
	}
	if err := work.Publish(ctx, job); !errors.Is(err, domain.ErrDocRangeIndexPending) {
		test.Fatal("proof handoff skipped pending metadata preparation", err)
	}
}

func TestDocCaptureWorkPGNextSpanRechecksSourceAndJob(test *testing.T) {
	for _, change := range []string{"source version", "ownership", "lease expiry", "job fence", "snapshot identity", "source incarnation"} {
		test.Run(change, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, _ := docCaptureWorkPGStore(test, storage, ctx)
			work := docCaptureWorkPGPrepare(test, peer, ctx, job)
			if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending {
				test.Fatal(progress, err)
			}
			legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, change)
			public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
			private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
			progress, err := work.Step(ctx, job)
			if err == nil || progress == outbound.DocRangeWorkPublishable || work.work != nil || work.source.size != domain.ConversationManifestChunkBytes {
				test.Fatal("revoked or replaced capture advanced", change, progress, err)
			}
			if change == "ownership" || change == "snapshot identity" {
				if !errors.Is(err, domain.ErrNotFound) {
					test.Fatal("ownership/identity miss changed error", err)
				}
			} else if !errors.Is(err, domain.ErrConflict) {
				test.Fatal("source/job witness change was not a conflict", err)
			}
			if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
				test.Fatal("failed capture changed persisted state")
			}
		})
	}
}

func TestDocCaptureWorkPGPrivateDamageAndCancellation(test *testing.T) {
	for _, damage := range []string{"checksum", "truncate", "extra", "cancellation"} {
		test.Run(damage, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, trace := docCaptureWorkPGStore(test, storage, ctx)
			work := docCaptureWorkPGPrepare(test, peer, ctx, job)
			docCaptureWorkPGCopy(test, work, ctx, job)
			path := work.source.file.Name()
			before := trace.queries.Load()
			selected, cancel := context.WithCancel(ctx)
			defer cancel()
			var mutation error
			switch damage {
			case "checksum":
				_, mutation = work.source.file.WriteAt([]byte("!"), 0)
			case "truncate":
				mutation = work.source.file.Truncate(int64(work.source.size - 1))
			case "extra":
				_, mutation = work.source.file.WriteAt([]byte("!"), int64(work.source.size))
			case "cancellation":
				cancel()
			}
			if mutation != nil {
				test.Fatal(mutation)
			}
			progress, err := docCaptureWorkPGDrainPreparation(work, selected, job)
			if damage == "cancellation" {
				if !errors.Is(err, context.Canceled) || trace.queries.Load() != before {
					test.Fatal("canceled capture issued confirmation SQL or changed error", err, trace.queries.Load()-before)
				}
			} else if !errors.Is(err, domain.ErrPreparedDocRangeObject) || !errors.Is(err, domain.ErrIntegrity) {
				test.Fatal("private corruption was mistaken for original source failure", err)
			}
			if progress == outbound.DocRangeWorkPublishable || work.work != nil {
				test.Fatal("damaged capture exposed a verified plan")
			}
			for attempt := 0; attempt < 2; attempt++ {
				if err := work.Close(); err != nil {
					test.Fatal(err)
				}
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				test.Fatal("closed capture leaked its private file", err)
			}
		})
	}
}

func TestDocCaptureWorkPGRoundTripRepresentations(test *testing.T) {
	for _, representation := range []string{"plain", "zstd", "pretty", "pretty-zstd", "manifest", "manifest-zstd",
		"empty", "empty-zstd", "empty-pretty", "empty-pretty-zstd", "empty-null", "empty-null-zstd"} {
		test.Run(representation, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			var fixture legacyByteStagePGFixture
			if strings.HasPrefix(representation, "empty") {
				fixture, _ = docRangeWorkEmissionPGSource(test, storage, ctx, 0)
			} else {
				fixture = legacyByteStagePGSource(test, storage, ctx, 64<<10)
			}
			stored := bytes.Clone(fixture.raw)
			if strings.Contains(representation, "null") {
				stored = bytes.Replace(stored, []byte(`"events":[]`), []byte(`"events":null`), 1)
				if bytes.Equal(stored, fixture.raw) {
					test.Fatal("empty null-events fixture did not require normalization")
				}
			}
			if strings.Contains(representation, "pretty") {
				var decoded domain.CIRDocument
				if err := json.Unmarshal(stored, &decoded); err != nil {
					test.Fatal(err)
				}
				var err error
				stored, err = json.MarshalIndent(decoded, "", "  ")
				if err != nil {
					test.Fatal(err)
				}
			}
			if strings.HasPrefix(representation, "manifest") {
				plan, ok := domain.PlanDocChunks(fixture.raw)
				if !ok {
					test.Fatal("legacy manifest fixture not available")
				}
				if _, _, err := storage.PutChunks(ctx, fixture.repo, plan.Bodies); err != nil {
					test.Fatal(err)
				}
				var err error
				stored, err = json.Marshal(plan.Manifest)
				if err != nil {
					test.Fatal(err)
				}
			}
			if strings.Contains(representation, "zstd") {
				stored = docCompress(stored)
			}
			if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
				test.Fatal(err)
			}
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, trace := docCaptureWorkPGStore(test, storage, ctx)
			work := docCaptureWorkPGPrepare(test, peer, ctx, job)
			docCaptureWorkPGCopy(test, work, ctx, job)
			docCaptureWorkPGDecode(test, work, ctx, job)
			docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
			original := strings.HasPrefix(representation, "manifest")
			if work.original != original || work.canonicalProof.Valid() == original {
				test.Fatal("representation selected the wrong canonical-proof or original-normalization handoff", representation, work.original, work.canonicalProof.Valid())
			}
			progress, err := work.Step(ctx, job)
			if err != nil {
				test.Fatal(err)
			}
			if strings.HasPrefix(representation, "manifest") {
				if progress != outbound.DocRangeWorkCompatibilityRequired || work.work != nil || trace.bodyReads.Load() != 1 {
					test.Fatal("manifest did not request explicit late compatibility", progress, trace.bodyReads.Load())
				}
				if err := work.Close(); err != nil {
					test.Fatal(err)
				}
				if err := peer.BuildDocRangeJob(ctx, job); err != nil {
					test.Fatal("synchronous drainer did not dispatch compatibility", err)
				}
			} else {
				for ordinal := 0; progress != outbound.DocRangeWorkPublishable && ordinal < docCaptureWorkPGPreparationStepLimit; ordinal++ {
					progress, err = work.Step(ctx, job)
					if err != nil || progress == outbound.DocRangeWorkCompatibilityRequired {
						test.Fatal("monolithic source changed preparation behavior", progress, err)
					}
				}
				if progress != outbound.DocRangeWorkPublishable {
					test.Fatal("bounded fixture did not become publishable")
				}
				if work.work == nil {
					test.Fatal("publishable representation lost its prepared delegate")
				}
				stream, err := work.work.data.Read(ctx, 0, len(fixture.stream))
				if err != nil || !bytes.Equal(stream, fixture.stream) ||
					!bytes.Equal(domain.AssembleCanonicalDoc(fixture.envelope, [][]byte{stream}), fixture.raw) {
					test.Fatal("representation changed the exact prepared canonical event stream", representation, err)
				}
				if err := work.Publish(ctx, job); err != nil {
					test.Fatal(err)
				}
				legacyDerivedPublishedPG(test, peer, ctx, fixture)
			}
			proof, err := peer.ReadVerifiedDoc(ctx, fixture.repo, fixture.hash)
			if err != nil || !bytes.Equal(proof.Bytes(), fixture.raw) {
				test.Fatal(fmt.Sprintf("%s source changed canonical identity or context", representation), err)
			}
		})
	}
}

type docCaptureConfirmationContext struct {
	context.Context
	paused  atomic.Bool
	release sync.Once
	entered chan struct{}
	resume  chan struct{}
	target  string
}

func (ctx *docCaptureConfirmationContext) Err() error {
	if !ctx.paused.Load() {
		var callers [2]uintptr
		frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
		caller, _ := frames.Next()
		target := ctx.target
		if target == "" {
			target = "store.(*preparedLegacyCaptureWork).handoffCanonical"
		}
		if strings.HasSuffix(caller.Function, target) && ctx.paused.CompareAndSwap(false, true) {
			close(ctx.entered)
			<-ctx.resume
		}
	}
	return ctx.Context.Err()
}

func (ctx *docCaptureConfirmationContext) unblock() {
	ctx.release.Do(func() { close(ctx.resume) })
}

func TestDocCaptureWorkPGConfirmationFencesEveryProvisionalOutcome(test *testing.T) {
	for _, outcome := range []string{"valid", "invalid", "manifest", "private"} {
		for _, changed := range []bool{false, true} {
			test.Run(fmt.Sprintf("%s/changed=%t", outcome, changed), func(test *testing.T) {
				storage, ctx := chunkReusePG(test)
				fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
				stored := bytes.Clone(fixture.raw)
				switch outcome {
				case "invalid":
					stored = bytes.Replace(stored, []byte(`"seq":2`), []byte(`"seq":3`), 1)
					if bytes.Equal(stored, fixture.raw) {
						test.Fatal("source corruption fixture did not change")
					}
				case "manifest":
					plan, ok := domain.PlanDocChunks(fixture.raw)
					if !ok {
						test.Fatal("manifest fixture unavailable")
					}
					var err error
					stored, err = json.Marshal(plan.Manifest)
					if err != nil {
						test.Fatal(err)
					}
				}
				if _, err := storage.pool.Exec(ctx, `UPDATE blobs SET bytes=$2 WHERE hash=$1`, fixture.hash, stored); err != nil {
					test.Fatal(err)
				}
				job := legacyByteStagePGClaim(test, storage, ctx, fixture)
				peer, _ := docCaptureWorkPGStore(test, storage, ctx)
				work := docCaptureWorkPGPrepare(test, peer, ctx, job)
				docCaptureWorkPGCopy(test, work, ctx, job)
				if outcome == "private" {
					if _, err := work.source.file.WriteAt([]byte("!"), 0); err != nil {
						test.Fatal(err)
					}
				} else {
					docCaptureWorkPGDecode(test, work, ctx, job)
					if outcome == "invalid" {
						for quantum := 0; quantum < docCaptureWorkPGPreparationStepLimit && work.normalization == nil; quantum++ {
							if progress, err := work.Step(ctx, job); err != nil || progress != outbound.DocRangeWorkPending || work.work != nil {
								test.Fatal("invalid source did not reach provisional normalization", progress, err)
							}
						}
						if work.normalization == nil {
							test.Fatal("invalid source never reached provisional normalization")
						}
					} else {
						docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
					}
				}
				base, cancel := context.WithCancel(ctx)
				paused := &docCaptureConfirmationContext{Context: base, entered: make(chan struct{}), resume: make(chan struct{})}
				if outcome == "private" || outcome == "invalid" {
					paused.target = "store.(*PostgresStore).confirmLegacyCaptureFailure"
				} else if outcome != "valid" {
					paused.target = "store.(*preparedLegacyCaptureWork).prepare"
				}
				type result struct {
					progress outbound.DocRangeWorkProgress
					err      error
				}
				results, finished := make(chan result, 1), make(chan struct{})
				test.Cleanup(func() {
					cancel()
					paused.unblock()
					<-finished
				})
				go func() {
					defer close(finished)
					var got result
					if outcome == "private" || outcome == "invalid" {
						got.progress, got.err = docCaptureWorkPGDrainPreparation(work, paused, job)
					} else {
						got.progress, got.err = work.Step(paused, job)
					}
					results <- got
				}()
				select {
				case <-paused.entered:
				case <-finished:
					test.Fatal("provisional result escaped before confirmation barrier", <-results)
				case <-ctx.Done():
					test.Fatal("capture did not reach confirmation", ctx.Err())
				}
				if changed {
					legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
				}
				private := legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1)
				public := legacyByteStagePGPublicImage(test, storage, ctx, fixture)
				paused.unblock()
				var got result
				select {
				case got = <-results:
					<-finished
				case <-ctx.Done():
					test.Fatal("capture confirmation stalled", ctx.Err())
				}
				if changed {
					if !errors.Is(got.err, domain.ErrConflict) || got.progress != 0 || work.work != nil {
						test.Fatal("obsolete provisional result won over current source", got)
					}
				} else if outcome == "private" {
					if !errors.Is(got.err, domain.ErrPreparedDocRangeObject) || work.work != nil {
						test.Fatal("current private corruption changed classification", got)
					}
				} else if outcome == "invalid" {
					if !errors.Is(got.err, domain.ErrIntegrity) || errors.Is(got.err, domain.ErrPreparedDocRangeObject) || work.work != nil {
						test.Fatal("unchanged original corruption changed classification", got)
					}
				} else if outcome == "manifest" {
					if got.err != nil || got.progress != outbound.DocRangeWorkCompatibilityRequired || work.work != nil {
						test.Fatal("current manifest failed its explicit handoff", got)
					}
				} else if got.err != nil || got.progress != outbound.DocRangeWorkPending || work.work == nil {
					test.Fatal("current verified source did not hand off", got)
				}
				if legacyByteStagePGImage(test, storage, ctx, fixture.hash, -1) != private || legacyByteStagePGPublicImage(test, storage, ctx, fixture) != public {
					test.Fatal("provisional confirmation changed persisted state")
				}
			})
		}
	}
}

func TestDocCaptureWorkPGCleanupFailureDoesNotDelegate(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	rangeSpoolTempDir(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, _ := docCaptureWorkPGStore(test, storage, ctx)
	prepared, err := peer.PrepareDocRangeWork(ctx, job)
	if err != nil || prepared == nil {
		test.Fatal(err)
	}
	work := prepared.(*preparedLegacyCaptureWork)
	test.Cleanup(func() { _ = work.Close() })
	docCaptureWorkPGCopy(test, work, ctx, job)
	docCaptureWorkPGDecode(test, work, ctx, job)
	docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
	base, cancel := context.WithCancel(ctx)
	paused := &docCaptureConfirmationContext{Context: base, entered: make(chan struct{}), resume: make(chan struct{})}
	results, finished := make(chan error, 1), make(chan struct{})
	test.Cleanup(func() { cancel(); paused.unblock(); <-finished })
	go func() {
		defer close(finished)
		_, err := work.Step(paused, job)
		results <- err
	}()
	select {
	case <-paused.entered:
	case <-finished:
		test.Fatal("cleanup injection missed its canonical handoff confirmation barrier", <-results)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	path := work.decoding.decoded.file.Name()
	if err := os.Rename(path, path+".retained"); err != nil {
		test.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		test.Fatal(err)
	}
	if err := os.WriteFile(path+"/block", []byte("owned fault injection"), 0600); err != nil {
		test.Fatal(err)
	}
	paused.unblock()
	select {
	case err = <-results:
		<-finished
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	if err == nil || work.work != nil {
		test.Fatal("canonical handoff cleanup failure exposed an advanceable delegate", err, work.work != nil)
	}
	entries, readErr := os.ReadDir(filepath.Dir(path))
	if readErr != nil || len(entries) != 2 {
		test.Fatal("canonical handoff cleanup failure retained newly constructed worker files", entries, readErr)
	}
}

func TestDocCaptureWorkPGAppendFailurePreservesCurrentSourcePriority(test *testing.T) {
	for _, changed := range []bool{false, true} {
		test.Run(fmt.Sprintf("changed=%t", changed), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 1<<20)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, _ := docCaptureWorkPGStore(test, storage, ctx)
			prepared, err := peer.PrepareDocRangeWork(ctx, job)
			if err != nil || prepared == nil {
				test.Fatal(err)
			}
			work := prepared.(*preparedLegacyCaptureWork)
			test.Cleanup(func() { _ = work.Close() })
			base, cancel := context.WithCancel(ctx)
			paused := &docCaptureConfirmationContext{Context: base, entered: make(chan struct{}), resume: make(chan struct{}), target: "store.(*legacyDocSpool).Append"}
			results, finished := make(chan error, 1), make(chan struct{})
			test.Cleanup(func() { cancel(); paused.unblock(); <-finished })
			go func() {
				defer close(finished)
				_, err := work.Step(paused, job)
				results <- err
			}()
			select {
			case <-paused.entered:
			case <-finished:
				test.Fatal("append failure injection missed its barrier", <-results)
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			if err := work.source.file.Close(); err != nil {
				test.Fatal(err)
			}
			if changed {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
			}
			paused.unblock()
			select {
			case err = <-results:
				<-finished
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			if changed && !errors.Is(err, domain.ErrConflict) || !changed && !errors.Is(err, os.ErrClosed) || work.work != nil {
				test.Fatal("local append failure lost current-source error priority", changed, err)
			}
		})
	}
}

func TestDocCaptureWorkPGCancellationAtConfirmationSkipsSQL(test *testing.T) {
	storage, ctx := chunkReusePG(test)
	fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
	job := legacyByteStagePGClaim(test, storage, ctx, fixture)
	peer, trace := docCaptureWorkPGStore(test, storage, ctx)
	work := docCaptureWorkPGPrepare(test, peer, ctx, job)
	docCaptureWorkPGCopy(test, work, ctx, job)
	docCaptureWorkPGDecode(test, work, ctx, job)
	docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
	before := trace.queries.Load()
	base, cancel := context.WithCancel(ctx)
	paused := &docCaptureConfirmationContext{Context: base, entered: make(chan struct{}), resume: make(chan struct{})}
	results, finished := make(chan error, 1), make(chan struct{})
	test.Cleanup(func() { cancel(); paused.unblock(); <-finished })
	go func() {
		defer close(finished)
		_, err := work.Step(paused, job)
		results <- err
	}()
	select {
	case <-paused.entered:
	case <-finished:
		test.Fatal("cancellation missed confirmation barrier", <-results)
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
	cancel()
	paused.unblock()
	select {
	case err := <-results:
		<-finished
		if !errors.Is(err, context.Canceled) || trace.queries.Load() != before || work.work != nil {
			test.Fatal("cancellation reached confirmation SQL or exposed proof", err, trace.queries.Load()-before)
		}
	case <-ctx.Done():
		test.Fatal(ctx.Err())
	}
}

func TestDocCaptureWorkPGConstructionFailureRechecksSource(test *testing.T) {
	for _, changed := range []bool{false, true} {
		test.Run(fmt.Sprintf("changed=%t", changed), func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, trace := docCaptureWorkPGStore(test, storage, ctx)
			work := docCaptureWorkPGPrepare(test, peer, ctx, job)
			docCaptureWorkPGCopy(test, work, ctx, job)
			docCaptureWorkPGDecode(test, work, ctx, job)
			docCaptureWorkPGBeforeHandoff(test, work, ctx, job)
			entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var pause, release sync.Once
			trace.commit.Store(&docCaptureCommitHook{run: func() {
				pause.Do(func() { close(entered); <-resume })
			}})
			base, cancel := context.WithCancel(ctx)
			results := make(chan error, 1)
			test.Cleanup(func() { cancel(); release.Do(func() { close(resume) }); <-finished })
			go func() {
				defer close(finished)
				_, err := work.Step(base, job)
				results <- err
			}()
			select {
			case <-entered:
			case <-finished:
				test.Fatal("construction injection missed the completed confirmation", <-results)
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			test.Setenv("TMPDIR", test.TempDir()+"/missing")
			if changed {
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
			}
			release.Do(func() { close(resume) })
			var err error
			select {
			case err = <-results:
				<-finished
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			if changed && !errors.Is(err, domain.ErrConflict) || !changed && !errors.Is(err, os.ErrNotExist) || work.work != nil {
				test.Fatal("worker construction failure lost current-source precedence or exposed a delegate", changed, err)
			}
		})
	}
}

func TestDocCaptureWorkPGPrepareFailurePreservesCancellationAndSourcePriority(test *testing.T) {
	for _, scenario := range []string{"unchanged", "source", "cancellation"} {
		test.Run(scenario, func(test *testing.T) {
			storage, ctx := chunkReusePG(test)
			fixture := legacyByteStagePGSource(test, storage, ctx, 64<<10)
			job := legacyByteStagePGClaim(test, storage, ctx, fixture)
			peer, trace := docCaptureWorkPGStore(test, storage, ctx)
			entered, resume, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var paused atomic.Bool
			var release sync.Once
			trace.commit.Store(&docCaptureCommitHook{run: func() {
				if trace.captureWitness.Load() && paused.CompareAndSwap(false, true) {
					close(entered)
					<-resume
				}
			}})
			base, cancel := context.WithCancel(ctx)
			type result struct {
				work outbound.PreparedDocRangeWork
				err  error
			}
			results := make(chan result, 1)
			test.Cleanup(func() { cancel(); release.Do(func() { close(resume) }); <-finished })
			go func() {
				defer close(finished)
				work, err := peer.PrepareDocRangeWork(base, job)
				results <- result{work: work, err: err}
			}()
			select {
			case <-entered:
			case <-finished:
				test.Fatal("Prepare did not finish its witness transaction before the creation fault", <-results)
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			test.Setenv("TMPDIR", test.TempDir()+"/missing")
			before := trace.queries.Load()
			switch scenario {
			case "source":
				legacyBytePrepareBoundaryWrite(test, storage, ctx, fixture, "source version")
			case "cancellation":
				cancel()
			}
			release.Do(func() { close(resume) })
			var got result
			select {
			case got = <-results:
				<-finished
			case <-ctx.Done():
				test.Fatal(ctx.Err())
			}
			if got.work != nil {
				defer got.work.Close()
			}
			want := error(os.ErrNotExist)
			if scenario == "source" {
				want = domain.ErrConflict
			}
			if scenario == "cancellation" {
				want = context.Canceled
				if trace.queries.Load() != before {
					test.Fatal("canceled initial capture issued confirmation SQL", trace.queries.Load()-before)
				}
			}
			if got.work != nil || !errors.Is(got.err, want) || trace.bodyReads.Load() != 0 {
				test.Fatalf("Prepare error lost priority or returned nonzero work: scenario=%s work=%T error=%v want=%v", scenario, got.work, got.err, want)
			}
		})
	}
}
