//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type memoryCapturePersistenceCostKey struct{}
type memoryCapturePersistenceQueryKey struct{}
type memoryCapturePersistenceBatchKey struct{}

type memoryCapturePersistenceQuery struct {
	phase    string
	started  time.Time
	duration time.Duration
	finished bool
	err      error
}

type memoryCapturePersistenceBatch struct {
	started  time.Time
	duration time.Duration
	queued   int
	observed int
	finished bool
	err      error
}

type memoryCapturePersistenceCost struct {
	started    time.Time
	firstQuery time.Time
	queries    []*memoryCapturePersistenceQuery
	batches    []*memoryCapturePersistenceBatch
}

type memoryCapturePersistenceTracer struct{}

var _ pgx.QueryTracer = memoryCapturePersistenceTracer{}
var _ pgx.BatchTracer = memoryCapturePersistenceTracer{}

func (memoryCapturePersistenceTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	cost, ok := ctx.Value(memoryCapturePersistenceCostKey{}).(*memoryCapturePersistenceCost)
	if !ok {
		return ctx
	}
	started := time.Now()
	if cost.firstQuery.IsZero() {
		cost.firstQuery = started
	}
	query := &memoryCapturePersistenceQuery{phase: memoryCapturePersistencePhase(data.SQL), started: started}
	cost.queries = append(cost.queries, query)
	return context.WithValue(ctx, memoryCapturePersistenceQueryKey{}, query)
}

func (memoryCapturePersistenceTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if query, ok := ctx.Value(memoryCapturePersistenceQueryKey{}).(*memoryCapturePersistenceQuery); ok {
		query.duration, query.finished, query.err = time.Since(query.started), true, data.Err
	}
}

func (memoryCapturePersistenceTracer) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	cost, ok := ctx.Value(memoryCapturePersistenceCostKey{}).(*memoryCapturePersistenceCost)
	if !ok {
		return ctx
	}
	batch := &memoryCapturePersistenceBatch{started: time.Now(), queued: len(data.Batch.QueuedQueries)}
	cost.batches = append(cost.batches, batch)
	return context.WithValue(ctx, memoryCapturePersistenceBatchKey{}, batch)
}

func (memoryCapturePersistenceTracer) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if batch, ok := ctx.Value(memoryCapturePersistenceBatchKey{}).(*memoryCapturePersistenceBatch); ok {
		batch.observed++
		if data.Err != nil {
			batch.err = data.Err
		}
	}
}

func (memoryCapturePersistenceTracer) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	if batch, ok := ctx.Value(memoryCapturePersistenceBatchKey{}).(*memoryCapturePersistenceBatch); ok {
		batch.duration, batch.finished = time.Since(batch.started), true
		if data.Err != nil {
			batch.err = data.Err
		}
	}
}

func memoryCapturePersistencePhase(sql string) string {
	query := strings.ToLower(strings.TrimSpace(sql))
	switch {
	case strings.HasPrefix(query, "begin"):
		return "begin"
	case strings.Contains(query, "pg_advisory_xact_lock"):
		return "lock_advisory"
	case strings.HasPrefix(query, "select id from repos"):
		return "lock_repo"
	case strings.HasPrefix(query, "select exists(select 1 from memory_capture_headers"):
		return "existing_capture"
	case strings.HasPrefix(query, "select clock_timestamp()"):
		return "expiry_clock"
	case strings.HasPrefix(query, "insert into memory_capture_headers"):
		return "header"
	case query == "commit":
		return "commit"
	case query == "rollback":
		return "rollback"
	default:
		return "other"
	}
}

func memoryCapturePersistenceFixture(test *testing.T, storage *PostgresStore, ctx context.Context, repo domain.ContentHash, count int) domain.MemoryCaptureBundle {
	test.Helper()
	generation := memorySourceGenerationPG(test, ctx, storage, repo)
	parts := make([]domain.MemoryCapturePart, 0, count)
	unique := make(map[domain.ContentHash]bool, count)
	for ordinal := 0; ordinal < count; ordinal++ {
		seed := fmt.Sprintf("%s/snapshot-%06d", repo, ordinal)
		identifier := domain.HashContent([]byte(seed))
		snapshot := domain.Snapshot{
			ID: identifier, RepoID: repo, Branch: "cost-probe",
			Parents: []domain.ContentHash{}, DocHash: identifier,
			Message:   strings.Repeat("synthetic metadata ", 12) + seed,
			CreatedAt: time.Date(2026, time.January, 1, 0, 0, ordinal, 0, time.UTC),
		}
		body, err := json.Marshal(snapshot)
		if err != nil {
			test.Fatal(err)
		}
		digest := domain.HashContent(body)
		if unique[digest] {
			test.Fatal("synthetic capture bodies must be unique")
		}
		unique[digest] = true
		parts = append(parts, domain.MemoryCapturePart{Kind: "graph:snapshot", Key: string(snapshot.ID), Sequence: generation.Graph.Sequence, Value: body})
	}
	header := domain.MemoryCaptureHeader{Version: domain.MemoryCaptureVersion, Rules: domain.MemoryCaptureRules,
		Generation: generation, Selection: domain.EffectiveMemorySelection{SnapshotID: domain.ContentHash(parts[0].Key), CodeCommit: strings.Repeat("a", 40)}}
	return memoryBundlePGSeal(test, ctx, header, parts)
}

func memoryCapturePersistencePool(test *testing.T, storage *PostgresStore, ctx context.Context) {
	test.Helper()
	original := storage.pool
	config := original.Config().Copy()
	config.ConnConfig.Tracer = memoryCapturePersistenceTracer{}
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
}

func (cost *memoryCapturePersistenceCost) report(test *testing.T, count, sample int, total, validation time.Duration, writeErr error) {
	test.Helper()
	var queryTime, batchTime time.Duration
	batchStatements, batchResults := 0, 0
	phases := make(map[string]time.Duration)
	phaseCounts := make(map[string]int)
	for _, query := range cost.queries {
		queryTime += query.duration
		phases[query.phase] += query.duration
		phaseCounts[query.phase]++
		test.Logf("D3ac-persistence parts=%d sample=%d phase=%s pgx_statements=1 duration=%s finished=%t error=%v",
			count, sample, query.phase, query.duration, query.finished, query.err)
		if !query.finished || query.err != nil {
			test.Errorf("incomplete or failed query trace: phase=%s finished=%t error=%v", query.phase, query.finished, query.err)
		}
	}
	for index, batch := range cost.batches {
		batchTime += batch.duration
		batchStatements += batch.queued
		batchResults += batch.observed
		test.Logf("D3ac-persistence parts=%d sample=%d batch=%d pgx_queued_statements=%d pgx_observed_results=%d send_through_close=%s finished=%t error=%v",
			count, sample, index+1, batch.queued, batch.observed, batch.duration, batch.finished, batch.err)
		if !batch.finished || batch.err != nil || batch.queued != batch.observed {
			test.Errorf("incomplete or failed batch trace: queued=%d observed=%d finished=%t error=%v", batch.queued, batch.observed, batch.finished, batch.err)
		}
	}
	var preQuery time.Duration
	if !cost.firstQuery.IsZero() {
		preQuery = cost.firstQuery.Sub(cost.started)
	}
	test.Logf("D3ac-persistence parts=%d sample=%d write_total=%s validation_probe_outside_write=%s pre_first_query_validation_and_acquire=%s lock=%s batches=%s header=%s commit=%s standalone_query_wall=%s untraced_residual=%s batch_calls=%d pgx_query_statements=%d pgx_batch_statements=%d pgx_batch_results=%d pgx_issued_statements=%d error=%v",
		count, sample, total, validation, preQuery, phases["lock_advisory"]+phases["lock_repo"], batchTime, phases["header"], phases["commit"], queryTime, total-queryTime-batchTime,
		len(cost.batches), len(cost.queries), batchStatements, batchResults, len(cost.queries)+batchStatements, writeErr)
	if phaseCounts["begin"] != 1 || phaseCounts["header"] != 1 || phaseCounts["commit"] != 1 || phaseCounts["rollback"] != 0 || len(cost.batches) == 0 {
		test.Errorf("expected one new capture transaction, not reuse: phases=%v batches=%d", phaseCounts, len(cost.batches))
	}
}

func TestPGMemoryCapturePersistenceCostD3ac(test *testing.T) {
	if os.Getenv("CXT_MEMORY_COST_PROBE") != "1" {
		test.Skip("set CXT_MEMORY_COST_PROBE=1 and CXT_TEST_DSN to a disposable cost-only PostgreSQL database")
	}
	if os.Getenv("CXT_TEST_DSN") == "" {
		test.Fatal("CXT_TEST_DSN must identify a disposable cost-only PostgreSQL database")
	}
	test.Log("store-only synthetic full capture; unique snapshot JSON bodies; not application/source-graph qualification; three raw samples, no percentile claim")
	test.Log("pgx statement counts exclude server-nested SQL, triggers, and protocol preparation; batch wall time includes send through result close, not individual server statement latency")
	test.Log("validation_probe_outside_write is separate and not additive; the one actual WriteMemoryCapture validates again; untraced residual includes validation, hashing, batch assembly, and pool acquisition; COMMIT includes deferred constraints and durable commit")
	for _, count := range []int{100, 1000} {
		for sample := 1; sample <= 3; sample++ {
			test.Run(fmt.Sprintf("parts_%d/sample_%d", count, sample), func(test *testing.T) {
				storage, ctx, repo := catalogPG(test)
				bundle := memoryCapturePersistenceFixture(test, storage, ctx, repo, count)
				memoryCapturePersistencePool(test, storage, ctx)
				validationStarted := time.Now()
				header, bodyBytes, keyBytes, err := memoryCaptureBundleSizes(ctx, bundle)
				validation := time.Since(validationStarted)
				if err != nil {
					test.Fatal(err)
				}
				expires := time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond)
				cost := &memoryCapturePersistenceCost{}
				observed := context.WithValue(ctx, memoryCapturePersistenceCostKey{}, cost)
				cost.started = time.Now()
				writeErr := storage.WriteMemoryCapture(observed, bundle, expires)
				total := time.Since(cost.started)
				cost.report(test, count, sample, total, validation, writeErr)
				if writeErr != nil {
					test.Fatal(writeErr)
				}
				memoryBundlePGRead(test, storage, ctx, bundle)
				if counts := memoryBundlePGCounts(test, storage, ctx, repo); counts != [3]int{1, count, count} {
					test.Fatal("capture did not persist all unique parts and blocks", counts)
				}
				test.Logf("D3ac-persistence parts=%d sample=%d header_bytes=%d body_bytes=%d key_bytes=%d unique_blocks=%d persisted_roundtrip=matched capture_id=%s",
					count, sample, len(header), bodyBytes, keyBytes, count, bundle.ID)
			})
		}
	}
}
