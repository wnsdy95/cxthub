//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
)

func TestGraphQueryTracerNoObserverPreservesContext(test *testing.T) {
	ctx := context.Background()
	tracer := graphQueryTracer{}
	if allocations := testing.AllocsPerRun(100, func() {
		bound := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "secret-sql"})
		if bound != ctx {
			test.Fatal("unobserved SQL changed context")
		}
		tracer.TraceQueryEnd(bound, nil, pgx.TraceQueryEndData{})
	}); allocations != 0 {
		test.Fatal("unobserved SQL allocated", allocations)
	}
}

func TestGraphQueryTracerCountsOnlyKnownCommandRowsAndRedacts(test *testing.T) {
	ctx, recorder := graphdiagnostics.NewContext(context.Background())
	ctx, span := graphdiagnostics.Start(ctx, graphdiagnostics.Snapshots)
	tracer := graphQueryTracer{}
	for _, result := range []pgx.TraceQueryEndData{
		{CommandTag: pgconn.NewCommandTag("SELECT 4")},
		{CommandTag: pgconn.NewCommandTag("SELECT 0")},
		{CommandTag: pgconn.NewCommandTag("UPDATE 2")},
		{CommandTag: pgconn.NewCommandTag("BEGIN")},
		{CommandTag: pgconn.NewCommandTag("SELECT 99"), Err: errors.New("secret-error")},
		{},
	} {
		bound := tracer.TraceQueryStart(ctx, nil, pgx.TraceQueryStartData{SQL: "SELECT secret-sql", Args: []any{"secret-token", "secret-body"}})
		tracer.TraceQueryEnd(bound, nil, result)
	}
	span.End()
	snapshot := recorder.Snapshot()
	if snapshot.SQL.Count != 6 || snapshot.SQL.Rows != 6 || snapshot.SQL.RowsKnown != 3 || snapshot.SQL.RowsUnknown != 3 || snapshot.SQL.Errors != 1 || snapshot.SQL.ElapsedMS <= 0 || len(snapshot.Stages) != 1 || snapshot.Stages[0].SQL != snapshot.SQL {
		test.Fatal("inaccurate SQL metrics", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded), "secret-") || strings.Contains(string(encoded), "SELECT") {
		test.Fatal("query payload retained", string(encoded), err)
	}
	other, otherRecorder := graphdiagnostics.NewContext(ctx)
	bound := tracer.TraceQueryStart(other, nil, pgx.TraceQueryStartData{})
	tracer.TraceQueryEnd(bound, nil, pgx.TraceQueryEndData{CommandTag: pgconn.NewCommandTag("SELECT 1")})
	if recorder.Snapshot().SQL.Count != 6 || otherRecorder.Snapshot().SQL.Count != 1 {
		test.Fatal("SQL crossed request boundaries")
	}
}
