//go:build postgres

package store

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type rootWriterCostSamplePG struct {
	trace          *rootPublicationCostPG
	allocatedBytes uint64
	mallocs        uint64
	collections    uint32
}

func rootWriterCostOptInPG(test *testing.T) {
	test.Helper()
	if os.Getenv("CXT_ROOT_WRITER_COST") != "1" {
		test.Skip("set CXT_ROOT_WRITER_COST=1 for bounded disposable local PostgreSQL samples")
	}
	if strings.TrimSpace(os.Getenv("CXT_TEST_DSN")) == "" {
		test.Skip("CXT_TEST_DSN unset; fixture setup and measurements not run")
	}
}

func rootWriterCostMeasurePG(ctx context.Context, received *atomic.Int64, operation func(context.Context) error) (rootWriterCostSamplePG, error) {
	trace := &rootPublicationCostPG{received: received, phases: make(map[string]*rootPublicationCostPhasePG), kinds: make(map[string]int)}
	observed := context.WithValue(ctx, rootPublicationCostKeyPG{}, trace)
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	beforeBytes := received.Load()
	started := time.Now()
	err := operation(observed)
	trace.elapsed, trace.receivedBytes = time.Since(started), received.Load()-beforeBytes
	runtime.ReadMemStats(&after)
	return rootWriterCostSamplePG{trace: trace, allocatedBytes: after.TotalAlloc - before.TotalAlloc, mallocs: after.Mallocs - before.Mallocs, collections: after.NumGC - before.NumGC}, err
}

func (sample rootWriterCostSamplePG) report(test *testing.T, phase string) {
	test.Helper()
	var transaction time.Duration
	if !sample.trace.committed.IsZero() {
		transaction = sample.trace.committed.Sub(sample.trace.transactionStarted)
	}
	test.Logf("RF13-LP1 phase=%s wall_ns=%d final_transaction_ns=%d allocated_bytes=%d mallocs=%d gc_cycles=%d allocation_scope=process_wide_whole_method trace_overhead_included=true",
		phase, sample.trace.elapsed, transaction, sample.allocatedBytes, sample.mallocs, sample.collections)
	sample.trace.report(test, phase)
}

func rootWriterCostSpoolPG(test *testing.T, directory string) {
	test.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		test.Fatal(err)
	}
	var size int64
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			test.Fatal(err)
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			test.Fatal("prepared spool is not a private regular file", info.Mode())
		}
		size += info.Size()
	}
	test.Logf("RF13-LP1 prepared_spool_files=%d prepared_spool_bytes=%d spool_stat_excluded_from_measurements=true", len(entries), size)
}

func rootWriterCostPublishedPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob, fixture rootReaderFixture) {
	test.Helper()
	done, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil || done.State != "completed" || done.DocumentRef() != job.DocumentRef() || done.Version != job.Version || !done.LeaseUntil.IsZero() {
		test.Fatal("invalid completion receipt", done, err)
	}
	owned, err := storage.HasDocs(ctx, job.RepoID, []domain.ContentHash{job.DocHash})
	if err != nil || len(owned) != 1 {
		test.Fatal("completed root ownership missing", owned, err)
	}
	index, err := domain.BuildDocRangeIndex(ctx, rootPublicationProof(test, fixture))
	if err != nil {
		test.Fatal(err)
	}
	header, err := storage.DocRangeIndex(ctx, job.RepoID, job.DocHash)
	if err != nil || !reflect.DeepEqual(header, index.Header()) {
		test.Fatal("published compatibility header differs from the public builder", err)
	}
	proof, _, err := domain.BuildVerifiedDocRangeGeneration(ctx, index)
	if err != nil {
		test.Fatal(err)
	}
	wantVisible, err := domain.BuildDocVisibleGeneration(ctx, proof, func(context.Context, domain.DocRangeObject) error { return nil })
	if err != nil {
		test.Fatal(err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		ranges, err := storage.OpenDocRangeGeneration(bound, job.RepoID, job.DocHash)
		if err != nil {
			return err
		}
		visible, err := storage.OpenDocVisibleGeneration(bound, job.RepoID, job.DocHash)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(ranges.Descriptor(), proof.Descriptor()) || !reflect.DeepEqual(visible.Descriptor(), wantVisible) || ranges.Binding() != visible.Binding() {
			return fmt.Errorf("published range/visible descriptors or source binding differ from the public builders")
		}
		for ordinal, pageHash := range header.Pages {
			raw, err := index.Page(bound, ordinal)
			if err != nil {
				return err
			}
			want, err := header.DecodePage(ordinal, raw)
			if err != nil {
				return err
			}
			legacy, err := storage.DocRangePage(bound, job.RepoID, header, ordinal)
			if err != nil {
				return err
			}
			current, err := ranges.ReadPage(bound, ordinal)
			if err != nil {
				return err
			}
			actualHash, err := ranges.PageHash(bound, ordinal)
			if err != nil || actualHash != pageHash || !reflect.DeepEqual(legacy, want) || !reflect.DeepEqual(current, want) {
				return fmt.Errorf("range page %d differs from the public builder: %v", ordinal, err)
			}
		}
		for ordinal, want := range fixture.manifest.Chunks {
			chunk, err := ranges.Chunk(bound, ordinal)
			if err != nil || chunk != want {
				return fmt.Errorf("range chunk %d differs from the manifest: %v", ordinal, err)
			}
		}
		count := len(fixture.cir.Events)
		for _, mode := range []string{"all", "chat", "prompts"} {
			if wantVisible.Views[mode].Units != count {
				return fmt.Errorf("all-user-message fixture has unexpected %s unit count", mode)
			}
			for _, position := range []int{0, domain.DocVisiblePageUnits - 1, domain.DocVisiblePageUnits, count / 2, count - 1} {
				if position < 0 || position >= count {
					continue
				}
				want := domain.DocVisibleUnit{From: position, To: position + 1, EventIndex: position, TurnIndex: position, RenderedBefore: position}
				for _, byEvent := range []bool{false, true} {
					unit, ordinal, err := visible.Unit(bound, mode, position, byEvent)
					if err != nil || unit != want || ordinal != position {
						return fmt.Errorf("visible %s position %d by_event=%t differs: unit=%+v ordinal=%d error=%v", mode, position, byEvent, unit, ordinal, err)
					}
				}
			}
		}
		return nil
	}); err != nil {
		test.Fatal(err)
	}
	document, err := storage.ReadVerifiedDoc(ctx, job.RepoID, job.DocHash)
	if err != nil {
		test.Fatal(err)
	}
	assertRootProof(test, document, fixture)
}

func TestPGRootWriterCost(test *testing.T) {
	rootWriterCostOptInPG(test)
	test.Logf("RF13-LP1 fixture=rangeFixture baseline_reference=6d3b38a577bf0c3d9fa2e80dcdc3c1a0e73af8eb go=%s gomaxprocs=%d", runtime.Version(), runtime.GOMAXPROCS(0))
	test.Log("three raw samples per event count; every sample owns a fresh root/job and calls actual PrepareRootDocJob and Complete exactly once; testing.AllocsPerRun is not used because it warms up and repeats side effects")
	test.Log("run serially in an otherwise idle test process and disposable local database; no fixture goroutines or competing work during allocation windows; runtime/driver background allocations may still contribute")
	test.Log("setup, connection establishment, MemStats reads, spool inspection, correctness oracles and cleanup are outside wall timers; allocations include pgx, tracing and natural GC; no forced GC or statement-cache warmup")
	test.Log("final_transaction is actual BEGIN start through COMMIT end; allocation deltas cover the whole method, not transaction-only allocations, retained heap, peak memory, CPU time or server memory")
	test.Log("small all-user-message corpus with unique per-sample identities and ordinary Go/database/OS caches; not 128k events, 512MiB, a 16MiB heap/64MiB spool proof, FIFO proof, cold-cache result, throughput study or full p95; no latency/allocation thresholds")
	for _, count := range []int{128, 1024, 8192} {
		for sample := 1; sample <= 3; sample++ {
			test.Run(fmt.Sprintf("events-%d/sample-%d", count, sample), func(test *testing.T) {
				directory := rangeSpoolTempDir(test)
				storage, ctx := chunkReusePG(test)
				ctx = rootPublicationContext(ctx)
				repo, namespace := rootPGRepo(test, ctx, storage)
				fixture := rangeFixture(test, count)
				job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
				test.Logf("RF13-LP1 events=%d sample=%d canonical_bytes=%d stream_bytes=%d manifest_bytes=%d manifest_chunks=%d unique_chunks=%d setup_excluded=true",
					count, sample, len(fixture.canonical), fixture.manifest.StreamBytes, len(fixture.manifestBytes), len(fixture.manifest.Chunks), len(fixture.bodies))
				before := rootPGImage(test, ctx, storage, repo, namespace)
				beforeIndexes := docVisiblePGImage(test, storage, ctx, job.DocHash)
				original := storage.pool
				pool, received, _ := rootPublicationCostPoolPG(test, ctx, original)
				storage.pool = pool
				test.Cleanup(func() { storage.pool = original })
				var publication outbound.PreparedRootDocPublication
				test.Cleanup(func() {
					if publication != nil {
						if err := publication.Close(); err != nil {
							test.Error("close prepared root", err)
						}
					}
					assertRangeSpoolsRemoved(test, directory)
				})
				prepare, err := rootWriterCostMeasurePG(ctx, received, func(observed context.Context) error {
					var prepareErr error
					publication, prepareErr = storage.PrepareRootDocJob(observed, job)
					return prepareErr
				})
				if err != nil || publication == nil {
					test.Fatal("actual root preparation failed", err)
				}
				prepare.report(test, "prepare")
				assertRootStreamingUnpublishedPG(test, storage, ctx, job)
				if rootPGImage(test, ctx, storage, repo, namespace) != before || docVisiblePGImage(test, storage, ctx, job.DocHash) != beforeIndexes {
					test.Fatal("preparation changed durable root, ownership, usage, receipt or range/visible state")
				}
				rootWriterCostSpoolPG(test, directory)
				complete, err := rootWriterCostMeasurePG(ctx, received, func(observed context.Context) error {
					return publication.Complete(observed, job, time.Now())
				})
				if err != nil {
					test.Fatal("actual root completion failed", err)
				}
				if complete.trace.transactionStarted.IsZero() || complete.trace.committed.IsZero() || complete.trace.phase("begin").Statements != 1 || complete.trace.phase("commit").Statements != 1 || complete.trace.phase("rollback").Statements != 0 {
					test.Fatal("completion did not perform exactly one committed final transaction")
				}
				complete.report(test, "complete")
				if err := publication.Close(); err != nil {
					test.Fatal(err)
				}
				assertRangeSpoolsRemoved(test, directory)
				rootWriterCostPublishedPG(test, storage, ctx, job, fixture)
			})
		}
	}
}

type rootWriterPreparationBarrierKeyPG struct{}

type rootWriterPreparationBarrierPG struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (barrier *rootWriterPreparationBarrierPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if rootPublicationCostSQLPhasePG(data.SQL) == "chunk_payload" {
		return context.WithValue(ctx, rootWriterPreparationBarrierKeyPG{}, barrier)
	}
	return ctx
}

func (barrier *rootWriterPreparationBarrierPG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err != nil || ctx.Value(rootWriterPreparationBarrierKeyPG{}) != barrier {
		return
	}
	barrier.once.Do(func() {
		close(barrier.reached)
		select {
		case <-barrier.release:
		case <-ctx.Done():
		}
	})
}

func TestPGRootWriterCostPreparationReferenceRead(test *testing.T) {
	rootWriterCostOptInPG(test)
	test.Log("correctness only: pause actual preparation at the first successful chunk-payload query end; independent same-repository reference reads must finish before release; performance_sample=false")
	directory := rangeSpoolTempDir(test)
	storage, ctx := chunkReusePG(test)
	ctx = rootPublicationContext(ctx)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	repo, _ := rootPGRepo(test, ctx, storage)
	reference := rangeFixture(test, 128)
	referenceJob, _ := rootPublicationClaimPG(test, storage, ctx, reference, repo)
	referencePublication := prepareRootStreamingPG(test, storage, ctx, referenceJob, directory)
	if err := referencePublication.Complete(ctx, referenceJob, time.Now()); err != nil {
		test.Fatal(err)
	}
	if err := referencePublication.Close(); err != nil {
		test.Fatal(err)
	}
	fixture := rangeFixture(test, 1024)
	job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, repo)
	original := storage.pool
	observer := &PostgresStore{pool: original}
	barrier := &rootWriterPreparationBarrierPG{reached: make(chan struct{}), release: make(chan struct{})}
	config := original.Config().Copy()
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	config.HealthCheckPeriod = time.Hour
	config.ConnConfig.Tracer = barrier
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() {
		storage.pool = original
		pool.Close()
	})
	if err := pool.Ping(ctx); err != nil {
		test.Fatal(err)
	}
	storage.pool = pool
	var publication outbound.PreparedRootDocPublication
	var prepareErr error
	finished := make(chan struct{})
	var resume sync.Once
	unpause := func() { resume.Do(func() { close(barrier.release) }) }
	go func() {
		defer close(finished)
		publication, prepareErr = storage.PrepareRootDocJob(ctx, job)
	}()
	defer func() {
		unpause()
		cancel()
		<-finished
		if publication != nil {
			if err := publication.Close(); err != nil {
				test.Error(err)
			}
		}
		assertRangeSpoolsRemoved(test, directory)
	}()
	select {
	case <-barrier.reached:
	case <-finished:
		test.Fatal("preparation missed the actual chunk-read barrier", prepareErr)
	case <-ctx.Done():
		test.Fatal("preparation barrier deadline", ctx.Err())
	}
	rootWriterCostPublishedPG(test, observer, ctx, referenceJob, reference)
	assertRootStreamingUnpublishedPG(test, observer, ctx, job)
	if err := ctx.Err(); err != nil {
		test.Fatal("reference reads exceeded the correctness deadline", err)
	}
	select {
	case <-finished:
		test.Fatal("preparation escaped its barrier before the reference reads completed")
	default:
	}
	unpause()
	select {
	case <-finished:
	case <-ctx.Done():
		test.Fatal("preparation did not resume", ctx.Err())
	}
	if prepareErr != nil || publication == nil {
		test.Fatal("preparation after reference read", prepareErr)
	}
	if err := publication.Complete(ctx, job, time.Now()); err != nil {
		test.Fatal(err)
	}
	rootWriterCostPublishedPG(test, observer, ctx, job, fixture)
	test.Log("reference_read_before_preparation_release=true same_repository=true independent_pools=true performance_sample=false")
}
