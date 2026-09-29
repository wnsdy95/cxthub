package domain

import "time"

const WorkingStateVersion = 1

// ObservationFreshness distinguishes stored captures from proof that a watcher
// observed every active provider. Reading an empty pending list proves no such thing.
type ObservationFreshness struct {
	Source         string     `json:"source"`
	WatcherState   string     `json:"watcher_state"`
	ServerChecked  bool       `json:"server_checked"`
	LastCaptureAt  *time.Time `json:"last_capture_at,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
}

type WorkingSelection struct {
	RepoID               string      `json:"repo_id"`
	WorktreeID           string      `json:"worktree_id"`
	GitBranch            string      `json:"git_branch,omitempty"`
	GitCommit            string      `json:"git_commit,omitempty"`
	ContextMode          string      `json:"context_mode"`
	ContextBranch        string      `json:"context_branch,omitempty"`
	ContextBranchID      string      `json:"context_branch_id,omitempty"`
	ContextSnapshot      ContentHash `json:"context_snapshot,omitempty"`
	SelectedCodeCommit   string      `json:"selected_code_commit,omitempty"`
	CodeMatchesSelection bool        `json:"code_matches_selection"`
}

type WorkingMemory struct {
	Pinned             bool        `json:"pinned"`
	Source             ContentHash `json:"source,omitempty"`
	ObservedSource     ContentHash `json:"observed_source,omitempty"`
	AppliedHash        ContentHash `json:"applied_hash,omitempty"`
	ObservedAttachment ContentHash `json:"observed_attachment,omitempty"`
	AttachmentChanged  bool        `json:"attachment_changed"`
}

type StagedSummary struct {
	Key        ContentHash  `json:"key"`
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
	SourceID   ContentHash  `json:"source_id"`
	Generation ContentHash  `json:"generation"`
	DocHash    ContentHash  `json:"doc_hash"`
	Events     int          `json:"events"`
	StartEvent int          `json:"start_event"`
	CapturedAt time.Time    `json:"captured_at"`
}

type PendingSummary struct {
	Provider   ProviderKind `json:"provider"`
	SessionID  string       `json:"session_id"`
	Target     ContentHash  `json:"target"`
	Branch     string       `json:"branch,omitempty"`
	UpdatedAt  time.Time    `json:"updated_at"`
	ActivityAt *time.Time   `json:"activity_at,omitempty"`
	Dismissed  bool         `json:"dismissed"`
	Scope      string       `json:"scope"`
}

// A local finalization is not a remote acknowledgement. Publication IDs let an
// explicit sync query find matching server receipts without guessing from time.
type LocalCommitSummary struct {
	OperationID        string      `json:"operation_id"`
	Target             ContentHash `json:"target"`
	IndexRevision      ContentHash `json:"index_revision"`
	CreatedAt          time.Time   `json:"created_at"`
	LocalFinalized     bool        `json:"local_finalized"`
	SelectedHistory    bool        `json:"selected_history"`
	PublicationIDs     []string    `json:"publication_ids"`
	ServerReceiptState string      `json:"server_receipt_state"`
}

type WorkingState struct {
	AppliedProjection *AppliedProjectionSummary `json:"applied_projection,omitempty"`
	Version           int                       `json:"version"`
	Revision          ContentHash               `json:"revision"`
	HistoryRevision   ContentHash               `json:"history_revision"`
	IndexRevision     ContentHash               `json:"index_revision"`
	IndexSequence     uint64                    `json:"index_sequence"`
	Selection         WorkingSelection          `json:"selection"`
	Memory            WorkingMemory             `json:"memory"`
	Freshness         ObservationFreshness      `json:"freshness"`
	Staged            []StagedSummary           `json:"staged"`
	Pending           []PendingSummary          `json:"pending"`
	LocalCommits      []LocalCommitSummary      `json:"local_commits"`
	LocalCommitScope  string                    `json:"local_commit_scope"`
	OutboxState       string                    `json:"outbox_state"`
	Coverage          string                    `json:"coverage"`
	Gaps              []string                  `json:"gaps"`
}

// A stored receipt proves a past authorized application, never current access.
type AppliedProjectionSummary struct {
	ReceiptID        ContentHash `json:"receipt_id"`
	ContextStateHash ContentHash `json:"context_state_hash"`
	MemoryStateHash  ContentHash `json:"memory_state_hash,omitempty"`
	GitCommit        string      `json:"git_commit"`
	MatchesSelection bool        `json:"matches_selection"`
	AppliedAt        time.Time   `json:"applied_at"`
}
