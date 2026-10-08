//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const catalogUpgradeVersion = "0074_catalog_changes.sql"

// Use the configured server only to create a disposable database. Neither
// migrations nor fixture rows are written to the database in CXT_TEST_DSN.
func catalogUpgradeStore(t *testing.T, ctx context.Context) (*PostgresStore, string) {
	t.Helper()
	dsn := os.Getenv("CXT_TEST_DSN")
	if dsn == "" {
		t.Skip("CXT_TEST_DSN unset")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
	if err != nil {
		t.Fatal(err)
	}
	database := domain.NewID("catalog_upgrade_")
	quoted := pgx.Identifier{database}.Sanitize()
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	var pool *pgxpool.Pool
	t.Cleanup(func() {
		if pool != nil {
			pool.Close()
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := admin.Exec(cleanup, "DROP DATABASE "+quoted)
		admin.Close()
		if err != nil {
			t.Error(err)
		}
	})
	cfg.ConnConfig.Database = database
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	cfg.MaxConns = 6
	pool, err = pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	st := &PostgresStore{pool: pool}
	legacy, upgrade := t.TempDir(), t.TempDir()
	migrations := "../../../../schemas/db/migrations"
	files, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".sql") || f.Name() > catalogUpgradeVersion {
			continue
		}
		data, err := os.ReadFile(filepath.Join(migrations, f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(upgrade, f.Name()), data, 0600); err != nil {
			t.Fatal(err)
		}
		if f.Name() < catalogUpgradeVersion {
			if err = os.WriteFile(filepath.Join(legacy, f.Name()), data, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err = os.Stat(filepath.Join(upgrade, catalogUpgradeVersion)); err != nil {
		t.Fatal(err)
	}
	if _, err = st.ApplyMigrations(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	var version string
	var catalogAbsent bool
	if err = pool.QueryRow(ctx, `SELECT max(version), to_regclass('repository_catalog_state') IS NULL FROM schema_migrations`).Scan(&version, &catalogAbsent); err != nil {
		t.Fatal(err)
	}
	if version != "0073_repository_initialization.sql" || !catalogAbsent {
		t.Fatalf("expected pre-catalog schema through 0073; version=%q catalog absent=%v", version, catalogAbsent)
	}
	return st, upgrade
}

type catalogUpgradeKey struct{ repo, kind, key string }

type catalogUpgradeFixture struct {
	repo, emptyRepo, snapshot string
	images                    map[catalogUpgradeKey]map[string]any
}

func catalogUpgradeSeed(t *testing.T, ctx context.Context, st *PostgresStore) catalogUpgradeFixture {
	t.Helper()
	f := catalogUpgradeFixture{
		repo:      string(domain.HashContent([]byte("catalog-upgrade-full"))),
		emptyRepo: string(domain.HashContent([]byte("catalog-upgrade-empty"))),
		snapshot:  string(domain.HashContent([]byte("catalog-upgrade-child"))),
		images:    make(map[catalogUpgradeKey]map[string]any),
	}
	root := string(domain.HashContent([]byte("catalog-upgrade-root")))
	memory := string(domain.HashContent([]byte("catalog-upgrade-memory")))
	created := "2026-10-07T12:34:56.123456Z"
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := st.pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO repos(id,remote_url,team,context_protocol) VALUES($1,'https://example.test/full','fixture',1),($2,'https://example.test/empty','fixture',0)`, f.repo, f.emptyRepo)
	for _, h := range []string{root, f.snapshot, memory} {
		exec(`INSERT INTO blobs(hash,bytes) VALUES($1,$2)`, h, []byte(h))
	}
	exec(`INSERT INTO snapshots(repo_id,id,branch,doc_hash,provider,fidelity,created_at) VALUES($1,$2,'main',$2,'unknown','reconstructed',$3)`, f.repo, root, created)
	exec(`INSERT INTO snapshots(repo_id,id,branch,parents,doc_hash,memory_hash,claude_settings,agents_settings,codex_settings,
		provider,fidelity,message,author_name,author_email,author_team,created_at,grafted,graft_parents,graft_seq,session_id,models,compaction_count)
		VALUES($1,$2,'feature',$3,$2,$4,'claude-settings','agents-settings','codex-settings',
		'codex','full','superseded message','Fixture Author','author@example.test','fixture',$5,true,$3,7,'session-upgrade',$6,2)`,
		f.repo, f.snapshot, []string{root}, memory, created, []string{"model-a", "model-b"})
	// Seeding must capture current rows, not fabricate the pre-upgrade history.
	exec(`UPDATE snapshots SET message='current message' WHERE repo_id=$1 AND id=$2`, f.repo, f.snapshot)
	for _, id := range []string{root, f.snapshot} {
		f.images[catalogUpgradeKey{f.repo, "snapshot", id}] = map[string]any{
			"id": id, "repo_id": f.repo, "branch": "main", "parents": []string{}, "doc_hash": id,
			"memory_hash": "", "claude_settings": "", "agents_settings": "", "codex_settings": "",
			"provider": "unknown", "fidelity": "reconstructed", "message": "",
			"author": map[string]any{"name": "", "email": "", "team": ""}, "created_at": created,
			"grafted": false, "graft_parents": []string{}, "graft_seq": 0, "session_id": "",
			"models": []string{}, "compaction_count": 0,
		}
	}
	child := f.images[catalogUpgradeKey{f.repo, "snapshot", f.snapshot}]
	for key, value := range map[string]any{
		"branch": "feature", "parents": []string{root}, "memory_hash": memory,
		"claude_settings": "claude-settings", "agents_settings": "agents-settings", "codex_settings": "codex-settings",
		"provider": "codex", "fidelity": "full", "message": "current message",
		"author":  map[string]any{"name": "Fixture Author", "email": "author@example.test", "team": "fixture"},
		"grafted": true, "graft_parents": []string{root}, "graft_seq": 7, "session_id": "session-upgrade",
		"models": []string{"model-a", "model-b"}, "compaction_count": 2,
	} {
		child[key] = value
	}
	for _, ref := range []struct{ kind, name, target, symbolic, branchID string }{
		{"head", "HEAD", "", "main", ""},
		{"branch", "main", root, "", "branch-upgrade"},
		{"tag", "release", f.snapshot, "", ""},
		{"session", "session-upgrade", f.snapshot, "", "branch-upgrade"},
	} {
		exec(`INSERT INTO refs(repo_id,kind,name,target,symbolic,branch_id) VALUES($1,$2,$3,NULLIF($4,''),$5,$6)`,
			f.repo, ref.kind, ref.name, ref.target, ref.symbolic, ref.branchID)
		key, _ := json.Marshal([]string{ref.kind, ref.name})
		f.images[catalogUpgradeKey{f.repo, "ref", string(key)}] = map[string]any{
			"repo_id": f.repo, "kind": ref.kind, "name": ref.name, "target": ref.target, "symbolic": ref.symbolic, "branch_id": ref.branchID,
		}
	}
	exec(`INSERT INTO refs(repo_id,kind,name,target) VALUES($1,'tag','removed-before-upgrade',$2)`, f.repo, root)
	exec(`DELETE FROM refs WHERE repo_id=$1 AND name='removed-before-upgrade'`, f.repo)
	// This event's target deliberately differs from the raw branch ref. Migration
	// must retain both facts without replaying history or projecting a new ref.
	event := map[string]any{
		"id": strings.Repeat("a", 32), "repo_id": f.repo, "branch_id": "branch-upgrade", "branch": "main",
		"kind": "advance", "source": root, "target": f.snapshot, "created_at": created,
	}
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO context_history(repo_id,id,event,received_at) VALUES($1,$2,$3::jsonb,$4)`, f.repo, event["id"], string(body), created)
	f.images[catalogUpgradeKey{f.repo, "history", event["id"].(string)}] = event
	f.images[catalogUpgradeKey{f.repo, "protocol", f.repo}] = map[string]any{"context_protocol": 1}
	f.images[catalogUpgradeKey{f.emptyRepo, "protocol", f.emptyRepo}] = map[string]any{"context_protocol": 0}
	return f
}

func catalogUpgradeAssertBaseline(t *testing.T, ctx context.Context, st *PostgresStore, f catalogUpgradeFixture) {
	t.Helper()
	rows, err := st.pool.Query(ctx, `SELECT c.repo_id,c.entity_kind,c.entity_key,c.deleted,c.after_image,c.epoch=st.epoch
		FROM repository_catalog_changes c JOIN repository_catalog_state st USING(repo_id) WHERE c.seq=0`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := make(map[catalogUpgradeKey]bool)
	for rows.Next() {
		var key catalogUpgradeKey
		var deleted, sameEpoch bool
		var body []byte
		if err = rows.Scan(&key.repo, &key.kind, &key.key, &deleted, &body, &sameEpoch); err != nil {
			t.Fatal(err)
		}
		if key.kind == "ref" {
			var parts []string
			if err = json.Unmarshal([]byte(key.key), &parts); err != nil {
				t.Fatal(err)
			}
			canonical, _ := json.Marshal(parts)
			key.key = string(canonical)
		}
		want, ok := f.images[key]
		if !ok || seen[key] || deleted || !sameEpoch {
			t.Fatalf("unexpected baseline row %+v: known=%v duplicate=%v deleted=%v same epoch=%v", key, ok, seen[key], deleted, sameEpoch)
		}
		seen[key] = true
		var got map[string]any
		if err = json.Unmarshal(body, &got); err != nil {
			t.Fatal(err)
		}
		if key.kind == "snapshot" {
			stamp, ok := got["created_at"].(string)
			if !ok {
				t.Fatalf("missing snapshot timestamp: %s", body)
			}
			parsed, err := time.Parse(time.RFC3339Nano, stamp)
			if err != nil {
				t.Fatal(err)
			}
			got["created_at"] = parsed.UTC().Format(time.RFC3339Nano)
		}
		encoded, err := json.Marshal(want)
		if err != nil {
			t.Fatal(err)
		}
		var normalized map[string]any
		if err = json.Unmarshal(encoded, &normalized); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, normalized) {
			t.Errorf("baseline %+v:\n got %s\nwant %s", key, body, encoded)
		}
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != len(f.images) {
		t.Fatalf("baseline has %d images, want %d", len(seen), len(f.images))
	}
}

func catalogUpgradeContents(t *testing.T, ctx context.Context, st *PostgresStore) string {
	t.Helper()
	var contents string
	if err := st.pool.QueryRow(ctx, `SELECT jsonb_build_object(
		'state',(SELECT jsonb_agg(to_jsonb(s) ORDER BY repo_id) FROM repository_catalog_state s),
		'changes',(SELECT jsonb_agg(to_jsonb(c) ORDER BY repo_id,epoch,seq,entity_kind,entity_key) FROM repository_catalog_changes c)
	)::text`).Scan(&contents); err != nil {
		t.Fatal(err)
	}
	return contents
}

func TestPostgresCatalogUpgradeSeedsCurrentRowsAndPreservesCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	st, upgrade := catalogUpgradeStore(t, ctx)
	f := catalogUpgradeSeed(t, ctx, st)
	if n, err := st.ApplyMigrations(ctx, upgrade); err != nil || n != 1 {
		t.Fatalf("upgrade applied %d migrations: %v", n, err)
	}
	catalogUpgradeAssertBaseline(t, ctx, st, f)
	var states, epochs, pristine, changes int
	if err := st.pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT epoch),
		count(*) FILTER(WHERE head_seq=0 AND floor_seq=0 AND writer_xid IS NULL) FROM repository_catalog_state`).Scan(&states, &epochs, &pristine); err != nil {
		t.Fatal(err)
	}
	if states != 2 || epochs != 2 || pristine != 2 {
		t.Fatalf("baseline states=%d distinct epochs=%d pristine=%d, want 2 each", states, epochs, pristine)
	}
	if err := st.pool.QueryRow(ctx, `SELECT count(*) FROM repository_catalog_changes`).Scan(&changes); err != nil || changes != len(f.images) {
		t.Fatalf("baseline changes=%d, want %d: %v", changes, len(f.images), err)
	}
	// Advance the cursor before restarting, so an accidental reseed/reset cannot
	// hide behind another identical sequence-zero baseline.
	if _, err := st.pool.Exec(ctx, `UPDATE snapshots SET message='after upgrade' WHERE repo_id=$1 AND id=$2`, f.repo, f.snapshot); err != nil {
		t.Fatal(err)
	}
	var head int64
	if err := st.pool.QueryRow(ctx, `SELECT head_seq FROM repository_catalog_state WHERE repo_id=$1`, f.repo).Scan(&head); err != nil || head != 1 {
		t.Fatalf("post-upgrade head=%d, want 1: %v", head, err)
	}
	before := catalogUpgradeContents(t, ctx, st)
	if n, err := st.ApplyMigrations(ctx, upgrade); err != nil || n != 0 {
		t.Fatalf("restart applied %d migrations: %v", n, err)
	}
	if after := catalogUpgradeContents(t, ctx, st); after != before {
		t.Fatalf("restart changed catalog state or images:\nbefore %s\nafter  %s", before, after)
	}
	catalogUpgradeAssertBaseline(t, ctx, st, f)
}

func catalogUpgradeWaitForLock(t *testing.T, ctx context.Context, st *PostgresStore, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var found bool
		if err := st.pool.QueryRow(ctx, query, args...).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("expected migration/write lock was not observed: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func TestPostgresCatalogUpgradeBlocksWriterUntilBaselineCommits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	st, upgrade := catalogUpgradeStore(t, ctx)
	f := catalogUpgradeSeed(t, ctx, st)
	blocker, err := st.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback(context.Background())
	if _, err = blocker.Exec(ctx, `LOCK TABLE refs IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	type result struct {
		count int
		err   error
	}
	migrated := make(chan result, 1)
	go func() {
		n, err := st.ApplyMigrations(ctx, upgrade)
		migrated <- result{n, err}
	}()
	// Pause the actual migration after it locks snapshots but before seeding.
	// Observing pg_locks avoids assuming that a goroutine has reached its query.
	catalogUpgradeWaitForLock(t, ctx, st, `SELECT EXISTS(
		SELECT 1 FROM pg_locks held JOIN pg_locks waiting ON held.pid=waiting.pid
		WHERE held.database=(SELECT oid FROM pg_database WHERE datname=current_database())
		AND held.relation='snapshots'::regclass AND held.mode='ShareRowExclusiveLock' AND held.granted
		AND waiting.relation='refs'::regclass AND waiting.mode='ShareRowExclusiveLock' AND NOT waiting.granted)`)
	writer, err := st.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pid := writer.Conn().PgConn().PID()
	written := make(chan error, 1)
	go func() {
		defer writer.Release()
		_, err := writer.Exec(ctx, `UPDATE snapshots SET message='write after migration lock' WHERE repo_id=$1 AND id=$2`, f.repo, f.snapshot)
		written <- err
	}()
	catalogUpgradeWaitForLock(t, ctx, st, `SELECT EXISTS(SELECT 1 FROM pg_locks
		WHERE pid=$1 AND relation='snapshots'::regclass AND mode='RowExclusiveLock' AND NOT granted)`, int64(pid))
	if err = blocker.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-migrated:
		if r.err != nil || r.count != 1 {
			t.Fatalf("upgrade applied %d migrations: %v", r.count, r.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err = <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	catalogUpgradeAssertBaseline(t, ctx, st, f)
	var deltaCount int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM repository_catalog_changes c
		JOIN repository_catalog_state st USING(repo_id)
		WHERE c.repo_id=$1 AND c.epoch=st.epoch AND c.seq=1 AND st.head_seq=1 AND st.floor_seq=0
		AND c.entity_kind='snapshot' AND c.entity_key=$2 AND NOT c.deleted
		AND c.after_image->>'message'='write after migration lock'`, f.repo, f.snapshot).Scan(&deltaCount); err != nil {
		t.Fatal(err)
	}
	var total int
	if err = st.pool.QueryRow(ctx, `SELECT count(*) FROM repository_catalog_changes`).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if deltaCount != 1 || total != len(f.images)+1 {
		t.Fatalf("blocked write delta=%d total=%d; want one delta after %d baseline images", deltaCount, total, len(f.images))
	}
}
