//go:build postgres

package store

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
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
