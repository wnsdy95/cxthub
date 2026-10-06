package domain

// IsInitialMemorySelection identifies an explicit same-worktree selection of
// the first self-owned memory root after an empty or explicitly inherited pin.
// Both events must pass ValidateHistoryEvent. Callers must verify the complete
// predecessor memory chain/owner and the successor's self-owned root digest.
func IsInitialMemorySelection(before, after HistoryEvent) bool {
	if !(before.Kind == "position" && after.Kind == "position" &&
		after.MemorySelectionParent == before.ID && before.ID != "" && before.ID != after.ID &&
		before.RepoID == after.RepoID && before.BranchID == after.BranchID &&
		before.Branch == after.Branch && before.LocalBranch == after.LocalBranch &&
		before.WorktreeID != "" && before.WorktreeID == after.WorktreeID &&
		(ValidateGitOID(before.GitAfter) == nil) && before.GitAfter == after.GitAfter &&
		before.Target != "" && after.Source == before.Target && after.Target == before.Target &&
		before.MemoryPinned && after.MemoryPinned && after.MemoryHash != "" &&
		(after.MemorySource == "" || after.MemorySource == after.Target)) {
		return false
	}
	if before.MemoryHash == "" {
		// Preserve the original pinned-empty contract exactly.
		return before.Source == before.Target && before.MemorySource == ""
	}
	// A position may have moved from another source context to Target. Its
	// explicit inherited owner, not that source or a mutable attachment, is proof.
	return before.MemorySource != "" && before.MemorySource != before.Target
}
