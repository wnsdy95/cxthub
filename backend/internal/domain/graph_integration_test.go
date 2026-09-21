package domain

import (
	"reflect"
	"testing"
)

func TestGraphIntegrationPreservesMergeOrderAndAvoidsHeadCycle(t *testing.T) {
	for _, head := range []ContentHash{"later-main", "source-a", "base"} {
		t.Run(string(head), func(t *testing.T) {
			snaps := []Snapshot{{ID: "base"}, {ID: "source-a", Parents: []ContentHash{"base"}}, {ID: "source-b", Parents: []ContentHash{"base"}}, {ID: "later-main", Parents: []ContentHash{"base"}}}
			g := GraphState{PrimaryBranch: "main", BranchContexts: map[string]BranchContext{"main": {BranchID: "main-id", SnapshotID: head, SnapshotIDs: []ContentHash{head, "source-a", "source-b", "base"}, Merges: []BranchContextMerge{{EventID: "a", Source: "source-a", State: "included", Order: 1}, {EventID: "b", Source: "source-b", State: "included", Order: 0}}}}, Operations: GraphOperations{Merges: []GraphMerge{{ID: "merge-b", EventID: "b", Scope: "main-id", Source: "source-b", Withdrawn: true}, {ID: "merge-a", EventID: "a", Scope: "main-id", Source: "source-a", HistoricalOnly: true}}}, ArchivedOnlyIDs: []ContentHash{"source-a", "unrelated"}, Previous: []GraphProgressGroup{{CollapsibleIDs: []ContentHash{"source-a", "unrelated"}}}}
			ApplyGraphIntegrations(&g, snaps)
			if len(g.Integrations) != 1 {
				t.Fatal("missing integration")
			}
			in := g.Integrations[0]
			if !reflect.DeepEqual(in.Parents["merge-b"], []string{"merge-a", "source-b"}) {
				t.Fatal("receipt arrival order won over verified Git order")
			}
			if head == "later-main" {
				if in.Head != string(head) || !reflect.DeepEqual(in.Parents[string(head)], []string{"merge-b"}) || len(in.ExtraParents) != 0 {
					t.Fatal("continuation disconnected")
				}
			} else if in.Head != "merge-b" || len(in.ExtraParents) != 0 {
				t.Fatal("integration would create a head cycle")
			}
			if !reflect.DeepEqual(g.ArchivedOnlyIDs, []ContentHash{"unrelated"}) || !reflect.DeepEqual(g.Previous[0].CollapsibleIDs, []ContentHash{"unrelated"}) {
				t.Fatal("included context remains hidden")
			}
			if !g.Operations.Merges[0].Withdrawn || !g.Operations.Merges[1].HistoricalOnly {
				t.Fatal("historical placement facts were rewritten")
			}
		})
	}
}

// A destination checkpoint between PRs must continue that destination, rather
// than becoming another incoming arm named after the source of the later PR.
func TestGraphIntegrationKeepsDestinationCheckpointsOnSpine(t *testing.T) {
	for _, branch := range []string{"main", "release"} {
		t.Run(branch, func(t *testing.T) {
			snaps := []Snapshot{{ID: "base"}, {ID: "a", Parents: []ContentHash{"base"}},
				{ID: "checkpoint-1", Parents: []ContentHash{"a"}}, {ID: "checkpoint-2", Parents: []ContentHash{"checkpoint-1"}},
				{ID: "b", Parents: []ContentHash{"checkpoint-2"}}, {ID: "current", Parents: []ContentHash{"b"}}}
			original := append([]Snapshot(nil), snaps...)
			g := GraphState{BranchContexts: map[string]BranchContext{branch: {BranchID: "destination", SnapshotID: "current", Merges: []BranchContextMerge{
				{EventID: "a", Source: "a", Before: "base", State: "included"},
				{EventID: "b", Source: "b", Before: "checkpoint-2", State: "included"},
			}}}, Operations: GraphOperations{Merges: []GraphMerge{
				{ID: "merge-a", EventID: "a", Scope: "destination", Source: "a", After: "a"},
				{ID: "merge-b", EventID: "b", Scope: "destination", Source: "b", After: "b"},
			}}}
			ApplyGraphIntegrations(&g, snaps)
			in := g.Integrations[0]
			if !reflect.DeepEqual(in.Parents["merge-b"], []string{"checkpoint-2", "b"}) {
				t.Fatalf("checkpoint became a merge arm: %v", in.Parents["merge-b"])
			}
			if !reflect.DeepEqual(in.Parents["checkpoint-1"], []string{"merge-a"}) {
				t.Fatalf("checkpoint skipped prior integration: %v", in.Parents["checkpoint-1"])
			}
			if !reflect.DeepEqual(in.Parents["current"], []string{"merge-b"}) || len(in.ExtraParents) != 0 {
				t.Fatalf("current continuation left the spine: %+v", in)
			}
			if !reflect.DeepEqual(snaps, original) {
				t.Fatal("stored snapshots changed")
			}
		})
	}
}

func TestGraphIntegrationRejectsUnprovenContinuationRoutes(t *testing.T) {
	for _, scenario := range []string{"missing", "legacy-graft", "another-source", "cycle"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint := Snapshot{ID: "checkpoint", Parents: []ContentHash{"a"}}
			aBefore := ContentHash("base")
			switch scenario {
			case "missing":
				checkpoint.Parents = []ContentHash{"missing"}
			case "legacy-graft":
				checkpoint.Grafted = true
			case "another-source":
				checkpoint.Parents = []ContentHash{"other"}
			case "cycle":
				aBefore = "checkpoint"
			}
			snaps := []Snapshot{{ID: "base"}, {ID: "a", Parents: []ContentHash{"base"}}, checkpoint, {ID: "other", Parents: []ContentHash{"a"}}, {ID: "b", Parents: []ContentHash{"checkpoint"}}}
			g := GraphState{BranchContexts: map[string]BranchContext{"main": {BranchID: "main", SnapshotID: "b", Merges: []BranchContextMerge{{EventID: "a", Source: "a", Before: aBefore, State: "included"}, {EventID: "b", Source: "b", Before: "checkpoint", State: "included"}}}}, Operations: GraphOperations{Merges: []GraphMerge{{ID: "ma", EventID: "a", Source: "a", After: "a", Scope: "main"}, {ID: "mb", EventID: "b", Source: "b", After: "b", Scope: "main"}, {ID: "mo", EventID: "o", Source: "other", Scope: "other"}}}}
			ApplyGraphIntegrations(&g, snaps)
			if _, exists := g.Integrations[0].Parents["checkpoint"]; exists {
				t.Fatalf("invented route for %s", scenario)
			}
			if !reflect.DeepEqual(g.Integrations[0].Parents["mb"], []string{"ma", "checkpoint", "b"}) {
				t.Fatal("unproven continuation replaced conservative plan")
			}
		})
	}
}

func TestGraphIntegrationDoesNotChooseBetweenSharedCaptureOwners(t *testing.T) {
	snaps := []Snapshot{{ID: "base"}, {ID: "a", Parents: []ContentHash{"base"}}, {ID: "checkpoint", Parents: []ContentHash{"a"}}, {ID: "b", Parents: []ContentHash{"checkpoint"}}}
	g := GraphState{BranchContexts: map[string]BranchContext{}}
	for _, branch := range []string{"main", "release"} {
		g.BranchContexts[branch] = BranchContext{BranchID: branch, SnapshotID: "b", Merges: []BranchContextMerge{{EventID: branch + "a", Source: "a", Before: "base", State: "included"}, {EventID: branch + "b", Source: "b", Before: "checkpoint", State: "included"}}}
		for _, source := range []ContentHash{"a", "b"} {
			g.Operations.Merges = append(g.Operations.Merges, GraphMerge{ID: branch + string(source), EventID: branch + string(source), Source: source, After: source, Scope: branch})
		}
	}
	ApplyGraphIntegrations(&g, snaps)
	for _, plan := range g.Integrations {
		if _, ok := plan.Parents["checkpoint"]; ok {
			t.Fatalf("branch-name order chose an owner: %+v", plan)
		}
	}
}

func TestGraphIntegrationKeepsLocalContinuationAfterContributorMerge(t *testing.T) {
	// The local conversation continues shared, not the incoming contributor's
	// source A. It must follow merge A without redirecting shared through A.
	snaps := []Snapshot{{ID: "base"}, {ID: "shared", Parents: []ContentHash{"base"}},
		{ID: "a", Parents: []ContentHash{"shared"}}, {ID: "checkpoint", Parents: []ContentHash{"shared"}},
		{ID: "b", Parents: []ContentHash{"checkpoint"}}}
	g := GraphState{BranchContexts: map[string]BranchContext{"main": {BranchID: "main", SnapshotID: "b", Merges: []BranchContextMerge{
		{EventID: "a", Source: "a", Before: "a", State: "included"},
		{EventID: "b", Source: "b", Before: "checkpoint", State: "included"},
	}}}, Operations: GraphOperations{Merges: []GraphMerge{
		{ID: "ma", EventID: "a", Scope: "main", Source: "a", After: "a"},
		{ID: "mb", EventID: "b", Scope: "main", Source: "b", After: "b"},
	}}}
	ApplyGraphIntegrations(&g, snaps)
	p := g.Integrations[0].Parents
	if !reflect.DeepEqual(p["mb"], []string{"checkpoint", "b"}) || !reflect.DeepEqual(p["checkpoint"], []string{"ma"}) {
		t.Fatalf("destination continuation detached: %v", p)
	}
	if _, overridden := p["shared"]; overridden {
		t.Fatal("shared ancestor was routed back through its contributor")
	}
}
