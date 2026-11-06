package graphdiagnostics

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDiagnosticsDefaultTextHandlerHasStructuredFields(test *testing.T) {
	ctx, recorder := NewContext(context.Background())
	ctx, span := Start(ctx, Snapshots)
	SelectRoute(ctx, Legacy)
	StartSQL(ctx).End(3, true, false)
	RecordCache(ctx, GraphCache, Hit)
	span.End()
	var output bytes.Buffer
	slog.New(slog.NewTextHandler(&output, nil)).Info("graph_read", "diagnostics", recorder.Snapshot())
	for _, field := range []string{"diagnostics.route=legacy", "diagnostics.sql.count=1", "diagnostics.sql.rows=3", "diagnostics.stages.snapshots.count=1", "diagnostics.caches.graph.hit=1"} {
		if !strings.Contains(output.String(), field) {
			test.Fatal("text logger lost structured field", field, output.String())
		}
	}
}

func TestDiagnosticsNoObserverIsAllocationFree(test *testing.T) {
	ctx := context.Background()
	allocations := testing.AllocsPerRun(100, func() {
		bound, span := Start(ctx, Query)
		if bound != ctx || span != nil || StartSQL(ctx) != nil {
			test.Fatal("unobserved call changed context or created measurements")
		}
		span.End()
		SelectRoute(ctx, Legacy)
		RecordCache(ctx, GraphCache, Miss)
		_, _ = Read(ctx, Query, func(bound context.Context) (int, error) { return 1, bound.Err() })
	})
	if allocations != 0 {
		test.Fatal("unobserved calls allocate", allocations)
	}
}

func TestDiagnosticsNestedStagesAndSQLAttribution(test *testing.T) {
	ctx, recorder := NewContext(context.Background())
	parent, outer := Start(ctx, LegacyRead)
	child, inner := Start(parent, Snapshots)
	inner.started = time.Now().Add(-10 * time.Millisecond)
	StartSQL(child).End(7, true, false)
	StartSQL(child).End(100, true, true)
	StartSQL(parent).End(0, false, false)
	inner.End()
	outer.End()
	SelectRoute(ctx, Legacy)
	SelectRoute(ctx, Published)
	RecordCache(ctx, GraphCache, Miss)
	RecordCache(ctx, GraphCache, Hit)
	RecordCache(ctx, BranchCache, Ineligible)
	result := recorder.Snapshot()
	if result.Route != "mixed" || result.LegacyReads != 1 || result.PublishedReads != 1 || result.SQL.Count != 3 || result.SQL.Rows != 7 || result.SQL.RowsKnown != 1 || result.SQL.RowsUnknown != 2 || result.SQL.Errors != 1 {
		test.Fatal("incorrect request aggregates", result)
	}
	if len(result.Stages) != 2 || result.Stages[0].Name != "legacy_read" || result.Stages[0].SQL.Count != 1 || result.Stages[1].Name != "snapshots" || result.Stages[1].SQL.Count != 2 || result.Stages[1].ElapsedMS < 10 {
		test.Fatal("SQL counted twice or timing lost", result.Stages)
	}
	if len(result.Caches) != 2 || result.Caches[0].Hit != 1 || result.Caches[0].Miss != 1 || result.Caches[1].Ineligible != 1 {
		test.Fatal("cache events lost", result.Caches)
	}
	if header := result.ServerTiming(20 * time.Millisecond); !strings.HasPrefix(header, "graph_pre;dur=20.000, graph_sql;dur=") || !strings.Contains(header, "snapshots;dur=") {
		test.Fatal("invalid timing header", header)
	}
	result.Stages[0].Count = 99
	if recorder.Snapshot().Stages[0].Count != 1 {
		test.Fatal("snapshot exposed mutable state")
	}
}

func TestDiagnosticsConcurrentRequestsRemainIsolatedAndBounded(test *testing.T) {
	first, recorder := NewContext(context.Background())
	second, other := NewContext(first)
	var workers sync.WaitGroup
	for index := 0; index < 40; index++ {
		workers.Go(func() {
			ctx, span := Start(first, History)
			StartSQL(ctx).End(2, true, false)
			RecordCache(ctx, GraphCache, Miss)
			span.End()
			_ = recorder.Snapshot()
		})
	}
	workers.Wait()
	StartSQL(second).End(3, true, false)
	if snapshot := recorder.Snapshot(); snapshot.SQL.Count != 40 || snapshot.SQL.Rows != 80 || len(snapshot.Stages) != 1 || snapshot.Stages[0].Count != 40 {
		test.Fatal("concurrent measurements lost", snapshot)
	}
	if snapshot := other.Snapshot(); snapshot.SQL.Count != 1 || snapshot.SQL.Rows != 3 || len(snapshot.Stages) != 0 || len(snapshot.Caches) != 0 {
		test.Fatal("request measurements leaked", snapshot)
	}
	if ctx, span := Start(first, Stage(255)); ctx != first || span != nil {
		test.Fatal("unknown stage created telemetry")
	}
	SelectRoute(first, Route(255))
	RecordCache(first, Cache(255), Hit)
	RecordCache(first, GraphCache, CacheResult(255))
}
