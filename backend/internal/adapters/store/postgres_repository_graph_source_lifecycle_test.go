//go:build postgres

package store

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

const graphLifecycleMigration = "0101_repository_graph_source.sql"

func graphLifecycleStorePG(t *testing.T, upgraded bool) (*PostgresStore, context.Context, domain.ContentHash, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	storage, catalog := catalogUpgradeStore(t, ctx)
	if _, err := storage.ApplyMigrations(ctx, catalog); err != nil {
		t.Fatal(err)
	}
	legacy, upgrade := t.TempDir(), t.TempDir()
	migrations := "../../../../schemas/db/migrations"
	files, err := os.ReadDir(migrations)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".sql") || file.Name() <= catalogUpgradeVersion || file.Name() > graphLifecycleMigration {
			continue
		}
		destination := legacy
		if file.Name() == graphLifecycleMigration {
			destination = upgrade
		}
		raw, err := os.ReadFile(filepath.Join(migrations, file.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(destination, file.Name()), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := storage.ApplyMigrations(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	var version string
	var absent bool
	if err := storage.pool.QueryRow(ctx, `SELECT max(version),to_regclass('repository_graph_source_state') IS NULL FROM schema_migrations`).Scan(&version, &absent); err != nil {
		t.Fatal(err)
	}
	if version != "0100_pending_projection.sql" || !absent {
		t.Fatalf("expected schema0100 without graph source: version=%s absent=%v", version, absent)
	}
	if upgraded {
		if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 1 {
			t.Fatalf("graph migration: applied=%d err=%v", applied, err)
		}
	}
	repo := domain.HashContent([]byte(t.Name()))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	initializationPGSnapshot(t, storage, repo, "graph lifecycle snapshot")
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO pending_contexts(repo_id,session_id,data) VALUES($1,'graph-lifecycle-session','{"branch":"main"}'::jsonb)`, string(repo))
	return storage, ctx, repo, upgrade
}

type graphLifecycleEvidence struct {
	kind  string
	key   string
	image map[string]any
}

func graphLifecycleSeedPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) []graphLifecycleEvidence {
	t.Helper()
	origin := "github:example/graph-lifecycle"
	commit, tree, parent := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	participation, prContribution, contribution := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	receipt, admission, head, proof := strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64), strings.Repeat("7", 64)
	tx := catalogTxPG(t, ctx, storage)
	var evidence []graphLifecycleEvidence
	insert := func(kind, key, sql string, identity map[string]any, args ...any) {
		t.Helper()
		payload := map[string]any{"marker": "graph-lifecycle-private-evidence", "kind": kind, "stage": "seed"}
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		catalogExecPG(t, ctx, tx, sql, append([]any{string(repo), string(raw)}, args...)...)
		identity["repo_id"], identity["payload"] = string(repo), payload
		evidence = append(evidence, graphLifecycleEvidence{kind: kind, key: key, image: identity})
	}
	key := func(parts ...string) string {
		t.Helper()
		raw, err := json.Marshal(parts)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	insert("git_commit", key(origin, commit), `INSERT INTO git_commit_trees(repo_id,payload,origin,commit_oid) VALUES($1,$2::jsonb,$3,$4)`, map[string]any{"origin": origin, "commit_oid": commit}, origin, commit)
	insert("git_tree", key(origin, tree), `INSERT INTO git_tree_nodes(repo_id,payload,origin,oid) VALUES($1,$2::jsonb,$3,$4)`, map[string]any{"origin": origin, "oid": tree}, origin, tree)
	insert("git_delta", key(origin, commit, parent), `INSERT INTO git_commit_deltas(repo_id,payload,id,origin,commit_oid,parent_oid,keys,inverse_keys) VALUES($1,$2::jsonb,'graph-lifecycle-delta',$3,$4,$5,'{}','{}')`, map[string]any{"id": "graph-lifecycle-delta", "origin": origin, "commit_oid": commit, "parent_oid": parent}, origin, commit, parent)
	insert("git_observation", "graph-lifecycle-observation", `INSERT INTO git_ref_observations(repo_id,payload,id,observed_at) VALUES($1,$2::jsonb,'graph-lifecycle-observation',now())`, map[string]any{"id": "graph-lifecycle-observation"})
	insert("pr_checkpoint", key(origin, "17", "23"), `INSERT INTO pr_history_checkpoints(repo_id,payload,origin,provider_repository_id,number,version) VALUES($1,$2::jsonb,$3,17,23,1)`, map[string]any{"origin": origin, "provider_repository_id": float64(17), "number": float64(23), "version": float64(1)}, origin)
	insert("pr_participation", participation, `INSERT INTO pr_participations(repo_id,payload,id,origin,provider_repository_id,number) VALUES($1,$2::jsonb,$3,$4,17,23)`, map[string]any{"id": participation, "origin": origin, "provider_repository_id": float64(17), "number": float64(23)}, participation, origin)
	insert("pr_contribution", prContribution, `INSERT INTO pr_contributions(repo_id,payload,id) VALUES($1,$2::jsonb,$3)`, map[string]any{"id": prContribution}, prContribution)
	insert("git_contribution", contribution, `INSERT INTO git_contributions(repo_id,payload,id) VALUES($1,$2::jsonb,$3)`, map[string]any{"id": contribution}, contribution)
	insert("git_receipt", key(origin, "1"), `INSERT INTO git_head_receipts(repo_id,payload,origin,version,id) VALUES($1,$2::jsonb,$3,1,$4)`, map[string]any{"id": receipt, "origin": origin, "version": float64(1)}, origin, receipt)
	insert("git_admission", admission, `INSERT INTO git_contribution_admissions(repo_id,payload,id,admission_key,origin,receipt_id) VALUES($1,$2::jsonb,$3,$3,$4,$5)`, map[string]any{"id": admission, "admission_key": admission, "origin": origin, "receipt_id": receipt}, admission, origin, receipt)
	insert("git_context_head", head, `INSERT INTO git_context_heads(repo_id,payload,id,origin,destination_branch_id,destination_branch,read_version,receipt_id) VALUES($1,$2::jsonb,$3,$4,'graph-lifecycle-branch','main',1,$5)`, map[string]any{"id": head, "origin": origin, "destination_branch_id": "graph-lifecycle-branch", "destination_branch": "main", "read_version": float64(1), "receipt_id": receipt}, head, origin, receipt)
	insert("git_contribution_proof", proof, `INSERT INTO git_contribution_proofs(repo_id,payload,id,admission_id,context_head_id,contribution_id) VALUES($1,$2::jsonb,$3,$4,$5,$6)`, map[string]any{"id": proof, "admission_id": admission, "context_head_id": head, "contribution_id": contribution}, proof, admission, head, contribution)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return evidence
}

func graphLifecyclePagePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) domain.RepositoryViewSourcePage {
	t.Helper()
	page, err := storage.RepositoryGraphSourceChanges(ctx, repo, request)
	if err != nil {
		t.Fatal(err)
	}
	if page.Version != 1 || page.Scope != "repository-graph-v1" || page.RepoID != repo || page.Epoch == "" || page.Through < 0 || page.Entries == nil || (page.Mode != "baseline" && page.Mode != "delta") {
		t.Fatalf("invalid graph envelope: %+v", page)
	}
	limit := request.Limit
	if limit == 0 {
		limit = domain.DefaultRepositoryViewSourceLimit
	}
	if len(page.Entries) > limit {
		t.Fatalf("graph page exceeded requested limit: %+v", page)
	}
	for _, entry := range page.Entries {
		if entry.Key == "" || entry.Sequence < 0 || entry.Sequence > page.Through || (entry.Deleted && (len(entry.Value) != 0 || page.Mode == "baseline")) || (!entry.Deleted && !json.Valid(entry.Value)) {
			t.Fatalf("invalid graph entry: %+v", entry)
		}
	}
	if page.NextCursor != "" {
		if page.Checkpoint != nil || len(page.Entries) == 0 {
			t.Fatalf("partial graph page checkpointed or made no progress: %+v", page)
		}
	} else if page.Checkpoint == nil || page.Checkpoint.Version != page.Version || page.Checkpoint.Scope != page.Scope || page.Checkpoint.Epoch != page.Epoch || page.Checkpoint.Sequence != page.Through || page.Checkpoint.RepoID != repo {
		t.Fatalf("invalid graph checkpoint: %+v", page)
	}
	return page
}

func graphLifecycleDrainPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, first domain.RepositoryViewSourcePage) ([]domain.RepositoryViewSourceEntry, domain.RepositoryViewSourceCheckpoint) {
	t.Helper()
	entries := []domain.RepositoryViewSourceEntry{}
	page := first
	for count := 0; count < 128; count++ {
		if page.Scope != first.Scope || page.Epoch != first.Epoch || page.Through != first.Through || page.Mode != first.Mode {
			t.Fatalf("graph traversal changed generation: first=%+v page=%+v", first, page)
		}
		for _, entry := range page.Entries {
			if len(entries) != 0 {
				previous := entries[len(entries)-1]
				ordered := previous.Kind < entry.Kind || (previous.Kind == entry.Kind && previous.Key < entry.Key)
				if page.Mode == "delta" {
					ordered = previous.Sequence < entry.Sequence || (previous.Sequence == entry.Sequence && ordered)
				}
				if !ordered {
					t.Fatalf("graph entries repeated or unordered: %+v then %+v", previous, entry)
				}
			}
			entries = append(entries, entry)
		}
		if page.NextCursor == "" {
			return entries, *page.Checkpoint
		}
		page = graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: page.NextCursor, Limit: 2})
	}
	t.Fatal("graph traversal exceeded fixture bound")
	return nil, domain.RepositoryViewSourceCheckpoint{}
}

func graphLifecycleErrorPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, request domain.RepositoryViewSourceRequest, want error) {
	t.Helper()
	page, err := storage.RepositoryGraphSourceChanges(ctx, repo, request)
	if !errors.Is(err, want) || !reflect.DeepEqual(page, domain.RepositoryViewSourcePage{}) {
		t.Fatalf("graph read error=%v want=%v page=%+v", err, want, page)
	}
}

func graphLifecycleGenerationPG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash, want domain.RepositoryViewSourceCheckpoint) {
	t.Helper()
	if got, err := storage.ReadRepositoryGraphSourceGeneration(ctx, repo); err != nil || got != want {
		t.Fatalf("point generation differs from graph checkpoint: got=%+v want=%+v err=%v", got, want, err)
	}
}

func graphLifecycleAssertPrivatePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) {
	t.Helper()
	view, _ := repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}), 1)
	for _, entry := range view {
		switch entry.Kind {
		case "snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision":
		default:
			t.Fatalf("graph evidence leaked into repository-view-v1: %+v", entry)
		}
		if strings.Contains(string(entry.Value), "graph-lifecycle-private-evidence") {
			t.Fatalf("graph payload leaked into repository-view-v1: %+v", entry)
		}
	}
	public, _ := catalogDrainPG(t, ctx, storage, repo, catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1)
	for _, entry := range public {
		switch entry.Kind {
		case "snapshot", "ref", "history", "protocol":
		default:
			t.Fatalf("graph evidence leaked into public sync: %+v", entry)
		}
		if strings.Contains(string(entry.Value), "graph-lifecycle-private-evidence") {
			t.Fatalf("graph payload leaked into public sync: %+v", entry)
		}
	}
}

func TestPGRepositoryGraphSourceLifecycleUpgradeFrom0100(t *testing.T) {
	storage, ctx, repo, upgrade := graphLifecycleStorePG(t, false)
	evidence := graphLifecycleSeedPG(t, ctx, storage, repo)
	oldFirst := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	oldWant, oldCheckpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, oldFirst, 1)
	publicFirst := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	publicWant, publicCheckpoint := catalogDrainPG(t, ctx, storage, repo, publicFirst, 1)
	if oldFirst.NextCursor == "" || publicFirst.NextCursor == "" {
		t.Fatal("upgrade fixture requires pre-migration continuations")
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='latest before graph upgrade' WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=data || '{"dismissed":true}'::jsonb WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_contribution_proofs SET payload=payload || '{"stage":"latest-before-upgrade"}'::jsonb WHERE repo_id=$1`, string(repo))
	evidence[len(evidence)-1].image["payload"].(map[string]any)["stage"] = "latest-before-upgrade"
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO git_ref_observations(repo_id,id,payload,observed_at) VALUES($1,'removed-before-graph-upgrade','{}',now())`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM git_ref_observations WHERE repo_id=$1 AND id='removed-before-graph-upgrade'`, string(repo))
	oldDelta, oldEnd := repositoryViewSourceDeltaPG(t, ctx, storage, repo, oldCheckpoint)
	publicDelta, publicEnd := catalogDrainPG(t, ctx, storage, repo, catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicCheckpoint, Limit: 1}), 1)
	before := catalogReadStatePG(t, ctx, storage, repo)
	oldBaseline := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	var oldCoverage int64
	if err := storage.pool.QueryRow(ctx, `SELECT baseline_seq FROM repository_view_source_state WHERE repo_id=$1`, string(repo)).Scan(&oldCoverage); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(upgrade, graphLifecycleMigration)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte{}, raw...), []byte("\nSELECT 1/0;\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err == nil || applied != 0 {
		t.Fatalf("failed graph migration committed: applied=%d err=%v", applied, err)
	}
	var rolledBack bool
	if err := storage.pool.QueryRow(ctx, `SELECT to_regclass('repository_graph_source_state') IS NULL AND to_regclass('repository_graph_source_changes') IS NULL AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, graphLifecycleMigration).Scan(&rolledBack); err != nil || !rolledBack {
		t.Fatalf("failed graph migration left schema or history: rolledBack=%v err=%v", rolledBack, err)
	}
	if got := catalogReadStatePG(t, ctx, storage, repo); got != before {
		t.Fatalf("failed graph migration changed catalog generation: before=%+v after=%+v", before, got)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 1 {
		t.Fatalf("graph upgrade: applied=%d err=%v", applied, err)
	}
	if applied, err := storage.ApplyMigrations(ctx, upgrade); err != nil || applied != 0 {
		t.Fatalf("graph upgrade replay: applied=%d err=%v", applied, err)
	}
	if got := catalogReadStatePG(t, ctx, storage, repo); got != before {
		t.Fatalf("graph upgrade changed public generation: before=%+v after=%+v", before, got)
	}
	var epoch string
	var baseline, coverage int64
	if err := storage.pool.QueryRow(ctx, `SELECT graph.epoch::text,graph.baseline_seq,source.baseline_seq FROM repository_graph_source_state graph JOIN repository_view_source_state source USING(repo_id) WHERE graph.repo_id=$1`, string(repo)).Scan(&epoch, &baseline, &coverage); err != nil {
		t.Fatal(err)
	}
	if epoch != before.epoch || baseline != before.head || coverage != oldCoverage || baseline <= oldCheckpoint.Sequence {
		t.Fatalf("graph coverage invalidated old scope: epoch=%s graph=%d view=%d before=%+v", epoch, baseline, coverage, before)
	}
	if got, end := repositoryViewSourceDrainPG(t, ctx, storage, repo, oldFirst, 1); !reflect.DeepEqual(got, oldWant) || end != oldCheckpoint {
		t.Fatalf("graph migration invalidated old-source cursor: entries=%+v checkpoint=%+v", got, end)
	}
	if got, end := catalogDrainPG(t, ctx, storage, repo, publicFirst, 1); !reflect.DeepEqual(got, publicWant) || end != publicCheckpoint {
		t.Fatalf("graph migration invalidated public cursor: entries=%+v checkpoint=%+v", got, end)
	}
	if got, end := repositoryViewSourceDeltaPG(t, ctx, storage, repo, oldCheckpoint); !reflect.DeepEqual(got, oldDelta) || end != oldEnd {
		t.Fatalf("graph migration invalidated old-source checkpoint: entries=%+v checkpoint=%+v", got, end)
	}
	if got, end := catalogDrainPG(t, ctx, storage, repo, catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicCheckpoint, Limit: 1}), 1); !reflect.DeepEqual(got, publicDelta) || end != publicEnd {
		t.Fatalf("graph migration invalidated public checkpoint: entries=%+v checkpoint=%+v", got, end)
	}
	if got := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1}); !reflect.DeepEqual(got, oldBaseline) {
		t.Fatalf("graph migration changed old-source baseline: before=%+v after=%+v", oldBaseline, got)
	}
	entries, checkpoint := graphLifecycleDrainPG(t, ctx, storage, repo, graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 2}))
	if checkpoint.Sequence != baseline || checkpoint.Epoch != epoch || len(entries) != len(oldBaseline.Entries)+len(evidence) {
		t.Fatalf("graph baseline lost evidence or old-source images: entries=%+v checkpoint=%+v", entries, checkpoint)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, checkpoint)
	for _, expected := range oldBaseline.Entries {
		if got := repositoryViewSourceEntryPG(t, entries, expected.Kind, expected.Key); !reflect.DeepEqual(got, expected) {
			t.Fatalf("graph baseline changed inherited source image: got=%+v want=%+v", got, expected)
		}
	}
	for _, expected := range evidence {
		matches := 0
		for _, entry := range entries {
			if entry.Kind != expected.kind {
				continue
			}
			matches++
			key := entry.Key
			if strings.HasPrefix(key, "[") {
				canonical, err := json.Marshal(catalogJSONPG[[]string](t, []byte(key)))
				if err != nil {
					t.Fatal(err)
				}
				key = string(canonical)
			}
			if key != expected.key || entry.Sequence != baseline || entry.Deleted || !reflect.DeepEqual(catalogJSONPG[map[string]any](t, entry.Value), expected.image) {
				t.Fatalf("incorrect seeded %s: entry=%+v want=%+v", expected.kind, entry, expected)
			}
		}
		if matches != 1 {
			t.Fatalf("expected exactly one seeded %s, got %d", expected.kind, matches)
		}
	}
	prior := checkpoint
	prior.Sequence--
	graphLifecycleErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &prior}, domain.ErrCatalogResetRequired)
	graphLifecycleAssertPrivatePG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_contribution_proofs SET payload=payload || '{"stage":"post-upgrade"}'::jsonb WHERE repo_id=$1`, string(repo))
	changes := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &checkpoint})
	if changes.Through != checkpoint.Sequence+1 || len(changes.Entries) != 1 || changes.Entries[0].Kind != "git_contribution_proof" {
		t.Fatalf("first upgraded proof generation missing: %+v", changes)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, *changes.Checkpoint)
	if page := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &oldEnd}); len(page.Entries) != 0 || page.Through != changes.Through {
		t.Fatalf("post-upgrade proof leaked into old source: %+v", page)
	}
	if page := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, After: &publicEnd}); len(page.Entries) != 0 || page.Through != changes.Through {
		t.Fatalf("post-upgrade proof leaked into public sync: %+v", page)
	}
}

func TestPGRepositoryGraphSourceLifecyclePruneRetainsAnchors(t *testing.T) {
	storage, ctx, repo, _ := graphLifecycleStorePG(t, true)
	graphLifecycleSeedPG(t, ctx, storage, repo)
	old := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	for _, stage := range []string{"obsolete", "floor-anchor"} {
		catalogExecPG(t, ctx, storage.pool, `UPDATE git_contribution_proofs SET payload=jsonb_set(payload,'{stage}',to_jsonb($2::text)) WHERE repo_id=$1`, string(repo), stage)
	}
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM git_ref_observations WHERE repo_id=$1`, string(repo))
	floor := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_contribution_proofs SET payload=payload || '{"stage":"after-floor"}'::jsonb WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_tree_nodes SET payload=payload || '{"stage":"after-floor"}'::jsonb WHERE repo_id=$1`, string(repo))
	first := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	delta := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: floor.Checkpoint, Limit: 1})
	if first.NextCursor == "" || delta.NextCursor == "" {
		t.Fatal("prune fixture requires baseline and delta continuations")
	}
	want, checkpoint := graphLifecycleDrainPG(t, ctx, storage, repo, first)
	wantDelta, deltaCheckpoint := graphLifecycleDrainPG(t, ctx, storage, repo, delta)
	countRows := func() int {
		t.Helper()
		var count int
		if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_graph_source_changes WHERE repo_id=$1`, string(repo)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	beforeCount := countRows()
	before := catalogReadStatePG(t, ctx, storage, repo)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `SELECT cxt_prune_catalog($1,$2)`, string(repo), floor.Through)
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if countRows() != beforeCount || catalogReadStatePG(t, ctx, storage, repo) != before {
		t.Fatal("rolled-back prune changed graph history or generation")
	}
	if got, end := graphLifecycleDrainPG(t, ctx, storage, repo, first); !reflect.DeepEqual(got, want) || end != checkpoint {
		t.Fatalf("rolled-back prune invalidated graph cursor: %+v %+v", got, end)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, checkpoint)
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), floor.Through)
	if countRows() != beforeCount-3 {
		t.Fatalf("prune did not remove exactly two superseded proofs and one deleted observation: before=%d after=%d", beforeCount, countRows())
	}
	state := catalogReadStatePG(t, ctx, storage, repo)
	if state.epoch != before.epoch || state.head != before.head || state.floor != floor.Through {
		t.Fatalf("prune changed generation instead of retention floor: before=%+v after=%+v", before, state)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, checkpoint)
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, After: old.Checkpoint}, {Version: 1, Cursor: first.NextCursor}, {Version: 1, Cursor: delta.NextCursor}} {
		graphLifecycleErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	if got, end := graphLifecycleDrainPG(t, ctx, storage, repo, graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})); !reflect.DeepEqual(got, want) || end != checkpoint {
		t.Fatalf("prune changed graph baseline or resurrected evidence: %+v %+v", got, end)
	}
	if got, end := graphLifecycleDrainPG(t, ctx, storage, repo, graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: floor.Checkpoint, Limit: 1})); !reflect.DeepEqual(got, wantDelta) || end != deltaCheckpoint || len(got) != 2 {
		t.Fatalf("prune lost delta after valid floor: %+v %+v", got, end)
	}
	var anchorCount, tombstoneCount int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE entity_kind='git_contribution_proof' AND NOT deleted AND after_image->'payload'->>'stage'='floor-anchor'),count(*) FILTER (WHERE entity_kind='git_observation' AND deleted AND after_image IS NULL) FROM repository_graph_source_changes WHERE repo_id=$1 AND seq<=$2`, string(repo), floor.Through).Scan(&anchorCount, &tombstoneCount); err != nil || anchorCount != 1 || tombstoneCount != 1 {
		t.Fatalf("prune lost live/tombstone anchors: proof=%d tombstone=%d err=%v", anchorCount, tombstoneCount, err)
	}
	graphLifecycleAssertPrivatePG(t, ctx, storage, repo)
}

func TestPGRepositoryGraphSourceLifecycleResetAndRecreate(t *testing.T) {
	storage, ctx, repo, _ := graphLifecycleStorePG(t, true)
	graphLifecycleSeedPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM git_ref_observations WHERE repo_id=$1`, string(repo))
	other := domain.HashContent([]byte(t.Name() + "other"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	graphLifecycleSeedPG(t, ctx, storage, other)
	otherBefore := graphLifecyclePagePG(t, ctx, storage, other, domain.RepositoryViewSourceRequest{Version: 1})
	first := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	want, before := graphLifecycleDrainPG(t, ctx, storage, repo, first)
	oldFirst := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	oldWant, oldBefore := repositoryViewSourceDrainPG(t, ctx, storage, repo, oldFirst, 1)
	publicFirst := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	publicWant, publicBefore := catalogDrainPG(t, ctx, storage, repo, publicFirst, 1)
	tx := catalogTxPG(t, ctx, storage)
	catalogExecPG(t, ctx, tx, `SELECT cxt_reset_catalog($1)`, string(repo))
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got, end := graphLifecycleDrainPG(t, ctx, storage, repo, first); !reflect.DeepEqual(got, want) || end != before {
		t.Fatalf("rolled-back reset changed graph generation: %+v %+v", got, end)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, before)
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, After: &before}, {Version: 1, Cursor: first.NextCursor}} {
		graphLifecycleErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, After: &oldBefore}, {Version: 1, Cursor: oldFirst.NextCursor}} {
		repositoryViewSourceErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	for _, request := range []domain.CatalogRequest{{Version: 1, After: &publicBefore}, {Version: 1, Cursor: publicFirst.NextCursor}} {
		catalogExpectErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	for index := range want {
		want[index].Sequence = 0
	}
	for index := range oldWant {
		oldWant[index].Sequence = 0
	}
	for index := range publicWant {
		publicWant[index].Sequence = 0
	}
	got, after := graphLifecycleDrainPG(t, ctx, storage, repo, graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}))
	if !reflect.DeepEqual(got, want) || after.Epoch == before.Epoch || after.Sequence != 0 {
		t.Fatalf("reset did not reseed exact live graph at zero: %+v %+v", got, after)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, after)
	if entries, end := repositoryViewSourceDrainPG(t, ctx, storage, repo, repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1}), 1); !reflect.DeepEqual(entries, oldWant) || end.Epoch != after.Epoch || end.Sequence != 0 {
		t.Fatalf("graph reset changed old-source images: %+v %+v", entries, end)
	}
	if entries, end := catalogDrainPG(t, ctx, storage, repo, catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1}), 1); !reflect.DeepEqual(entries, publicWant) || end.Epoch != after.Epoch || end.Sequence != 0 {
		t.Fatalf("graph reset changed public images: %+v %+v", entries, end)
	}
	var oldRows int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM repository_graph_source_changes WHERE repo_id=$1 AND (epoch<>$2 OR seq<>0 OR deleted)`, string(repo), after.Epoch).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatalf("reset retained old graph generation or tombstones: rows=%d err=%v", oldRows, err)
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE git_contribution_proofs SET payload=payload || '{"stage":"after-reset"}'::jsonb WHERE repo_id=$1`, string(repo))
	change := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: &after})
	if change.Through != 1 || change.Epoch != after.Epoch || len(change.Entries) != 1 || change.Entries[0].Kind != "git_contribution_proof" || change.Entries[0].Sequence != 1 {
		t.Fatalf("post-reset proof did not start next generation: %+v", change)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, *change.Checkpoint)
	graphLifecycleAssertPrivatePG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM snapshots WHERE repo_id=$1`, string(repo))
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM repos WHERE id=$1`, string(repo))
	var remaining int
	if err := storage.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM repository_graph_source_state WHERE repo_id=$1)+(SELECT count(*) FROM repository_graph_source_changes WHERE repo_id=$1)`, string(repo)).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("repository deletion left graph state: rows=%d err=%v", remaining, err)
	}
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: repo}); err != nil {
		t.Fatal(err)
	}
	recreated := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1})
	if recreated.Epoch == after.Epoch || recreated.Epoch == before.Epoch || recreated.Through != 1 {
		t.Fatalf("recreated repository reused graph generation: %+v", recreated)
	}
	graphLifecycleGenerationPG(t, ctx, storage, repo, *recreated.Checkpoint)
	graphLifecycleErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, After: change.Checkpoint}, domain.ErrCatalogResetRequired)
	if otherAfter := graphLifecyclePagePG(t, ctx, storage, other, domain.RepositoryViewSourceRequest{Version: 1}); !reflect.DeepEqual(otherAfter, otherBefore) {
		t.Fatalf("reset/deletion crossed repositories: before=%+v after=%+v", otherBefore, otherAfter)
	}
	graphLifecycleAssertPrivatePG(t, ctx, storage, repo)
}

func TestPGRepositoryGraphSourceLifecycleRejectsCrossScopeCursors(t *testing.T) {
	storage, ctx, repo, _ := graphLifecycleStorePG(t, true)
	graphLifecycleSeedPG(t, ctx, storage, repo)
	graph := graphLifecyclePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	_, graphCheckpoint := graphLifecycleDrainPG(t, ctx, storage, repo, graph)
	view := repositoryViewSourcePagePG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Limit: 1})
	_, viewCheckpoint := repositoryViewSourceDrainPG(t, ctx, storage, repo, view, 1)
	public := catalogPagePG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Limit: 1})
	if graph.NextCursor == "" || view.NextCursor == "" || public.NextCursor == "" {
		t.Fatal("scope fixture requires real opaque cursors for all three readers")
	}
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, Cursor: view.NextCursor}, {Version: 1, After: &viewCheckpoint}} {
		graphLifecycleErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, Cursor: graph.NextCursor}, {Version: 1, After: &graphCheckpoint}} {
		repositoryViewSourceErrorPG(t, ctx, storage, repo, request, domain.ErrCatalogResetRequired)
	}
	graphLifecycleErrorPG(t, ctx, storage, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: public.NextCursor}, domain.ErrValidation)
	catalogExpectErrorPG(t, ctx, storage, repo, domain.CatalogRequest{Version: 1, Cursor: graph.NextCursor}, domain.ErrValidation)
	other := domain.HashContent([]byte(t.Name() + "other"))
	if _, err := storage.PutRepo(ctx, domain.Repo{ID: other}); err != nil {
		t.Fatal(err)
	}
	for _, request := range []domain.RepositoryViewSourceRequest{{Version: 1, Cursor: graph.NextCursor}, {Version: 1, After: &graphCheckpoint}} {
		graphLifecycleErrorPG(t, ctx, storage, other, request, domain.ErrCatalogResetRequired)
	}
	graphLifecycleAssertPrivatePG(t, ctx, storage, repo)
}
