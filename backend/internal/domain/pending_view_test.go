package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestProjectPendingViewPreservesMetadata(testingT *testing.T) {
	activityAt := time.Unix(123, 456).UTC()
	generation := &GraphReadGeneration{
		Generation: "generation", SourceEpoch: "source", AppliedEpoch: "applied",
		Rules: 7, Complete: true, Current: false,
	}
	graphGeneration := &GraphReadGeneration{Generation: "graph-generation"}
	graph := &GraphState{
		ReadGeneration: graphGeneration, Revision: RepositoryRevision{Graph: 3},
		PrimaryBranch: "main", SnapshotIDs: []ContentHash{"target"},
	}
	base := RepositoryView{
		Revision: RepositoryRevision{Graph: 1<<63 + 1, Pending: 1<<63 + 2, Evidence: 1<<63 + 3},
		Pending: []Pending{{
			RepoID: "repo", SessionID: "session", Branch: "topic", Provider: ProviderCodex,
			Target: "target", UpdatedAt: activityAt, ActivityAt: &activityAt, Dismissed: true,
		}},
		ArchivedSessions: []SessionArchiveView{{
			SessionArchive: SessionArchive{
				RepoID: "repo", Key: "archive", SnapshotID: "archived", Provider: ProviderCodex,
				SessionID: "archived-session", ArchivedAt: activityAt, ArchivedBy: "maintainer",
			},
			LatestSnapshotID: "latest", SnapshotIDs: []ContentHash{"archived", "latest"},
			Message: "archived message", Branch: "topic", UpdatedAt: activityAt,
			Origin: SessionOrigin{ParentSnapshotID: "parent", MainBranch: "main"},
		}},
		Graph: graph, ReadGeneration: generation, Snapshots: []Snapshot{{ID: "target"}},
	}

	view, err := ProjectPendingView(base)
	if err != nil {
		testingT.Fatal(err)
	}
	want := PendingView{
		Revision: base.Revision, Pending: base.Pending, ArchivedSessions: base.ArchivedSessions,
		Graph: graph, ReadGeneration: generation, Snapshots: []Snapshot{{ID: "target"}},
	}
	if !reflect.DeepEqual(view, want) {
		testingT.Fatalf("view = %+v, want %+v", view, want)
	}
	if view.Graph != graph || view.ReadGeneration != generation || view.Graph.ReadGeneration != graphGeneration {
		testingT.Fatal("graph or read generation pointer changed")
	}
	if &view.Pending[0] != &base.Pending[0] || view.Pending[0].ActivityAt != &activityAt {
		testingT.Fatal("pending slice or activity pointer changed")
	}
	if &view.ArchivedSessions[0] != &base.ArchivedSessions[0] {
		testingT.Fatal("archived sessions slice changed")
	}
}

func TestProjectPendingViewDeduplicatesTargets(testingT *testing.T) {
	base := RepositoryView{
		Pending: []Pending{
			{SessionID: "second-session", Target: "second", Dismissed: true},
			{SessionID: "first-session", Target: "first"},
			{SessionID: "duplicate-second", Target: "second"},
			{SessionID: "duplicate-first", Target: "first"},
		},
		Snapshots: []Snapshot{
			{ID: "unrelated"}, {ID: "first", Message: "first capture"},
			{ID: "second", Message: "second capture"},
		},
	}

	view, err := ProjectPendingView(base)
	if err != nil {
		testingT.Fatal(err)
	}
	want := []Snapshot{base.Snapshots[2], base.Snapshots[1]}
	if !reflect.DeepEqual(view.Snapshots, want) {
		testingT.Fatalf("snapshots = %+v, want %+v", view.Snapshots, want)
	}
	if !reflect.DeepEqual(view.Pending, base.Pending) {
		testingT.Fatalf("pending = %+v, want %+v", view.Pending, base.Pending)
	}
}

func TestProjectPendingViewEmptySlices(testingT *testing.T) {
	for _, testCase := range []struct {
		name string
		base RepositoryView
	}{
		{name: "nil"},
		{name: "empty", base: RepositoryView{
			Pending: []Pending{}, ArchivedSessions: []SessionArchiveView{}, Snapshots: []Snapshot{},
		}},
		{name: "unrelated snapshot", base: RepositoryView{Snapshots: []Snapshot{{ID: "unrelated"}}}},
	} {
		testingT.Run(testCase.name, func(testingT *testing.T) {
			view, err := ProjectPendingView(testCase.base)
			if err != nil {
				testingT.Fatal(err)
			}
			if view.Snapshots == nil || len(view.Snapshots) != 0 {
				testingT.Fatalf("snapshots = %#v, want nonnil empty slice", view.Snapshots)
			}
			if !reflect.DeepEqual(view.Pending, testCase.base.Pending) {
				testingT.Fatalf("pending = %#v, want %#v", view.Pending, testCase.base.Pending)
			}
			if !reflect.DeepEqual(view.ArchivedSessions, testCase.base.ArchivedSessions) {
				testingT.Fatalf("archives = %#v, want %#v", view.ArchivedSessions, testCase.base.ArchivedSessions)
			}
			if view.Graph != nil || view.ReadGeneration != nil {
				testingT.Fatal("nil graph or read generation changed")
			}
		})
	}
}

func TestProjectPendingViewMissingTarget(testingT *testing.T) {
	for _, testCase := range []struct {
		name string
		base RepositoryView
	}{
		{name: "nil snapshots", base: RepositoryView{Pending: []Pending{{Target: "missing"}}}},
		{name: "empty snapshots", base: RepositoryView{
			Pending: []Pending{{Target: "missing"}}, Snapshots: []Snapshot{},
		}},
		{name: "missing after existing target", base: RepositoryView{
			Revision:  RepositoryRevision{Graph: 3, Pending: 5, Evidence: 7},
			Pending:   []Pending{{Target: "existing"}, {Target: "missing"}},
			Snapshots: []Snapshot{{ID: "existing", Branches: []string{"topic"}}},
		}},
	} {
		testingT.Run(testCase.name, func(testingT *testing.T) {
			view, err := ProjectPendingView(testCase.base)
			if err != ErrNotFound {
				testingT.Fatalf("error = %v, want ErrNotFound", err)
			}
			if !reflect.DeepEqual(view, PendingView{}) {
				testingT.Fatalf("view = %+v, want zero view", view)
			}
		})
	}
}

func TestProjectPendingViewDoesNotMutateSnapshotBranches(testingT *testing.T) {
	original := []Snapshot{
		{
			ID: "first", RepoID: "repo", Branch: "birth", Branches: []string{"main", "topic"},
			Parents: []ContentHash{"parent"}, DocHash: "document", MemoryHash: "memory",
			Provider: ProviderCodex, Fidelity: FidelityFull, Message: "first capture",
			CreatedAt: time.Unix(123, 456).UTC(), Grafted: true,
			GraftParents: []ContentHash{"graft"}, GraftSeq: 9, SessionID: "session",
			Models: []string{"model"}, CompactionCount: 2,
		},
		{ID: "second", Branches: []string{}, Message: "second capture"},
		{ID: "unrelated", Branches: []string{"other"}},
	}
	base := RepositoryView{
		Pending:   []Pending{{Target: "first"}, {Target: "second"}, {Target: "first"}},
		Snapshots: append([]Snapshot{}, original...),
	}
	wantFirst, wantSecond := original[0], original[1]
	wantFirst.Branches, wantSecond.Branches = nil, nil

	view, err := ProjectPendingView(base)
	if err != nil {
		testingT.Fatal(err)
	}
	if !reflect.DeepEqual(view.Snapshots, []Snapshot{wantFirst, wantSecond}) {
		testingT.Fatalf("snapshots = %+v, want captures with nil branches", view.Snapshots)
	}
	if !reflect.DeepEqual(base.Snapshots, original) {
		testingT.Fatalf("original snapshots changed: %+v", base.Snapshots)
	}
	if &view.Snapshots[0] == &base.Snapshots[0] {
		testingT.Fatal("projected snapshot aliases the source snapshot value")
	}
}
