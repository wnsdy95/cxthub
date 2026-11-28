package domain

import "time"

// Repo is the root of the session storage space (data model, sync protocol).
//
// One per detected code repository. The sessions, notes and branch ledger of the repository belong to it.
//
// Invariants:
//   - R1: ID remains stable throughout the repo's lifetime (remote promotion is only via migration).
//   - R2: DefaultBranch is a valid branch name, set when the repository is registered and kept afterwards.
//
// ID Derivation (R-ID): Hash of normalized remote URL (if present) or cwd absolute path (fallback).
// The server trusts the ID as a multi-tenant isolation key (output is CLI responsibility).
// LocalPath is always an empty string on the server (local-only field, sync protocol).
type Repo struct {
	RequiredDocIdentity DocumentIdentity `json:"required_doc_identity,omitempty"`
	ID                  ContentHash      `json:"id"`
	RemoteURL           string           `json:"remote_url"`
	LocalPath           string           `json:"local_path"`
	DefaultBranch       string           `json:"default_branch"`
	// RepositoryID is the containing repository (visibility boundary). During push it is
	// derived from /<owner_username>/<repository-slug>/… in RemoteURL. "" means unowned (legacy).
	RepositoryID string `json:"repository_id,omitempty"`
	// GitRemoteURL is the code repository's Git origin (for example, GitHub), used by the web "Connected" tab.
	GitRemoteURL string `json:"git_remote_url,omitempty"`
	// About fields correspond to GitHub's repository About panel and are editable on the web.
	Description string   `json:"description,omitempty"`
	Website     string   `json:"website,omitempty"`
	Topics      []string `json:"topics,omitempty"`
}

// SettingsFile is a single file for team default settings bundle (path is relative to bundle root).
type SettingsFile struct {
	Path       string `json:"path"`
	ContentB64 string `json:"content_b64"`
}

// SettingsBundle is the team default agent settings for the repo
// (.claude/.agents/.codex folder).
// Upload folder (claude|agents|codex) via web; team members receive it via cxt
// and overwrite the matching local directory.
// Kind ∈ {claude, agents, codex}.
type SettingsBundle struct {
	Kind      string         `json:"kind"`
	Files     []SettingsFile `json:"files"`
	UpdatedAt time.Time      `json:"updated_at"`
	UpdatedBy string         `json:"updated_by,omitempty"`
}
