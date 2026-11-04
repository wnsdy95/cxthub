//go:build postgres

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGRepositoryViewSourceUpgradePreservesPublicCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	store, _ := catalogUpgradeStore(t, ctx)
	legacy := t.TempDir()
	files, err := os.ReadDir("../../../../schemas/db/migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".sql") || file.Name() >= "0099" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("../../../../schemas/db/migrations", file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacy, file.Name()), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.ApplyMigrations(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := store.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	initializationPGSnapshot(t, store, repo, "migration source")
	catalogExecPG(t, ctx, store.pool, `INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,'upgrade-session','{"branch":"main","private_extra":"do-not-project"}'::jsonb)`, string(repo))
	before := catalogReadStatePG(t, ctx, store, repo)
	publicBefore, err := store.CatalogChanges(ctx, repo, domain.CatalogRequest{Version: 1})
	if err != nil || publicBefore.Checkpoint == nil {
		t.Fatalf("public baseline: %+v %v", publicBefore, err)
	}
	raw, err := os.ReadFile("../../../../schemas/db/migrations/0099_repository_view_source.sql")
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(legacy, "0099_repository_view_source.sql")
	if err := os.WriteFile(broken, append(append([]byte{}, raw...), []byte("\nSELECT 1/0;\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMigrations(ctx, legacy); err == nil {
		t.Fatal("failed migration committed")
	}
	var absent bool
	if err := store.pool.QueryRow(ctx, `SELECT to_regclass('repository_view_source_state') IS NULL`).Scan(&absent); err != nil || !absent {
		t.Fatalf("failed migration left derived state: %v %v", absent, err)
	}
	if got := catalogReadStatePG(t, ctx, store, repo); got != before {
		t.Fatalf("failed migration changed public state: %+v %+v", before, got)
	}
	if err := os.WriteFile(broken, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMigrations(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if got := catalogReadStatePG(t, ctx, store, repo); got != before {
		t.Fatalf("migration invalidated existing CLI cursors: %+v %+v", before, got)
	}
	publicAfter, err := store.CatalogChanges(ctx, repo, domain.CatalogRequest{Version: 1})
	if err != nil || !reflect.DeepEqual(publicBefore, publicAfter) {
		t.Fatalf("public catalog changed: before=%+v after=%+v err=%v", publicBefore, publicAfter, err)
	}
	var baseline int64
	if err := store.pool.QueryRow(ctx, `SELECT baseline_seq FROM repository_view_source_state WHERE repo_id=$1`, string(repo)).Scan(&baseline); err != nil || baseline != before.head {
		t.Fatalf("coverage boundary = %d want %d: %v", baseline, before.head, err)
	}
	source, err := store.RepositoryViewSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1})
	if err != nil || source.Checkpoint == nil || source.Through != baseline {
		t.Fatalf("new source baseline: %+v %v", source, err)
	}
	found := false
	for _, entry := range source.Entries {
		if entry.Kind == "pending" {
			found = true
			if entry.Key != "upgrade-session" || entry.Sequence != baseline || strings.Contains(string(entry.Value), "do-not-project") {
				t.Fatalf("invalid seeded pending: %+v", entry)
			}
		}
	}
	if !found {
		t.Fatal("migration omitted preexisting pending")
	}
	prior := *source.Checkpoint
	prior.Sequence--
	if _, err := store.RepositoryViewSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &prior}); !errors.Is(err, domain.ErrCatalogResetRequired) {
		t.Fatalf("accepted unsupported pre-upgrade coverage: %v", err)
	}
	catalogExecPG(t, ctx, store.pool, `UPDATE pending_contexts SET data=data || '{"dismissed":true}'::jsonb WHERE repo_id=$1`, string(repo))
	publicDelta, err := store.CatalogChanges(ctx, repo, domain.CatalogRequest{Version: 1, After: publicBefore.Checkpoint})
	if err != nil || len(publicDelta.Entries) != 0 || publicDelta.Checkpoint == nil || publicDelta.Through != before.head+1 {
		t.Fatalf("private update leaked into public catalog: %+v %v", publicDelta, err)
	}
}
