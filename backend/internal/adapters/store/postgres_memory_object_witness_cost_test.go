//go:build postgres

package store

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGMemoryObjectWitnessMeasuredMetadataCost(test *testing.T) {
	storage, ctx, repo := catalogPG(test)
	digest := memoryCaptureTestDigest()
	digest.SnapshotID = repo
	digest.Summary = strings.Repeat("metadata cost "+string(repo), 2500)
	hash, err := storage.PutMemory(ctx, repo, digest)
	if err != nil {
		test.Fatal(err)
	}
	object := memoryWitnessPGCapture(test, storage, ctx, repo, hash)
	if len(object.Witness.Components) == 0 {
		test.Fatal("fixture did not create memory components")
	}
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
	validate := func(ctx context.Context) error {
		return reader.WithinReadSnapshot(ctx, func(bound context.Context) error {
			return reader.ValidateMemoryObjectWitnesses(bound, repo, []domain.MemoryObjectWitness{object.Witness})
		})
	}
	var wires []int64
	for _, size := range []int{100, 1000} {
		test.Run(fmt.Sprint(size), func(test *testing.T) {
			transaction := catalogTxPG(test, ctx, storage)
			catalogExecPG(test, ctx, transaction, `INSERT INTO blobs(hash,bytes) SELECT repeat(md5($1 || ordinal::text),2),convert_to(repeat('unrelated payload',1024),'UTF8') FROM generate_series(1,$2::int) ordinal ON CONFLICT(hash) DO NOTHING`, string(repo), size)
			catalogExecPG(test, ctx, transaction, `INSERT INTO repo_blobs(repo_id,kind,hash) SELECT $1,'memory',repeat(md5($1 || ordinal::text),2) FROM generate_series(1,$2::int) ordinal ON CONFLICT DO NOTHING`, string(repo), size)
			if err := transaction.Commit(ctx); err != nil {
				test.Fatal(err)
			}
			if err := validate(ctx); err != nil {
				test.Fatal(err)
			}
			counts := &memoryReadRangeCounts{}
			before := received.Load()
			if err := validate(context.WithValue(ctx, memoryReadRangeTraceKey{}, counts)); err != nil {
				test.Fatal(err)
			}
			wire := received.Load() - before
			wires = append(wires, wire)
			wanted := int64(1 + len(object.Witness.Components))
			if counts.queries.Load() != 1 || counts.rows.Load() != wanted || wire >= 8192 {
				test.Fatalf("metadata verification exceeded selected objects: SELECTs=%d rows=%d wire=%d", counts.queries.Load(), counts.rows.Load(), wire)
			}
			test.Logf("unrelated_objects=%d selected_objects=%d SELECTs=%d rows=%d received_bytes=%d logical_summary_bytes=%d", size, wanted, counts.queries.Load(), counts.rows.Load(), wire, len(digest.Summary))
		})
	}
	if len(wires) != 2 || wires[0] != wires[1] {
		test.Fatal("unrelated repository size changed verification wire cost", wires)
	}
}
