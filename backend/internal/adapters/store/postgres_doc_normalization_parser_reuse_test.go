//go:build postgres

package store

import (
	"context"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

type legacyNormalizationParserReusePGCounts struct {
	queries          int
	bodyArguments    int
	bodyBytes        int
	maxBodyArguments int
	maxBodyBytes     int
	objectInserts    int
	hashReads        int
}

type legacyNormalizationParserReusePGTrace struct {
	mutex  sync.Mutex
	counts legacyNormalizationParserReusePGCounts
}

func (trace *legacyNormalizationParserReusePGTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.mutex.Lock()
	defer trace.mutex.Unlock()
	trace.counts.queries++
	arguments, size := 0, 0
	for _, argument := range query.Args {
		switch body := argument.(type) {
		case []byte:
			arguments++
			size += len(body)
		case [][]byte:
			for _, payload := range body {
				arguments++
				size += len(payload)
			}
		}
	}
	trace.counts.bodyArguments += arguments
	trace.counts.bodyBytes += size
	trace.counts.maxBodyArguments = max(trace.counts.maxBodyArguments, arguments)
	trace.counts.maxBodyBytes = max(trace.counts.maxBodyBytes, size)
	if strings.HasPrefix(query.SQL, "INSERT INTO doc_legacy_byte_stage_checkpoint_objects_v1") {
		trace.counts.objectInserts++
	}
	if strings.Contains(query.SQL, "FROM doc_legacy_byte_stage_checkpoint_objects_v1") &&
		strings.Contains(query.SQL, "encode(sha256(bytes),'hex')") {
		trace.counts.hashReads++
	}
	return ctx
}

func (*legacyNormalizationParserReusePGTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}

func (trace *legacyNormalizationParserReusePGTrace) TraceBatchStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	for _, query := range data.Batch.QueuedQueries {
		trace.TraceQueryStart(ctx, connection, pgx.TraceQueryStartData{SQL: query.SQL, Args: query.Arguments})
	}
	return ctx
}

func (*legacyNormalizationParserReusePGTrace) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {
}

func (*legacyNormalizationParserReusePGTrace) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {
}

func (trace *legacyNormalizationParserReusePGTrace) snapshot() legacyNormalizationParserReusePGCounts {
	trace.mutex.Lock()
	defer trace.mutex.Unlock()
	return trace.counts
}
