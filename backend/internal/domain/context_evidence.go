package domain

import "sort"

// Completion is a historical fact. Placement and lineage describe this read
// generation only; neither can revoke the server's completion receipt.
type ContextMergeEvidence struct {
	EventID         string `json:"event_id"`
	BirthID         string `json:"birth_id,omitempty"`
	Completed       bool   `json:"completed"`
	PlacementIntact bool   `json:"placement_intact"`
	SourceAvailable bool   `json:"source_available"`
	Lineage         string `json:"lineage"`
}

type ContextSemantics struct {
	Version int                    `json:"version"`
	Merges  []ContextMergeEvidence `json:"merges"`
}

// ProjectContextSemantics runs before display filtering or collapsing. All
// clients consume these facts from the same repository generation.
func ProjectContextSemantics(snapshots []Snapshot, history []HistoryEvent) ContextSemantics {
	out := ContextSemantics{Version: 1, Merges: []ContextMergeEvidence{}}
	byID := make(map[ContentHash]Snapshot, len(snapshots))
	for _, s := range snapshots {
		byID[s.ID] = s
	}
	allAncestry := newContextAncestry(byID, false)
	naturalAncestry := newContextAncestry(byID, true)
	births := make(map[string][]HistoryEvent)
	for _, h := range history {
		if h.Kind == "birth" || h.Kind == "orphan" {
			births[h.BranchID] = append(births[h.BranchID], h)
		}
	}
	for _, h := range history {
		if h.Kind != "pr-merge" || !h.PRCompleted || h.PR == nil {
			continue
		}
		_, available := byID[h.Source]
		e := ContextMergeEvidence{EventID: h.ID, Completed: true, SourceAvailable: available, Lineage: "unknown"}
		e.PlacementIntact = available && allAncestry.reaches(h.Target, h.Source) && allAncestry.reaches(h.Target, h.SharedTarget)
		if bs := births[h.SourceBranchID]; len(bs) == 1 {
			b := bs[0]
			e.BirthID = b.ID
			_, birthAvailable := byID[b.Source]
			switch {
			case !available:
				e.Lineage = "missing"
			case b.Kind == "orphan":
				e.Lineage = "orphan"
			case !birthAvailable:
				e.Lineage = "missing"
			case b.Source == h.Source:
				e.Lineage = "unchanged"
			case naturalAncestry.reaches(h.Source, b.Source):
				e.Lineage = "natural"
			default:
				_, complete := contextEvidenceClosure(byID, h.Source, true)
				if !complete {
					e.Lineage = "missing"
				} else {
					included, complete := contextEvidenceClosure(byID, h.Source, false)
					if included[b.Source] {
						e.Lineage = "graft"
					} else if complete {
						e.Lineage = "disconnected"
					} else {
						e.Lineage = "missing"
					}
				}
			}
		} else if !available {
			e.Lineage = "missing"
		}
		out.Merges = append(out.Merges, e)
	}
	// Facts are keyed by immutable operation identity, not response order.
	sort.Slice(out.Merges, func(i, j int) bool { return out.Merges[i].EventID < out.Merges[j].EventID })
	return out
}

func contextEvidenceClosure(byID map[ContentHash]Snapshot, root ContentHash, conversation bool) (map[ContentHash]bool, bool) {
	seen := map[ContentHash]bool{}
	stack, complete := []ContentHash{root}, true
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		s, ok := byID[id]
		if !ok {
			complete = false
			continue
		}
		seen[id] = true
		parents := s.ReachabilityParents()
		if conversation {
			parents = s.Parents
			// Legacy destructive grafts used the first natural slot for inclusion.
			if s.Grafted && len(s.GraftParents) == 0 && len(parents) > 0 {
				parents = parents[1:]
			}
		}
		stack = append(stack, parents...)
	}
	return seen, complete
}
