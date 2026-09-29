package domain

// EmptyRepositoryProof is an authorized, unfiltered server catalog observation.
// It is not a snapshot, a durable emptiness claim, or provider acceptance.
type EmptyRepositoryProof struct {
	RepositoryID string             `json:"repository_id"`
	StateHash    ContentHash        `json:"state_hash"`
	Revision     RepositoryRevision `json:"revision"`
}

type AgentBootstrapProof struct {
	Server            EmptyRepositoryProof `json:"server"`
	Branch            string               `json:"branch"`
	CodeCommit        string               `json:"code_commit,omitempty"`
	Unborn            bool                 `json:"unborn"`
	WorktreeStateHash ContentHash          `json:"worktree_state_hash"`
}

func (p AgentBootstrapProof) Validate() error {
	if ValidateContentHash(ContentHash(p.Server.RepositoryID)) != nil || ValidateContentHash(p.Server.StateHash) != nil || ValidateContentHash(p.WorktreeStateHash) != nil || ValidateBranchName(p.Branch) != nil {
		return ErrHashMismatch
	}
	if p.Unborn {
		if p.CodeCommit != "" {
			return ErrHashMismatch
		}
	} else if !ValidGitOID(p.CodeCommit) {
		return ErrHashMismatch
	}
	return nil
}
