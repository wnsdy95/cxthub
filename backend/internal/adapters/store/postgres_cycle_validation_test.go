//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// Keep the former production query verbatim as an independent SQL oracle.
const cyclePGOldSQL = `
		WITH RECURSIVE edges(child,parent) AS (
			SELECT id, unnest(COALESCE(parents,'{}'::text[]) || COALESCE(graft_parents,'{}'::text[]))
			  FROM snapshots WHERE repo_id=$1
		), reach(start,node) AS (
			SELECT child,parent FROM edges
			UNION
			SELECT reach.start, edges.parent
			  FROM reach JOIN edges ON edges.child=reach.node
		)
		SELECT EXISTS(SELECT 1 FROM reach WHERE start=node)`

func cyclePGOld(ctx context.Context, tx pgx.Tx, repo domain.ContentHash) error {
	var cycle bool
	if err := tx.QueryRow(ctx, cyclePGOldSQL, string(repo)).Scan(&cycle); err != nil {
		return err
	}
	if cycle {
		return domain.ErrConflict
	}
	return nil
}

type cyclePGNode struct {
	name            string
	parents, grafts []string
}

// SQL NULL is deliberately distinct from any named (possibly absent) vertex.
const cyclePGNull = "<SQL NULL>"

func cyclePGSeed(t *testing.T, ctx context.Context, st *PostgresStore, nodes []cyclePGNode) (domain.ContentHash, map[string]domain.ContentHash) {
	t.Helper()
	repo := domain.HashContent([]byte(fmt.Sprintf("%s/%d", t.Name(), time.Now().UnixNano())))
	if _, err := st.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]domain.ContentHash)
	id := func(name string) domain.ContentHash { return domain.HashContent([]byte(string(repo) + "/" + name)) }
	array := func(names []string) []*string {
		out := make([]*string, len(names))
		for i, name := range names {
			if name != cyclePGNull {
				value := string(id(name))
				out[i] = &value
			}
		}
		return out
	}
	tx, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollbackPG(tx)
	var blobs, snapshots [][]any
	// Reverse order prevents insertion order from providing a topological sort.
	for i := len(nodes) - 1; i >= 0; i-- {
		n := nodes[i]
		hash := id(n.name)
		ids[n.name] = hash
		blobs = append(blobs, []any{string(hash), []byte(string(repo) + "/" + n.name)})
		snapshots = append(snapshots, []any{string(hash), string(repo), "main", string(hash), "unknown", "full", array(n.parents), array(n.grafts), false})
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"blobs"}, []string{"hash", "bytes"}, pgx.CopyFromRows(blobs)); err != nil {
		t.Fatal(err)
	}
	// Raw metadata is intentional: the old SQL accepts dangling/NULL edges that
	// GetSnapshot or publication validation need not accept. No user bodies.
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"snapshots"}, []string{"id", "repo_id", "branch", "doc_hash", "provider", "fidelity", "parents", "graft_parents", "grafted"}, pgx.CopyFromRows(snapshots)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return repo, ids
}

func cyclePGChain(n int) []cyclePGNode {
	out := make([]cyclePGNode, n)
	for i := range out {
		out[i].name = fmt.Sprintf("n%d", i)
		if i > 0 {
			out[i].parents = []string{out[i-1].name}
		}
	}
	return out
}

func cyclePGParity(t *testing.T, ctx context.Context, tx pgx.Tx, repo domain.ContentHash, wantCycle bool) {
	t.Helper()
	for _, check := range []struct {
		name string
		fn   func(context.Context, pgx.Tx, domain.ContentHash) error
	}{{"old SQL", cyclePGOld}, {"current guard", ensureNoReachabilityCycle}} {
		err := check.fn(ctx, tx, repo)
		if wantCycle && !errors.Is(err, domain.ErrConflict) || !wantCycle && err != nil {
			t.Fatalf("%s: cycle=%v, error=%v", check.name, wantCycle, err)
		}
	}
}

func TestPGCycleValidationSQLParity(t *testing.T) {
	chain := cyclePGChain(128)
	backEdge := cyclePGChain(128)
	backEdge[0].grafts = []string{"n127"}
	cases := []struct {
		name  string
		nodes []cyclePGNode
		cycle bool
	}{
		{"empty", nil, false},
		{"singleton", []cyclePGNode{{name: "a"}}, false},
		{"chain", chain, false},
		{"diamond", []cyclePGNode{{name: "a"}, {name: "b", parents: []string{"a"}}, {name: "c", parents: []string{"a"}}, {name: "d", parents: []string{"b", "c"}}}, false},
		{"mixed_edges_grafted_false", []cyclePGNode{{name: "a"}, {name: "b", grafts: []string{"a"}}, {name: "c", parents: []string{"b"}, grafts: []string{"a"}}}, false},
		{"duplicate_parent_and_graft", []cyclePGNode{{name: "a"}, {name: "b", parents: []string{"a", "a"}, grafts: []string{"a", "a"}}}, false},
		{"dangling", []cyclePGNode{{name: "a", parents: []string{"absent"}, grafts: []string{"also_absent"}}}, false},
		{"null_natural_and_graft", []cyclePGNode{{name: "a", parents: []string{cyclePGNull}, grafts: []string{cyclePGNull}}, {name: "b", parents: []string{"a", cyclePGNull}, grafts: []string{cyclePGNull, "absent"}}}, false},
		{"natural_cycle", []cyclePGNode{{name: "a", parents: []string{"b"}}, {name: "b", parents: []string{"a"}}}, true},
		{"graft_cycle_grafted_false", []cyclePGNode{{name: "a", grafts: []string{"b"}}, {name: "b", grafts: []string{"a"}}}, true},
		{"mixed_cycle", []cyclePGNode{{name: "a", parents: []string{"b"}}, {name: "b", grafts: []string{"a"}}}, true},
		{"self_natural", []cyclePGNode{{name: "a", parents: []string{"a"}}}, true},
		{"self_graft", []cyclePGNode{{name: "a", grafts: []string{"a"}}}, true},
		{"null_dangling_and_self", []cyclePGNode{{name: "a", parents: []string{cyclePGNull, "absent"}, grafts: []string{cyclePGNull, "a"}}}, true},
		{"disconnected_cycle", []cyclePGNode{{name: "a"}, {name: "b", parents: []string{"a"}}, {name: "x", parents: []string{"y"}}, {name: "y", grafts: []string{"x"}}}, true},
		{"long_back_edge", backEdge, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			repo, _ := cyclePGSeed(t, ctx, st, tc.nodes)
			// A cycle in another repository must never contaminate this decision.
			cyclePGSeed(t, ctx, st, []cyclePGNode{{name: "foreign", parents: []string{"foreign"}}})
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(tx)
			cyclePGParity(t, ctx, tx, repo, tc.cycle)
		})
	}
}

// Compare all persisted publication metadata, including trigger side effects.
func cyclePGImage(t *testing.T, ctx context.Context, db pgDatabase, repo domain.ContentHash) string {
	t.Helper()
	var image string
	err := db.QueryRow(ctx, `SELECT jsonb_build_object(
 'snapshots',(SELECT jsonb_agg(to_jsonb(s) ORDER BY id) FROM snapshots s WHERE repo_id=$1),
 'refs',(SELECT jsonb_agg(to_jsonb(r) ORDER BY kind,name) FROM refs r WHERE repo_id=$1),
 'reflog',(SELECT jsonb_agg(to_jsonb(r) ORDER BY id) FROM reflog r WHERE repo_id=$1),
 'revisions',(SELECT to_jsonb(r) FROM repository_revisions r WHERE repo_id=$1),
 'catalog',(SELECT to_jsonb(c) FROM repository_catalog_state c WHERE repo_id=$1),
 'changes',(SELECT jsonb_agg(to_jsonb(c) ORDER BY seq,entity_kind,entity_key) FROM repository_catalog_changes c WHERE repo_id=$1)
 )::text`, string(repo)).Scan(&image)
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func TestPGCycleValidationTransactionLocalAndRepair(t *testing.T) {
	st, ctx := chunkReusePG(t)
	repo, ids := cyclePGSeed(t, ctx, st, []cyclePGNode{{name: "a"}, {name: "b"}})
	before := cyclePGImage(t, ctx, st.pool, repo)
	rollback := errors.New("injected outer rollback")
	err := st.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := st.AddGraftParents(bound, repo, ids["a"], []domain.ContentHash{ids["b"]}); err != nil {
			return err
		}
		// This nested savepoint must see the first, still uncommitted edge.
		if err := st.AddGraftParents(bound, repo, ids["b"], []domain.ContentHash{ids["a"]}); !errors.Is(err, domain.ErrConflict) {
			t.Fatalf("nested opposite edge: %v", err)
		}
		tx, err := st.db(bound).Begin(bound)
		if err != nil {
			return err
		}
		defer rollbackPG(tx)
		cyclePGParity(t, bound, tx, repo, false)
		if _, err := tx.Exec(bound, `UPDATE snapshots SET parents=ARRAY[$3]::text[] WHERE repo_id=$1 AND id=$2`, string(repo), string(ids["b"]), string(ids["a"])); err != nil {
			return err
		}
		cyclePGParity(t, bound, tx, repo, true)
		if _, err := tx.Exec(bound, `UPDATE snapshots SET parents='{}' WHERE repo_id=$1 AND id=$2`, string(repo), string(ids["b"])); err != nil {
			return err
		}
		cyclePGParity(t, bound, tx, repo, false)
		if err := tx.Commit(bound); err != nil {
			return err
		}
		if got := cyclePGImage(t, ctx, st.pool, repo); got != before {
			t.Fatal("uncommitted savepoint changes escaped to another connection")
		}
		return rollback
	})
	if !errors.Is(err, rollback) {
		t.Fatal(err)
	}
	if got := cyclePGImage(t, ctx, st.pool, repo); got != before {
		t.Fatal("outer rollback left graph or publication metadata")
	}
}

func cyclePGPeer(t *testing.T, ctx context.Context) *PostgresStore {
	t.Helper()
	st, err := NewPostgresStore(ctx, os.Getenv("CXT_TEST_DSN"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

// A held outer transaction gives a deterministic ordering. Observe the actual
// backend lock wait instead of depending on a sleep or scheduler race.
func cyclePGWaitBlocked(t *testing.T, ctx context.Context, st *PostgresStore, blocker uint32) {
	t.Helper()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err := st.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1::integer=ANY(pg_blocking_pids(pid)))`, int(blocker)).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-tick.C:
		}
	}
}

func TestPGCycleValidationConcurrentOppositeGrafts(t *testing.T) {
	st, ctx := chunkReusePG(t)
	peer := cyclePGPeer(t, ctx)
	repo, ids := cyclePGSeed(t, ctx, st, []cyclePGNode{{name: "a"}, {name: "b"}})
	done := make(chan error, 1)
	var winnerImage string
	err := st.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := st.AddGraftParents(bound, repo, ids["a"], []domain.ContentHash{ids["b"]}); err != nil {
			return err
		}
		winnerImage = cyclePGImage(t, bound, st.db(bound), repo)
		go func() { done <- peer.AddGraftParents(ctx, repo, ids["b"], []domain.ContentHash{ids["a"]}) }()
		cyclePGWaitBlocked(t, ctx, st, st.db(bound).(*repositoryTx).Conn().PgConn().PID())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("opposite-edge loser: %v", err)
	}
	if got := cyclePGImage(t, ctx, st.pool, repo); got != winnerImage {
		t.Fatal("loser left graft/sequence/ref/reflog/revision/catalog changes")
	}
	for name, want := range map[string]uint64{"a": 1, "b": 0} {
		snap, err := st.GetSnapshot(ctx, repo, ids[name])
		if err != nil || snap.GraftSeq != want || len(snap.GraftParents) != int(want) {
			t.Fatalf("%s: snapshot=%+v err=%v", name, snap, err)
		}
	}
}

func cyclePGJoinFixture(t *testing.T, ctx context.Context, st *PostgresStore) (domain.ContentHash, domain.JoinMutation) {
	t.Helper()
	repo, ids := cyclePGSeed(t, ctx, st, []cyclePGNode{{name: "h"}, {name: "x"}, {name: "tip", parents: []string{"x"}}})
	for _, ref := range []domain.Ref{
		{RepoID: repo, Kind: domain.RefBranch, Name: "main", Target: ids["h"]},
		{RepoID: repo, Kind: domain.RefSession, Name: domain.SessionRefPrefix("main") + "source", Target: ids["tip"]},
	} {
		if err := st.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
			t.Fatal(err)
		}
	}
	return repo, domain.JoinMutation{
		RepoID: repo, Branch: "main", Source: ids["x"], Segment: []domain.ContentHash{ids["x"], ids["tip"]},
		ExpectedHead: ids["h"], NewHead: ids["x"], ForkName: domain.SessionRefPrefix("main") + "new-fork", ForkTip: ids["tip"],
		Grafts: []domain.GraftPatch{{SnapshotID: ids["x"], Parents: []domain.ContentHash{ids["h"]}}},
	}
}

func TestPGCycleValidationJoinRollbackAndConcurrency(t *testing.T) {
	for _, mode := range []string{"cycle_rolls_back_fork", "outer_rollback", "same_head_race"} {
		t.Run(mode, func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			peer := cyclePGPeer(t, ctx)
			repo, m := cyclePGJoinFixture(t, ctx, st)
			if mode == "cycle_rolls_back_fork" {
				if err := st.AddGraftParents(ctx, repo, m.ExpectedHead, []domain.ContentHash{m.Source}); err != nil {
					t.Fatal(err)
				}
				before := cyclePGImage(t, ctx, st.pool, repo)
				if err := st.ApplyJoin(ctx, m); !errors.Is(err, domain.ErrConflict) {
					t.Fatalf("cyclic join: %v", err)
				}
				if got := cyclePGImage(t, ctx, st.pool, repo); got != before {
					t.Fatal("failed join leaked graft/fork/ref/reflog/catalog changes")
				}
				return
			}
			before := cyclePGImage(t, ctx, st.pool, repo)
			rollback := errors.New("injected error after successful join savepoint")
			done := make(chan error, 1)
			var winnerImage string
			err := st.WithinRepository(ctx, repo, func(bound context.Context) error {
				if err := st.ApplyJoin(bound, m); err != nil {
					return err
				}
				winnerImage = cyclePGImage(t, bound, st.db(bound), repo)
				if mode == "outer_rollback" {
					return rollback
				}
				go func() { done <- peer.ApplyJoin(ctx, m) }()
				cyclePGWaitBlocked(t, ctx, st, st.db(bound).(*repositoryTx).Conn().PgConn().PID())
				return nil
			})
			if mode == "outer_rollback" {
				if !errors.Is(err, rollback) || cyclePGImage(t, ctx, st.pool, repo) != before {
					t.Fatalf("join outer rollback: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := <-done; !errors.Is(err, domain.ErrRefConflict) {
				t.Fatalf("stale head loser: %v", err)
			}
			if got := cyclePGImage(t, ctx, st.pool, repo); got != winnerImage {
				t.Fatal("losing join changed persisted winner metadata")
			}
			ref, err := st.GetRef(ctx, repo, domain.RefBranch, "main")
			if err != nil || ref.Target != m.NewHead {
				t.Fatalf("join winner: %+v %v", ref, err)
			}
		})
	}
}

func TestPGCycleValidationCancellation(t *testing.T) {
	for _, shape := range []string{"empty", "cycle"} {
		t.Run(shape, func(t *testing.T) {
			st, ctx := chunkReusePG(t)
			var nodes []cyclePGNode
			if shape == "cycle" {
				nodes = []cyclePGNode{{name: "a", parents: []string{"a"}}}
			}
			repo, _ := cyclePGSeed(t, ctx, st, nodes)
			tx, err := st.pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(tx)
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			if err := ensureNoReachabilityCycle(cancelled, tx, repo); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled %s graph: %v", shape, err)
			}
		})
	}
	t.Run("lock_wait", func(t *testing.T) {
		st, ctx := chunkReusePG(t)
		peer := cyclePGPeer(t, ctx)
		repo, ids := cyclePGSeed(t, ctx, st, []cyclePGNode{{name: "a"}, {name: "b"}})
		before := cyclePGImage(t, ctx, st.pool, repo)
		waiting, cancel := context.WithCancel(ctx)
		defer cancel()
		done := make(chan error, 1)
		if err := st.WithinRepository(ctx, repo, func(bound context.Context) error {
			go func() { done <- peer.AddGraftParents(waiting, repo, ids["a"], []domain.ContentHash{ids["b"]}) }()
			cyclePGWaitBlocked(t, ctx, st, st.db(bound).(*repositoryTx).Conn().PgConn().PID())
			cancel()
			if err := <-done; !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled lock waiter: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if got := cyclePGImage(t, ctx, st.pool, repo); got != before {
			t.Fatal("canceled writer changed metadata")
		}
		if err := peer.AddGraftParents(ctx, repo, ids["a"], []domain.ContentHash{ids["b"]}); err != nil {
			t.Fatalf("writer did not recover after cancellation: %v", err)
		}
	})
}

type cyclePGCountTx struct {
	pgx.Tx
	queries int
}

func (tx *cyclePGCountTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx.queries++
	return tx.Tx.Query(ctx, sql, args...)
}

func (tx *cyclePGCountTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx.queries++
	return tx.Tx.QueryRow(ctx, sql, args...)
}

// Opt-in fixed-sample comparison, not testing.B calibration (which would add
// hidden oracle runs). Each call retains the production 30-second deadline.
// Fixture construction and GC are outside timing; both algorithms share the
// exact transaction, backend, content and graph lock for all three samples.
func TestPGCycleValidationComparison(t *testing.T) {
	if os.Getenv("CXT_CYCLE_BENCH") != "1" {
		t.Skip("set CXT_CYCLE_BENCH=1 for fixed 500/2000/5000-node comparison")
	}
	for _, n := range []int{500, 2000, 5000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			st, setupCtx := chunkReusePG(t)
			repo, _ := cyclePGSeed(t, setupCtx, st, cyclePGChain(n))
			tx, err := st.pool.Begin(setupCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer rollbackPG(tx)
			if err := lockRepoGraph(setupCtx, tx, repo); err != nil {
				t.Fatal(err)
			}
			var xid, version, workMem, statementTimeout string
			var count int
			if err := tx.QueryRow(setupCtx, `SELECT txid_current()::text,current_setting('server_version'),current_setting('work_mem'),current_setting('statement_timeout'),(SELECT count(*) FROM snapshots WHERE repo_id=$1)`, string(repo)).Scan(&xid, &version, &workMem, &statementTimeout, &count); err != nil || count != n {
				t.Fatalf("fixture metadata count=%d: %v", count, err)
			}
			for sample := 1; sample <= 3; sample++ {
				order := []string{"old_sql", "current_guard"}
				if sample == 2 {
					order[0], order[1] = order[1], order[0]
				}
				for _, algorithm := range order {
					runtime.GC()
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					counted := &cyclePGCountTx{Tx: tx}
					var before, after runtime.MemStats
					runtime.ReadMemStats(&before)
					start := time.Now()
					if algorithm == "old_sql" {
						err = cyclePGOld(ctx, counted, repo)
					} else {
						err = ensureNoReachabilityCycle(ctx, counted, repo)
					}
					elapsed := time.Since(start)
					runtime.ReadMemStats(&after)
					cancel()
					result := map[string]any{"algorithm": algorithm, "nodes": n, "edges": n - 1, "sample": sample,
						"elapsed_ns": elapsed.Nanoseconds(), "go_alloc_bytes": after.TotalAlloc - before.TotalAlloc,
						"go_allocations": after.Mallocs - before.Mallocs, "queries": counted.queries, "transaction": xid,
						"backend_pid": tx.Conn().PgConn().PID(), "repo": repo, "postgres": version, "work_mem": workMem,
						"statement_timeout": statementTimeout, "deadline_seconds": 30, "gomaxprocs": runtime.GOMAXPROCS(0), "error": nil}
					if err != nil {
						result["error"] = err.Error()
					}
					line, marshalErr := json.Marshal(result)
					if marshalErr != nil {
						t.Fatal(marshalErr)
					}
					t.Logf("CYCLE_SAMPLE %s", line)
					if err != nil {
						t.Fatalf("bounded %s failed; no deadline relaxation or remaining samples for this size: %v", algorithm, err)
					}
				}
			}
		})
	}
}
