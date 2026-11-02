//go:build postgres

package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphProjectionTreePG(t *testing.T, ctx context.Context, storage *PostgresStore, repo domain.ContentHash) {
	t.Helper()
	tree, err := domain.NewGitTreeNode(map[string]domain.GitEntry{"file": {OID: strings.Repeat("d", 40), Mode: "100644"}}, 40)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(tree)
	if err != nil {
		t.Fatal(err)
	}
	catalogExecPG(t, ctx, storage.pool, `INSERT INTO git_tree_nodes(repo_id,origin,oid,payload)
		VALUES($1,'https://github.com/example/graph-projection',$2,$3)`, string(repo), tree.OID, raw)
}

func TestPGGraphProjectionDeltaAndUnchangedVersionsIndependentOfPending(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	if err := storage.ConfigurePendingProjection(ctx, repo, true); err != nil {
		t.Fatal(err)
	}
	pendingJob := pendingProjectionCapturePG(t, ctx, storage, pendingProjectionClaimPG(t, ctx, storage, repo), 0)
	pendingView, pendingParts := pendingProjectionPartsPG(t, ctx, storage, pendingJob)
	if err := storage.PublishPendingProjection(ctx, pendingJob, pendingParts); err != nil {
		t.Fatal(err)
	}
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graph := graphProjectionPublishPG(t, ctx, storage, job)
	prior, priorRead := job, graphProjectionReadPG(t, ctx, storage, job, graph)
	pendingBefore := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo)
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	graphProjectionTreePG(t, ctx, storage, repo)
	job = graphProjectionClaimPG(t, ctx, storage, repo)
	page := graphProjectionPagePG(t, ctx, storage, job, 0)
	if page.Mode != "delta" || page.Checkpoint == nil || len(page.Entries) != 1 || page.Entries[0].Kind != "git_tree" || job.SourceRequest().After.Scope != domain.RepositoryGraphSourceScope {
		t.Fatalf("graph-only mutation was not captured as a graph delta: %+v", page)
	}
	if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	captured := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(captured.ready, before.ready) || !reflect.DeepEqual(captured.parts, before.parts) {
		t.Fatal("capture changed the old graph publication before projecting")
	}
	graphProjectionPriorReadPG(t, ctx, storage, prior, priorRead.Parts)
	if read, err := storage.ReadPendingProjection(ctx, repo); err != nil || read.Ready || len(read.Parts) != 0 {
		t.Fatalf("lagged pending projection exposed prior parts: %+v err=%v", read, err)
	}
	project := graphProjectionClaimPG(t, ctx, storage, repo)
	inputs, err := storage.LoadGraphProjectionInputs(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	var changed int
	for _, input := range inputs {
		if input.Sequence == project.InputSequence {
			changed++
			if input.Kind != "git_tree" {
				t.Fatalf("unchanged graph input was recopied: %+v", input)
			}
		}
	}
	if changed != 1 {
		t.Fatalf("graph delta changed %d input versions, want 1", changed)
	}
	if got := graphProjectionPublishPG(t, ctx, storage, project); !reflect.DeepEqual(got, graph) {
		t.Fatal("unreferenced Git tree changed the graph fixture")
	}
	after := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(after.parts, before.parts) {
		t.Fatal("unchanged graph output rewrote part versions")
	}
	if got := pendingProjectionEmptyDeltaSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(got, pendingBefore) {
		t.Fatal("graph capture/publication modified pending work or rows")
	}
	pendingJob = pendingProjectionClaimPG(t, ctx, storage, repo)
	pendingPage := repositoryViewSourcePagePG(t, ctx, storage, repo, pendingJob.SourceRequest())
	if len(pendingPage.Entries) != 0 || pendingPage.Checkpoint == nil || pendingPage.Scope != domain.RepositoryViewSourceScope {
		t.Fatalf("graph-only evidence leaked into pending inputs: %+v", pendingPage)
	}
	if err := storage.SavePendingProjectionPage(ctx, pendingJob, pendingPage); err != nil {
		t.Fatal(err)
	}
	pendingJob.InputSequence = pendingPage.Through
	pendingProjectionReadPG(t, ctx, storage, pendingJob, pendingView)
	if got := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(got, after) {
		t.Fatal("pending empty delta rewrote graph durable state")
	}
	catalogExecPG(t, ctx, storage.pool, `DELETE FROM git_tree_nodes WHERE repo_id=$1`, string(repo))
	job = graphProjectionClaimPG(t, ctx, storage, repo)
	page = graphProjectionPagePG(t, ctx, storage, job, 0)
	if len(page.Entries) != 1 || !page.Entries[0].Deleted || len(page.Entries[0].Value) != 0 {
		t.Fatalf("graph delta lost evidence tombstone: %+v", page)
	}
	if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	project = graphProjectionClaimPG(t, ctx, storage, repo)
	inputs, err = storage.LoadGraphProjectionInputs(ctx, project)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range inputs {
		if input.Kind == "git_tree" || input.Deleted {
			t.Fatal("deleted graph input survived latest-image loading")
		}
	}
	graphProjectionPublishPG(t, ctx, storage, project)
}

func TestPGGraphProjectionEmptyDeltaAdvancesOnlyCheckpoint(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graph := graphProjectionPublishPG(t, ctx, storage, job)
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `UPDATE repository_catalog_state SET head_seq=head_seq+1 WHERE repo_id=$1`, string(repo))
	job = graphProjectionClaimPG(t, ctx, storage, repo)
	page := graphProjectionPagePG(t, ctx, storage, job, 0)
	if page.Mode != "delta" || len(page.Entries) != 0 || page.Checkpoint == nil || page.NextCursor != "" || page.Through <= job.AppliedSequence {
		t.Fatalf("fixture did not produce a whole empty graph delta: %+v", page)
	}
	if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
		t.Fatal(err)
	}
	job.InputSequence = page.Through
	graphProjectionReadPG(t, ctx, storage, job, graph)
	after := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(after.inputs, before.inputs) || !reflect.DeepEqual(after.parts, before.parts) {
		t.Fatal("empty graph delta rewrote materialized inputs or parts")
	}
	if _, err := storage.ClaimGraphProjection(ctx, time.Minute); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("empty graph delta scheduled a redundant projection: %v", err)
	}
}

func TestPGGraphProjectionEmptyPartialTailStillRequiresProject(t *testing.T) {
	for _, mode := range []string{"baseline", "delta"} {
		t.Run(mode, func(t *testing.T) {
			storage, ctx, repo := graphProjectionPG(t)
			if mode == "delta" {
				job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
				graphProjectionPublishPG(t, ctx, storage, job)
				catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
			}
			job := graphProjectionClaimPG(t, ctx, storage, repo)
			page := graphProjectionPagePG(t, ctx, storage, job, 0)
			if page.Checkpoint == nil || len(page.Entries) == 0 || page.Mode != mode {
				t.Fatalf("partial-tail fixture requires complete nonempty input: %+v", page)
			}
			var floor, baseline int64
			if err := storage.pool.QueryRow(ctx, `SELECT catalog.floor_seq,GREATEST(graph.baseline_seq,view_source.baseline_seq)
				FROM repository_catalog_state catalog JOIN repository_graph_source_state graph USING(repo_id)
				JOIN repository_view_source_state view_source USING(repo_id) WHERE repo_id=$1`, string(repo)).Scan(&floor, &baseline); err != nil {
				t.Fatal(err)
			}
			last := page.Entries[len(page.Entries)-1]
			cursor := repositoryViewSourceCursor{Checkpoint: *page.Checkpoint, Floor: floor, Baseline: baseline, Mode: mode,
				Sequence: last.Sequence, Kind: last.Kind, Key: last.Key}
			if mode == "delta" {
				cursor.After = job.AppliedSequence
			}
			raw, err := json.Marshal(cursor)
			if err != nil {
				t.Fatal(err)
			}
			page.Checkpoint, page.NextCursor = nil, base64.RawURLEncoding.EncodeToString(raw)
			if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
				t.Fatal(err)
			}
			job = graphProjectionClaimPG(t, ctx, storage, repo)
			before := graphProjectionSnapshotPG(t, ctx, storage, repo)
			page = graphProjectionPagePG(t, ctx, storage, job, 0)
			if page.Checkpoint == nil || len(page.Entries) != 0 || page.NextCursor != "" {
				t.Fatalf("fixture did not reach empty final graph tail: %+v", page)
			}
			if err := storage.SaveGraphProjectionPage(ctx, job, page); err != nil {
				t.Fatal(err)
			}
			after := graphProjectionSnapshotPG(t, ctx, storage, repo)
			if !reflect.DeepEqual(after.ready, before.ready) || !reflect.DeepEqual(after.parts, before.parts) || !reflect.DeepEqual(after.inputs, before.inputs) {
				t.Fatal("empty partial tail fabricated graph readiness or materialized rows")
			}
			project := graphProjectionClaimPG(t, ctx, storage, repo)
			if project.Phase != "project" || project.InputSequence != page.Through || project.Dataset != job.Dataset {
				t.Fatalf("empty partial tail skipped graph projection: %+v", project)
			}
			graph := graphProjectionPublishPG(t, ctx, storage, project)
			if (mode == "baseline" && graph.HoldCounts["main"] != 1) || (mode == "delta" && len(graph.HoldCounts) != 0) {
				t.Fatalf("empty tail lost previously captured graph inputs: %+v", graph)
			}
		})
	}
}

func TestPGGraphProjectionEpochResetRetainsOldReadyWithoutExposingIt(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graphProjectionPublishPG(t, ctx, storage, job)
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	job = graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	_, parts := graphProjectionPartsPG(t, ctx, storage, job)
	catalogExecPG(t, ctx, storage.pool, `SELECT cxt_reset_catalog($1)`, string(repo))
	current := catalogReadStatePG(t, ctx, storage, repo)
	for _, readMethod := range []func(context.Context, domain.ContentHash) (domain.GraphProjectionRead, error){storage.ReadGraphProjection, storage.ReadGraphProjectionStatus} {
		read, err := readMethod(ctx, repo)
		if err != nil || !read.Enabled || read.Ready || read.Complete || len(read.Parts) != 0 || read.Source.Epoch != current.epoch || read.Source.Epoch == job.Epoch {
			t.Fatalf("old graph publication survived epoch reset: %+v err=%v", read, err)
		}
	}
	for _, err := range []error{storage.PublishGraphProjection(ctx, job, parts), storage.RenewGraphProjection(ctx, job, time.Minute)} {
		if !errors.Is(err, domain.ErrCatalogResetRequired) {
			t.Fatalf("old-epoch graph lease survived reset: %v", err)
		}
	}
	catalogExecPG(t, ctx, storage.pool, `UPDATE graph_projection_work SET lease_until=clock_timestamp()-interval '1 second' WHERE repo_id=$1`, string(repo))
	fresh := graphProjectionClaimPG(t, ctx, storage, repo)
	if fresh.Dataset == job.Dataset || fresh.Epoch != current.epoch || fresh.AppliedSequence != -1 || fresh.InputSequence != -1 || fresh.Cursor != "" || fresh.Phase != "capture" {
		t.Fatalf("reset reused old graph progress: before=%+v after=%+v", job, fresh)
	}
	retained := graphProjectionSnapshotPG(t, ctx, storage, repo)
	if !reflect.DeepEqual(retained.ready, before.ready) || !reflect.DeepEqual(retained.parts, before.parts) {
		t.Fatal("graph rebootstrap destroyed the previous ready publication")
	}
	fresh = graphProjectionCapturePG(t, ctx, storage, fresh, 1)
	graph := graphProjectionPublishPG(t, ctx, storage, fresh)
	if len(graph.HoldCounts) != 0 || len(graph.OrphanSessions) != 0 {
		t.Fatal("graph rebootstrap reused pre-reset inputs")
	}
	var oldRows int
	if err := storage.pool.QueryRow(ctx, `SELECT count(*) FROM graph_projection_parts WHERE repo_id=$1 AND dataset<>$2`, string(repo), fresh.Dataset).Scan(&oldRows); err != nil || oldRows != 0 {
		t.Fatalf("new publication retained obsolete graph datasets: rows=%d err=%v", oldRows, err)
	}
}

func TestPGGraphProjectionPublishRollbackPreservesReadyAndLease(t *testing.T) {
	storage, ctx, repo := graphProjectionPG(t)
	job := graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	priorGraph := graphProjectionPublishPG(t, ctx, storage, job)
	prior, priorRead := job, graphProjectionReadPG(t, ctx, storage, job, priorGraph)
	catalogExecPG(t, ctx, storage.pool, `UPDATE pending_contexts SET data=jsonb_set(data,'{dismissed}','true') WHERE repo_id=$1`, string(repo))
	job = graphProjectionCapturePG(t, ctx, storage, graphProjectionClaimPG(t, ctx, storage, repo), 0)
	graph, parts := graphProjectionPartsPG(t, ctx, storage, job)
	name := pgx.Identifier{domain.NewID("graph_publish_")}.Sanitize()
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := storage.pool.Exec(cleanup, fmt.Sprintf(`DROP TRIGGER IF EXISTS %s ON graph_projection_work; DROP FUNCTION IF EXISTS %s()`, name, name)); err != nil {
			t.Error(err)
		}
	})
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			IF EXISTS(SELECT 1 FROM graph_projection_ready WHERE repo_id=NEW.repo_id AND dataset=NEW.dataset
				AND source_seq=NEW.applied_seq AND source_seq>OLD.applied_seq) THEN
				RAISE EXCEPTION 'forced graph publish failure after ready' USING ERRCODE='P0001';
			END IF;
			RAISE EXCEPTION 'graph ready was not advanced' USING ERRCODE='P0002';
		END $$`, name))
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`CREATE TRIGGER %s BEFORE UPDATE ON graph_projection_work
		FOR EACH ROW WHEN (NEW.repo_id='%s' AND NEW.phase='idle') EXECUTE FUNCTION %s()`, name, string(repo), name))
	before := graphProjectionSnapshotPG(t, ctx, storage, repo)
	err := storage.PublishGraphProjection(ctx, job, parts)
	var databaseError *pgconn.PgError
	if !errors.As(err, &databaseError) || databaseError.Code != "P0001" || databaseError.Message != "forced graph publish failure after ready" {
		t.Fatalf("publication did not reach injected atomicity failure: %v", err)
	}
	if after := graphProjectionSnapshotPG(t, ctx, storage, repo); !reflect.DeepEqual(after, before) {
		t.Fatal("failed graph publication changed ready, parts, inputs, or lease")
	}
	graphProjectionPriorReadPG(t, ctx, storage, prior, priorRead.Parts)
	catalogExecPG(t, ctx, storage.pool, fmt.Sprintf(`DROP TRIGGER %s ON graph_projection_work; DROP FUNCTION %s()`, name, name))
	if err := storage.PublishGraphProjection(ctx, job, parts); err != nil {
		t.Fatalf("rollback consumed graph project lease: %v", err)
	}
	graphProjectionReadPG(t, ctx, storage, job, graph)
}
