package domain

import (
	"encoding/hex"
	"strings"
)

const MaxGitAdmissionWitnesses = 128

type GitContributionAdmission struct {
	ID                     string                  `json:"id"`
	Destination            GitDestinationCandidate `json:"destination"`
	SourceBranchID         string                  `json:"source_branch_id"`
	SourceBranch           string                  `json:"source_branch"`
	SourceAnchorID         string                  `json:"source_anchor_id"`
	DestinationAnchorID    string                  `json:"destination_anchor_id"`
	PublicationID          string                  `json:"publication_id"`
	Capture                ContentHash             `json:"capture"`
	OriginalHead           string                  `json:"original_head"`
	SourceHead             string                  `json:"source_head"`
	SourceObservationID    string                  `json:"source_observation_id"`
	OrdinaryObservationIDs []string                `json:"ordinary_observation_ids"`
	CarryObservationIDs    []string                `json:"carry_observation_ids"`
	GraphRevision          uint64                  `json:"graph_revision,string"`
	EvidenceRevision       uint64                  `json:"evidence_revision,string"`
	AfterReadVersion       int64                   `json:"after_read_version,string"`
}

func (admission GitContributionAdmission) WithID() GitContributionAdmission {
	admission.ID = ""
	admission.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		GitContributionAdmission
	}{Version: 1, GitContributionAdmission: admission})
	return admission
}

func (admission GitContributionAdmission) Key() string {
	return gitEvidenceKey(struct {
		Version              int         `json:"version"`
		RepoID               ContentHash `json:"repo_id"`
		GitOrigin            string      `json:"git_origin"`
		InstallationID       int64       `json:"installation_id"`
		ProviderRepositoryID int64       `json:"provider_repository_id"`
		SourceBranchID       string      `json:"source_branch_id"`
		DestinationBranchID  string      `json:"destination_branch_id"`
		PublicationID        string      `json:"publication_id"`
		Capture              ContentHash `json:"capture"`
		OriginalHead         string      `json:"original_head"`
		SourceHead           string      `json:"source_head"`
	}{1, admission.Destination.RepoID, admission.Destination.GitOrigin, admission.Destination.InstallationID,
		admission.Destination.ProviderRepositoryID, admission.SourceBranchID, admission.Destination.DestinationBranchID,
		admission.PublicationID, admission.Capture, admission.OriginalHead, admission.SourceHead})
}

func (admission GitContributionAdmission) Validate() error {
	destination := admission.Destination
	if admission.ID != admission.WithID().ID || destination.Validate() != nil ||
		!validPRContributionIdentity(admission.SourceBranchID) || len(admission.SourceBranchID) > 128 ||
		ValidateBranchName(admission.SourceBranch) != nil || admission.SourceBranch == destination.DestinationBranch ||
		admission.SourceBranchID == destination.DestinationBranchID || ValidateContentHash(admission.Capture) != nil ||
		ValidateGitOID(admission.OriginalHead) != nil || ValidateGitOID(admission.SourceHead) != nil ||
		len(admission.SourceHead) != len(admission.OriginalHead) || len(admission.SourceHead) != len(destination.DestinationHead) ||
		admission.SourceHead == destination.DestinationHead || admission.AfterReadVersion < destination.ReadVersion ||
		!validAdmissionEventID(admission.PublicationID) {
		return ErrValidation
	}
	for _, anchor := range []struct{ id, branch, name string }{
		{admission.SourceAnchorID, admission.SourceBranchID, admission.SourceBranch},
		{admission.DestinationAnchorID, destination.DestinationBranchID, destination.DestinationBranch},
	} {
		if anchor.id == "" {
			if anchor.branch != LegacyContextBranchID(string(destination.RepoID), anchor.name) {
				return ErrValidation
			}
		} else if !validAdmissionEventID(anchor.id) {
			return ErrValidation
		}
	}
	for _, ids := range [][]string{admission.OrdinaryObservationIDs, admission.CarryObservationIDs} {
		if len(ids) == 0 || len(ids) > MaxGitAdmissionWitnesses {
			return ErrValidation
		}
		for index, id := range ids {
			if !validAdmissionEventID(id) || id == admission.PublicationID || (index > 0 && ids[index-1] >= id) {
				return ErrValidation
			}
		}
	}
	provider := GitProviderObservation{InstallationID: destination.InstallationID, RepositoryID: destination.ProviderRepositoryID}
	source := GitRefObservation{RepoID: destination.RepoID, GitOrigin: destination.GitOrigin, Source: "reconciliation", Ref: "refs/heads/" + admission.SourceBranch, After: admission.SourceHead, Provider: &provider}.WithID()
	if source.Validate() != nil || source.ID != admission.SourceObservationID {
		return ErrValidation
	}
	return nil
}

func (admission GitContributionAdmission) MatchesBaseline(receipt GitHeadReceipt) bool {
	if admission.Validate() != nil || !admission.Destination.MatchesReceipt(receipt) {
		return false
	}
	head, found := receipt.SourceHead("refs/heads/" + admission.SourceBranch)
	return found && head.Commit == admission.SourceHead && head.ObservationID == admission.SourceObservationID
}

func (admission GitContributionAdmission) MatchesLaterRead(candidate GitDestinationCandidate, receipt GitHeadReceipt) bool {
	return admission.matchesLaterRead(candidate, receipt, false)
}

func (admission GitContributionAdmission) MatchesLaterContextHead(head GitContextHead, receipt GitHeadReceipt) bool {
	return head.Validate() == nil && admission.matchesLaterRead(head.Destination, receipt, true)
}

func (admission GitContributionAdmission) matchesLaterRead(candidate GitDestinationCandidate, receipt GitHeadReceipt, boundContext bool) bool {
	baseline := admission.Destination
	return admission.Validate() == nil && candidate.MatchesReceipt(receipt) && candidate.ReadVersion > admission.AfterReadVersion &&
		candidate.RepoID == baseline.RepoID && candidate.GitOrigin == baseline.GitOrigin &&
		candidate.InstallationID == baseline.InstallationID && candidate.ProviderRepositoryID == baseline.ProviderRepositoryID &&
		candidate.DestinationBranchID == baseline.DestinationBranchID && (boundContext || candidate.DestinationBranch == baseline.DestinationBranch)
}

func validAdmissionEventID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
