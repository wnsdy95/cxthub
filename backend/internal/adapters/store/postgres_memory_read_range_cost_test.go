//go:build postgres

package store

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type memoryReadRangeTraceKey struct{}

type memoryReadRangeCounts struct {
	queries atomic.Int64
	rows    atomic.Int64
}

type memoryReadRangeTracer struct{}

func (memoryReadRangeTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	return ctx
}

func (memoryReadRangeTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if counts, ok := ctx.Value(memoryReadRangeTraceKey{}).(*memoryReadRangeCounts); ok && data.CommandTag.Select() {
		counts.queries.Add(1)
		counts.rows.Add(data.CommandTag.RowsAffected())
	}
}

type memoryReadRangeCountingConnection struct {
	net.Conn
	bytes *atomic.Int64
}

func (connection memoryReadRangeCountingConnection) Read(buffer []byte) (int, error) {
	count, err := connection.Conn.Read(buffer)
	connection.bytes.Add(int64(count))
	return count, err
}

func TestPGMemoryReadRangeMeasuredCost(test *testing.T) {
	for _, count := range []int{640, 6400} {
		test.Run(fmt.Sprint(count), func(test *testing.T) {
			storage, ctx, bundle := memoryReadRangePGFixture(test, count)
			repo := bundle.Header.Source.Graph.RepoID
			var received atomic.Int64
			config := storage.pool.Config()
			config.MaxConns, config.MinConns = 1, 0
			config.ConnConfig.Tracer = memoryReadRangeTracer{}
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
			legacy := func(ctx context.Context) error {
				header, err := reader.ReadMemoryReadHeader(ctx, repo, bundle.ID)
				if err != nil {
					return err
				}
				for ordinal := range 2 {
					raw, err := reader.ReadMemoryReadChunk(ctx, repo, bundle.ID, ordinal)
					if err != nil {
						return err
					}
					if _, err := domain.DecodeMemoryReadChunk(ctx, header, ordinal, raw); err != nil {
						return err
					}
				}
				return nil
			}
			ranged := func(ctx context.Context) error {
				return reader.WithMemoryReadRange(ctx, repo, bundle.ID, func(bound context.Context, _ domain.MemoryReadHeader, chunks outbound.MemoryReadChunkReader) error {
					for ordinal := range 2 {
						if _, err := chunks.ReadChunk(bound, ordinal); err != nil {
							return err
						}
					}
					return nil
				})
			}
			var wires [2]int64
			var allocated [2]uint64
			for index, read := range []func(context.Context) error{legacy, ranged} {
				if err := read(ctx); err != nil {
					test.Fatal("warmup", err)
				}
				counts := &memoryReadRangeCounts{}
				observed := context.WithValue(ctx, memoryReadRangeTraceKey{}, counts)
				beforeWire := received.Load()
				if err := read(observed); err != nil {
					test.Fatal(err)
				}
				wires[index] = received.Load() - beforeWire
				queries, rows := int64(14), int64(11+3*len(bundle.Chunks))
				if index == 1 {
					queries, rows = 5, 5
				}
				if counts.queries.Load() != queries || counts.rows.Load() != rows {
					test.Fatalf("path=%d select_queries=%d rows=%d want %d/%d", index, counts.queries.Load(), counts.rows.Load(), queries, rows)
				}
				allocations := testing.AllocsPerRun(3, func() {
					if err := read(ctx); err != nil {
						test.Fatal(err)
					}
				})
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				for range 3 {
					if err := read(ctx); err != nil {
						test.Fatal(err)
					}
				}
				runtime.ReadMemStats(&after)
				allocated[index] = (after.TotalAlloc - before.TotalAlloc) / 3
				test.Logf("items=%d path=%d selected_chunks=2 SELECTs=%d rows=%d received_bytes=%d allocations=%.0f allocated_bytes=%d", count, index, queries, rows, wires[index], allocations, allocated[index])
			}
			if wires[1] >= wires[0] {
				test.Fatal("range did not reduce actual received SQL bytes", wires)
			}
			header, err := domain.MemoryReadHeaderBytes(ctx, bundle.Header)
			if err != nil {
				test.Fatal(err)
			}
			test.Logf("items=%d header_bytes=%d selected_payload_bytes=%d remaining_header_cost=O(index)", count, len(header), len(bundle.Chunks[0].Bytes)+len(bundle.Chunks[1].Bytes))
		})
	}
}
