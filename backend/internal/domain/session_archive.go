package domain

import (
	"encoding/json"
	"time"
)

type SessionArchive struct {
	RepoID     ContentHash  `json:"repo_id"`
	Key        ContentHash  `json:"key"`
	SnapshotID ContentHash  `json:"snapshot_id"`
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
	ArchivedAt time.Time    `json:"archived_at"`
	ArchivedBy string       `json:"archived_by"`
}

func SessionArchiveKey(snapshot Snapshot) ContentHash {
	identity := []string{"session-archive-v1", string(snapshot.Provider), snapshot.SessionID}
	if snapshot.SessionID == "" {
		identity = []string{"snapshot-archive-v1", string(snapshot.ID)}
	}
	encoded, _ := json.Marshal(identity)
	return HashContent(encoded)
}

type SessionArchiveView struct {
	SessionArchive
	LatestSnapshotID ContentHash   `json:"latest_snapshot_id"`
	SnapshotIDs      []ContentHash `json:"snapshot_ids"`
	Message          string        `json:"message"`
	Branch           string        `json:"branch"`
	Author           TeamIdentity  `json:"author"`
	UpdatedAt        time.Time     `json:"updated_at"`
	Origin           SessionOrigin `json:"origin"`
}

type SessionOrigin struct {
	ParentSnapshotID ContentHash  `json:"parent_snapshot_id,omitempty"`
	ParentSessionID  string       `json:"parent_session_id,omitempty"`
	ParentProvider   ProviderKind `json:"parent_provider,omitempty"`
	MainSnapshotID   ContentHash  `json:"main_snapshot_id,omitempty"`
	MainGitCommit    string       `json:"main_git_commit,omitempty"`
	MainBranch       string       `json:"main_branch"`
}
