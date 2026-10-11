package domain

import (
	"slices"
	"testing"
)

func TestGraphIntegrationGroupsKeepDestinationIdentity(t *testing.T) {
	graph := GraphState{BranchContexts: map[string]BranchContext{}}
	for _, branch := range []string{"main", "release"} {
		inclusion := BranchContext{BranchID: branch}
		for _, capture := range []ContentHash{"original", "supplement"} {
			event := branch + string(capture)
			graph.Operations.Merges = append(graph.Operations.Merges, GraphMerge{ID: event, EventID: event, Scope: branch, Source: capture})
			inclusion.Merges = append(inclusion.Merges, BranchContextMerge{EventID: event, Source: capture, PRNumber: 488, MergeSHA: "merge", State: "included"})
		}
		graph.BranchContexts[branch] = inclusion
	}
	lookup := groupIntegrationOperations(&graph)
	if len(graph.Operations.Merges) != 2 {
		t.Fatalf("merged different destination identities: %+v", graph.Operations.Merges)
	}
	if lookup["mainoriginal"].ID == lookup["releaseoriginal"].ID {
		t.Fatal("different scopes share a virtual operation")
	}
	for _, operation := range graph.Operations.Merges {
		if len(operation.SourceArms) != 2 {
			t.Fatalf("lost source arms: %+v", operation)
		}
	}
}

func TestGraphIntegrationGroupsGenericAndMixedCapturesAtSameCommit(t *testing.T) {
	for _, mixed := range []bool{false, true} {
		graph := GraphState{BranchContexts: map[string]BranchContext{}}
		inclusion := BranchContext{BranchID: "main", SnapshotID: "base"}
		for index, capture := range []ContentHash{"original", "supplement"} {
			event := string(capture)
			pr := 0
			if mixed && index == 1 {
				pr = 488
			}
			graph.Operations.Merges = append(graph.Operations.Merges, GraphMerge{ID: event, EventID: event, Scope: "main", Source: capture, PRNumber: pr})
			inclusion.Merges = append(inclusion.Merges, BranchContextMerge{EventID: event, Source: capture, DestinationBranchID: "main", PRNumber: pr, MergeSHA: "same-integration", State: "included"})
		}
		graph.BranchContexts["main"] = inclusion
		ApplyGraphIntegrations(&graph, []Snapshot{{ID: "base"}, {ID: "original"}, {ID: "supplement"}})
		if len(graph.Operations.Merges) != 1 || len(graph.Integrations) != 1 {
			t.Fatalf("invented sequential integrations: %+v", graph)
		}
		merge := graph.Operations.Merges[0]
		if !slices.Equal(merge.SourceArms, []ContentHash{"original", "supplement"}) || (mixed && merge.PRNumber != 488) {
			t.Fatalf("lost capture arms or known PR: %+v", merge)
		}
		parents := graph.Integrations[0].Parents
		if len(parents) != 1 || !slices.Equal(parents[merge.ID], []string{"original", "supplement"}) {
			t.Fatalf("same commit became an artificial chain: %+v", parents)
		}
	}
}
