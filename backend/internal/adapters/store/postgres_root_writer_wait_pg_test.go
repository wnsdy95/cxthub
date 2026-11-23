//go:build postgres

package store

import (
	"context"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

type rootWriterWaitFenceKeyPG struct{}

type rootWriterWaitTracePG struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	pid     uint32
	pending bool
}

func (trace *rootWriterWaitTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	statement := strings.ToLower(strings.Join(strings.Fields(data.SQL), " "))
	if strings.Contains(statement, "from doc_finalization_jobs") && strings.Contains(statement, "for update") {
		return context.WithValue(ctx, rootWriterWaitFenceKeyPG{}, true)
	}
	return ctx
}

func (trace *rootWriterWaitTracePG) TraceQueryEnd(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryEndData) {
	if data.Err != nil || ctx.Value(rootWriterWaitFenceKeyPG{}) != true {
		return
	}
	trace.once.Do(func() {
		trace.pid = connection.PgConn().PID()
		trace.pending = true
		defer func() { trace.pending = false }()
		close(trace.reached)
		select {
		case <-trace.release:
		case <-ctx.Done():
		}
	})
}
