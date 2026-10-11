package domain

type GitContextHead struct {
	ID               string                  `json:"id"`
	Destination      GitDestinationCandidate `json:"destination"`
	AnchorID         string                  `json:"anchor_id"`
	ContextTarget    ContentHash             `json:"context_target"`
	GraphRevision    uint64                  `json:"graph_revision,string"`
	EvidenceRevision uint64                  `json:"evidence_revision,string"`
}

func (head GitContextHead) WithID() GitContextHead {
	head.ID = ""
	head.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		GitContextHead
	}{1, head})
	return head
}

func (head GitContextHead) Validate() error {
	if head.ID != head.WithID().ID || head.Destination.Validate() != nil || ValidateContentHash(head.ContextTarget) != nil {
		return ErrValidation
	}
	if head.AnchorID == "" {
		if head.Destination.DestinationBranchID != LegacyContextBranchID(string(head.Destination.RepoID), head.Destination.DestinationBranch) {
			return ErrValidation
		}
	} else if !validAdmissionEventID(head.AnchorID) {
		return ErrValidation
	}
	return nil
}

type GitContributionProof struct {
	ID            string          `json:"id"`
	AdmissionID   string          `json:"admission_id"`
	ContextHeadID string          `json:"context_head_id"`
	Contribution  GitContribution `json:"contribution"`
}

func (proof GitContributionProof) WithID() GitContributionProof {
	proof.ID = ""
	proof.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		GitContributionProof
	}{1, proof})
	return proof
}

func (proof GitContributionProof) Validate() error {
	if proof.ID != proof.WithID().ID || proof.Contribution.Validate() != nil || ValidateContentHash(ContentHash("sha256:"+proof.AdmissionID)) != nil || ValidateContentHash(ContentHash("sha256:"+proof.ContextHeadID)) != nil {
		return ErrValidation
	}
	return nil
}

func (proof GitContributionProof) MatchesInputs(admission GitContributionAdmission, head GitContextHead, receipt GitHeadReceipt) bool {
	if proof.Validate() != nil || admission.Validate() != nil || head.Validate() != nil || proof.AdmissionID != admission.ID || proof.ContextHeadID != head.ID || !admission.MatchesLaterContextHead(head, receipt) {
		return false
	}
	record := proof.Contribution
	return record.RepoID == admission.Destination.RepoID && record.GitOrigin == admission.Destination.GitOrigin &&
		record.ProviderRepositoryID == admission.Destination.ProviderRepositoryID && record.DestinationBranchID == admission.Destination.DestinationBranchID &&
		record.SourceBranchID == admission.SourceBranchID && record.Capture == admission.Capture && record.OriginalHead == admission.OriginalHead
}
