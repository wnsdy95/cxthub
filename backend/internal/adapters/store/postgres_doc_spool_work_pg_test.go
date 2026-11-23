//go:build postgres

package store

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

type docSpoolWorkPGTrace struct {
	sourceReads     atomic.Int64
	privateWrites   atomic.Int64
	authorityChecks atomic.Int64
	writeMu         sync.Mutex
	writeQueries    map[string]int
	writeDocument   domain.ContentHash
	unboundWrites   int
}

type docSpoolWorkPGRead struct {
	offset int
	length int
}

type docSpoolWorkPGSource struct {
	spool *legacyDocSpool
	reads []docSpoolWorkPGRead
}

func (source *docSpoolWorkPGSource) Append(ctx context.Context, body []byte) error {
	return source.spool.Append(ctx, body)
}

func (source *docSpoolWorkPGSource) Read(ctx context.Context, offset, length int) ([]byte, error) {
	source.reads = append(source.reads, docSpoolWorkPGRead{offset: offset, length: length})
	return source.spool.Read(ctx, offset, length)
}

func (trace *docSpoolWorkPGTrace) TraceQueryStart(ctx context.Context, connection *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if strings.Contains(strings.ReplaceAll(query.SQL, "octet_length(source.bytes)", ""), "source.bytes") {
		trace.sourceReads.Add(1)
	}
	statement := strings.ToUpper(strings.Join(strings.Fields(query.SQL), " "))
	if strings.HasPrefix(statement, "WITH RETAINED_SOURCE AS MATERIALIZED") &&
		strings.Contains(statement, "RETAINED_JOB AS MATERIALIZED") && !strings.Contains(statement, "RETAINED_STAGE AS MATERIALIZED") {
		trace.authorityChecks.Add(1)
	}
	mutation := strings.HasPrefix(statement, "INSERT ") || strings.HasPrefix(statement, "UPDATE ") ||
		strings.HasPrefix(statement, "DELETE ") || strings.HasPrefix(statement, "COPY ")
	if mutation && strings.Contains(statement, "DOC_LEGACY_BYTE_STAGE") {
		trace.privateWrites.Add(1)
	}
	if strings.HasPrefix(statement, "WITH ") {
		unlocked := strings.ReplaceAll(statement, "FOR UPDATE", "FOR SHARE")
		mutation = strings.Contains(unlocked, " INSERT ") || strings.Contains(unlocked, " UPDATE ") ||
			strings.Contains(unlocked, " DELETE ") || strings.Contains(unlocked, " COPY ")
	}
	if mutation {
		trace.writeMu.Lock()
		if trace.writeQueries != nil {
			trace.writeQueries[statement]++
			if connection.PgConn().TxStatus() != 'T' || len(query.Args) == 0 || fmt.Sprint(query.Args[0]) != string(trace.writeDocument) {
				trace.unboundWrites++
			}
		}
		trace.writeMu.Unlock()
	}
	return ctx
}

func (*docSpoolWorkPGTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (trace *docSpoolWorkPGTrace) TraceBatchStart(ctx context.Context, connection *pgx.Conn, batch pgx.TraceBatchStartData) context.Context {
	for _, query := range batch.Batch.QueuedQueries {
		trace.TraceQueryStart(ctx, connection, pgx.TraceQueryStartData{SQL: query.SQL, Args: query.Arguments})
	}
	return ctx
}

func (*docSpoolWorkPGTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (*docSpoolWorkPGTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}
