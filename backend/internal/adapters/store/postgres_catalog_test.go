//go:build postgres

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
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

type catalogRowPG struct {
	seq     int64
	kind    string
	key     string
	deleted bool
	value   []byte
}

func catalogRowsPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, after int64) []catalogRowPG {
	t.Helper()
	rows, err := s.pool.Query(ctx, `SELECT c.seq,c.entity_kind,c.entity_key,c.deleted,c.after_image
		FROM repository_catalog_changes c JOIN repository_catalog_state st USING(repo_id,epoch)
		WHERE c.repo_id=$1 AND c.seq>$2 ORDER BY c.seq,c.entity_kind,c.entity_key`, string(repo), after)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []catalogRowPG
	for rows.Next() {
		var row catalogRowPG
		if err := rows.Scan(&row.seq, &row.kind, &row.key, &row.deleted, &row.value); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func catalogJSONPG[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("catalog image %s: %v", raw, err)
	}
	return out
}

func catalogHistoryPG(repo domain.ContentHash) domain.HistoryEvent {
	return domain.HistoryEvent{ID: "0123456789abcdef0123456789abcdef", RepoID: string(repo), BranchID: "catalog-branch", Branch: "catalog", Kind: "orphan", CreatedAt: time.Now().UTC().Truncate(time.Microsecond)}
}

func TestPGCatalogStagingDoesNotDependOnGraphRevision(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	initial := catalogReadStatePG(t, ctx, s, repo)
	rows := catalogRowsPG(t, ctx, s, repo, -1)
	if initial.head != 1 || initial.floor != 0 || initial.epoch == "" || len(rows) != 1 || rows[0].seq != 1 || rows[0].kind != "protocol" || rows[0].key != string(repo) {
		t.Fatalf("new repository must have protocol sequence 1: state=%+v rows=%+v", initial, rows)
	}
	before, err := s.RepositoryRevision(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	snap := initializationPGSnapshot(t, s, repo, "unpublished staging")
	after, err := s.RepositoryRevision(ctx, repo)
	if err != nil || after != before {
		t.Fatalf("fixture unexpectedly published graph revision: before=%+v after=%+v err=%v", before, after, err)
	}
	rows = catalogRowsPG(t, ctx, s, repo, initial.head)
	if len(rows) != 1 || rows[0].seq != initial.head+1 || rows[0].kind != "snapshot" || rows[0].key != string(snap.ID) || rows[0].deleted {
		t.Fatalf("staged snapshot missing from catalog: %+v", rows)
	}
	image := catalogJSONPG[domain.Snapshot](t, rows[0].value)
	stored, err := s.GetSnapshot(ctx, repo, snap.ID)
	if err != nil || image.ID != stored.ID || image.RepoID != repo || image.DocHash != stored.DocHash || !image.CreatedAt.Equal(stored.CreatedAt) || image.CreatedAt.IsZero() {
		t.Fatalf("staged immutable image=%+v stored=%+v err=%v", image, stored, err)
	}
	// Document bodies and publication counters are outside this catalog's scope.
	if err := s.AdvanceRepositoryRevision(ctx, repo, false); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got.head != initial.head+1 {
		t.Fatalf("publication revision fabricated a metadata change: %+v", got)
	}
}

func TestPGCatalogDirectSQLCoverageCoalescingAndNoOps(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "a")
	b := initializationPGSnapshot(t, s, repo, "b")
	ref := domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "remove/me", Target: a.ID}
	if err := s.CompareAndSwapRef(ctx, repo, ref, ""); err != nil {
		t.Fatal(err)
	}
	before := catalogReadStatePG(t, ctx, s, repo)
	event := catalogHistoryPG(repo)
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	tx := catalogTxPG(t, ctx, s)
	// A valid blob FK is sufficient for a direct SQL pointer update. The
	// trigger must record it even when no application metadata method is used.
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='intermediate' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='final',memory_hash=$3,grafted=true,graft_parents=ARRAY[$3]::text[],graft_seq=7,session_id='capture',models=ARRAY['test-model'],compaction_count=2 WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID), string(b.ID))
	catalogExecPG(t, ctx, tx, `DELETE FROM refs WHERE repo_id=$1 AND kind='tag' AND name=$2`, string(repo), ref.Name)
	catalogExecPG(t, ctx, tx, `INSERT INTO context_history(repo_id,id,event) VALUES($1,$2,$3)`, string(repo), event.ID, raw)
	catalogExecPG(t, ctx, tx, `UPDATE repos SET context_protocol=1 WHERE id=$1`, string(repo))
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("uncommitted catalog state visible: %+v -> %+v", before, got)
	}
	if rows := catalogRowsPG(t, ctx, s, repo, before.head); len(rows) != 0 {
		t.Fatalf("uncommitted journal visible: %+v", rows)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows := catalogRowsPG(t, ctx, s, repo, before.head)
	if len(rows) != 4 {
		t.Fatalf("one coalesced entry per entity expected: %+v", rows)
	}
	for i, kind := range []string{"history", "protocol", "ref", "snapshot"} {
		if rows[i].seq != before.head+1 || rows[i].kind != kind {
			t.Fatalf("transaction must share one ordered sequence: %+v", rows)
		}
	}
	if got := catalogJSONPG[domain.HistoryEvent](t, rows[0].value); !reflect.DeepEqual(got, event) || rows[0].key != event.ID {
		t.Fatalf("history image=%+v want=%+v", got, event)
	}
	if got := catalogJSONPG[map[string]int](t, rows[1].value); got["context_protocol"] != 1 {
		t.Fatalf("protocol image=%v", got)
	}
	if key := catalogJSONPG[[]string](t, []byte(rows[2].key)); !reflect.DeepEqual(key, []string{"tag", ref.Name}) || !rows[2].deleted || len(rows[2].value) != 0 {
		t.Fatalf("raw ref deletion lost: %+v", rows[2])
	}
	image := catalogJSONPG[domain.Snapshot](t, rows[3].value)
	if image.Message != "final" || image.MemoryHash != b.ID || !image.Grafted || !reflect.DeepEqual(image.GraftParents, []domain.ContentHash{b.ID}) || image.GraftSeq != 7 || image.SessionID != "capture" || !reflect.DeepEqual(image.Models, []string{"test-model"}) || image.CompactionCount != 2 {
		t.Fatalf("coalesced snapshot image omitted mutable fields: %+v", image)
	}
	// Source bookkeeping and identical projected images are not catalog changes.
	stable := catalogReadStatePG(t, ctx, s, repo)
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message=message,memory_hash=memory_hash WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, s.pool, `UPDATE context_history SET event=event,received_at=received_at+interval '1 second' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, s.pool, `UPDATE repos SET default_branch='metadata-outside-v1',context_protocol=context_protocol WHERE id=$1`, string(repo))
	if err := s.DeleteSnapshot(ctx, repo, domain.HashContent([]byte("absent snapshot"))); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != stable {
		t.Fatalf("no-op writes advanced catalog: %+v -> %+v", stable, got)
	}
	// Direct history UPDATE and DELETE must both be captured, not just INSERT.
	catalogExecPG(t, ctx, s.pool, `UPDATE context_history SET event=jsonb_set(event,'{branch}','"renamed"'::jsonb) WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, s.pool, `DELETE FROM context_history WHERE repo_id=$1`, string(repo))
	rows = catalogRowsPG(t, ctx, s, repo, stable.head)
	if len(rows) != 2 || rows[0].kind != "history" || catalogJSONPG[domain.HistoryEvent](t, rows[0].value).Branch != "renamed" || rows[1].kind != "history" || !rows[1].deleted || rows[1].key != event.ID {
		t.Fatalf("history mutation/deletion missing: %+v", rows)
	}
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

func TestPGCatalogDirectAdaptersAndGCDeletion(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "a")
	b := initializationPGSnapshot(t, s, repo, "b")
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID), domain.HookMessagePrefix+"pending")
	before := catalogReadStatePG(t, ctx, s, repo)
	memory, err := s.PutMemory(ctx, repo, domain.MemoryDigest{SnapshotID: a.ID, Summary: "catalog pointer fixture", Provider: domain.ProviderClaude})
	if err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("unattached memory body generated metadata change: %+v -> %+v", before, got)
	}
	if err := s.CompareAndSwapSnapshotMemory(ctx, repo, a.ID, "", memory); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateSnapshotMessage(ctx, repo, a.ID, "adapter message"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetGraftParents(ctx, repo, a.ID, []domain.ContentHash{b.ID}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyHistoryEvent(ctx, catalogHistoryPG(repo)); err != nil {
		t.Fatal(err)
	}
	if err := s.EnableContextProtocol(ctx, repo); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSnapshot(ctx, repo, a.ID); err != nil {
		t.Fatal(err)
	}
	rows := catalogRowsPG(t, ctx, s, repo, before.head)
	found := map[string]bool{}
	for _, row := range rows {
		switch row.kind {
		case "snapshot":
			if row.deleted && row.key == string(a.ID) {
				found["delete"] = true
			} else if !row.deleted {
				image := catalogJSONPG[domain.Snapshot](t, row.value)
				found["memory"] = found["memory"] || image.MemoryHash == memory
				found["message"] = found["message"] || image.Message == "adapter message"
				found["graft"] = found["graft"] || (image.GraftSeq > 0 && reflect.DeepEqual(image.GraftParents, []domain.ContentHash{b.ID}))
			}
		case "history", "protocol":
			found[row.kind] = true
		}
	}
	for _, key := range []string{"memory", "message", "graft", "history", "protocol", "delete"} {
		if !found[key] {
			t.Errorf("direct adapter %s missing: %+v", key, rows)
		}
	}
	deleted := catalogReadStatePG(t, ctx, s, repo)
	if err := s.DeleteSnapshot(ctx, repo, a.ID); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != deleted {
		t.Fatalf("idempotent GC deletion advanced catalog: %+v -> %+v", deleted, got)
	}
}

func TestPGCatalogWriterCommitOrder(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "writer-a")
	b := initializationPGSnapshot(t, s, repo, "writer-b")
	before := catalogReadStatePG(t, ctx, s, repo)
	first := catalogTxPG(t, ctx, s)
	second := catalogTxPG(t, ctx, s)
	var firstPID, secondPID int
	if err := first.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&firstPID); err != nil {
		t.Fatal(err)
	}
	if err := second.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&secondPID); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, first, `UPDATE snapshots SET message='writer-a' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	done := make(chan error, 1)
	go func() {
		_, err := second.Exec(ctx, `UPDATE snapshots SET message='writer-b' WHERE repo_id=$1 AND id=$2`, string(repo), string(b.ID))
		done <- err
	}()
	// Observe an actual PostgreSQL blocker, not elapsed scheduler time. The
	// snapshots differ, so this is the catalog's repository serialization lock.
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	blocked := false
	for !blocked {
		if err := s.pool.QueryRow(ctx, `SELECT $1::int=ANY(pg_blocking_pids($2::int))`, firstPID, secondPID).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case err := <-done:
			t.Fatalf("second writer escaped uncommitted predecessor: %v", err)
		case <-deadline.C:
			t.Fatal("second writer never reached repository catalog lock")
		case <-tick.C:
		}
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("reader observed in-flight sequence: %+v -> %+v", before, got)
	}
	if err := first.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got.head != before.head+1 {
		t.Fatalf("second uncommitted writer became visible: %+v", got)
	}
	if err := second.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows := catalogRowsPG(t, ctx, s, repo, before.head)
	if len(rows) != 2 || rows[0].seq != before.head+1 || rows[0].key != string(a.ID) || rows[1].seq != before.head+2 || rows[1].key != string(b.ID) {
		t.Fatalf("catalog sequence does not follow commit order: %+v", rows)
	}
}

func TestPGCatalogSavepointAndOuterRollback(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "rollback")
	before := catalogReadStatePG(t, ctx, s, repo)
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `SAVEPOINT first_write`)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='discarded first allocation' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `ROLLBACK TO SAVEPOINT first_write`)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='retained' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `SAVEPOINT overlay`)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='discarded overlay' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `ROLLBACK TO SAVEPOINT overlay`)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	rows := catalogRowsPG(t, ctx, s, repo, before.head)
	if len(rows) != 1 || rows[0].seq != before.head+1 || catalogJSONPG[domain.Snapshot](t, rows[0].value).Message != "retained" {
		t.Fatalf("savepoint left sequence gap or stale after-image: %+v", rows)
	}
	before = catalogReadStatePG(t, ctx, s, repo)
	abort := errors.New("catalog outer rollback")
	err := s.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := s.SetGraftParents(bound, repo, a.ID, nil); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatalf("outer transaction error=%v", err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("outer rollback changed catalog state: %+v -> %+v", before, got)
	}
	if rows := catalogRowsPG(t, ctx, s, repo, before.head); len(rows) != 0 {
		t.Fatalf("outer rollback retained changes: %+v", rows)
	}
	got, err := s.GetSnapshot(ctx, repo, a.ID)
	if err != nil || got.Message != "retained" || got.GraftSeq != 0 {
		t.Fatalf("outer rollback did not restore source: %+v %v", got, err)
	}
}

func TestPGCatalogDeferredCommitFailureRollsBackJournal(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "deferred failure")
	before := catalogReadStatePG(t, ctx, s, repo)
	tx := catalogTxPG(t, ctx, s)
	// A transaction-local deferred FK deterministically fails at COMMIT, after
	// the source UPDATE and its catalog trigger both completed successfully.
	catalogExecPG(t, ctx, tx, `CREATE TEMP TABLE catalog_commit_parent(id int PRIMARY KEY) ON COMMIT DROP`)
	catalogExecPG(t, ctx, tx, `CREATE TEMP TABLE catalog_commit_child(id int REFERENCES catalog_commit_parent(id) DEFERRABLE INITIALLY DEFERRED) ON COMMIT DROP`)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='must not survive commit failure' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, tx, `INSERT INTO catalog_commit_child VALUES(1)`)
	var pgerr *pgconn.PgError
	if err := tx.Commit(ctx); !errors.As(err, &pgerr) || pgerr.Code != "23503" {
		t.Fatalf("expected deferred FK failure at COMMIT, got %v", err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("failed commit changed catalog: %+v -> %+v", before, got)
	}
	if rows := catalogRowsPG(t, ctx, s, repo, before.head); len(rows) != 0 {
		t.Fatalf("failed commit retained journal: %+v", rows)
	}
	got, err := s.GetSnapshot(ctx, repo, a.ID)
	if err != nil || got.Message != "" {
		t.Fatalf("failed commit retained source update: %+v %v", got, err)
	}
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='next committed write' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	if got := catalogReadStatePG(t, ctx, s, repo); got.head != before.head+1 {
		t.Fatalf("failed commit consumed a durable sequence: %+v", got)
	}
}

func catalogPagePG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, request domain.CatalogRequest) domain.CatalogPage {
	t.Helper()
	page, err := s.CatalogChanges(ctx, repo, request)
	if err != nil {
		t.Fatal(err)
	}
	if page.Version != 1 || page.RepoID != repo || page.Epoch == "" || page.Through < 0 || (page.Mode != "baseline" && page.Mode != "delta") {
		t.Fatalf("invalid catalog page envelope: %+v", page)
	}
	limit := request.Limit
	if limit == 0 {
		limit = 256
	}
	if len(page.Entries) > limit || page.Entries == nil {
		t.Fatalf("unbounded or null entries: %+v", page)
	}
	if page.NextCursor != "" {
		if page.Checkpoint != nil || len(page.Entries) == 0 {
			t.Fatalf("partial page must not publish a checkpoint: %+v", page)
		}
	} else if page.Checkpoint == nil || page.Checkpoint.Version != 1 || page.Checkpoint.RepoID != repo || page.Checkpoint.Epoch != page.Epoch || page.Checkpoint.Sequence != page.Through {
		t.Fatalf("final page must checkpoint its fixed head: %+v", page)
	}
	for _, entry := range page.Entries {
		if entry.Sequence < 0 || entry.Sequence > page.Through || entry.Key == "" || (entry.Deleted && len(entry.Value) != 0) || (!entry.Deleted && !json.Valid(entry.Value)) {
			t.Fatalf("invalid catalog entry: %+v", entry)
		}
		if page.Mode == "baseline" && entry.Deleted {
			t.Fatalf("baseline exposed tombstone: %+v", entry)
		}
	}
	return page
}

// Continue an already opened traversal so tests can mutate source rows between
// pages. Every entry, including one from a transaction split across pages, is
// staged until a checkpoint arrives on the final page.
func catalogDrainPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, first domain.CatalogPage, limit int) ([]domain.CatalogEntry, domain.CatalogCheckpoint) {
	t.Helper()
	page := first
	var entries []domain.CatalogEntry
	seen := map[string]bool{}
	for pages := 0; pages < 4096; pages++ {
		if page.Epoch != first.Epoch || page.Through != first.Through || page.Mode != first.Mode {
			t.Fatalf("continuation changed fixed image: first=%+v next=%+v", first, page)
		}
		for _, entry := range page.Entries {
			if len(entries) > 0 {
				last := entries[len(entries)-1]
				ordered := last.Kind < entry.Kind || (last.Kind == entry.Kind && last.Key < entry.Key)
				if page.Mode == "delta" {
					ordered = last.Sequence < entry.Sequence || (last.Sequence == entry.Sequence && ordered)
				}
				if !ordered {
					t.Fatalf("catalog entries not strictly ordered: %+v then %+v", last, entry)
				}
			}
			entries = append(entries, entry)
		}
		if page.NextCursor == "" {
			return entries, *page.Checkpoint
		}
		if seen[page.NextCursor] {
			t.Fatal("catalog cursor repeated without progress")
		}
		seen[page.NextCursor] = true
		page = catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Cursor: page.NextCursor, Limit: limit})
	}
	t.Fatal("catalog traversal exceeded fixture bound")
	return nil, domain.CatalogCheckpoint{}
}

func catalogCheckpointPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash) domain.CatalogCheckpoint {
	t.Helper()
	_, checkpoint := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1}), 0)
	return checkpoint
}

func TestPGCatalogBaselineKeepsImmutableImagesThroughConcurrentDeletion(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "baseline-a")
	b := initializationPGSnapshot(t, s, repo, "baseline-b")
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='baseline image' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	first := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	if first.Mode != "baseline" || first.NextCursor == "" || len(first.Entries) != 1 || first.Entries[0].Kind != "protocol" {
		t.Fatalf("fixture must pause before snapshot images: %+v", first)
	}
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='newer than traversal' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	catalogExecPG(t, ctx, tx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2`, string(repo), string(b.ID))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, checkpoint := catalogDrainPG(t, ctx, s, repo, first, 1)
	if len(entries) != 3 {
		t.Fatalf("baseline lost an entity between pages: %+v", entries)
	}
	images := map[domain.ContentHash]domain.Snapshot{}
	for _, entry := range entries {
		if entry.Kind == "snapshot" {
			image := catalogJSONPG[domain.Snapshot](t, entry.Value)
			images[image.ID] = image
		}
	}
	if len(images) != 2 || images[a.ID].Message != "baseline image" || images[b.ID].ID != b.ID {
		t.Fatalf("baseline read mutable source rows instead of fixed images: %+v", images)
	}
	delta, end := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &checkpoint, Limit: 1}), 1)
	if len(delta) != 2 || end.Sequence != checkpoint.Sequence+1 {
		t.Fatalf("post-baseline transaction lost: entries=%+v checkpoint=%+v", delta, end)
	}
	for _, entry := range delta {
		switch entry.Key {
		case string(a.ID):
			if entry.Deleted || catalogJSONPG[domain.Snapshot](t, entry.Value).Message != "newer than traversal" {
				t.Fatalf("new after-image lost: %+v", entry)
			}
		case string(b.ID):
			if !entry.Deleted {
				t.Fatalf("concurrent deletion lost: %+v", entry)
			}
		default:
			t.Fatalf("unexpected delta entry: %+v", entry)
		}
	}
	fresh, _ := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	if len(fresh) != 2 {
		t.Fatalf("fresh baseline retained deleted entity: %+v", fresh)
	}
}

func TestPGCatalogDeltaSplitsTransactionWithoutEarlyCheckpoint(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "delta-a")
	b := initializationPGSnapshot(t, s, repo, "delta-b")
	start := catalogCheckpointPG(t, ctx, s, repo)
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='fixed transaction' WHERE repo_id=$1`, string(repo))
	// The byte order of these names differs from common locale orderings.
	for _, name := range []string{"é", "z", "A"} {
		catalogExecPG(t, ctx, tx, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag',$2,$3)`, string(repo), name, string(a.ID))
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	first := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &start, Limit: 2})
	if first.Mode != "delta" || first.NextCursor == "" || first.Through != start.Sequence+1 {
		t.Fatalf("expected split transaction: %+v", first)
	}
	// A second transaction supersedes images that have not yet been delivered.
	tx = catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `DELETE FROM refs WHERE repo_id=$1 AND kind='tag' AND name='é'`, string(repo))
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='next transaction' WHERE repo_id=$1 AND id=$2`, string(repo), string(b.ID))
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, checkpoint := catalogDrainPG(t, ctx, s, repo, first, 2)
	if len(entries) != 5 || checkpoint.Sequence != start.Sequence+1 {
		t.Fatalf("transaction was skipped, duplicated, or extended: %+v %+v", entries, checkpoint)
	}
	for _, entry := range entries {
		if entry.Sequence != checkpoint.Sequence || entry.Deleted {
			t.Fatalf("newer transaction leaked into fixed delta: %+v", entry)
		}
		if entry.Kind == "snapshot" && catalogJSONPG[domain.Snapshot](t, entry.Value).Message != "fixed transaction" {
			t.Fatalf("delta read a newer mutable row: %+v", entry)
		}
	}
	next, final := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &checkpoint, Limit: 1}), 1)
	if len(next) != 2 || final.Sequence != checkpoint.Sequence+1 || next[0].Kind != "ref" || !next[0].Deleted || next[1].Key != string(b.ID) {
		t.Fatalf("follow-up transaction lost: %+v %+v", next, final)
	}
	empty := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &final})
	if len(empty.Entries) != 0 || empty.Checkpoint == nil || *empty.Checkpoint != final {
		t.Fatalf("empty delta must acknowledge unchanged committed head: %+v", empty)
	}
}

func TestPGCatalogLimitsAndRawRefImages(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "bounded pages")
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target,branch_id)
		SELECT $1,'tag','catalog-'||lpad(n::text,4,'0'),$2,'raw-branch-identity' FROM generate_series(1,1001) n`, string(repo), string(a.ID))
	first := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1})
	if len(first.Entries) != 256 || first.NextCursor == "" {
		t.Fatalf("default limit is not 256: count=%d cursor=%q", len(first.Entries), first.NextCursor)
	}
	entries, _ := catalogDrainPG(t, ctx, s, repo, first, 0)
	if len(entries) != 1003 {
		t.Fatalf("default pagination lost entries: %d", len(entries))
	}
	for _, entry := range entries {
		if entry.Kind != "ref" {
			continue
		}
		ref := catalogJSONPG[domain.Ref](t, entry.Value)
		key := catalogJSONPG[[]string](t, []byte(entry.Key))
		if ref.RepoID != repo || ref.Target != a.ID || ref.BranchID != "raw-branch-identity" || !reflect.DeepEqual(key, []string{string(ref.Kind), ref.Name}) {
			t.Fatalf("raw ref identity or payload lost: %+v key=%v", ref, key)
		}
		object := catalogJSONPG[map[string]json.RawMessage](t, entry.Value)
		if object["version"] != nil || object["updated_at"] != nil {
			t.Fatalf("source bookkeeping leaked into ref wire image: %s", entry.Value)
		}
	}
	max := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1000})
	if len(max.Entries) != 1000 || max.NextCursor == "" {
		t.Fatalf("maximum valid page size not honored: %d", len(max.Entries))
	}
	entries, _ = catalogDrainPG(t, ctx, s, repo, max, 1000)
	if len(entries) != 1003 {
		t.Fatalf("maximum-size traversal lost entries: %d", len(entries))
	}
	stable := catalogReadStatePG(t, ctx, s, repo)
	catalogExecPG(t, ctx, s.pool, `UPDATE refs SET target=target,version=version+1,updated_at=clock_timestamp() WHERE repo_id=$1`, string(repo))
	if got := catalogReadStatePG(t, ctx, s, repo); got != stable {
		t.Fatalf("raw-ref bookkeeping generated catalog changes: %+v -> %+v", stable, got)
	}
}

func catalogExpectErrorPG(t *testing.T, ctx context.Context, s *PostgresStore, repo domain.ContentHash, request domain.CatalogRequest, want error) {
	t.Helper()
	page, err := s.CatalogChanges(ctx, repo, request)
	if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.CatalogPage{}) {
		t.Fatalf("error must fail closed: want=%v got=%v page=%+v", want, err, page)
	}
}

func TestPGCatalogRejectsInvalidCheckpointsAndCursors(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	initializationPGSnapshot(t, s, repo, "cursor")
	checkpoint := catalogCheckpointPG(t, ctx, s, repo)
	first := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	other := domain.HashContent([]byte(string(repo) + "other"))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		req  domain.CatalogRequest
		want error
	}{
		{"version zero", domain.CatalogRequest{}, domain.ErrValidation},
		{"future version", domain.CatalogRequest{Version: 2}, domain.ErrValidation},
		{"negative limit", domain.CatalogRequest{Version: 1, Limit: -1}, domain.ErrValidation},
		{"excessive limit", domain.CatalogRequest{Version: 1, Limit: 1001}, domain.ErrValidation},
		{"ambiguous resume", domain.CatalogRequest{Version: 1, After: &checkpoint, Cursor: first.NextCursor}, domain.ErrValidation},
		{"malformed cursor", domain.CatalogRequest{Version: 1, Cursor: "not!base64"}, domain.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) { catalogExpectErrorPG(t, ctx, s, repo, tc.req, tc.want) })
	}
	for _, tc := range []struct {
		name   string
		change func(*domain.CatalogCheckpoint)
		want   error
	}{
		{"checkpoint future", func(c *domain.CatalogCheckpoint) { c.Sequence++ }, domain.ErrCatalogResetRequired},
		{"checkpoint negative", func(c *domain.CatalogCheckpoint) { c.Sequence = -1 }, domain.ErrValidation},
		{"checkpoint epoch", func(c *domain.CatalogCheckpoint) { c.Epoch = "00000000-0000-0000-0000-000000000000" }, domain.ErrCatalogResetRequired},
		{"checkpoint malformed epoch", func(c *domain.CatalogCheckpoint) { c.Epoch = "not-an-epoch" }, domain.ErrValidation},
		{"checkpoint repository", func(c *domain.CatalogCheckpoint) { c.RepoID = other }, domain.ErrCatalogResetRequired},
		{"checkpoint version", func(c *domain.CatalogCheckpoint) { c.Version = 2 }, domain.ErrCatalogResetRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := checkpoint
			tc.change(&bad)
			catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &bad}, tc.want)
		})
	}
	cur, err := decodeCatalogCursor(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func(*catalogCursor)
		want   error
	}{
		{"cursor future", func(c *catalogCursor) { c.Through++ }, domain.ErrCatalogResetRequired},
		{"cursor repository", func(c *catalogCursor) { c.RepoID = other }, domain.ErrCatalogResetRequired},
		{"cursor epoch", func(c *catalogCursor) { c.Epoch = "00000000-0000-0000-0000-000000000000" }, domain.ErrCatalogResetRequired},
		{"cursor scope", func(c *catalogCursor) { c.Scope = "other-scope" }, domain.ErrCatalogResetRequired},
		{"cursor version", func(c *catalogCursor) { c.Version++ }, domain.ErrCatalogResetRequired},
		{"cursor kind", func(c *catalogCursor) { c.Kind = "blob" }, domain.ErrValidation},
		{"cursor sequence", func(c *catalogCursor) { c.Seq = c.Through + 1 }, domain.ErrValidation},
		{"cursor floor", func(c *catalogCursor) { c.Floor = -1 }, domain.ErrValidation},
		{"cursor mode", func(c *catalogCursor) { c.Mode = "anything" }, domain.ErrValidation},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bad := cur
			tc.change(&bad)
			raw, err := json.Marshal(bad)
			if err != nil {
				t.Fatal(err)
			}
			catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Cursor: base64.RawURLEncoding.EncodeToString(raw)}, tc.want)
		})
	}
	catalogExpectErrorPG(t, ctx, s, other, domain.CatalogRequest{Version: 1, Cursor: first.NextCursor}, domain.ErrCatalogResetRequired)
	catalogExpectErrorPG(t, ctx, s, domain.HashContent([]byte(string(repo)+"missing")), domain.CatalogRequest{Version: 1}, domain.ErrNotFound)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	catalogExpectErrorPG(t, cancelled, s, repo, domain.CatalogRequest{Version: 1}, context.Canceled)
}

func TestPGCatalogRejectsCallerWriteTransaction(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "uncommitted checkpoint")
	before := catalogReadStatePG(t, ctx, s, repo)
	abort := errors.New("discard uncommitted source")
	err := s.WithinRepository(ctx, repo, func(bound context.Context) error {
		if err := s.SetGraftParents(bound, repo, a.ID, nil); err != nil {
			return err
		}
		catalogExpectErrorPG(t, bound, s, repo, domain.CatalogRequest{Version: 1}, domain.ErrConflict)
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("rejected write context changed committed state: %+v -> %+v", before, got)
	}
	if err := s.WithinReadSnapshot(ctx, func(read context.Context) error {
		page := catalogPagePG(t, read, s, repo, domain.CatalogRequest{Version: 1})
		if page.Through != before.head {
			t.Fatalf("read-only nested catalog read head=%d want=%d", page.Through, before.head)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPGCatalogPruneRetainsAnchorsAndInvalidatesContinuations(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "prune-live")
	b := initializationPGSnapshot(t, s, repo, "prune-deleted")
	old := catalogCheckpointPG(t, ctx, s, repo)
	for _, message := range []string{"discarded older image", "floor anchor"} {
		catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message=$3 WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID), message)
	}
	if err := s.DeleteSnapshot(ctx, repo, b.ID); err != nil {
		t.Fatal(err)
	}
	floor := catalogCheckpointPG(t, ctx, s, repo)
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='after floor' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','retained-ref',$2)`, string(repo), string(a.ID))
	baseline := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	delta := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &floor, Limit: 1})
	if baseline.NextCursor == "" || delta.NextCursor == "" {
		t.Fatal("prune fixture must have both baseline and delta continuations")
	}
	wantBaseline, wantCheckpoint := catalogDrainPG(t, ctx, s, repo, baseline, 1)
	before := catalogReadStatePG(t, ctx, s, repo)
	oldRows := catalogRowsPG(t, ctx, s, repo, -1)
	// Both floor and deletions must roll back together; a failed maintenance
	// transaction must not invalidate cursors or silently remove old images.
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `SELECT cxt_prune_catalog($1,$2)`, string(repo), floor.Sequence)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("rolled-back prune advanced floor: %+v -> %+v", before, got)
	}
	if got := catalogRowsPG(t, ctx, s, repo, -1); !reflect.DeepEqual(got, oldRows) {
		t.Fatal("rolled-back prune removed journal entries")
	}
	catalogExecPG(t, ctx, s.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), floor.Sequence)
	pruned := catalogReadStatePG(t, ctx, s, repo)
	if pruned.epoch != before.epoch || pruned.head != before.head || pruned.floor != floor.Sequence {
		t.Fatalf("prune must change only retention floor: %+v -> %+v", before, pruned)
	}
	rows := catalogRowsPG(t, ctx, s, repo, -1)
	if len(rows) >= len(oldRows) {
		t.Fatal("fixture did not actually prune any historical images")
	}
	anchors := map[string]catalogRowPG{}
	for _, row := range rows {
		if row.seq > floor.Sequence {
			continue
		}
		key := row.kind + ":" + row.key
		if _, exists := anchors[key]; exists {
			t.Fatalf("prune retained multiple anchors for %s", key)
		}
		anchors[key] = row
	}
	if row, ok := anchors["snapshot:"+string(a.ID)]; !ok || row.deleted || catalogJSONPG[domain.Snapshot](t, row.value).Message != "floor anchor" {
		t.Fatalf("latest live anchor lost: %+v", row)
	}
	if row, ok := anchors["snapshot:"+string(b.ID)]; !ok || !row.deleted {
		t.Fatalf("deletion anchor lost; older image could resurrect: %+v", row)
	}
	gotBaseline, gotCheckpoint := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	if !reflect.DeepEqual(gotBaseline, wantBaseline) || gotCheckpoint != wantCheckpoint {
		t.Fatalf("fresh baseline changed after prune: entries=%+v checkpoint=%+v", gotBaseline, gotCheckpoint)
	}
	catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &old}, domain.ErrCatalogResetRequired)
	catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Cursor: baseline.NextCursor}, domain.ErrCatalogResetRequired)
	// Even a delta whose lower bound is at the new floor conservatively resets
	// its old cursor when the captured retention floor has changed.
	catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Cursor: delta.NextCursor}, domain.ErrCatalogResetRequired)
	entries, end := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &floor, Limit: 1}), 1)
	if len(entries) != 2 || end != wantCheckpoint {
		t.Fatalf("valid delta from retention floor lost changes: %+v %+v", entries, end)
	}
	for _, invalid := range []int64{floor.Sequence - 1, before.head + 1} {
		if _, err := s.pool.Exec(ctx, `SELECT cxt_prune_catalog($1,$2)`, string(repo), invalid); err == nil {
			t.Fatalf("invalid prune floor accepted: %d", invalid)
		}
		if got := catalogReadStatePG(t, ctx, s, repo); got != pruned {
			t.Fatalf("failed prune changed state: %+v -> %+v", pruned, got)
		}
	}
}

func TestPGCatalogEpochResetReseedsCurrentRowsAtZero(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "reset-live")
	b := initializationPGSnapshot(t, s, repo, "reset-deleted")
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='latest reset image' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	if err := s.DeleteSnapshot(ctx, repo, b.ID); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','seed-ref',$2)`, string(repo), string(a.ID))
	if err := s.ApplyHistoryEvent(ctx, catalogHistoryPG(repo)); err != nil {
		t.Fatal(err)
	}
	other := domain.HashContent([]byte(string(repo) + "unaffected scope"))
	if _, err := s.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	otherCheckpoint := catalogCheckpointPG(t, ctx, s, other)
	first := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	want, checkpoint := catalogDrainPG(t, ctx, s, repo, first, 1)
	before := catalogReadStatePG(t, ctx, s, repo)
	oldRows := catalogRowsPG(t, ctx, s, repo, -1)
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `SELECT cxt_reset_catalog($1)`, string(repo))
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("rolled-back reset changed epoch: %+v -> %+v", before, got)
	}
	if got := catalogRowsPG(t, ctx, s, repo, -1); !reflect.DeepEqual(got, oldRows) {
		t.Fatal("rolled-back reset changed journal")
	}
	catalogExecPG(t, ctx, s.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
	state := catalogReadStatePG(t, ctx, s, repo)
	if state.epoch == before.epoch || state.head != 0 || state.floor != 0 {
		t.Fatalf("reset must rotate epoch and restart sequence: %+v -> %+v", before, state)
	}
	catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &checkpoint}, domain.ErrCatalogResetRequired)
	catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Cursor: first.NextCursor}, domain.ErrCatalogResetRequired)
	seeded, newCheckpoint := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	if len(seeded) != len(want) || newCheckpoint.Sequence != 0 || newCheckpoint.Epoch != state.epoch {
		t.Fatalf("reset lost current entities: %+v %+v", seeded, newCheckpoint)
	}
	for i := range want {
		want[i].Sequence = 0
	}
	if !reflect.DeepEqual(seeded, want) {
		t.Fatalf("seeded images differ from current baseline: got=%+v want=%+v", seeded, want)
	}
	if got := catalogCheckpointPG(t, ctx, s, other); got != otherCheckpoint {
		t.Fatalf("reset changed another repository: %+v -> %+v", otherCheckpoint, got)
	}
	catalogExecPG(t, ctx, s.pool, `UPDATE snapshots SET message='first new epoch write' WHERE repo_id=$1 AND id=$2`, string(repo), string(a.ID))
	page := catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &newCheckpoint})
	if len(page.Entries) != 1 || page.Entries[0].Sequence != 1 || page.Entries[0].Key != string(a.ID) || page.Epoch != state.epoch {
		t.Fatalf("first post-reset delta lost: %+v", page)
	}
}

func TestPGCatalogMigrationSeedsExistingRows(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "existing before migration")
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','existing-ref',$2)`, string(repo), string(a.ID))
	if err := s.ApplyHistoryEvent(ctx, catalogHistoryPG(repo)); err != nil {
		t.Fatal(err)
	}
	migration, err := os.ReadFile("../../../../schemas/db/migrations/0074_catalog_changes.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Execute the actual migration in a private transactional schema containing
	// pre-existing source rows. This neither edits the shared migration ledger
	// nor removes another test's catalog; rollback drops the whole schema.
	tx := catalogTxPG(t, ctx, s)
	schema := pgx.Identifier{fmt.Sprintf("catalog_seed_%d", time.Now().UnixNano())}.Sanitize()
	catalogExecPG(t, ctx, tx, `CREATE SCHEMA `+schema)
	for _, table := range []string{"repos", "snapshots", "refs", "context_history"} {
		name := pgx.Identifier{table}.Sanitize()
		catalogExecPG(t, ctx, tx, `CREATE TABLE `+schema+`.`+name+` (LIKE public.`+name+` INCLUDING ALL)`)
		column := "repo_id"
		if table == "repos" {
			column = "id"
		}
		catalogExecPG(t, ctx, tx, `INSERT INTO `+schema+`.`+name+` SELECT * FROM public.`+name+` WHERE `+column+`=$1`, string(repo))
	}
	catalogExecPG(t, ctx, tx, `SET LOCAL search_path TO `+schema+`, public`)
	catalogExecPG(t, ctx, tx, string(migration))
	var head, floor int64
	var epoch string
	if err := tx.QueryRow(ctx, `SELECT epoch::text,head_seq,floor_seq FROM repository_catalog_state WHERE repo_id=$1`, string(repo)).Scan(&epoch, &head, &floor); err != nil {
		t.Fatal(err)
	}
	if epoch == "" || head != 0 || floor != 0 {
		t.Fatalf("migration invented mutation generations: epoch=%q head=%d floor=%d", epoch, head, floor)
	}
	var count, kinds, nonzero int
	if err := tx.QueryRow(ctx, `SELECT count(*),count(DISTINCT entity_kind),count(*) FILTER (WHERE seq<>0 OR deleted OR after_image IS NULL) FROM repository_catalog_changes WHERE repo_id=$1`, string(repo)).Scan(&count, &kinds, &nonzero); err != nil {
		t.Fatal(err)
	}
	if count != 4 || kinds != 4 || nonzero != 0 {
		t.Fatalf("migration baseline did not seed all four entity kinds at zero: count=%d kinds=%d invalid=%d", count, kinds, nonzero)
	}
	catalogExecPG(t, ctx, tx, `UPDATE snapshots SET message='first post-migration mutation' WHERE repo_id=$1`, string(repo))
	if err := tx.QueryRow(ctx, `SELECT head_seq FROM repository_catalog_state WHERE repo_id=$1`, string(repo)).Scan(&head); err != nil || head != 1 {
		t.Fatalf("post-migration trigger head=%d err=%v", head, err)
	}
}

func TestPGCatalogRejectsTruncateWithoutLosingSourceRows(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	initializationPGSnapshot(t, s, repo, "truncate guard")
	before := catalogReadStatePG(t, ctx, s, repo)
	for _, table := range []string{"snapshots", "refs", "context_history", "repos"} {
		t.Run(table, func(t *testing.T) {
			tx := catalogTxPG(t, ctx, s)
			// CASCADE lets the statement reach the catalog guard instead of a
			// referencing-table error. It always runs in a rolled-back transaction.
			_, err := tx.Exec(ctx, `TRUNCATE TABLE `+pgx.Identifier{table}.Sanitize()+` CASCADE`)
			var pgerr *pgconn.PgError
			if !errors.As(err, &pgerr) || !strings.Contains(strings.ToLower(pgerr.Message), "catalog") {
				t.Fatalf("missing catalog TRUNCATE guard: %v", err)
			}
			if err := tx.Rollback(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
	if got := catalogReadStatePG(t, ctx, s, repo); got != before {
		t.Fatalf("rejected TRUNCATE changed state: %+v -> %+v", before, got)
	}
	if snapshots, err := s.ListSnapshots(ctx, repo, ""); err != nil || len(snapshots) != 1 {
		t.Fatalf("rejected TRUNCATE lost source rows: %+v %v", snapshots, err)
	}
}

func TestPGCatalogKeyRenameAndDeleteReinsertCoalesce(t *testing.T) {
	s, ctx, repo := catalogPG(t)
	a := initializationPGSnapshot(t, s, repo, "coalesce-a")
	b := initializationPGSnapshot(t, s, repo, "coalesce-b")
	catalogExecPG(t, ctx, s.pool, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','before',$2)`, string(repo), string(a.ID))
	checkpoint := catalogCheckpointPG(t, ctx, s, repo)
	tx := catalogTxPG(t, ctx, s)
	catalogExecPG(t, ctx, tx, `UPDATE refs SET name='after' WHERE repo_id=$1 AND kind='tag' AND name='before'`, string(repo))
	catalogExecPG(t, ctx, tx, `DELETE FROM refs WHERE repo_id=$1 AND kind='tag' AND name='after'`, string(repo))
	catalogExecPG(t, ctx, tx, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','after',$2)`, string(repo), string(b.ID))
	catalogExecPG(t, ctx, tx, `INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','ephemeral',$2)`, string(repo), string(b.ID))
	catalogExecPG(t, ctx, tx, `DELETE FROM refs WHERE repo_id=$1 AND kind='tag' AND name='ephemeral'`, string(repo))
	var original []byte
	if err := tx.QueryRow(ctx, `DELETE FROM snapshots WHERE repo_id=$1 AND id=$2 RETURNING to_jsonb(snapshots)`, string(repo), string(a.ID)).Scan(&original); err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, tx, `INSERT INTO snapshots SELECT * FROM jsonb_populate_record(NULL::snapshots,jsonb_set($1::jsonb,'{message}','"reinserted"'::jsonb))`, original)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	entries, end := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, After: &checkpoint, Limit: 1}), 1)
	if len(entries) != 4 || end.Sequence != checkpoint.Sequence+1 {
		t.Fatalf("same-transaction keys did not coalesce: %+v %+v", entries, end)
	}
	for _, entry := range entries {
		if entry.Sequence != end.Sequence {
			t.Fatalf("same transaction allocated multiple sequences: %+v", entries)
		}
		if entry.Kind == "snapshot" {
			if entry.Deleted || entry.Key != string(a.ID) || catalogJSONPG[domain.Snapshot](t, entry.Value).Message != "reinserted" {
				t.Fatalf("snapshot delete/reinsert did not keep final image: %+v", entry)
			}
			continue
		}
		key := catalogJSONPG[[]string](t, []byte(entry.Key))
		if len(key) != 2 || key[0] != "tag" {
			t.Fatalf("invalid raw ref key: %v", key)
		}
		switch key[1] {
		case "after":
			if entry.Deleted || catalogJSONPG[domain.Ref](t, entry.Value).Target != b.ID {
				t.Fatalf("ref delete/reinsert did not keep final target: %+v", entry)
			}
		case "before", "ephemeral":
			if !entry.Deleted {
				t.Fatalf("old/removed key must end as tombstone: %+v", entry)
			}
		default:
			t.Fatalf("unexpected ref after rename: %+v", entry)
		}
	}
	baseline, _ := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	if len(baseline) != 4 { // protocol, two snapshots, and only the renamed ref
		t.Fatalf("coalesced deletion resurrected in baseline: %+v", baseline)
	}
}

func TestPGCatalogFrozenImportCommitsOneCompleteGeneration(t *testing.T) {
	// The importer requires an empty target; use the existing isolated-database
	// helper rather than clearing any data in the shared disposable cluster.
	s := importTestStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source := NewFSStore(t.TempDir())
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := source.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	doc := domain.SessionDoc{CIR: domain.CIRDocument{Envelope: domain.CIREnvelope{CIRVersion: "1", SourceProvider: domain.ProviderClaude, Fidelity: domain.FidelityFull}, Events: []domain.CIREvent{{Kind: domain.EventMessage, Role: "user", Blocks: []domain.ContentBlock{{Type: "text", Text: "synthetic catalog import"}}}}}}
	raw, err := domain.CanonicalBytes(doc.CIR)
	if err != nil {
		t.Fatal(err)
	}
	doc.Hash = domain.HashContent(raw)
	if _, err := source.PutDoc(ctx, repo, doc); err != nil {
		t.Fatal(err)
	}
	created := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Microsecond)
	snap := domain.Snapshot{RepoID: repo, ID: doc.Hash, DocHash: doc.Hash, Provider: domain.ProviderClaude, Fidelity: domain.FidelityFull, Branch: "main", CreatedAt: created}
	if err := source.PutSnapshot(ctx, snap); err != nil {
		t.Fatal(err)
	}
	if err := source.CompareAndSwapRef(ctx, repo, domain.Ref{RepoID: repo, Kind: domain.RefTag, Name: "imported", Target: snap.ID}, ""); err != nil {
		t.Fatal(err)
	}
	if err := source.ApplyHistoryEvent(ctx, catalogHistoryPG(repo)); err != nil {
		t.Fatal(err)
	}
	if report, err := s.ImportFrozenFS(ctx, source.dataDir, false); err != nil || report.Applied {
		t.Fatalf("import dry-run=%+v err=%v", report, err)
	}
	catalogExpectErrorPG(t, ctx, s, repo, domain.CatalogRequest{Version: 1}, domain.ErrNotFound)
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM repository_catalog_changes WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("dry-run leaked catalog changes: count=%d err=%v", count, err)
	}
	if report, err := s.ImportFrozenFS(ctx, source.dataDir, true); err != nil || !report.Applied {
		t.Fatalf("import apply=%+v err=%v", report, err)
	}
	entries, checkpoint := catalogDrainPG(t, ctx, s, repo, catalogPagePG(t, ctx, s, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	if len(entries) != 4 || checkpoint.Sequence != 1 {
		t.Fatalf("import did not commit all four entity kinds in one generation: %+v %+v", entries, checkpoint)
	}
	for i, kind := range []string{"history", "protocol", "ref", "snapshot"} {
		if entries[i].Kind != kind || entries[i].Sequence != 1 {
			t.Fatalf("import catalog split or omitted source kind: %+v", entries)
		}
	}
	if image := catalogJSONPG[domain.Snapshot](t, entries[3].Value); image.ID != snap.ID || !image.CreatedAt.Equal(created) {
		t.Fatalf("import catalog preserved intermediate insert instead of final source timestamp: %+v", image)
	}
}
