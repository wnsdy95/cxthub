package domain

// GraftPatch is a single graft LWW register to be replaced by join.
type GraftPatch struct {
	SnapshotID  ContentHash
	ExpectedSeq uint64
	Parents     []ContentHash
}

// JoinMutation is an atomic join change set sent to the store.
type JoinMutation struct {
	BranchID string
	RepoID   ContentHash
	Branch   string
	// Source is the commit X pulled by the user. The store revalidates that the entire Segment is still attached to the target branch or scoped internal session ref, and that the single-leaf condition of first-parent is maintained within the repo graph lock/transaction.
	Source ContentHash
	// Segment is the unique first-parent child path calculated by the server from X to tip X…tip.
	// Used for revalidation of attachment/cross-git-branch in storage atomic boundary.
	Segment      []ContentHash
	ExpectedHead ContentHash
	NewHead      ContentHash
	ForkName     string
	ForkTip      ContentHash
	Grafts       []GraftPatch
}
