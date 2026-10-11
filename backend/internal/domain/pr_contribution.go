package domain

import (
	"encoding/hex"
	"strings"
	"unicode"
	"unicode/utf8"
)

type PRContribution struct {
	ID                   string           `json:"id"`
	RepoID               ContentHash      `json:"repo_id"`
	GitOrigin            string           `json:"git_origin"`
	ProviderRepositoryID int64            `json:"provider_repository_id"`
	PullRequestID        int64            `json:"pull_request_id"`
	PR                   PullRequestMerge `json:"pr"`
	DestinationBranchID  string           `json:"destination_branch_id"`
	SourceBranchID       string           `json:"source_branch_id"`
	Capture              ContentHash      `json:"capture"`
	OriginalHead         string           `json:"original_head"`
}

func (contribution PRContribution) WithID() PRContribution {
	contribution.ID = ""
	contribution.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		PRContribution
	}{Version: 1, PRContribution: contribution})
	return contribution
}

func (contribution PRContribution) Validate() error {
	if contribution.ID != contribution.WithID().ID || ValidateContentHash(contribution.RepoID) != nil || ValidateContentHash(contribution.Capture) != nil || !validPRContributionIdentity(contribution.GitOrigin) || contribution.ProviderRepositoryID <= 0 || contribution.PullRequestID <= 0 || contribution.PR.Validate() != nil || ValidateGitOID(contribution.OriginalHead) != nil {
		return ErrValidation
	}
	if len(contribution.PR.HeadSHA) != len(contribution.PR.MergeSHA) || len(contribution.OriginalHead) != len(contribution.PR.HeadSHA) {
		return ErrValidation
	}
	if ValidateGitOID(contribution.PR.HeadSHA) != nil || ValidateGitOID(contribution.PR.MergeSHA) != nil {
		return ErrValidation
	}
	for _, branch := range []string{contribution.DestinationBranchID, contribution.SourceBranchID} {
		if !validPRContributionIdentity(branch) || len(branch) > 128 {
			return ErrValidation
		}
	}
	return nil
}

func validPRContributionIdentity(value string) bool {
	return value != "" && utf8.ValidString(value) && strings.TrimSpace(value) == value && strings.IndexFunc(value, unicode.IsControl) == -1
}

type PRContributionMemory struct {
	ID             string      `json:"id"`
	RepoID         ContentHash `json:"repo_id"`
	ContributionID string      `json:"contribution_id"`
	PublicationID  string      `json:"publication_id"`
	MemoryHash     ContentHash `json:"memory_hash"`
	MemorySource   ContentHash `json:"memory_source"`
	MemoryPinned   bool        `json:"memory_pinned"`
}

func (memory PRContributionMemory) WithID() PRContributionMemory {
	memory.ID = ""
	memory.ID = gitEvidenceKey(struct {
		Version int `json:"version"`
		PRContributionMemory
	}{Version: 1, PRContributionMemory: memory})
	return memory
}

func (memory PRContributionMemory) Validate() error {
	if memory.ID != memory.WithID().ID || ValidateContentHash(memory.RepoID) != nil || !memory.MemoryPinned || ValidateOptionalContentHash(memory.MemoryHash) != nil || ValidateOptionalContentHash(memory.MemorySource) != nil || (memory.MemoryHash != "" && memory.MemorySource == "") || ValidateContentHash(ContentHash("sha256:"+memory.ContributionID)) != nil || len(memory.PublicationID) != 32 || memory.PublicationID != strings.ToLower(memory.PublicationID) {
		return ErrValidation
	}
	if _, err := hex.DecodeString(memory.PublicationID); err != nil {
		return ErrValidation
	}
	return nil
}
