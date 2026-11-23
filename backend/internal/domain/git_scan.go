package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

func gitEvidenceKey(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// GitRefObservation is an immutable delivery fact. A signed push can arrive
// out of order, so observing it does NOT move a shared context ref. Blank before
// or after represents ref creation/deletion respectively.
type GitRefObservation struct {
	Delivery    string                     `json:"delivery,omitempty"`
	ID          string                     `json:"id"`
	RepoID      ContentHash                `json:"repo_id"`
	GitOrigin   string                     `json:"git_origin"`
	Source      string                     `json:"source"`
	Ref         string                     `json:"ref"`
	Before      string                     `json:"before,omitempty"`
	After       string                     `json:"after,omitempty"`
	Forced      bool                       `json:"forced"`
	PullRequest *GitPullRequestObservation `json:"pull_request,omitempty"`
	Provider    *GitProviderObservation    `json:"provider,omitempty"`
}

type GitProviderObservation struct {
	InstallationID int64 `json:"installation_id"`
	RepositoryID   int64 `json:"repository_id"`
}

type GitPullRequestObservation struct {
	InstallationID   int64     `json:"installation_id"`
	RepositoryID     int64     `json:"repository_id"`
	ID               int64     `json:"id"`
	Number           int       `json:"number"`
	Action           string    `json:"action"`
	HeadRepositoryID int64     `json:"head_repository_id"`
	BaseRepositoryID int64     `json:"base_repository_id"`
	BaseRef          string    `json:"base_ref"`
	MergeSHA         string    `json:"merge_sha,omitempty"`
	ProviderEventID  string    `json:"provider_event_id,omitempty"`
	OccurredAt       time.Time `json:"occurred_at,omitempty,omitzero"`
}

func (o GitRefObservation) WithID() GitRefObservation {
	o.ID = ""
	if o.Provider != nil {
		if o.Source == "github-push" {
			o.ID = gitEvidenceKey([]string{"qualified-git-ref-observation-v1", string(o.RepoID), o.GitOrigin, o.Source, o.Delivery})
		} else {
			o.ID = gitEvidenceKey(struct {
				Version int `json:"version"`
				GitRefObservation
			}{Version: 1, GitRefObservation: o})
		}
		return o
	}
	if o.Source == "github-push" || o.Source == "github-pull-request" || o.Source == "github-pr-reconciliation" {
		o.ID = gitEvidenceKey([]string{string(o.RepoID), o.GitOrigin, o.Source, o.Delivery})
	} else {
		o.ID = gitEvidenceKey(o)
	}
	return o
}
