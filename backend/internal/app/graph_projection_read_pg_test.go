//go:build postgres

package app

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/auth"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/gitengine"
	"github.com/wnsdy95/cxthub/backend/internal/adapters/store"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
	"github.com/wnsdy95/cxthub/backend/internal/ports/inbound"
)

type graphBundleReadPG struct {
	*store.PostgresStore
	liveReads int
}

func (storage *graphBundleReadPG) ListSnapshots(context.Context, domain.ContentHash, string) ([]domain.Snapshot, error) {
	storage.liveReads++
	return nil, domain.ErrIntegrity
}

func (storage *graphBundleReadPG) ListPRContributions(context.Context, domain.ContentHash) ([]domain.PRContribution, error) {
	storage.liveReads++
	return nil, domain.ErrIntegrity
}

func assertGraphBundleServingPG(t *testing.T, storage *store.PostgresStore, repo domain.ContentHash, want domain.RepositoryView, current bool) {
	t.Helper()
	guard := &graphBundleReadPG{PostgresStore: storage}
	reader := NewService(guard, storage, auth.NewTeamTokenAuth(), gitengine.NewEngine(storage), storage)
	if err := reader.ConfigureGraphReads([]domain.ContentHash{repo}); err != nil {
		t.Fatal(err)
	}
	ctx, gate := admissionTestContext(time.Second)
	ctx = inbound.WithSystemActor(ctx)
	_, release, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	full, err := reader.GetRepositoryView(ctx, repo)
	if err != nil || full.ReadGeneration == nil || !full.ReadGeneration.Complete || full.ReadGeneration.Current != current ||
		!reflect.DeepEqual(full.ReadGeneration, full.Graph.ReadGeneration) {
		t.Fatal("published full-view generation", full.ReadGeneration, err)
	}
	generation := full.ReadGeneration
	full.ReadGeneration, full.Graph.ReadGeneration = nil, nil
	if !reflect.DeepEqual(full, want) {
		t.Fatal("serving read mixed graph and metadata generations")
	}
	graph, err := reader.QueryGraphState(ctx, repo, "")
	if err != nil || !reflect.DeepEqual(graph.ReadGeneration, generation) {
		t.Fatal("graph generation", err)
	}
	graph.ReadGeneration = nil
	if !reflect.DeepEqual(graph, *want.Graph) {
		t.Fatal("graph endpoint differs from published bundle")
	}
	live, err := reader.GetPendingView(ctx, repo)
	if err != nil || live.Graph == nil || !reflect.DeepEqual(live.ReadGeneration, generation) || !reflect.DeepEqual(live.Graph.ReadGeneration, generation) {
		t.Fatal("live generation", err)
	}
	live.ReadGeneration, live.Graph.ReadGeneration = nil, nil
	expected, err := pendingViewFromRepository(want)
	if err != nil || !reflect.DeepEqual(live, expected) {
		t.Fatal("live endpoint differs from published bundle", err)
	}
	notice, err := reader.RepositoryChangeState(ctx, repo)
	if err != nil || notice.GraphProjection == nil || notice.GraphProjection.Generation != generation.Generation ||
		notice.GraphProjection.Current != current || !notice.GraphProjection.Complete {
		t.Fatal("notice differs from bundle", notice, err)
	}
	release()
	if len(want.Graph.Positions) == 0 {
		t.Fatal("fixture must exercise historical selection")
	}
	for _, position := range want.Graph.Positions {
		actual, err := reader.QueryGraphState(ctx, repo, position.EventID)
		if err != nil || !reflect.DeepEqual(actual.ReadGeneration, generation) {
			t.Fatal("historical generation", err)
		}
		actual.ReadGeneration = nil
		historical, err := (&Service{}).projectGraphState(ctx, want, position.EventID)
		if err != nil || !reflect.DeepEqual(actual, historical) {
			t.Fatal("historical projection changed stored semantics", position.EventID, err)
		}
	}
	_, err = reader.QueryGraphState(ctx, repo, "not-yet-published-position")
	wantError := domain.ErrNotFound
	if !current {
		wantError = domain.ErrGraphProjectionNotReady
	}
	if !errors.Is(err, wantError) {
		t.Fatal("missing historical position misclassified", err)
	}
	if guard.liveReads != 0 || reader.graphCache.Stats().Entries != 0 || len(reader.graphProjectionRepos) != 0 {
		t.Fatal("published reader used live source or enabled worker", guard.liveReads)
	}
	assertResourcesReleased(t, gate)
}
