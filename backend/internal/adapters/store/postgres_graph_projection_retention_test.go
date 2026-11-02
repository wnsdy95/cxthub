//go:build postgres

package store

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGraphProjectionIndependentSchemaAndKinds(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	for _, relation := range []struct{ table, parent string }{
		{"graph_projection_work", "repos"},
		{"graph_projection_inputs", "graph_projection_work"},
		{"graph_projection_parts", "graph_projection_work"},
		{"graph_projection_ready", "graph_projection_work"},
	} {
		var foreignKeys, correct int
		if err := storage.pool.QueryRow(ctx, `SELECT count(*),count(*) FILTER (WHERE confrelid=$2::regclass AND confdeltype='c')
			FROM pg_constraint WHERE conrelid=$1::regclass AND contype='f'`, relation.table, relation.parent).Scan(&foreignKeys, &correct); err != nil || foreignKeys != 1 || correct != 1 {
			t.Fatalf("%s has non-independent FKs: count=%d correct=%d err=%v", relation.table, foreignKeys, correct, err)
		}
	}
	job := graphProjectionClaimPG(t, ctx, storage, repo)
	for _, kind := range []string{"snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision",
		"git_commit", "git_tree", "git_delta", "git_observation", "pr_checkpoint", "pr_participation", "pr_contribution", "git_contribution", "git_receipt", "git_admission", "git_context_head", "git_contribution_proof"} {
		if !domain.RepositoryGraphSourceKindValid(kind) {
			t.Fatalf("schema fixture includes a non-graph kind: %s", kind)
		}
		catalogExecPG(t, ctx, storage.pool, `INSERT INTO graph_projection_inputs(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
			VALUES($1,$2,0,$3,'schema-fixture',false,'{}')`, string(repo), job.Dataset, kind)
	}
	_, err := storage.pool.Exec(ctx, `INSERT INTO graph_projection_inputs(repo_id,dataset,source_seq,kind,entity_key,deleted,image)
		VALUES($1,$2,0,'secret','schema-fixture',false,'{}')`, string(repo), job.Dataset)
	var constraint *pgconn.PgError
	if !errors.As(err, &constraint) || constraint.Code != "23514" {
		t.Fatalf("graph input allowlist accepted unknown kind: %v", err)
	}
	if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	pending := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM graph_projection_work WHERE repo_id=$1`, string(repo))
	var rows int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM graph_projection_inputs WHERE repo_id=$1`, string(repo)).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("graph work deletion failed to cascade independently: rows=%d err=%v", rows, err)
	}
	if after := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, pending) {
		t.Fatal("deleting graph work cascaded into pending work")
	}
}

func TestPGGraphProjectionPinsAreIndependentAndBounded(t *testing.T) {
	for _, owner := range []string{"graph", "pending"} {
		t.Run(owner, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
				t.Fatal(err)
			}
			graphProjectionTreePG(t, ctx, storage, repo)
			job := graphProjectionClaimPG(t, ctx, storage, repo)
			page := graphProjectionPagePG(t, ctx, storage, job, 1)
			if page.NextCursor == "" {
				t.Fatal("pin fixture requires a partial graph baseline")
			}
			if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
				t.Fatal(err)
			}
			inactive := "pending_projection_work"
			if owner == "pending" {
				inactive = "graph_projection_work"
			}
			catalogExecPG(t, ctx, storage.pool, `UPDATE `+inactive+` SET enabled=false WHERE repo_id=$1`, string(repo))
			catalogExecPG(t, ctx, storage.pool, `DELETE FROM git_tree_nodes WHERE repo_id=$1`, string(repo))
			catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='after graph pin' WHERE repo_id=$1`, string(repo))
			state := catalogReadStatePG(t, ctx, storage, repo)
			var bounded bool
			var pin int64
			if err := storage.pool.QueryRow(ctx, `SELECT pin_seq,retention_until-clock_timestamp() BETWEEN interval '23 hours 59 minutes' AND interval '24 hours 1 second'
				FROM `+owner+`_projection_work WHERE repo_id=$1`, string(repo)).Scan(&pin, &bounded); err != nil || !bounded || pin >= state.head {
				t.Fatalf("%s pin was unbounded or failed to protect old history: pin=%d bounded=%v err=%v", owner, pin, bounded, err)
			}
			_, err := storage.pool.Exec(ctx, `SELECT cxt_prune_catalog($1,$2)`, string(repo), state.head)
			var constraint *pgconn.PgError
			if !errors.As(err, &constraint) || constraint.Code != "23514" || !strings.Contains(constraint.Message, owner+" projection pin") {
				t.Fatalf("%s pin did not independently block prune: %v", owner, err)
			}
			if after := catalogReadStatePG(t, ctx, storage, repo); after != state {
				t.Fatal("blocked prune advanced shared source state")
			}
			if _, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: page.NextCursor}); err != nil {
				t.Fatalf("independent pin did not protect fixed graph cursor: %v", err)
			}
			catalogExecPG(t, ctx, storage.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), pin)
		})
	}
}

func TestPGGraphProjectionInactivePinsAllowExistingGraphAndViewPruning(t *testing.T) {
	for _, inactive := range []struct{ name, change string }{
		{"disabled", `enabled=false`},
		{"expired", `retention_until=clock_timestamp()-interval '1 second'`},
		{"wrong_epoch", `source_epoch=gen_random_uuid()`},
	} {
		t.Run(inactive.name, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			graphProjectionTreePG(t, ctx, storage, repo)
			job := graphProjectionClaimPG(t, ctx, storage, repo)
			page := graphProjectionPagePG(t, ctx, storage, job, 1)
			if page.NextCursor == "" {
				t.Fatal("fixture requires graph continuation")
			}
			if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
				t.Fatal(err)
			}
			catalogExecPG(t, ctx, storage.pool, `DELETE FROM git_tree_nodes WHERE repo_id=$1`, string(repo))
			catalogExecPG(t, ctx, storage.pool, `UPDATE snapshots SET message='after expired graph capture' WHERE repo_id=$1`, string(repo))
			catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
			catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET `+inactive.change+` WHERE repo_id=$1`, string(repo))
			sources := []struct{ table, kind string }{
				{"repository_catalog_changes", "snapshot"},
				{"repository_view_source_changes", "pending"},
				{"repository_graph_source_changes", "git_tree"},
			}
			for _, source := range sources {
				var rows int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM `+source.table+` WHERE repo_id=$1 AND entity_kind=$2`, string(repo), source.kind).Scan(&rows); err != nil || rows < 2 {
					t.Fatalf("prune fixture lacks superseded %s images: rows=%d err=%v", source.table, rows, err)
				}
			}
			state := catalogReadStatePG(t, ctx, storage, repo)
			catalogExecPG(t, ctx, storage.pool, `SELECT cxt_prune_catalog($1,$2)`, string(repo), state.head)
			if after := catalogReadStatePG(t, ctx, storage, repo); after.floor != state.head {
				t.Fatalf("inactive graph pin blocked catalog floor: %+v", after)
			}
			for _, source := range sources {
				var rows int
				if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM `+source.table+` WHERE repo_id=$1 AND entity_kind=$2`, string(repo), source.kind).Scan(&rows); err != nil || rows != 1 {
					t.Fatalf("0101 pruning no longer coalesces %s: rows=%d err=%v", source.table, rows, err)
				}
			}
			if _, err := storage.RepositoryGraphSourceChanges(ctx, repo, domain.RepositoryViewSourceRequest{Version: 1, Cursor: page.NextCursor}); !errors.Is(err, domain.ErrCatalogResetRequired) {
				t.Fatalf("pruned graph continuation did not require reset: %v", err)
			}
			if inactive.name == "disabled" {
				if err := storage.ConfigureGraphProjection(ctx, repo, true); err != nil {
					t.Fatal(err)
				}
			}
			fresh := graphProjectionClaimPG(t, ctx, storage, repo)
			if fresh.Dataset == job.Dataset || fresh.AppliedSequence != -1 || fresh.InputSequence != -1 || fresh.Cursor != "" || fresh.Phase != "capture" {
				t.Fatalf("pruned graph reused incomplete dataset: before=%+v after=%+v", job, fresh)
			}
			fresh = graphProjectionCapturePG(t, ctx, storage, fresh, 1)
			graph := graphProjectionPublishPG(t, ctx, storage, fresh)
			if len(graph.HoldCounts) != 0 || len(graph.OrphanSessions) != 0 {
				t.Fatal("prune/rebootstrap lost the latest private pending image")
			}
		})
	}
}

func TestPGGraphProjectionUsesMaximumGraphAndViewBaseline(t *testing.T) {
	for _, higher := range []string{"repository_graph_source_state", "repository_view_source_state"} {
		t.Run(higher, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
			priorGraph := graphProjectionPublishPG(t, ctx, storage, job)
			priorRead := graphProjectionReadPG(t, ctx, storage, job, priorGraph)
			prior := job
			catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
			state := catalogReadStatePG(t, ctx, storage, repo)
			catalogExecPG(t, ctx, storage.pool, `UPDATE repository_graph_source_state SET baseline_seq=$2 WHERE repo_id=$1`, string(repo), prior.InputSequence)
			catalogExecPG(t, ctx, storage.pool, `UPDATE repository_view_source_state SET baseline_seq=$2 WHERE repo_id=$1`, string(repo), prior.InputSequence)
			catalogExecPG(t, ctx, storage.pool, `UPDATE `+higher+` SET baseline_seq=$2 WHERE repo_id=$1`, string(repo), state.head)
			job = graphProjectionClaimPG(t, ctx, storage, repo)
			if job.Dataset == prior.Dataset || job.AppliedSequence != -1 || job.InputSequence != -1 || job.Cursor != "" || job.SourceRequest().After != nil {
				t.Fatalf("graph ignored maximum source baseline from %s: before=%+v after=%+v", higher, prior, job)
			}
			graphProjectionPriorReadPG(t, ctx, storage, prior, priorRead.Parts)
			page := graphProjectionPagePG(t, ctx, storage, job, 1)
			cursor, err := decodeRepositoryViewSourceCursor(page.NextCursor)
			if err != nil || cursor.Baseline != state.head || cursor.Checkpoint.Scope != domain.RepositoryGraphSourceScope {
				t.Fatalf("graph cursor used wrong combined baseline: %+v err=%v", cursor, err)
			}
			if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
				t.Fatalf("graph engine rejected combined-baseline cursor: %v", err)
			}
			resumed := graphProjectionClaimPG(t, ctx, storage, repo)
			if resumed.Dataset != job.Dataset || resumed.Cursor != page.NextCursor {
				t.Fatalf("graph engine reset valid max-baseline progress: %+v", resumed)
			}
			resumed = graphProjectionCapturePG(t, ctx, storage, resumed, 1)
			graph := graphProjectionPublishPG(t, ctx, storage, resumed)
			if len(graph.HoldCounts) != 0 {
				t.Fatal("baseline reset lost current graph source")
			}
		})
	}
}
