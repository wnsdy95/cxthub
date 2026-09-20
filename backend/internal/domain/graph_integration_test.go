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
				if in.Head != string(head) || !reflect.DeepEqual(in.ExtraParents, []string{"merge-b"}) {
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
