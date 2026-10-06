package domain

// IsInitialMemorySelection identifies an explicit same-worktree selection
// dependency, not an ordering inferred from clocks or a mutable attachment.
// Both events must pass ValidateHistoryEvent. The caller must also verify the
// successor digest is a root owned by Target.
func IsInitialMemorySelection(before, after HistoryEvent) bool {
	return before.Kind == "position" && after.Kind == "position" &&
		after.MemorySelectionParent == before.ID && before.ID != "" && before.ID != after.ID &&
		before.RepoID == after.RepoID && before.BranchID == after.BranchID &&
		before.Branch == after.Branch && before.LocalBranch == after.LocalBranch &&
		before.WorktreeID != "" && before.WorktreeID == after.WorktreeID &&
		(ValidateGitOID(before.GitAfter) == nil) && before.GitAfter == after.GitAfter &&
		before.Target != "" && before.Source == before.Target &&
		after.Source == before.Target && after.Target == before.Target &&
		before.MemoryPinned && before.MemoryHash == "" && before.MemorySource == "" &&
		after.MemoryPinned && after.MemoryHash != "" &&
		(after.MemorySource == "" || after.MemorySource == after.Target)
}
