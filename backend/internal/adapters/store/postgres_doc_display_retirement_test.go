//go:build postgres

package store

import (
	"context"
	"strings"

	"github.com/jackc/pgx/v5"
)

type docRetirementCommitPG struct {
	pid    uint32
	resume chan struct{}
}

type docRetirementTracePG struct {
	commits   chan docRetirementCommitPG
	queries   []string
	arguments [][]any
}

func (trace *docRetirementTracePG) TraceQueryStart(ctx context.Context, connection *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "DELETE FROM doc_tool_key_observations_v1") {
		trace.queries = append(trace.queries, data.SQL)
		trace.arguments = append(trace.arguments, append([]any{}, data.Args...))
	}
	if strings.EqualFold(strings.TrimSpace(data.SQL), "commit") {
		gate := docRetirementCommitPG{pid: connection.PgConn().PID(), resume: make(chan struct{})}
		select {
		case trace.commits <- gate:
		case <-ctx.Done():
			return ctx
		}
		select {
		case <-gate.resume:
		case <-ctx.Done():
		}
	}
	return ctx
}

func (*docRetirementTracePG) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}
