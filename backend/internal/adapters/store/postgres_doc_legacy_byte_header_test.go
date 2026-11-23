//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type legacyByteHeaderPGQueryKey struct{}

type legacyByteHeaderPGTrace struct {
	queries []string
	inserts []int
	cancel  context.CancelFunc
}

func (trace *legacyByteHeaderPGTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	trace.queries = append(trace.queries, query.SQL)
	if strings.HasPrefix(query.SQL, "INSERT INTO doc_legacy_byte_stage_headers_v1") {
		body, _ := query.Args[3].([]byte)
		trace.inserts = append(trace.inserts, len(body))
	}
	return context.WithValue(ctx, legacyByteHeaderPGQueryKey{}, query.SQL)
}

func (trace *legacyByteHeaderPGTrace) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	statement, _ := ctx.Value(legacyByteHeaderPGQueryKey{}).(string)
	if trace.cancel != nil && query.Err == nil && strings.HasPrefix(statement, "INSERT INTO doc_legacy_byte_stage_headers_v1") {
		trace.cancel()
	}
}
