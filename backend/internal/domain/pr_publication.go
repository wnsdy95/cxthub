package domain

// IsPublicationProof identifies the ordinary observation finalized by p. A
// publication's own timestamp is not evidence of when its source was observed.
func IsPublicationProof(p, observation HistoryEvent) bool {
	return p.Kind == "publish" && observation.Kind != "publish" && observation.Kind != "pr-merge" &&
		observation.RepoID == p.RepoID && observation.BranchID == p.BranchID && observation.Branch == p.Branch &&
		observation.LocalBranch == p.LocalBranch && observation.WorktreeID == p.WorktreeID &&
		observation.GitAfter == p.GitAfter && observation.Target == p.Target
}

// MatchesPRSourcePublication recognizes a canonical branch or a recorded native
// alias. Alias attachment must precede the ordinary proof, not the publication:
// finalization can be delayed or created after a wall-clock rollback.
func MatchesPRSourcePublication(p HistoryEvent, head string, events []HistoryEvent) bool {
	if p.Kind != "publish" {
		return false
	}
	if p.Branch == head {
		return true
	}
	if p.LocalBranch != head || p.WorktreeID == "" {
		return false
	}
	for _, proof := range events {
		if !IsPublicationProof(p, proof) {
			continue
		}
		for _, attached := range events {
			if attached.Kind == "attach" && attached.RepoID == p.RepoID && attached.LocalBranch != "" &&
				attached.WorktreeID == p.WorktreeID && attached.BranchID == p.BranchID &&
				!attached.CreatedAt.After(proof.CreatedAt) {
				return true
			}
		}
	}
	return false
}
