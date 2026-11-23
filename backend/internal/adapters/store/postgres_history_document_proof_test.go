//go:build postgres

package store

import (
	"context"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
)

type historyProofTracePG struct {
	reads, pins int
	chunkKeys   map[string]bool
	err         error
}

func (tr *historyProofTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.Contains(q.SQL, "b.bytes") {
		tr.reads++
		if strings.Contains(q.SQL, "rb.kind='chunk'") {
			for _, key := range q.Args[1].([]string) {
				tr.chunkKeys[key] = true
			}
		}
	}
	if strings.HasSuffix(q.SQL, "FOR SHARE NOWAIT") {
		tr.pins++
		if len(q.Args[len(q.Args)-1].([]string)) > 16 {
			tr.err = errors.New("unbounded pin query")
		}
	}
	return ctx
}
func (*historyProofTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

type historyProofCommitRaceTracePG struct {
	target            string
	start, committing chan struct{}
}

func (tr *historyProofCommitRaceTracePG) TraceQueryStart(ctx context.Context, _ *pgx.Conn, q pgx.TraceQueryStartData) context.Context {
	if strings.Contains(q.SQL, "FROM blobs") && strings.HasSuffix(q.SQL, "FOR SHARE NOWAIT") {
		for _, key := range q.Args[0].([]string) {
			if key == tr.target {
				// Synchronize at the actual final-blob locking query, after
				// earlier batches, not merely at the start of Pin.
				close(tr.start)
				select {
				case <-tr.committing:
				case <-ctx.Done():
				}
			}
		}
	}
	return ctx
}
func (*historyProofCommitRaceTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {
}
