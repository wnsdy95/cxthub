package domain

// Validate the inventory before any object requests. The application must then
// reconstruct its full exact-token graph and validate all dependency objects;
// a metadata delta alone is not that graph.
func ValidateBranchPullPlan(repo string, request BranchPullRequest, plan BranchPullPlan) error {
	bad := ErrHashMismatch
	if plan.Version != BranchPullVersion || plan.RepoID != repo || plan.Branch != request.Branch || plan.ContextProtocol < 0 || plan.ContextProtocol > 1 {
		return bad
	}
	selected := plan.SelectedRef
	if selected.RepoID != repo || selected.Kind != RefBranch || selected.Name != request.Branch || selected.Target == "" || selected.Symbolic != "" || ValidateRef(selected) != nil {
		return bad
	}
	index := map[ContentHash]bool{}
	for _, id := range plan.SnapshotIndex {
		if ValidateContentHash(id) != nil || index[id] || ValidateContentHash(plan.SnapshotStates[id]) != nil {
			return bad
		}
		index[id] = true
	}
	if len(index) != len(plan.SnapshotStates) || !index[selected.Target] {
		return bad
	}
	seenRefs := map[string]bool{}
	selectedCount := 0
	for _, ref := range plan.Refs {
		if ref.RepoID != repo || ValidateRef(ref) != nil || ref.Symbolic != "" || !index[ref.Target] {
			return bad
		}
		key := string(ref.Kind) + "/" + ref.Name
		if seenRefs[key] {
			return bad
		}
		seenRefs[key] = true
		if ref.Kind == RefBranch {
			if ref != selected {
				return bad
			}
			selectedCount++
		} else {
			lifecycle, ok, err := ParseBranchLifecycleRef(ref)
			if err != nil || !ok || lifecycle.Branch != request.Branch {
				return bad
			}
		}
	}
	if selectedCount != 1 {
		return bad
	}
	latest, found, err := LatestBranchLifecycle(plan.Refs, request.Branch)
	if err != nil || (selected.BranchID == "" && found && latest.State == BranchArchived && latest.Target == selected.Target) {
		return bad
	}
	for _, event := range plan.History {
		if event.RepoID != repo || ValidateHistoryEvent(event) != nil {
			return bad
		}
		for _, id := range []ContentHash{event.Source, event.Target, event.SharedTarget, event.MemorySource} {
			if id != "" && !index[id] {
				return bad
			}
		}
	}
	ordered, err := OrderHistoryEvents(plan.History)
	if err != nil || len(ordered) != len(plan.History) {
		return bad
	}
	projection, err := ProjectContextBranches(ordered)
	if err != nil {
		return bad
	}
	identity := selected.BranchID
	if identity == "" {
		identity = LegacyContextBranchID(repo, request.Branch)
	}
	active, modern := projection.Active[request.Branch]
	if (modern && active.ID != identity) || (!modern && (projection.Released[request.Branch] != "" || identity != LegacyContextBranchID(repo, request.Branch))) {
		return bad
	}
	// A receipt alone cannot prove which source context completed the PR.
	// Preserve competing pins, but require an exact ordinary source witness.
	type sourcePin struct {
		identity, code string
		target         ContentHash
	}
	pins := map[sourcePin]bool{}
	for _, event := range ordered {
		if IsPinnedContextEvent(event) {
			pins[sourcePin{event.BranchID, event.GitAfter, event.Target}] = true
		}
	}
	for _, event := range ordered {
		if event.BranchID == identity && event.Kind == "pr-merge" && event.PRCompleted && !pins[sourcePin{event.SourceBranchID, event.PR.HeadSHA, event.Source}] {
			return bad
		}
	}
	requested := map[ContentHash]bool{}
	for _, id := range request.ObservationRoots {
		requested[id] = true
	}
	absent := map[ContentHash]bool{}
	for _, id := range plan.AbsentRoots {
		if !requested[id] || absent[id] || index[id] {
			return bad
		}
		absent[id] = true
	}
	for id := range requested {
		if !index[id] && !absent[id] {
			return bad
		}
	}
	settings := map[ContentHash]bool{}
	for _, setting := range plan.SettingsObjects {
		if ValidateContentHash(setting.Hash) != nil || settings[setting.Hash] || (setting.Kind != "claude" && setting.Kind != "codex" && setting.Kind != "agents") {
			return bad
		}
		settings[setting.Hash] = true
	}
	return nil
}
