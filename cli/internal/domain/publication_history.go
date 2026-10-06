package domain

import (
	"fmt"
	"sort"
)

// PublicationHistoryBranches enumerates durable local identities for a bounded
// history-only drain. Remote acceptance is deliberately absent: it acknowledges
// delivery, never chooses local authority. Observations alone cannot invent a
// modern canonical name. Unbound ordinary/server evidence remains available to
// the planner as a witness, without acquiring independent publication authority.
func PublicationHistoryBranches(repo string, events []HistoryEvent) ([]PublicationBranch, error) {
	if err := ValidateContentHash(ContentHash(repo)); err != nil {
		return nil, err
	}
	catalog, err := indexPublicationHistory(repo, events, nil)
	if err != nil {
		return nil, err
	}
	projection, err := ProjectContextBranches(catalog.all)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSyncConflict, err)
	}
	branches := map[string]PublicationBranch{}
	for _, e := range catalog.all {
		name := ""
		if known, ok := projection.ByID[e.BranchID]; ok {
			name = known.Name
		} else if e.Branch != "" && e.BranchID == LegacyContextBranchID(repo, e.Branch) {
			name = e.Branch
		} else if e.Kind == "position" || e.Kind == "attach" || e.Kind == "pr-merge" {
			// Tracking retains foreign ordinary proof without adopting its birth.
			// It must not starve the known local identity that consumes that proof.
			continue
		} else {
			return nil, fmt.Errorf("%w: local canonical name is unresolved for identity %q", ErrSyncConflict, e.BranchID)
		}
		// Validate only the scope envelope; a history-only identity needs no live tip.
		shape := Ref{Kind: RefBranch, Name: name, BranchID: e.BranchID, Target: ContentHash(repo)}
		if err := ValidateRef(shape); err != nil {
			return nil, err
		}
		b := PublicationBranch{Branch: name, BranchID: e.BranchID}
		if old, ok := branches[e.BranchID]; ok && old != b {
			return nil, ErrSyncConflict
		}
		branches[e.BranchID] = b
	}
	out := make([]PublicationBranch, 0, len(branches))
	for _, b := range branches {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BranchID < out[j].BranchID })
	return out, nil
}
