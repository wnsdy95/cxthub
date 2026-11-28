package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

func gitEvidenceKey(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// GitRefObservation is an immutable delivery fact: a push the GitHub App
// delivered for a branch. Only its identity reaches the branch ledger; the
// observation itself is not stored. A blank After represents the deletion of
// the branch.
type GitRefObservation struct {
	Delivery  string                  `json:"delivery,omitempty"`
	ID        string                  `json:"id"`
	RepoID    ContentHash             `json:"repo_id"`
	GitOrigin string                  `json:"git_origin"`
	Source    string                  `json:"source"`
	Ref       string                  `json:"ref"`
	After     string                  `json:"after,omitempty"`
	Provider  *GitProviderObservation `json:"provider,omitempty"`
}

type GitProviderObservation struct {
	InstallationID int64 `json:"installation_id"`
	RepositoryID   int64 `json:"repository_id"`
}

// WithID identifies a push the GitHub App delivered by its repository, origin,
// source and delivery, so a delivery processed again keeps its identity even
// when its content differs. Any other observation gets no identity, and the
// branch ledger leaves it out.
func (o GitRefObservation) WithID() GitRefObservation {
	o.ID = ""
	if o.Provider != nil && o.Source == "github-push" {
		o.ID = gitEvidenceKey([]string{"qualified-git-ref-observation-v1", string(o.RepoID), o.GitOrigin, o.Source, o.Delivery})
	}
	return o
}
