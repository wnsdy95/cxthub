package domain

// WorkingPosition is a worktree-local cursor, not the repository's shared
// branch head. Snapshot and memory are pinned when selected.
type WorkingPosition struct {
	RepoID       string        `json:"repo_id"`
	WorktreeID   string        `json:"worktree_id"`
	Branch       string        `json:"branch"`
	BranchID     string        `json:"branch_id,omitempty"`
	LocalBranch  string        `json:"local_branch,omitempty"`
	GitCommit    string        `json:"git_commit,omitempty"`
	Snapshot     ContentHash   `json:"snapshot,omitempty"`
	SharedTarget ContentHash   `json:"shared_target,omitempty"`
	MemoryHash   ContentHash   `json:"memory_hash,omitempty"`
	MemorySource ContentHash   `json:"memory_source,omitempty"`
	MemoryPinned bool          `json:"memory_pinned,omitempty"`
	Orphan       bool          `json:"orphan,omitempty"`
	Rewound      bool          `json:"rewound,omitempty"`
	Selection    *HistoryEvent `json:"selection,omitempty"`
}
