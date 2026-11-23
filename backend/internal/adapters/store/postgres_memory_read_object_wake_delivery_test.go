//go:build postgres

package store

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
)

type memoryReadObjectWakeSecondDeliveryPause struct {
	graphServingSizePause
	deletions atomic.Int64
}

func (pause *memoryReadObjectWakeSecondDeliveryPause) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, pause.prefix) && pause.deletions.Add(1) == 2 {
		return pause.graphServingSizePause.TraceQueryStart(ctx, connection, data)
	}
	return ctx
}
