package domain

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// SessionNoticeSelection identifies a local selection, never its applicability.
// Only the cloud query service interprets what this code position contains.
type SessionNoticeSelection struct {
	RepoID       string      `json:"repo_id"`
	WorktreeID   string      `json:"worktree_id"`
	BranchID     string      `json:"branch_id"`
	Snapshot     ContentHash `json:"snapshot_id"`
	CodeCommit   string      `json:"code_commit"`
	MemoryPinned bool        `json:"memory_pinned"`
	MemorySource ContentHash `json:"memory_source,omitempty"`
	MemoryHash   ContentHash `json:"memory_hash,omitempty"`
}

func (s SessionNoticeSelection) Validate() error {
	if ValidateContentHash(ContentHash(s.RepoID)) != nil || ValidateContentHash(s.Snapshot) != nil || !ValidGitOID(s.CodeCommit) || s.BranchID == "" || len(s.BranchID) > 128 {
		return ErrHashMismatch
	}
	for _, c := range s.BranchID {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return ErrHashMismatch
		}
	}

	if len(s.WorktreeID) != 32 || s.WorktreeID != strings.ToLower(s.WorktreeID) {
		return ErrHashMismatch
	}
	if _, err := hex.DecodeString(s.WorktreeID); err != nil {
		return ErrHashMismatch
	}
	if ValidateOptionalContentHash(s.MemoryHash) != nil || ValidateOptionalContentHash(s.MemorySource) != nil {
		return ErrHashMismatch
	}
	if (!s.MemoryPinned && (s.MemoryHash != "" || s.MemorySource != "")) || (s.MemoryHash == "") != (s.MemorySource == "") {
		return ErrHashMismatch
	}
	return nil
}

func (s SessionNoticeSelection) ID() ContentHash {
	raw, _ := json.Marshal(s)
	return HashContent(raw)
}

type SessionNoticeScope struct {
	RepoID     string       `json:"repo_id"`
	WorktreeID string       `json:"worktree_id"`
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
}

func (s SessionNoticeScope) Validate() error {
	if ValidateContentHash(ContentHash(s.RepoID)) != nil || len(s.WorktreeID) != 32 || s.WorktreeID != strings.ToLower(s.WorktreeID) {
		return ErrHashMismatch
	}
	if _, err := hex.DecodeString(s.WorktreeID); err != nil {
		return ErrHashMismatch
	}
	if s.Provider != ProviderClaude && s.Provider != ProviderCodex {
		return ErrHashMismatch
	}
	if len(s.SessionID) == 0 || len(s.SessionID) > 128 || strings.ContainsAny(s.SessionID, "/\\\x00\n\r") {
		return ErrHashMismatch
	}
	return nil
}
func (s SessionNoticeScope) Key() string {
	raw, _ := json.Marshal(s)
	return strings.TrimPrefix(string(HashContent(raw)), "sha256:")
}

type SessionNotice struct {
	ID        ContentHash            `json:"notice_id"`
	Selection SessionNoticeSelection `json:"selection"`
}

// Text is bounded by validated identifiers, with no collaborator-controlled
// labels or prose. Pinned empty memory explicitly forbids a latest fallback.
func (n SessionNotice) Text() string {
	raw, _ := json.Marshal(n)
	action := "Refresh CXTHub memory_load with repository=repo_id, ref=snapshot_id, code_commit and mode=effective."
	if n.Selection.MemoryPinned {
		if n.Selection.MemoryHash == "" {
			action = "This historical selection has no pinned memory. Keep it empty; do not substitute the latest memory."
		} else {
			action = "Refresh CXTHub memory_load with repository=repo_id, ref=memory_source, memory_hash, code_commit and mode=effective."
		}
	}
	return fmt.Sprintf("[CXTHub context selection]\n%s\n%s\nThis is a local position notice, not evidence that a change is applied or reverted. Preserve prior conversations; use the server's effective-memory result for current claims.", raw, action)
}
