package repositoryquery

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/wnsdy95/cxthub/backend/internal/app/graphdiagnostics"
	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func diagnosticStage(snapshot graphdiagnostics.Snapshot, name string) graphdiagnostics.StageStats {
	for _, stage := range snapshot.Stages {
		if stage.Name == name {
			return stage
		}
	}
	return graphdiagnostics.StageStats{}
}

func diagnosticCache(snapshot graphdiagnostics.Snapshot, name string) graphdiagnostics.CacheStats {
	for _, cache := range snapshot.Caches {
		if cache.Name == name {
			return cache
		}
	}
	return graphdiagnostics.CacheStats{}
}

func TestGraphDiagnosticsLegacyColdWarmAndFailure(test *testing.T) {
	fixture := newLegacyReadFixture(test, PinnedSnapshots)
	ctx, cold := graphdiagnostics.NewContext(fixture.ctx)
	first, err := fixture.queries.GetRepositoryView(ctx, fixture.probe.repo)
	if err != nil {
		test.Fatal(err)
	}
	snapshot := cold.Snapshot()
	if snapshot.Route != "legacy" || snapshot.LegacyReads != 1 || diagnosticCache(snapshot, "graph").Miss != 1 || diagnosticCache(snapshot, "branch").Miss != 1 {
		test.Fatal("cold route/cache missing", snapshot)
	}
	for _, name := range []string{"legacy_read", "load_view", "metadata", "refs", "snapshots", "reflog", "history", "pending", "unsync", "archives", "revision", "view_projection", "source_generation", "fingerprint", "build_admission", "projection", "graph_base", "pr_evidence", "git_evidence", "branch_projection", "cache_encode"} {
		if diagnosticStage(snapshot, name).Count == 0 {
			test.Fatal("missing stage", name, snapshot)
		}
	}
	ctx, warm := graphdiagnostics.NewContext(fixture.ctx)
	second, err := fixture.queries.GetRepositoryView(ctx, fixture.probe.repo)
	if err != nil || !reflect.DeepEqual(first, second) {
		test.Fatal("instrumentation changed response", err)
	}
	snapshot = warm.Snapshot()
	if diagnosticCache(snapshot, "graph").Hit != 1 || diagnosticStage(snapshot, "cache_decode").Count != 1 || diagnosticStage(snapshot, "projection").Count != 0 || diagnosticStage(snapshot, "build_admission").Count != 0 {
		test.Fatal("warm request inherited cold work", snapshot)
	}
	fixture.probe.errors["history"] = domain.ErrForbidden
	ctx, failure := graphdiagnostics.NewContext(fixture.ctx)
	_, err = fixture.queries.GetRepositoryView(ctx, fixture.probe.repo)
	if !errors.Is(err, domain.ErrForbidden) || diagnosticStage(failure.Snapshot(), "history").Count != 1 || diagnosticStage(failure.Snapshot(), "pending").Count != 0 {
		test.Fatal("failed read changed semantics or lost stage", err, failure.Snapshot())
	}
}

func TestGraphDiagnosticsCacheIneligibleAndBranchHit(test *testing.T) {
	fixture := newLegacyProjectFixture(test)
	fixture.scope.pinned = false
	ctx, recorder := graphdiagnostics.NewContext(context.Background())
	if _, err := fixture.projection.Cached(ctx, fixture.repo, fixture.view, ""); err != nil {
		test.Fatal(err)
	}
	if snapshot := recorder.Snapshot(); diagnosticCache(snapshot, "graph").Ineligible != 1 || diagnosticCache(snapshot, "branch").Ineligible != 1 {
		test.Fatal("ineligible caches reported as misses", snapshot)
	}
	fixture.scope.pinned = true
	if _, err := fixture.projection.Project(context.Background(), fixture.view, ""); err != nil {
		test.Fatal(err)
	}
	ctx, recorder = graphdiagnostics.NewContext(context.Background())
	if _, err := fixture.projection.Project(ctx, fixture.view, ""); err != nil {
		test.Fatal(err)
	}
	if snapshot := recorder.Snapshot(); diagnosticCache(snapshot, "branch").Hit != 1 || diagnosticStage(snapshot, "branch_projection").Count != 0 {
		test.Fatal("branch hit performed projection", snapshot)
	}
}

func TestGraphDiagnosticsPublishedSuccessPositionAndFailure(test *testing.T) {
	queries, probe, _ := newPublishedGraphFixture(test, PinnedSnapshots)
	ctx, recorder := graphdiagnostics.NewContext(context.Background())
	if _, err := queries.QueryGraphState(ctx, probe.repo, "position"); err != nil {
		test.Fatal(err)
	}
	snapshot := recorder.Snapshot()
	if snapshot.Route != "published" || snapshot.PublishedReads != 1 || len(snapshot.Caches) != 0 {
		test.Fatal("published route mislabeled", snapshot)
	}
	for _, name := range []string{"published_read", "published_load", "published_decode", "published_qualification", "build_admission", "position_projection"} {
		if diagnosticStage(snapshot, name).Count != 1 {
			test.Fatal("missing published stage", name, snapshot)
		}
	}
	probe.readErr = domain.ErrGraphProjectionNotReady
	ctx, recorder = graphdiagnostics.NewContext(context.Background())
	if _, err := queries.GetRepositoryView(ctx, probe.repo); !errors.Is(err, probe.readErr) {
		test.Fatal("read error changed", err)
	}
	if snapshot := recorder.Snapshot(); diagnosticStage(snapshot, "published_load").Count != 1 || diagnosticStage(snapshot, "published_decode").Count != 0 {
		test.Fatal("failed read stage inaccurate", snapshot)
	}
}
