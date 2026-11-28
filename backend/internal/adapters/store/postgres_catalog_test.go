//go:build postgres

package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// These tests deliberately mix public adapter calls and direct SQL. A catalog
// that only instruments graph publication misses staged and maintenance writes.
func catalogPG(t *testing.T) (*PostgresStore, context.Context, domain.ContentHash) {
	t.Helper()
	s, ctx := chunkReusePG(t)
	repo := domain.HashContent([]byte(t.Name() + time.Now().String()))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	return s, ctx, repo
}

type catalogPGExecutor interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}

func catalogExecPG(t *testing.T, ctx context.Context, db catalogPGExecutor, sql string, args ...any) {
	t.Helper()
	if _, err := db.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("catalog fixture SQL: %v", err)
	}
}

func catalogTxPG(t *testing.T, ctx context.Context, s *PostgresStore) pgx.Tx {
	t.Helper()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}

type catalogStatePG struct {
	epoch string
	head  int64
	floor int64
}

func catalogReadStatePG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash) catalogStatePG {
	t.Helper()
	var got catalogStatePG
	if err := s.pool.QueryRow(ctx, `SELECT epoch::text,head_seq,floor_seq FROM repository_catalog_state WHERE repo_id=$1`, string(repo)).Scan(&got.epoch, &got.head, &got.floor); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestPGCatalogBulkTransactionUpdatesStateRowOnce(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	state := catalogReadStatePG(t, ctx, s, repo)
	tx := catalogTxPG(t, ctx, s)
	stateUpdates := func() int64 {
		t.Helper()
		var count int64
		if err := tx.QueryRow(ctx, `SELECT pg_stat_get_xact_tuples_updated('repository_catalog_state'::regclass)`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	// Read both counters through this transaction's connection. Global table
	// statistics would include concurrent tests, and counting journal sequences
	// alone would miss repeated physical rewrites of the same state tuple.
	before := stateUpdates()
	const batch = 64
	catalogExecPG(t, ctx, tx, `INSERT INTO context_history(repo_id,id,event)
		SELECT $1,lpad(to_hex(n),32,'0'),jsonb_build_object(
			'id',lpad(to_hex(n),32,'0'),'repo_id',$1::text,
			'branch_id','bulk-'||n::text,'branch','bulk-'||n::text,
			'kind','orphan','created_at',statement_timestamp())
		FROM generate_series(1,$2::int) n`, string(repo), batch)
	after := stateUpdates()
	var entries, sequences int
	var sequence int64
	if err := tx.QueryRow(ctx, `SELECT count(*),count(DISTINCT seq),min(seq)
		FROM repository_catalog_changes WHERE repo_id=$1 AND entity_kind='history' AND seq>$2`, string(repo), state.head).Scan(&entries, &sequences, &sequence); err != nil {
		t.Fatal(err)
	}
	if entries != batch || sequences != 1 || sequence != state.head+1 {
		t.Fatalf("bulk fixture must journal all entities at one new sequence: entries=%d sequences=%d sequence=%d", entries, sequences, sequence)
	}
	t.Logf("catalog state tuple updates: before=%d after=%d delta=%d entities=%d", before, after, after-before, batch)
	if updates := after - before; updates != 1 {
		t.Fatalf("bulk transaction rewrote catalog state %d times for %d entities; want exactly 1 update", updates, batch)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got.head != state.head+1 || got.epoch != state.epoch {
		t.Fatalf("bulk transaction did not commit exactly one catalog generation: %+v -> %+v", state, got)
	}
}
