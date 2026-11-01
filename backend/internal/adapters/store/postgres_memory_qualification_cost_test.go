//go:build postgres

package store

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryQualificationCostTracer struct {
	memoryReadRangeTracer
	fullRead atomic.Bool
}

func (tracer *memoryQualificationCostTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	query := strings.Join(strings.Fields(strings.ToLower(data.SQL)), " ")
	if (strings.Contains(query, "blobs") && strings.Contains(query, "bytes")) || strings.Contains(query, "from snapshots") || strings.Contains(query, "from history_events") {
		tracer.fullRead.Store(true)
	}
	return ctx
}

func TestPGMemoryQualificationMeasuredSelectedRangeCost(test *testing.T) {
	var wires, rows, queries []int64
	for _, size := range []int{100, 1000} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			storage, ctx, qualification, _ := memoryQualificationSizedPG(test, size)
			identifier, err := storage.QualifyMemoryRead(ctx, qualification)
			if err != nil {
				test.Fatal(err)
			}
			var received atomic.Int64
			tracer := &memoryQualificationCostTracer{}
			config := storage.pool.Config()
			config.MaxConns, config.MinConns = 1, 0
			config.ConnConfig.Tracer = tracer
			dial := config.ConnConfig.DialFunc
			config.ConnConfig.DialFunc = func(ctx context.Context, network, address string) (net.Conn, error) {
				connection, err := dial(ctx, network, address)
				if err != nil {
					return nil, err
				}
				return memoryReadRangeCountingConnection{Conn: connection, bytes: &received}, nil
			}
			pool, err := pgxpool.NewWithConfig(ctx, config)
			if err != nil {
				test.Fatal(err)
			}
			defer pool.Close()
			reader := &PostgresStore{pool: pool}
			read := func(ctx context.Context) error {
				return reader.WithQualifiedMemoryReadRange(ctx, qualification.Source.Graph.RepoID, identifier, memoryQualificationAllowPG,
					func(bound context.Context, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
						_, err := chunks.ReadChunk(bound, 0)
						return err
					})
			}
			if err := read(ctx); err != nil {
				test.Fatal(err)
			}
			counts := &memoryReadRangeCounts{}
			before := received.Load()
			if err := read(context.WithValue(ctx, memoryReadRangeTraceKey{}, counts)); err != nil {
				test.Fatal(err)
			}
			wire := received.Load() - before
			wires, rows, queries = append(wires, wire), append(rows, counts.rows.Load()), append(queries, counts.queries.Load())
			if tracer.fullRead.Load() || wire > 64<<10 || counts.rows.Load() > 32 || counts.queries.Load() > 16 {
				test.Fatalf("qualified page exceeded bounded metadata/range reads: full=%v SELECTs=%d rows=%d wire=%d", tracer.fullRead.Load(), counts.queries.Load(), counts.rows.Load(), wire)
			}
			test.Logf("unrelated_objects=%d selected_output_chunks=1 SELECTs=%d rows=%d received_bytes=%d", size, counts.queries.Load(), counts.rows.Load(), wire)
		})
	}
	if len(wires) != 2 || queries[0] != queries[1] || rows[0] != rows[1] || wires[1]-wires[0] > 256 {
		test.Fatal("unrelated growth increased current range cost", queries, rows, wires)
	}
}
