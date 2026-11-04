//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
	"github.com/wnsdy95/cxthub/backend/internal/ports/outbound"
)

type graphSourceCapturePG struct {
	outbound.RepositoryGraphSourceStore
	afterFirst func()
	reads      int
}

func (source *graphSourceCapturePG) RepositoryGraphSourceChanges(ctx context.Context, repo domain.ContentHash, request domain.RepositoryViewSourceRequest) (domain.RepositoryViewSourcePage, error) {
	request.Limit = 1
	page, err := source.RepositoryGraphSourceStore.RepositoryGraphSourceChanges(ctx, repo, request)
	source.reads++
	if err == nil && source.reads == 1 && source.afterFirst != nil {
		source.afterFirst()
	}
	return page, err
}

func capturedGraphPG(t *testing.T, captured CapturedGraphSource, position string) domain.GraphState {
	t.Helper()
	ctx := systemTestContext()
	metadata, view, evidence, err := captured.Inputs(ctx)
	if err != nil {
		t.Fatal("captured view", err)
	}
	reader, err := store.NewGraphSourceReader(ctx, captured.Checkpoint(), metadata, evidence)
	if err != nil {
		t.Fatal("captured evidence", err)
	}
	graph, err := ProjectCapturedGraph(ctx, captured.Checkpoint(), view, reader, position)
	if err != nil {
		t.Fatal("captured projection", err)
	}
	return graph
}

func assertCapturedGraphParityPG(t *testing.T, core *Service, storage *store.PostgresStore, repo domain.ContentHash, previous *CapturedGraphSource) CapturedGraphSource {
	t.Helper()
	ctx := systemTestContext()
	captured, err := CaptureGraphSource(ctx, &graphSourceCapturePG{RepositoryGraphSourceStore: storage}, repo, previous)
	if err != nil {
		t.Fatal("capture", err)
	}
	direct, err := core.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal("direct projection", err)
	}
	positions := []string{""}
	if len(direct.Graph.Positions) != 0 {
		positions = append(positions, direct.Graph.Positions[0].EventID)
	}
	for _, position := range positions {
		want := direct.Graph
		if position != "" {
			graph, err := core.QueryGraphState(ctx, repo, position)
			if err != nil {
				t.Fatal(err)
			}
			want = &graph
		}
		got := capturedGraphPG(t, captured, position)
		if !reflect.DeepEqual(got, *want) {
			actual, _ := json.Marshal(got)
			expected, _ := json.Marshal(want)
			t.Fatalf("frozen graph/inclusion differs at %q:\ngot %s\nwant %s", position, actual, expected)
		}
	}
	fresh, err := CaptureGraphSource(ctx, storage, repo, nil)
	if err != nil || !reflect.DeepEqual(fresh, captured) {
		t.Fatalf("delta/page-size reconstruction differs: %v", err)
	}
	return captured
}

func graphSourceQueryFixturePG(t *testing.T) (*gitContributionQueryPGFixture, *pgxpool.Pool) {
	t.Helper()
	fixture := newGitContributionQueryPGFixture(t)
	ctx := systemTestContext()
	connection, err := pgxpool.New(ctx, fixture.basis.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Close)
	for _, commit := range fixture.storage.trees {
		payload, err := json.Marshal(commit)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(ctx, `INSERT INTO git_commit_trees(repo_id,origin,commit_oid,payload) VALUES($1,$2,$3,$4::jsonb)`, fixture.basis.repo.ID, fixture.basis.repo.GitRemoteURL, commit.Commit, string(payload)); err != nil {
			t.Fatal(err)
		}
	}
	fixture.storage.trees = nil
	return fixture, connection
}

func TestPGCapturedGraphGitPRDeltaAndSelectionParity(t *testing.T) {
	fixture, _ := graphSourceQueryFixturePG(t)
	ctx := systemTestContext()
	worker, repo, storage := fixture.basis.worker, fixture.basis.repo, fixture.storage.PostgresStore
	captured := assertCapturedGraphParityPG(t, worker.core, storage, repo.ID, nil)
	graph := capturedGraphPG(t, captured, "")
	fixture.assertMerge(t, pointerBranchContext(graph, "main"), "included", true)
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: fixture.admission.OriginalHead, MergeSHA: fixture.proof.Contribution.IntegrationSHA}
	if _, err := worker.core.SubmitPRPromotion(ctx, repo.ID, pr); err != nil {
		t.Fatal(err)
	}
	remote := &githubHistoryRemote{GitHubApp: worker.remote, pages: map[string]domain.GitHubPRHistoryPage{"": {
		RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7,
		BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(),
		Rewrites: []domain.GitHubHeadRewrite{{ID: "original-head-restored", Before: strings.Repeat("6", 40), After: pr.HeadSHA, CreatedAt: time.Unix(200, 0).UTC()}},
	}}}
	if err := worker.reconcilePRHistory(ctx, remote, fixture.storage, historyWork(t, worker, fixture.storage, repo)); err != nil {
		t.Fatal(err)
	}
	captured = assertCapturedGraphParityPG(t, worker.core, storage, repo.ID, &captured)
	graph = capturedGraphPG(t, captured, "")
	if len(graph.Operations.Merges) != 1 || len(graph.Integrations) != 1 || len(graph.BranchContexts["main"].Merges) != 1 {
		t.Fatalf("mixed ledgers duplicated integration: %+v", graph.Integrations)
	}
	fixture.selectMain(t, fixture.base, strings.Repeat("b", 40), strings.Repeat("9", 32))
	captured = assertCapturedGraphParityPG(t, worker.core, storage, repo.ID, &captured)
	graph = capturedGraphPG(t, captured, "")
	fixture.assertMerge(t, pointerBranchContext(graph, "main"), "not_selected", true)
	if err := worker.core.SetSessionArchived(ctx, repo.ID, fixture.admission.Capture, true); err != nil {
		t.Fatal(err)
	}
	captured = assertCapturedGraphParityPG(t, worker.core, storage, repo.ID, &captured)
	event := domain.HistoryEvent{ID: strings.Repeat("6", 32), RepoID: string(repo.ID), Kind: "rename", Branch: "release", PreviousBranch: "main", BranchID: fixture.main.BranchID,
		BindingParent: fixture.admission.DestinationAnchorID, Source: fixture.main.Target, Target: fixture.main.Target, CreatedAt: time.Now().UTC()}
	if err := worker.core.RecordHistory(ctx, event); err != nil {
		t.Fatal(err)
	}
	ref := fixture.main
	ref.Name = "release"
	if _, err := worker.core.UpdateRef(ctx, inbound.UpdateRefInput{RepoID: repo.ID, Ref: ref}); err != nil {
		t.Fatal(err)
	}
	assertCapturedGraphParityPG(t, worker.core, storage, repo.ID, &captured)
}

func pointerBranchContext(graph domain.GraphState, branch string) *domain.BranchContext {
	value := graph.BranchContexts[branch]
	return &value
}

func TestPGCapturedGraphDoesNotReadLiveProofsAfterCaptureStarts(t *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(t)
	ctx := systemTestContext()
	core, storage, repo := fixture.basis.worker.core, fixture.storage.PostgresStore, fixture.basis.repo.ID
	want, err := core.GetRepositoryView(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	source := &graphSourceCapturePG{RepositoryGraphSourceStore: storage, afterFirst: func() {
		if _, err := connection.Exec(ctx, `DELETE FROM git_contribution_proofs WHERE repo_id=$1`, repo); err != nil {
			t.Fatal(err)
		}
	}}
	captured, err := CaptureGraphSource(ctx, source, repo, nil)
	if err != nil || source.reads < 2 {
		t.Fatal("capture did not cross mutation", err)
	}
	if got := capturedGraphPG(t, captured, ""); !reflect.DeepEqual(got, *want.Graph) {
		t.Fatal("live proof deletion changed captured generation")
	}
	updated := assertCapturedGraphParityPG(t, core, storage, repo, &captured)
	if updated.Checkpoint().Sequence <= captured.Checkpoint().Sequence || reflect.DeepEqual(capturedGraphPG(t, updated, ""), *want.Graph) {
		t.Fatal("fresh capture did not observe proof deletion")
	}
	if _, err := connection.Exec(ctx, `SELECT cxt_reset_catalog($1)`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := CaptureGraphSource(ctx, storage, repo, &updated); !errors.Is(err, domain.ErrCatalogResetRequired) {
		t.Fatalf("epoch reset silently reused captures: %v", err)
	}
	assertCapturedGraphParityPG(t, core, storage, repo, nil)
}

func TestPGCapturedGraphPruneDuringCaptureDiscardsPartialResult(t *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(t)
	ctx := systemTestContext()
	core, storage, repo := fixture.basis.worker.core, fixture.storage.PostgresStore, fixture.basis.repo.ID
	previous := assertCapturedGraphParityPG(t, core, storage, repo, nil)
	before := capturedGraphPG(t, previous, "")
	source := &graphSourceCapturePG{RepositoryGraphSourceStore: storage, afterFirst: func() {
		collaborationSnapshot(t, storage, repo, "publication after pinned first page")
		if _, err := connection.Exec(ctx, `SELECT cxt_prune_catalog($1,head_seq) FROM repository_catalog_state WHERE repo_id=$1`, repo); err != nil {
			t.Fatal(err)
		}
	}}
	partial, err := CaptureGraphSource(ctx, source, repo, nil)
	if !errors.Is(err, domain.ErrCatalogResetRequired) || partial.Checkpoint().Epoch != "" || partial.entries != nil || source.reads < 2 {
		t.Fatalf("prune exposed partial inputs: %+v %v", partial.Checkpoint(), err)
	}
	if got := capturedGraphPG(t, previous, ""); !reflect.DeepEqual(got, before) {
		t.Fatal("prune mutated previously completed capture")
	}
	if _, err := CaptureGraphSource(ctx, storage, repo, &previous); !errors.Is(err, domain.ErrCatalogResetRequired) {
		t.Fatalf("expired delta continued: %v", err)
	}
	assertCapturedGraphParityPG(t, core, storage, repo, nil)
}
