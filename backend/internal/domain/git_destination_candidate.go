package domain

type GitDestinationCandidate struct {
	ID                   string      `json:"id"`
	RepoID               ContentHash `json:"repo_id"`
	GitOrigin            string      `json:"git_origin"`
	InstallationID       int64       `json:"installation_id"`
	ProviderRepositoryID int64       `json:"provider_repository_id"`
	ObservationID        string      `json:"observation_id"`
	ReadReceiptID        string      `json:"read_receipt_id"`
	ReadVersion          int64       `json:"read_version,string"`
	DestinationBranchID  string      `json:"destination_branch_id"`
	DestinationBranch    string      `json:"destination_branch"`
	DestinationHead      string      `json:"destination_head"`
}

func (witness GitDestinationCandidate) WithID() GitDestinationCandidate {
	witness.ID = ""
	witness.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		GitDestinationCandidate
	}{Version: 1, GitDestinationCandidate: witness})
	return witness
}

func (witness GitDestinationCandidate) Validate() error {
	if witness.ID != witness.WithID().ID || ValidateContentHash(witness.RepoID) != nil || !validPRContributionIdentity(witness.GitOrigin) || witness.InstallationID <= 0 || witness.ProviderRepositoryID <= 0 || ValidateContentHash(ContentHash("sha256:"+witness.ObservationID)) != nil || ValidateContentHash(ContentHash("sha256:"+witness.ReadReceiptID)) != nil || witness.ReadVersion <= 0 || !validPRContributionIdentity(witness.DestinationBranchID) || len(witness.DestinationBranchID) > 128 || ValidateBranchName(witness.DestinationBranch) != nil || ValidateGitOID(witness.DestinationHead) != nil {
		return ErrValidation
	}
	return nil
}

func (witness GitDestinationCandidate) MatchesReceipt(receipt GitHeadReceipt) bool {
	if witness.Validate() != nil || receipt.Validate() != nil || witness.RepoID != receipt.RepoID || witness.GitOrigin != receipt.GitOrigin || witness.ReadReceiptID != receipt.ID || witness.ReadVersion != receipt.Version || witness.InstallationID != receipt.Provider.InstallationID || witness.ProviderRepositoryID != receipt.Provider.RepositoryID {
		return false
	}
	for _, head := range receipt.Heads {
		if head.Ref == "refs/heads/"+witness.DestinationBranch && head.Commit == witness.DestinationHead && head.ObservationID == witness.ObservationID {
			return true
		}
	}
	return false
}

func (witness GitDestinationCandidate) MatchesObservation(observation GitRefObservation) bool {
	return witness.Validate() == nil && observation.Validate() == nil && observation.Source == "reconciliation" && observation.Provider != nil &&
		witness.RepoID == observation.RepoID && witness.GitOrigin == observation.GitOrigin && witness.ObservationID == observation.ID &&
		witness.InstallationID == observation.Provider.InstallationID && witness.ProviderRepositoryID == observation.Provider.RepositoryID &&
		observation.Ref == "refs/heads/"+witness.DestinationBranch && witness.DestinationHead == observation.After
}
