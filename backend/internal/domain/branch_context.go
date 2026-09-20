package domain

// BranchContext separates a branch's integrated knowledge from the mutable
// placement of a worker's conversation. CodeCommit is an exact recorded code
// position, never a timestamp-based guess at the remote Git head.
type BranchContext struct {
	BranchID   string               `json:"branch_id"`
	SnapshotID ContentHash          `json:"snapshot_id"`
	CodeCommit string               `json:"code_commit,omitempty"`
	Reason     string               `json:"reason"`
	Merges     []BranchContextMerge `json:"merges"`
	// Roots are oldest integrated contribution first, then the selected context.
	// SnapshotIDs is the authoritative, newest-first context timeline.
	Roots       []ContentHash `json:"roots"`
	SnapshotIDs []ContentHash `json:"snapshot_ids"`
}

type BranchContextMerge struct {
	DestinationBranchID string      `json:"destination_branch_id"`
	EventID             string      `json:"event_id"`
	Source              ContentHash `json:"source"`
	Before              ContentHash `json:"before,omitempty"`
	MergeSHA            string      `json:"merge_sha"`
	PRNumber            int         `json:"pr_number"`
	State               string      `json:"state"` // included, not_selected, review
	Reason              string      `json:"reason"`
	// Distance along the verified Git first-parent chain; zero is selected code.
	// -1 means no ordered integration proof, never a timestamp fallback.
	Order int `json:"order"`
}
