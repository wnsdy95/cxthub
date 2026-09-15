package domain

// LocalBranchBinding belongs to this replica. A tracking alias names the same
// server branch; it never creates another server ref or duplicates its history.
type LocalBranchBinding struct {
	LocalBranch string `json:"local_branch"`
	Branch      string `json:"branch"`
	BranchID    string `json:"branch_id"`
	Tracking    bool   `json:"tracking"`
	Inactive    bool   `json:"inactive,omitempty"`
	RenamedTo   string `json:"renamed_to,omitempty"`
}

func (p WorkingPosition) GitBranch() string {
	if p.LocalBranch != "" {
		return p.LocalBranch
	}
	return p.Branch
}
