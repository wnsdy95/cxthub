package domain

// Selected publication must reconcile every owner of its current/former names,
// but not independent lifecycle disputes elsewhere in the repository. The full
// catalog remains available for immutable-ID checks and exact foreign witnesses.
func projectPublicationBranches(events []HistoryEvent, selected []PublicationBranch) (ContextBranchProjection, error) {
	byID := map[string]HistoryEvent{}
	byIdentity, byName := map[string][]string{}, map[string][]string{}
	for _, e := range events {
		if !IsBranchBindingEvent(e) {
			continue
		}
		if _, exists := byID[e.ID]; exists {
			continue // The caller has already checked duplicate payload equality.
		}
		byID[e.ID] = e
		byIdentity[e.BranchID] = append(byIdentity[e.BranchID], e.ID)
		byName[e.Branch] = append(byName[e.Branch], e.ID)
		if e.PreviousBranch != "" && e.PreviousBranch != e.Branch {
			byName[e.PreviousBranch] = append(byName[e.PreviousBranch], e.ID)
		}
	}
	identities, names, seen := map[string]bool{}, map[string]bool{}, map[string]bool{}
	var queue []string
	addIdentity := func(id string) {
		if id != "" && !identities[id] {
			identities[id] = true
			queue = append(queue, byIdentity[id]...)
		}
	}
	addName := func(name string) {
		if name != "" && !names[name] {
			names[name] = true
			queue = append(queue, byName[name]...)
		}
	}
	for _, branch := range selected {
		addIdentity(branch.BranchID)
		addName(branch.Branch)
	}
	var component []HistoryEvent
	for i := 0; i < len(queue); i++ {
		id := queue[i]
		if seen[id] {
			continue
		}
		seen[id] = true
		e, exists := byID[id]
		if !exists {
			// Keep the referencing event. The normal projection reports its
			// missing dependency rather than dropping or fabricating that edge.
			continue
		}
		component = append(component, e)
		addIdentity(e.BranchID)
		addName(e.Branch)
		addName(e.PreviousBranch)
		for _, parent := range HistoryDependencies(e) {
			if parent != "" {
				queue = append(queue, parent)
			}
		}
	}
	return ProjectContextBranches(component)
}
