//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
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

type rootPublicationCostKeyPG struct{}
type rootPublicationCostIntervalKeyPG struct{}

type rootPublicationCostPhasePG struct {
	Statements    int           `json:"statements"`
	Rows          int64         `json:"rows"`
	WallNS        time.Duration `json:"wall_ns"`
	ReceivedBytes int64         `json:"received_bytes"`
}

type rootPublicationCostPG struct {
	received           *atomic.Int64
	phases             map[string]*rootPublicationCostPhasePG
	kinds              map[string]int
	queryCalls         int
	queryEnds          int
	batchCalls         int
	batchEnds          int
	batchQueued        int
	batchResults       int
	errors             int
	elapsed            time.Duration
	receivedBytes      int64
	transactionStarted time.Time
	committed          time.Time
	lockStarted        time.Time
	lockAcquired       time.Time
	afterFence         func(context.Context, *pgx.Conn)
}

type rootPublicationCostIntervalPG struct {
	cost     *rootPublicationCostPG
	phase    string
	started  time.Time
	received int64
}

type rootPublicationCostTracerPG struct{}

var _ pgx.QueryTracer = rootPublicationCostTracerPG{}
var _ pgx.BatchTracer = rootPublicationCostTracerPG{}

func rootPublicationCostSQLPhasePG(sql string) string {
	query := strings.ToLower(strings.Join(strings.Fields(sql), " "))
	switch {
	case strings.HasPrefix(query, "begin"):
		return "begin"
	case query == "commit", query == "rollback":
		return query
	case strings.Contains(query, "pg_advisory_xact_lock_shared"):
		return "identity_lock"
	case strings.Contains(query, "pg_advisory_xact_lock("):
		return "repository_lock"
	case strings.HasPrefix(query, "select id from repos"):
		return "repository_pin"
	case strings.Contains(query, "from doc_finalization_jobs") && strings.Contains(query, "for update"):
		return "job_fence"
	case strings.Contains(query, "doc_finalization_jobs"):
		return "job_receipt"
	case strings.Contains(query, "from repo_blobs rb join blobs b") && strings.Contains(query, "rb.kind='chunk'"):
		if strings.Contains(query, "substring(") {
			return "chunk_payload"
		}
		return "chunk_retention_metadata"
	case strings.Contains(query, "doc_visible_"):
		return "visible_publication"
	case strings.Contains(query, "doc_range_"):
		return "range_publication"
	case strings.Contains(query, "into repo_blobs"):
		return "root_ownership"
	case strings.Contains(query, "into blobs"), strings.Contains(query, "from blobs"):
		return "root_bytes"
	case strings.HasPrefix(query, "update repos"):
		return "writer_metadata_update"
	case strings.Contains(query, "from repos"):
		return "repository_policy"
	default:
		return "other"
	}
}

func (cost *rootPublicationCostPG) phase(name string) *rootPublicationCostPhasePG {
	if cost.phases[name] == nil {
		cost.phases[name] = &rootPublicationCostPhasePG{}
	}
	return cost.phases[name]
}

func (cost *rootPublicationCostPG) statement(sql string) string {
	phase := rootPublicationCostSQLPhasePG(sql)
	cost.phase(phase).Statements++
	fields := strings.Fields(sql)
	if len(fields) > 0 {
		cost.kinds[strings.ToUpper(fields[0])]++
	}
	return phase
}

func (rootPublicationCostTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	cost, ok := ctx.Value(rootPublicationCostKeyPG{}).(*rootPublicationCostPG)
	if !ok {
		return ctx
	}
	interval := &rootPublicationCostIntervalPG{cost: cost, started: time.Now(), received: cost.received.Load()}
	interval.phase = cost.statement(data.SQL)
	cost.queryCalls++
	if interval.phase == "begin" {
		cost.transactionStarted = interval.started
	}
	if interval.phase == "repository_lock" {
		cost.lockStarted = interval.started
	}
	return context.WithValue(ctx, rootPublicationCostIntervalKeyPG{}, interval)
}

func (interval *rootPublicationCostIntervalPG) finish(err error) {
	phase := interval.cost.phase(interval.phase)
	phase.WallNS += time.Since(interval.started)
	phase.ReceivedBytes += interval.cost.received.Load() - interval.received
	if err != nil {
		interval.cost.errors++
	}
}

func (rootPublicationCostTracerPG) TraceQueryEnd(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryEndData) {
	interval, ok := ctx.Value(rootPublicationCostIntervalKeyPG{}).(*rootPublicationCostIntervalPG)
	if !ok {
		return
	}
	interval.finish(data.Err)
	cost := interval.cost
	cost.queryEnds++
	cost.phase(interval.phase).Rows += data.CommandTag.RowsAffected()
	if data.Err == nil {
		switch interval.phase {
		case "commit":
			cost.committed = time.Now()
		case "repository_lock":
			cost.lockAcquired = time.Now()
		case "job_fence":
			if cost.afterFence != nil {
				cost.afterFence(ctx, connection)
			}
		}
	}
}

func (rootPublicationCostTracerPG) TraceBatchStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	cost, ok := ctx.Value(rootPublicationCostKeyPG{}).(*rootPublicationCostPG)
	if !ok {
		return ctx
	}
	interval := &rootPublicationCostIntervalPG{cost: cost, phase: "batch_io", started: time.Now(), received: cost.received.Load()}
	cost.batchCalls++
	cost.batchQueued += len(data.Batch.QueuedQueries)
	for _, query := range data.Batch.QueuedQueries {
		cost.statement(query.SQL)
	}
	return context.WithValue(ctx, rootPublicationCostIntervalKeyPG{}, interval)
}

func (rootPublicationCostTracerPG) TraceBatchQuery(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchQueryData) {
	if interval, ok := ctx.Value(rootPublicationCostIntervalKeyPG{}).(*rootPublicationCostIntervalPG); ok {
		interval.cost.batchResults++
		interval.cost.phase(rootPublicationCostSQLPhasePG(data.SQL)).Rows += data.CommandTag.RowsAffected()
		if data.Err != nil {
			interval.cost.errors++
		}
	}
}

func (rootPublicationCostTracerPG) TraceBatchEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceBatchEndData) {
	if interval, ok := ctx.Value(rootPublicationCostIntervalKeyPG{}).(*rootPublicationCostIntervalPG); ok {
		interval.finish(data.Err)
		interval.cost.batchEnds++
	}
}

type rootPublicationCostConnectionPG struct {
	net.Conn
	received *atomic.Int64
}

func (connection rootPublicationCostConnectionPG) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	connection.received.Add(int64(count))
	return count, err
}

func rootPublicationCostPoolPG(test *testing.T, ctx context.Context, source *pgxpool.Pool) (*pgxpool.Pool, *atomic.Int64, uint32) {
	test.Helper()
	received := &atomic.Int64{}
	config := source.Config().Copy()
	config.MaxConns, config.MinConns, config.MinIdleConns = 1, 0, 0
	config.HealthCheckPeriod = time.Hour
	config.ConnConfig.Tracer = rootPublicationCostTracerPG{}
	dial := config.ConnConfig.DialFunc
	config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return rootPublicationCostConnectionPG{Conn: connection, received: received}, nil
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(pool.Close)
	connection, err := pool.Acquire(ctx)
	if err != nil {
		test.Fatal(err)
	}
	pid := connection.Conn().PgConn().PID()
	connection.Release()
	if err := pool.Ping(ctx); err != nil {
		test.Fatal(err)
	}
	return pool, received, pid
}

func rootPublicationMeasurePG(ctx context.Context, received *atomic.Int64, afterFence func(context.Context, *pgx.Conn), operation func(context.Context) error) (*rootPublicationCostPG, error) {
	cost := &rootPublicationCostPG{received: received, phases: make(map[string]*rootPublicationCostPhasePG), kinds: make(map[string]int), afterFence: afterFence}
	observed := context.WithValue(ctx, rootPublicationCostKeyPG{}, cost)
	before := received.Load()
	started := time.Now()
	err := operation(observed)
	cost.elapsed, cost.receivedBytes = time.Since(started), received.Load()-before
	return cost, err
}

func (cost *rootPublicationCostPG) report(test *testing.T, method string) {
	test.Helper()
	var queryWall time.Duration
	var phaseBytes int64
	for _, phase := range cost.phases {
		queryWall += phase.WallNS
		phaseBytes += phase.ReceivedBytes
	}
	var transaction, critical, lock time.Duration
	if !cost.committed.IsZero() {
		transaction = cost.committed.Sub(cost.transactionStarted)
		critical = cost.committed.Sub(cost.lockAcquired)
		lock = cost.lockAcquired.Sub(cost.lockStarted)
		if cost.phase("begin").Statements != 1 || cost.phase("commit").Statements != 1 || cost.phase("rollback").Statements != 0 {
			test.Error("expected exactly one committed outer repository transaction")
		}
	}
	metadata, err := json.Marshal(struct {
		Kinds  map[string]int                         `json:"sql_kind_counts"`
		Phases map[string]*rootPublicationCostPhasePG `json:"phases"`
	}{cost.kinds, cost.phases})
	if err != nil {
		test.Fatal(err)
	}
	test.Logf("root_publication_cost method=%s elapsed=%s final_transaction=%s repository_lock_query=%s repository_critical_through_commit=%s pgx_wall=%s non_sql_residual=%s received_bytes=%d unattributed_received_bytes=%d query_calls=%d batch_calls=%d batch_queued=%d batch_results=%d sql_statements=%d metadata=%s",
		method, cost.elapsed, transaction, lock, critical, queryWall, cost.elapsed-queryWall, cost.receivedBytes, cost.receivedBytes-phaseBytes,
		cost.queryCalls, cost.batchCalls, cost.batchQueued, cost.batchResults, cost.queryCalls+cost.batchQueued, metadata)
	if cost.errors != 0 || cost.queryCalls != cost.queryEnds || cost.batchCalls != cost.batchEnds || cost.batchQueued != cost.batchResults || cost.receivedBytes <= 0 || phaseBytes > cost.receivedBytes {
		test.Error("incomplete, failed, or inconsistent PostgreSQL cost trace")
	}
}

func rootPublicationCostOptInPG(test *testing.T) {
	test.Helper()
	if os.Getenv("CXT_ROOT_PUBLICATION_COST") != "1" {
		test.Skip("set CXT_ROOT_PUBLICATION_COST=1 for disposable local PostgreSQL measurements")
	}
	if strings.TrimSpace(os.Getenv("CXT_TEST_DSN")) == "" {
		test.Skip("CXT_TEST_DSN unset; no fixture construction or aggregate measurements")
	}
}

func rootPublicationCostFixturePG(test *testing.T, payloadBytes int) (*PostgresStore, context.Context, domain.DocFinalizationJob) {
	test.Helper()
	storage, ctx := chunkReusePG(test)
	ctx = rootPublicationContext(ctx)
	fixture := rootFixture(test, "")
	fixture.cir.Envelope.SessionOriginID += ":" + time.Now().UTC().Format(time.RFC3339Nano)
	for position := range payloadBytes / 1024 {
		text := fmt.Sprintf("event-%d ", position) + strings.Repeat("x", 1024)
		event := domain.CIREvent{Seq: position, Kind: domain.EventMessage, Role: domain.RoleUser, Blocks: []domain.ContentBlock{{Type: "text", Text: text}}}
		if position%4 != 0 {
			event.Kind, event.Role, event.Blocks = domain.EventToolResult, "", nil
			event.CallID, event.Output = fmt.Sprintf("tool-%d", position), text
		}
		fixture.cir.Events = append(fixture.cir.Events, event)
	}
	var err error
	fixture.manifest, fixture.bodies, err = domain.ConversationManifestForCIR(fixture.cir)
	if err != nil {
		test.Fatal(err)
	}
	fixture.hash, err = domain.ConversationManifestHash(fixture.manifest)
	if err != nil {
		test.Fatal(err)
	}
	fixture.manifestBytes, err = domain.CanonicalConversationManifest(fixture.manifest)
	if err != nil {
		test.Fatal(err)
	}
	fixture.canonical, err = domain.CanonicalBytes(fixture.cir)
	if err != nil {
		test.Fatal(err)
	}
	job, _ := rootPublicationClaimPG(test, storage, ctx, fixture, "")
	var storedBytes int64
	if err := storage.pool.QueryRow(ctx, `SELECT coalesce(sum(octet_length(b.bytes)),0)::bigint
 FROM repo_blobs rb JOIN blobs b ON b.hash=rb.hash WHERE rb.repo_id=$1 AND rb.kind='chunk'`, job.RepoID).Scan(&storedBytes); err != nil {
		test.Fatal(err)
	}
	test.Logf("fixture nominal_payload_bytes=%d canonical_bytes=%d root_manifest_bytes=%d manifest_chunks=%d unique_chunks=%d stored_chunk_bytes=%d events=%d corpus=75pct_tool_results_1KiB_text_real_CIR setup_excluded=true",
		payloadBytes, len(fixture.canonical), len(fixture.manifestBytes), len(fixture.manifest.Chunks), len(fixture.bodies), storedBytes, len(fixture.cir.Events))
	return storage, ctx, job
}

func rootPublicationCostWriterPG(ctx context.Context, writer *PostgresStore, repo domain.ContentHash) error {
	return writer.WithinRepository(ctx, repo, func(bound context.Context) error {
		tag, err := writer.db(bound).Exec(bound, `UPDATE repos SET default_branch='root-publication-cost-writer' WHERE id=$1`, repo)
		if err == nil && tag.RowsAffected() != 1 {
			return fmt.Errorf("metadata writer updated %d repositories, want 1", tag.RowsAffected())
		}
		return err
	})
}

func rootPublicationCostPublishedPG(test *testing.T, storage *PostgresStore, ctx context.Context, job domain.DocFinalizationJob) {
	test.Helper()
	done, err := storage.GetDocJob(ctx, job.RepoID, job.ID)
	if err != nil || done.State != "completed" || done.DocumentRef() != job.DocumentRef() || done.Version != job.Version || !done.LeaseUntil.IsZero() {
		test.Fatalf("publication receipt mismatch: state=%s error=%v", done.State, err)
	}
	owned, err := storage.HasDocs(ctx, job.RepoID, []domain.ContentHash{job.DocHash})
	if err != nil || len(owned) != 1 {
		test.Fatal("published root ownership missing", err)
	}
	header, err := storage.DocRangeIndex(ctx, job.RepoID, job.DocHash)
	if err != nil || header.Hash != job.DocHash || len(header.Pages) == 0 {
		test.Fatal("published range index missing", err)
	}
	if _, err := storage.DocRangePage(ctx, job.RepoID, header, 0); err != nil {
		test.Fatal("published range page unreadable", err)
	}
	if err := storage.WithinReadSnapshot(ctx, func(bound context.Context) error {
		visible, err := storage.OpenDocVisibleGeneration(bound, job.RepoID, job.DocHash)
		if err != nil {
			return err
		}
		descriptor := visible.Descriptor()
		if descriptor.Hash != job.DocHash || int64(descriptor.Events) != header.Manifest.EventCount {
			return fmt.Errorf("visible publication does not match the completed document")
		}
		for _, mode := range []string{"all", "chat", "prompts"} {
			root, exists := descriptor.Views[mode]
			if !exists || root.Units == 0 {
				return fmt.Errorf("visible publication has no %s units", mode)
			}
			for _, position := range []int{0, root.Units - 1} {
				if _, _, err := visible.Unit(bound, mode, position, false); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		test.Fatal("published visible index unreadable", err)
	}
	var branch string
	if err := storage.pool.QueryRow(ctx, `SELECT default_branch FROM repos WHERE id=$1`, job.RepoID).Scan(&branch); err != nil || branch != "root-publication-cost-writer" {
		test.Fatal("independent metadata writer did not commit", err)
	}
}

type rootPublicationCostResultPG struct {
	cost *rootPublicationCostPG
	err  error
}

func TestPGRootPublicationCost(test *testing.T) {
	rootPublicationCostOptInPG(test)
	test.Log("one raw sample per size; fresh publication, preconnected pools, ordinary database/OS caches; not a cold-cache, percentile, throughput, or D1b-comparable corpus claim")
	test.Log("received_bytes counts socket reads including protocol/TLS overhead, not decoded payload; pgx SQL counts include batches but exclude protocol prepares and server-internal/trigger SQL; batch_io owns batch timing/bytes, not individual batch statements")
	test.Log("SQL phase names classify client-issued statements, not domain CPU phases; range_publication includes the shared range/visible object table; query wall time includes client row consumption")
	test.Log("final_transaction is BEGIN start through COMMIT end; repository_critical starts after actual repository lock acquisition; non_sql_residual includes decode/hash/spool/client/pool/trace overhead, not a CPU profile; no SQL arguments, SQL text, DSN, or secrets are logged")
	for _, sizeMiB := range []int{4, 32} {
		test.Run(fmt.Sprintf("%dMiB", sizeMiB), func(test *testing.T) {
			directory := rangeSpoolTempDir(test)
			storage, ctx, job := rootPublicationCostFixturePG(test, sizeMiB<<20)
			original := storage.pool
			pool, received, publisherPID := rootPublicationCostPoolPG(test, ctx, original)
			writerPool, writerReceived, writerPID := rootPublicationCostPoolPG(test, ctx, original)
			if publisherPID == writerPID {
				test.Fatal("writer must use an independent PostgreSQL backend")
			}
			storage.pool = pool
			test.Cleanup(func() { storage.pool = original })
			var publication outbound.PreparedRootDocPublication
			prepare, err := rootPublicationMeasurePG(ctx, received, nil, func(observed context.Context) error {
				var prepareErr error
				publication, prepareErr = storage.PrepareRootDocJob(observed, job)
				return prepareErr
			})
			if err != nil || publication == nil {
				test.Fatal("rootPrepare", err)
			}
			test.Cleanup(func() {
				if err := publication.Close(); err != nil {
					test.Error(err)
				}
				assertRangeSpoolsRemoved(test, directory)
			})
			prepare.report(test, "rootPrepare")
			assertRootStreamingUnpublishedPG(test, storage, ctx, job)
			writer := &PostgresStore{pool: writerPool}
			writerStart := make(chan struct{})
			writerDone := make(chan rootPublicationCostResultPG, 1)
			go func() {
				select {
				case <-writerStart:
					cost, err := rootPublicationMeasurePG(ctx, writerReceived, nil, func(observed context.Context) error {
						return rootPublicationCostWriterPG(observed, writer, job.RepoID)
					})
					writerDone <- rootPublicationCostResultPG{cost, err}
				case <-ctx.Done():
					writerDone <- rootPublicationCostResultPG{err: ctx.Err()}
				}
			}()
			var signal sync.Once
			complete, err := rootPublicationMeasurePG(ctx, received, func(context.Context, *pgx.Conn) {
				signal.Do(func() { close(writerStart) })
			}, func(observed context.Context) error { return publication.Complete(observed, job, time.Now()) })
			signal.Do(func() { close(writerStart) })
			result := <-writerDone
			if err != nil || result.err != nil {
				test.Fatalf("complete=%v writer=%v", err, result.err)
			}
			if complete.committed.IsZero() || result.cost.committed.IsZero() {
				test.Fatal("publication and independent writer must both commit real transactions")
			}
			complete.report(test, "rootComplete_unpaused")
			result.cost.report(test, "contending_metadata_writer_unpaused")
			overlapped := !result.cost.lockStarted.IsZero() && result.cost.lockStarted.Before(complete.committed)
			test.Logf("unpaused_writer_lock_attempt_before_publisher_commit_response=%t scheduling_overlap_not_guaranteed=true writer=real_metadata_update independent_backends=true", overlapped)
			rootPublicationCostPublishedPG(test, storage, ctx, job)
		})
	}
}

func TestPGRootPublicationCostWriterBarrier(test *testing.T) {
	rootPublicationCostOptInPG(test)
	test.Log("correctness only: publication deliberately pauses inside its repository critical section; all barrier-run durations are excluded from performance results")
	directory := rangeSpoolTempDir(test)
	storage, ctx, job := rootPublicationCostFixturePG(test, 4<<20)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	original := storage.pool
	pool, received, publisherPID := rootPublicationCostPoolPG(test, ctx, original)
	writerPool, _, writerPID := rootPublicationCostPoolPG(test, ctx, original)
	if publisherPID == writerPID {
		test.Fatal("writer must use an independent PostgreSQL backend")
	}
	storage.pool = pool
	test.Cleanup(func() { storage.pool = original })
	publication := prepareRootStreamingPG(test, storage, ctx, job, directory)
	reached, release := make(chan struct{}), make(chan struct{})
	var signal, resume sync.Once
	unpause := func() { resume.Do(func() { close(release) }) }
	defer unpause()
	publisherDone := make(chan error, 1)
	go func() {
		defer close(publisherDone)
		_, err := rootPublicationMeasurePG(ctx, received, func(bound context.Context, connection *pgx.Conn) {
			if connection.PgConn().PID() != publisherPID {
				cancel()
				return
			}
			signal.Do(func() { close(reached) })
			select {
			case <-release:
			case <-bound.Done():
			}
		}, func(observed context.Context) error { return publication.Complete(observed, job, time.Now()) })
		publisherDone <- err
	}()
	defer func() {
		unpause()
		cancel()
		<-publisherDone
	}()
	select {
	case <-reached:
	case err := <-publisherDone:
		test.Fatal("publication did not reach the critical-section barrier", err)
	case <-ctx.Done():
		test.Fatal("publication barrier deadline", ctx.Err())
	}
	writer := &PostgresStore{pool: writerPool}
	writerDone := make(chan error, 1)
	go func() {
		defer close(writerDone)
		writerDone <- rootPublicationCostWriterPG(ctx, writer, job.RepoID)
	}()
	defer func() {
		unpause()
		cancel()
		<-writerDone
	}()
	probe, stopProbe := context.WithTimeout(ctx, 3*time.Second)
	defer stopProbe()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blocked bool
		err := original.QueryRow(probe, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, publisherPID, writerPID).Scan(&blocked)
		if err != nil {
			test.Fatal("observe independent writer lock wait", err)
		}
		if blocked {
			break
		}
		select {
		case err := <-writerDone:
			test.Fatal("writer escaped publication repository lock", err)
		case <-probe.Done():
			test.Fatal("writer was not observed blocked by publication", probe.Err())
		case <-ticker.C:
		}
	}
	select {
	case err := <-writerDone:
		test.Fatal("writer completed while publication remained paused", err)
	default:
	}
	unpause()
	if err := <-publisherDone; err != nil {
		test.Fatal("publication after barrier release", err)
	}
	if err := <-writerDone; err != nil {
		test.Fatal("writer after publication commit", err)
	}
	rootPublicationCostPublishedPG(test, storage, ctx, job)
	test.Log("barrier_result=pass PostgreSQL_confirmed_publisher_blocks_independent_writer=true publication_and_real_metadata_update_committed=true performance_sample=false")
}
