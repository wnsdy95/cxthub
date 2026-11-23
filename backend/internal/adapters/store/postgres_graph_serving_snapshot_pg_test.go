//go:build postgres

package store

import (
	"context"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5"
)

type graphServingSizePauseKey struct{}

type graphServingSizePause struct {
	prefix  string
	reached chan struct{}
	release chan struct{}
	once    sync.Once
}

func (pause *graphServingSizePause) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.HasPrefix(data.SQL, pause.prefix) {
		return context.WithValue(ctx, graphServingSizePauseKey{}, true)
	}
	return ctx
}

func (pause *graphServingSizePause) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	if ctx.Value(graphServingSizePauseKey{}) != true {
		return
	}
	pause.once.Do(func() {
		close(pause.reached)
		select {
		case <-pause.release:
		case <-ctx.Done():
		}
	})
}
