//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
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
	if hashes, ok := query.Args[2].([]string); ok && len(query.Args) > 3 {
		if bodies, ok := query.Args[3].([][]byte); ok {
			for ordinal, hash := range hashes {
				if _, found := trace.input[domain.ContentHash(hash)]; found && ordinal < len(bodies) && len(bodies[ordinal]) != 0 {
					trace.inputUploads.Add(1)
				}
			}
		}
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
