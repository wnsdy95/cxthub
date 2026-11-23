//go:build postgres

package store

import (
	"context"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

type docMaintenancePickerTraceKeyPG struct{}

type docMaintenancePausedPickerPG struct {
	selected chan int32
	resume   chan struct{}
	once     sync.Once
}

func (trace *docMaintenancePausedPickerPG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, "SELECT locked_cursor.after_source,work.source_hash,work.ready") {
		return context.WithValue(ctx, docMaintenancePickerTraceKeyPG{}, true)
	}
	return ctx
}

func (trace *docMaintenancePausedPickerPG) TraceQueryEnd(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(docMaintenancePickerTraceKeyPG{}) == true {
		trace.selected <- int32(connection.PgConn().PID())
		<-trace.resume
	}
}

func (trace *docMaintenancePausedPickerPG) release() {
	trace.once.Do(func() { close(trace.resume) })
}
