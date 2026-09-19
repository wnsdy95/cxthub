package domain

// EffectiveMemorySelection identifies an immutable code position in the target
// worktree. MemoryHash optionally pins a historical attachment of SnapshotID.
type EffectiveMemorySelection struct {
	SnapshotID ContentHash `json:"snapshot_id"`
	CodeCommit string      `json:"code_commit"`
	MemoryHash ContentHash `json:"memory_hash,omitempty"`
}

func (s EffectiveMemorySelection) Validate() error {
	if ValidateContentHash(s.SnapshotID) != nil || !ValidGitOID(s.CodeCommit) || (s.MemoryHash != "" && ValidateContentHash(s.MemoryHash) != nil) {
		return ErrHashMismatch
	}
	return nil
}

// ValidGitOID accepts complete nonzero SHA-1/SHA-256 object IDs, never refs.
func ValidGitOID(s string) bool { return memoryClaimOID(s) }

// This is a read DTO, not a second implementation of server applicability rules.
type EffectiveMemoryItem struct {
	ID             ContentHash      `json:"id"`
	SourceSnapshot ContentHash      `json:"source_snapshot"`
	Kind           string           `json:"kind"`
	Text           string           `json:"text"`
	Code           *MemoryCodeScope `json:"code,omitempty"`
	State          string           `json:"state"`
	Reason         string           `json:"reason"`
}
type EffectiveMemoryRequest struct {
	Selection EffectiveMemorySelection
	Content   string
	Limit     int
	Cursor    string
}
type EffectiveMemoryPage struct {
	Content   string                   `json:"content,omitempty"`
	Selection EffectiveMemorySelection `json:"selection"`
	Revision  struct {
		Evidence uint64 `json:"evidence,string,omitempty"`
		Graph    uint64 `json:"graph,string"`
		Pending  uint64 `json:"pending,string"`
	} `json:"revision"`
	StateHash   ContentHash           `json:"state_hash"`
	LineageHash ContentHash           `json:"lineage_hash"`
	Items       []EffectiveMemoryItem `json:"items"`
	Total       int                   `json:"total"`
	NextCursor  string                `json:"next_cursor"`
}
