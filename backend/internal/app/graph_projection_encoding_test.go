package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func graphProjectionEncodingView(repo domain.ContentHash, branchCount int) domain.RepositoryView {
	view := domain.RepositoryView{
		DefaultBranch: "main",
		Revision:      domain.RepositoryRevision{Graph: 1<<53 + 1, Pending: 1<<53 + 3, Evidence: 1<<53 + 5},
		Snapshots:     make([]domain.Snapshot, 1024),
	}
	for index := range view.Snapshots {
		identity := hh(fmt.Sprintf("graph-encoding-snapshot-%d", index))
		snapshot := domain.Snapshot{ID: identity, DocHash: identity, RepoID: repo, Branch: "main"}
		if index > 0 {
			snapshot.Parents = []domain.ContentHash{view.Snapshots[index-1].ID}
		}
		view.Snapshots[index] = snapshot
	}
	tip := view.Snapshots[len(view.Snapshots)-1].ID
	for index := range branchCount {
		branch := "main"
		if index > 0 {
			branch = fmt.Sprintf("feature/%03d", index)
		}
		view.Refs = append(view.Refs, domain.Ref{
			Kind: domain.RefBranch, Name: branch, RepoID: repo, Target: tip,
			BranchID: domain.LegacyContextBranchID(string(repo), branch),
		})
	}
	view.History = []domain.HistoryEvent{{
		ID: fmt.Sprintf("%032x", 1), RepoID: string(repo), Kind: "position", Branch: "main",
		BranchID: view.Refs[0].BranchID, Target: tip,
		CreatedAt: time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC),
	}}
	return view
}

func TestGraphProjectionEncodingLargeGraphCacheWarmRead(t *testing.T) {
	svc, storage := newFsckSvc(t)
	svc.meta = &graphCacheReadState{FSStore: storage, readOnly: true}
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo := hh(t.Name())
	view := graphProjectionEncodingView(repo, 128)
	position := view.History[0].ID
	first, err := svc.cachedGraphState(ctx, repo, view, position)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= graphProjectionCacheBytes || len(raw) >= graphProjectionExpandedBytes {
		t.Fatalf("fixture must exceed the old raw budget: bytes=%d", len(raw))
	}
	if first.Revision != view.Revision || first.PositionEvent != position || first.PrimaryBranch != "main" || len(first.SnapshotIDs) != len(view.Snapshots) {
		t.Fatal("projection lost its revision, position, branch, or snapshots")
	}
	if len(first.Positions) != 1 || first.Positions[0].BranchID != view.Refs[0].BranchID || first.Positions[0].Snapshot != view.History[0].Target {
		t.Fatal("projection lost selected position identity")
	}
	encoded, err := encodeGraphProjection(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeGraphProjection(ctx, encoded)
	if err != nil || !reflect.DeepEqual(decoded, first) {
		t.Fatalf("large graph round trip changed the projection: %v", err)
	}
	if svc.graphCache.Stats().Entries != 1 {
		t.Fatalf("large graph was not retained: entries=%d", svc.graphCache.Stats().Entries)
	}
	keys := svc.graphCache.Keys()
	cachedEncoded, found := svc.graphCache.Get(keys[0])
	stats := svc.graphCache.Stats()
	if !found || len(cachedEncoded) == 0 || stats.Bytes != len(cachedEncoded) || stats.Bytes > graphProjectionCacheBytes {
		t.Fatalf("cache must account for compressed bytes: retained=%d entry=%d", stats.Bytes, len(cachedEncoded))
	}
	if keys[0].Repo != repo || keys[0].Revision != view.Revision || keys[0].Position != position || stats.Building != 0 {
		t.Fatal("retained entry lost identity or leaked its build reservation")
	}
	t.Logf("raw graph=%d bytes, retained gzip=%d bytes", len(raw), len(cachedEncoded))
	_, releaseBulk, err := AcquireResources(ctx, ResourceBulk)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseBulk()
	interactive, releaseRead, err := AcquireResources(ctx, ResourceInteractive)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRead()
	warm, err := svc.cachedGraphState(interactive, repo, view, position)
	if err != nil || !reflect.DeepEqual(warm, first) {
		t.Fatalf("large warm graph changed or required occupied bulk capacity: %v", err)
	}
	keys = svc.graphCache.Keys()
	warmEncoded, found := svc.graphCache.Get(keys[0])
	if svc.graphCache.Stats().Entries != 1 || !found || len(warmEncoded) == 0 || &warmEncoded[0] != &cachedEncoded[0] {
		t.Fatal("warm read replaced its retained encoding")
	}
	view.Revision.Pending++
	if _, err := svc.cachedGraphState(interactive, repo, view, position); !errors.Is(err, domain.ErrResourceOverloaded) || svc.graphCache.Stats().Building != 0 {
		t.Fatalf("new revision bypassed bulk admission or leaked its reservation: %v", err)
	}
	releaseRead()
	releaseBulk()
	assertResourcesReleased(t, gate)
}

func TestGraphProjectionEncodingOversizedGraphSkipsCache(t *testing.T) {
	svc, storage := newFsckSvc(t)
	svc.meta = &graphCacheReadState{FSStore: storage, readOnly: true}
	ctx, gate := admissionTestContext(10 * time.Millisecond)
	repo := hh(t.Name())
	view := graphProjectionEncodingView(repo, 450)
	position := view.History[0].ID
	graph, err := svc.cachedGraphState(ctx, repo, view, position)
	if err != nil {
		t.Fatalf("cache expansion budget rejected a valid graph query: %v", err)
	}
	if graph.Revision != view.Revision || graph.PositionEvent != position || len(graph.SnapshotIDs) != len(view.Snapshots) || len(graph.BranchSnapshots) != len(view.Refs) {
		t.Fatal("uncached graph query lost projection data")
	}
	minimumJSONBytes := 0
	for _, snapshots := range graph.BranchSnapshots {
		if len(snapshots) != len(view.Snapshots) {
			t.Fatal("fixture lost shared branch ancestry")
		}
		minimumJSONBytes += len(snapshots) * (len(view.Snapshots[0].ID) + 3)
	}
	if minimumJSONBytes <= graphProjectionExpandedBytes {
		t.Fatalf("fixture did not exercise the expansion limit: bytes>=%d", minimumJSONBytes)
	}
	encoded, err := encodeGraphProjection(ctx, graph)
	if !errors.Is(err, errGraphProjectionCacheLimit) || len(encoded) != 0 {
		t.Fatalf("encoder failed to enforce expansion budget: bytes=%d err=%v", len(encoded), err)
	}
	if stats := svc.graphCache.Stats(); stats.Entries != 0 || stats.Bytes != 0 || stats.Building != 0 {
		t.Fatal("oversized graph retained bytes or leaked its build reservation")
	}
	view.Refs = view.Refs[:1]
	if _, err := svc.cachedGraphState(ctx, repo, view, position); err != nil || svc.graphCache.Stats().Entries != 1 || svc.graphCache.Stats().Building != 0 {
		t.Fatalf("oversized query prevented a later cacheable query: %v", err)
	}
	assertResourcesReleased(t, gate)
}
