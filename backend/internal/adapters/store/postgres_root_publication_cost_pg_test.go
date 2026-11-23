//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
