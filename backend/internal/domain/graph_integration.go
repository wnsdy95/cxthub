package domain

import "sort"

// GraphIntegration routes a branch's verified integration history separately
// from conversation ancestry. Parents contain virtual merge IDs and real
// sources; Head selects a display node, never a stored ref mutation.
type GraphIntegration struct {
	Branch  string              `json:"branch"`
	Scope   string              `json:"scope"`
	Target  ContentHash         `json:"target"`
	Head    string              `json:"head"`
	Parents map[string][]string `json:"parents"`
	// ExtraParents connects a current continuation to its integrated history.
	// It is omitted when that continuation is already an ancestor of a source.
	ExtraParents []string `json:"extra_parents"`
}

// ApplyGraphIntegrations consumes server-verified inclusion, not Git names or
// timestamps. Original placement/withdrawal facts remain available unchanged.
func ApplyGraphIntegrations(graph *GraphState, snapshots []Snapshot) {
	graph.Integrations = []GraphIntegration{}
	byID := map[ContentHash]Snapshot{}
	for _, s := range snapshots {
		byID[s.ID] = s
	}
	merges := map[string]GraphMerge{}
	for _, m := range graph.Operations.Merges {
		merges[m.EventID] = m
	}
	branches := make([]string, 0, len(graph.BranchContexts))
	includedIDs := map[ContentHash]bool{}
	for name := range graph.BranchContexts {
		branches = append(branches, name)
	}
	sort.Strings(branches)
	for _, branch := range branches {
		c := graph.BranchContexts[branch]
		for _, id := range c.SnapshotIDs {
			includedIDs[id] = true
		}
		out := GraphIntegration{Branch: branch, Scope: c.BranchID, Target: c.SnapshotID, Head: string(c.SnapshotID), Parents: map[string][]string{}, ExtraParents: []string{}}
		previous := ""
		headIncluded := false
		for _, included := range c.Merges {
			if included.State != "included" {
				continue
			}
			m, ok := merges[included.EventID]
			if !ok || m.Scope != c.BranchID || m.Source != included.Source {
				continue
			}
			parents := []string{}
			if previous != "" {
				parents = append(parents, previous)
			}
			if included.Before != "" && included.Before != m.Source {
				parents = append(parents, string(included.Before))
				closure, _ := contextEvidenceClosure(byID, included.Before, false)
				headIncluded = headIncluded || closure[c.SnapshotID]
			}
			parents = append(parents, string(m.Source))
			out.Parents[m.ID] = parents
			previous = m.ID
			closure, _ := contextEvidenceClosure(byID, m.Source, false)
			headIncluded = headIncluded || closure[c.SnapshotID]
		}
		if previous == "" {
			continue
		}
		if headIncluded {
			out.Head = previous
		} else {
			out.ExtraParents = append(out.ExtraParents, previous)
		}
		graph.Integrations = append(graph.Integrations, out)
	}
	// Folding follows the same inclusion facts as the branch list. A previously
	// displaced PR cannot remain hidden in the archived/previous-only group.
	archived := []ContentHash{}
	for _, id := range graph.ArchivedOnlyIDs {
		if !includedIDs[id] {
			archived = append(archived, id)
		}
	}
	graph.ArchivedOnlyIDs = archived
	for i := range graph.Previous {
		kept := []ContentHash{}
		for _, id := range graph.Previous[i].CollapsibleIDs {
			if !includedIDs[id] {
				kept = append(kept, id)
			}
		}
		graph.Previous[i].CollapsibleIDs = kept
	}
	primary := graph.BranchContexts[graph.PrimaryBranch]
	joined := map[ContentHash]bool{}
	for _, m := range primary.Merges {
		if m.State == "included" {
			joined[m.Source] = true
		}
	}
	for i := range graph.Markers {
		if joined[graph.Markers[i].Target] {
			graph.Markers[i].Kind = "joined"
			graph.Markers[i].UniqueCount = 0
		}
	}
}
