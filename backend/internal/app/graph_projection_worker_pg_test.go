//go:build postgres

package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPGGraphProjectionWorkerResumesAndMatchesLiveGitPRGraph(t *testing.T) {
	fixture, connection := graphSourceQueryFixturePG(t)
	ctx, cancel := context.WithTimeout(systemTestContext(), 90*time.Second)
	defer cancel()
	provider, storage, repo := fixture.basis.worker, fixture.storage.PostgresStore, fixture.basis.repo
	pr := domain.PullRequestMerge{Number: 7, BaseBranch: "main", HeadBranch: "feature", HeadSHA: fixture.admission.OriginalHead, MergeSHA: fixture.proof.Contribution.IntegrationSHA}
	if _, err := provider.core.SubmitPRPromotion(ctx, repo.ID, pr); err != nil {
		t.Fatal(err)
	}
	remote := &githubHistoryRemote{GitHubApp: provider.remote, pages: map[string]domain.GitHubPRHistoryPage{"": {
		RepositoryID: 91, HeadRepositoryID: 91, PullRequestID: "PR_7", PullRequestDatabaseID: 701, Number: 7,
		BaseBranch: pr.BaseBranch, HeadBranch: pr.HeadBranch, HeadSHA: pr.HeadSHA, MergeSHA: pr.MergeSHA, Merged: true, MergedAt: time.Unix(300, 0).UTC(),
		Rewrites: []domain.GitHubHeadRewrite{{ID: "original-head-restored", Before: strings.Repeat("6", 40), After: pr.HeadSHA, CreatedAt: time.Unix(200, 0).UTC()}},
	}}}
	if err := provider.reconcilePRHistory(ctx, remote, fixture.storage, historyWork(t, provider, fixture.storage, repo)); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < domain.DefaultRepositoryViewSourceLimit+1; index++ {
		collaborationSnapshot(t, storage, repo.ID, fmt.Sprintf("graph capture page %03d", index))
	}
	worker := NewService(storage, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := worker.ConfigureGraphProjections(ctx, []domain.ContentHash{repo.ID}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(systemTestContext(), 5*time.Second)
		defer cancel()
		if err := storage.ConfigureGraphProjection(cleanup, repo.ID, false); err != nil {
			t.Error(err)
		}
	})
	if err := worker.ProcessGraphProjections(ctx, 1); err != nil {
		t.Fatal(err)
	}
	partial, err := storage.ReadGraphProjection(ctx, repo.ID)
	if err != nil || partial.Ready || len(partial.Parts) != 0 {
		t.Fatal("partial graph capture became readable", partial, err)
	}
	var phase, cursor, dataset string
	if err := connection.QueryRow(ctx, `SELECT phase,cursor,dataset::text FROM graph_projection_work WHERE repo_id=$1`, repo.ID).Scan(&phase, &cursor, &dataset); err != nil || phase != "capture" || cursor == "" {
		t.Fatal("first lease did not persist a resumable graph cursor", phase, cursor, err)
	}
	peer, err := store.NewPostgresStore(ctx, fixture.basis.dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	restarted := NewService(peer, peer, auth.NewTeamTokenAuth(), gitengine.NewEngine(peer), peer)
	if err := restarted.ConfigureGraphProjections(ctx, []domain.ContentHash{repo.ID}); err != nil {
		t.Fatal(err)
	}
	assertParity := func(label string) domain.GraphProjectionRead {
		t.Helper()
		want, err := provider.core.GetRepositoryView(ctx, repo.ID)
		if err != nil {
			t.Fatal(label, err)
		}
		before, err := peer.ReadRepositoryGraphSourceGeneration(ctx, repo.ID)
		if err != nil {
			t.Fatal(label, err)
		}
		for attempt := 0; attempt < 20; attempt++ {
			if err := restarted.ProcessGraphProjections(ctx, 1); err != nil {
				t.Fatal(label, err)
			}
			read, err := peer.ReadGraphProjection(ctx, repo.ID)
			if err != nil || read.NeedsAttention {
				t.Fatal(label, "graph generation failed", read, err)
			}
			if !read.Ready {
				continue
			}
			got, err := domain.DecodeGraphViewProjection(ctx, repo.ID, read.Parts)
			if err != nil || !reflect.DeepEqual(got, want) {
				actual, _ := json.Marshal(got)
				expected, _ := json.Marshal(want)
				t.Fatalf("%s durable graph differs from live graph: %v\ngot %s\nwant %s", label, err, actual, expected)
			}
			after, err := peer.ReadRepositoryGraphSourceGeneration(ctx, repo.ID)
			if err != nil || after != before || read.Source != read.Applied || read.Applied != before || read.Applied.Scope != domain.RepositoryGraphSourceScope || read.Dataset != dataset {
				t.Fatal(label, "worker rewrote source or lost pinned generation", before, after, read, err)
			}
			if len(got.Graph.Integrations) != 1 || len(got.Graph.Operations.Merges) != 1 || len(got.Graph.BranchContexts["main"].Merges) != 1 {
				t.Fatal(label, "mixed PR/Git evidence lost or duplicated integration", got.Graph.Integrations)
			}
			qualifyGraphBundlePG(t, peer, connection, repo.ID)
			assertGraphBundleServingPG(t, peer, repo.ID, want, true)
			return read
		}
		t.Fatal(label, "graph worker did not converge")
		return domain.GraphProjectionRead{}
	}
	baseline := assertParity("restart")
	if err := provider.core.SetSessionArchived(ctx, repo.ID, fixture.admission.Capture, true); err != nil {
		t.Fatal(err)
	}
	lagged, err := peer.ReadGraphProjection(ctx, repo.ID)
	if err != nil || lagged.Ready || lagged.Applied != baseline.Applied || lagged.Source.Sequence <= lagged.Applied.Sequence {
		t.Fatal("lagged graph advertised current readiness", lagged, err)
	}
	prior, err := domain.DecodeGraphViewProjection(ctx, repo.ID, baseline.Parts)
	if err != nil {
		t.Fatal(err)
	}
	assertGraphBundleServingPG(t, peer, repo.ID, prior, false)
	assertParity("archive delta")
	assertGraphBundleRepairPG(t, peer, connection, repo.ID, fixture.basis.dsn)
	if worker.graphCache.Stats().Entries != 0 || restarted.graphCache.Stats().Entries != 0 || len(restarted.pendingProjectionRepos) != 0 {
		t.Fatal("shadow worker populated serving state")
	}
}
