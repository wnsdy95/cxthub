package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// CollectorStateVersion is the layout of the collector's local state.
const CollectorStateVersion = 1

// CollectorState is what the automatic collector remembers between runs on
// this computer, so that a run uploads only what changed since the last one.
// It is a memory of what was sent, never a source of facts: every run still
// reads the tools' records and Git, and a missing or discarded state only
// makes the next run send more.
type CollectorState struct {
	Version int `json:"version"`
	// EnabledAt is when automatic reading began in this repository. Sessions
	// whose records all lie before it are uploaded only on request.
	EnabledAt string `json:"enabled_at"`
	// Target names the server repository the entries below were sent to.
	// Entries for another target are forgotten.
	Target   string                           `json:"target"`
	Sessions map[string]CollectorSessionState `json:"sessions"`
	// Branches is the fingerprint of the branch IDs last registered.
	Branches string `json:"branches,omitempty"`
	// Worktrees is the fingerprint of the worktree list last reported.
	Worktrees string        `json:"worktrees,omitempty"`
	LastRun   *CollectorRun `json:"last_run,omitempty"`
}

// CollectorSessionState is what was last sent for one session.
type CollectorSessionState struct {
	// Sent is the fingerprint of the report the server last stored.
	Sent string `json:"sent,omitempty"`
	// ContentKey names the records and the masking the stored content was
	// built from. The same key means the same document.
	ContentKey string           `json:"content_key,omitempty"`
	Content    *AnchoredContent `json:"content,omitempty"`
	FirstEvent []int            `json:"first_event,omitempty"`
	Events     []int            `json:"events,omitempty"`
	// Summarized lists the session's commits whose summary the server
	// stored, so that a later run does not write them down again.
	Summarized []string `json:"summarized,omitempty"`
}

// CollectorRun records how the last automatic run ended, so that a collector
// that does not work is visible.
type CollectorRun struct {
	At        string `json:"at"`
	Stored    int    `json:"stored"`
	Unchanged int    `json:"unchanged"`
	Refused   int    `json:"refused"`
	Error     string `json:"error,omitempty"`
}

// CollectorSessionKey is the key of a session in CollectorState.Sessions.
func CollectorSessionKey(provider ProviderKind, id string) string {
	return string(provider) + "/" + id
}

// Fingerprint is the hash of a value's JSON form. Two values with the same
// fingerprint were the same when they were sent.
func Fingerprint(value any) string {
	raw, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SessionContentKey names the records behind a session's ranges and the
// content epoch they would be converted in: the masking policy and the
// document identity. Records never change once written, so the same ranges in
// the same epoch give the same document and the same placement.
func SessionContentKey(records SessionRecordRanges, epoch string) string {
	return Fingerprint(struct {
		Provider ProviderKind `json:"provider"`
		ID       string       `json:"id"`
		Ranges   [][]string   `json:"ranges"`
		Epoch    string       `json:"epoch"`
	}{records.Provider, records.ID, records.Ranges, epoch})
}

// InAutomaticScope reports whether a session is read automatically: its last
// own record, or its recorded start when it has no record of its own, is at
// or after the time automatic reading began. An older session is uploaded
// only on request.
func InAutomaticScope(session SegmentedSession, enabledAt time.Time) bool {
	last := session.EndedAt
	if last == "" {
		last = session.StartedAt
	}
	if last == "" {
		return false
	}
	at, err := time.Parse(time.RFC3339Nano, last)
	if err != nil {
		return false
	}
	return !at.Before(enabledAt)
}
