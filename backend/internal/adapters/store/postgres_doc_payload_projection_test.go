//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type docPayloadProjectionCancelKeyPG struct{}
type docPayloadProjectionInsertKeyPG struct{}

type docPayloadProjectionCancelPG struct {
	cancel    context.CancelFunc
	triggered bool
}

type docPayloadProjectionTracerPG struct{}

func (docPayloadProjectionTracerPG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryStartData) context.Context {
	if observed, ok := ctx.Value(docPayloadProjectionCancelKeyPG{}).(*docPayloadProjectionCancelPG); ok &&
		strings.HasPrefix(query.SQL, "INSERT INTO doc_range_generations_v2(") {
		return context.WithValue(ctx, docPayloadProjectionInsertKeyPG{}, observed)
	}
	return ctx
}

func (docPayloadProjectionTracerPG) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, query pgx.TraceQueryEndData) {
	if observed, ok := ctx.Value(docPayloadProjectionInsertKeyPG{}).(*docPayloadProjectionCancelPG); ok &&
		!observed.triggered && query.Err == nil && query.CommandTag.RowsAffected() == 1 {
		observed.triggered = true
		observed.cancel()
	}
}
