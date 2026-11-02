package domain

import (
	"encoding/json"
	"errors"
)

const (
	RepositoryViewSourceVersion       = 1
	RepositoryViewSourceScope         = "repository-view-v1"
	RepositoryGraphSourceScope        = "repository-graph-v1"
	DefaultRepositoryViewSourceLimit  = 128
	MaxRepositoryViewSourceLimit      = 1000
	MaxRepositoryViewSourceImageBytes = 1 << 20
	MaxRepositoryViewSourcePageBytes  = 4 << 20
	MaxRepositoryViewSourceKeyBytes   = 4 << 10
)

var ErrRepositoryViewSourceLimit = errors.New("repository view source metadata exceeds read budget")

type RepositoryViewSourceCheckpoint struct {
	Version  int         `json:"version"`
	Scope    string      `json:"scope"`
	RepoID   ContentHash `json:"repo_id"`
	Epoch    string      `json:"epoch"`
	Sequence int64       `json:"sequence,string"`
}

func (checkpoint RepositoryViewSourceCheckpoint) Validate() error {
	if checkpoint.Version != RepositoryViewSourceVersion || (checkpoint.Scope != RepositoryViewSourceScope && checkpoint.Scope != RepositoryGraphSourceScope) {
		return ErrCatalogResetRequired
	}
	if ValidateContentHash(checkpoint.RepoID) != nil || !catalogEpochValid(checkpoint.Epoch) || checkpoint.Epoch == "00000000-0000-0000-0000-000000000000" || checkpoint.Sequence < 0 {
		return ErrValidation
	}
	return nil
}

type RepositoryViewSourceRequest struct {
	Version int
	After   *RepositoryViewSourceCheckpoint
	Cursor  string
	Limit   int
}

func (request RepositoryViewSourceRequest) Validate() error {
	if request.Version != RepositoryViewSourceVersion || request.Limit < 0 || request.Limit > MaxRepositoryViewSourceLimit || (request.After != nil && request.Cursor != "") || len(request.Cursor) > 16<<10 {
		return ErrValidation
	}
	if request.After != nil {
		return request.After.Validate()
	}
	return nil
}

func RepositoryViewSourceKindValid(kind string) bool {
	switch kind {
	case "snapshot", "ref", "history", "protocol", "repository", "pending", "unsync", "session_archive", "reflog", "revision":
		return true
	default:
		return false
	}
}

func RepositoryGraphSourceKindValid(kind string) bool {
	if RepositoryViewSourceKindValid(kind) {
		return true
	}
	switch kind {
	case "git_commit", "git_tree", "git_delta", "git_observation", "pr_checkpoint", "pr_participation", "pr_contribution", "git_contribution", "git_receipt", "git_admission", "git_context_head", "git_contribution_proof":
		return true
	default:
		return false
	}
}

type RepositoryViewSourceEntry struct {
	Sequence int64           `json:"sequence,string"`
	Kind     string          `json:"kind"`
	Key      string          `json:"key"`
	Deleted  bool            `json:"deleted,omitempty"`
	Value    json.RawMessage `json:"value,omitempty"`
}

type RepositoryViewSourcePage struct {
	Version    int                             `json:"version"`
	Scope      string                          `json:"scope"`
	RepoID     ContentHash                     `json:"repo_id"`
	Epoch      string                          `json:"epoch"`
	Through    int64                           `json:"through,string"`
	Mode       string                          `json:"mode"`
	Entries    []RepositoryViewSourceEntry     `json:"entries"`
	NextCursor string                          `json:"next_cursor,omitempty"`
	Checkpoint *RepositoryViewSourceCheckpoint `json:"checkpoint,omitempty"`
}
