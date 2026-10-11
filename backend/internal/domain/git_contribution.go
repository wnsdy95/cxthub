package domain

import (
	"encoding/hex"
	"strings"
)

type GitContribution struct {
	ID                   string      `json:"id"`
	RepoID               ContentHash `json:"repo_id"`
	GitOrigin            string      `json:"git_origin"`
	ProviderRepositoryID int64       `json:"provider_repository_id"`
	DestinationBranchID  string      `json:"destination_branch_id"`
	SourceBranchID       string      `json:"source_branch_id"`
	Capture              ContentHash `json:"capture"`
	OriginalHead         string      `json:"original_head"`
	IntegrationSHA       string      `json:"integration_sha"`
}

func (contribution GitContribution) WithID() GitContribution {
	contribution.ID = ""
	contribution.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		GitContribution
	}{Version: 1, GitContribution: contribution})
	return contribution
}

func (contribution GitContribution) Validate() error {
	if contribution.ID != contribution.WithID().ID || ValidateContentHash(contribution.RepoID) != nil || ValidateContentHash(contribution.Capture) != nil || !validPRContributionIdentity(contribution.GitOrigin) || contribution.ProviderRepositoryID <= 0 || ValidateGitOID(contribution.OriginalHead) != nil || ValidateGitOID(contribution.IntegrationSHA) != nil || len(contribution.OriginalHead) != len(contribution.IntegrationSHA) {
		return ErrValidation
	}
	for _, branch := range []string{contribution.DestinationBranchID, contribution.SourceBranchID} {
		if !validPRContributionIdentity(branch) || len(branch) > 128 {
			return ErrValidation
		}
	}
	return nil
}

type GitContributionMemory struct {
	ID             string      `json:"id"`
	RepoID         ContentHash `json:"repo_id"`
	ContributionID string      `json:"contribution_id"`
	PublicationID  string      `json:"publication_id"`
	MemoryHash     ContentHash `json:"memory_hash"`
	MemorySource   ContentHash `json:"memory_source"`
	MemoryPinned   bool        `json:"memory_pinned"`
}

func (memory GitContributionMemory) WithID() GitContributionMemory {
	memory.ID = ""
	memory.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		GitContributionMemory
	}{Version: 1, GitContributionMemory: memory})
	return memory
}

func (memory GitContributionMemory) Validate() error {
	if memory.ID != memory.WithID().ID || ValidateContentHash(memory.RepoID) != nil || !memory.MemoryPinned || ValidateOptionalContentHash(memory.MemoryHash) != nil || ValidateOptionalContentHash(memory.MemorySource) != nil || (memory.MemoryHash != "" && memory.MemorySource == "") || ValidateContentHash(ContentHash("sha256:"+memory.ContributionID)) != nil || len(memory.PublicationID) != 32 || memory.PublicationID != strings.ToLower(memory.PublicationID) {
		return ErrValidation
	}
	if _, err := hex.DecodeString(memory.PublicationID); err != nil {
		return ErrValidation
	}
	return nil
}
