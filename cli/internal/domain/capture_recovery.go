package domain

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// CaptureAttempt is the durable commit-capture journal. A later successful
// attempt does not change the original attempt's outcomes or completion bit.
type CaptureAttempt struct {
	Version     int              `json:"version"`
	Proof       HistoryEvent     `json:"proof"`
	Initial     ContentHash      `json:"initial,omitempty"`
	Outcomes    []CaptureOutcome `json:"outcomes"`
	Observation *HistoryEvent    `json:"observation,omitempty"`
	Complete    bool             `json:"complete"`
	// Version 2 retains input before projection. Version 1 is never upgraded
	// from a subsequently observed provider file.
	FrozenPosition  *WorkingPosition       `json:"frozen_position,omitempty"`
	Message         string                 `json:"message,omitempty"`
	Author          *TeamIdentity          `json:"author,omitempty"`
	Settings        map[string]ContentHash `json:"settings,omitempty"`
	InputsReady     bool                   `json:"inputs_ready,omitempty"`
	FinalMemory     *FrozenCaptureMemory   `json:"final_memory,omitempty"`
	MemoryFinalized bool                   `json:"memory_finalized,omitempty"`
	// A queued predecessor is selected at admission by the exact Git parent and
	// unchanged worktree cursor. Its result is pinned before any successor effect.
	Predecessor            *CapturePredecessor `json:"predecessor,omitempty"`
	PredecessorObservation *HistoryEvent       `json:"predecessor_observation,omitempty"`
}

type CapturePredecessor struct {
	AttemptID string `json:"attempt_id"`
	GitCommit string `json:"git_commit"`
}

type CaptureOutcome struct {
	MemoryHash   ContentHash          `json:"memory_hash,omitempty"`
	MemorySource ContentHash          `json:"memory_source,omitempty"`
	SessionID    string               `json:"session_id,omitempty"`
	Provider     string               `json:"provider"`
	State        string               `json:"state"`
	SessionPath  string               `json:"session_path,omitempty"`
	Target       ContentHash          `json:"target,omitempty"`
	Error        string               `json:"error,omitempty"`
	Input        *FrozenCaptureInput  `json:"input,omitempty"`
	MemoryPlan   *FrozenCaptureMemory `json:"memory_plan,omitempty"`
}

// FrozenCaptureMemory is persisted before changing the snapshot attachment.
// A retry uses the same CAS predecessor and immutable result, never a newer
// provider file or a concurrent writer's summary.
type FrozenCaptureMemory struct {
	Version         int           `json:"version"`
	Snapshot        ContentHash   `json:"snapshot"`
	ExpectedMemory  ContentHash   `json:"expected_memory,omitempty"`
	Memory          ContentHash   `json:"memory"`
	SelectionParent string        `json:"selection_parent,omitempty"`
	RootSelection   *HistoryEvent `json:"root_selection,omitempty"`
}

func (m FrozenCaptureMemory) Validate() error {
	if m.Version != 1 || ValidateContentHash(m.Snapshot) != nil || ValidateContentHash(m.Memory) != nil || ValidateOptionalContentHash(m.ExpectedMemory) != nil {
		return ErrHashMismatch
	}
	if m.RootSelection != nil && (ValidateHistoryEvent(*m.RootSelection) != nil || m.RootSelection.Target != m.Snapshot || m.RootSelection.MemorySelectionParent == "") {
		return ErrHashMismatch
	}
	return nil
}

func (p CaptureAttempt) Fingerprint() ContentHash {
	b, _ := json.Marshal(p)
	return HashContent(b)
}

func (p CaptureAttempt) MatchesObservation(target ContentHash, e HistoryEvent) bool {
	return e.Kind != "publish" && e.Kind != "pr-merge" && e.RepoID == p.Proof.RepoID &&
		e.BranchID == p.Proof.BranchID && e.Branch == p.Proof.Branch && e.LocalBranch == p.Proof.LocalBranch &&
		e.WorktreeID == p.Proof.WorktreeID && e.GitAfter == p.Proof.GitAfter && e.Target == target
}

func (p CaptureAttempt) Validate() error {
	if p.Predecessor != nil {
		d := p.Predecessor
		if p.Version != 2 || len(d.AttemptID) != 32 || strings.Trim(d.AttemptID, "0123456789abcdef") != "" || d.AttemptID == p.Proof.ID || !validCaptureOID(d.GitCommit) || d.GitCommit == p.Proof.GitAfter {
			return ErrHashMismatch
		}
		if e := p.PredecessorObservation; e != nil {
			knownID := e.ID == CaptureFinalObservationID(d.AttemptID) || e.ID == CaptureBaselineObservationID(d.AttemptID) || e.ID == CaptureContinuationObservationID(d.AttemptID)
			for i := 0; i < 2; i++ {
				knownID = knownID || e.ID == CaptureProviderObservationID(d.AttemptID, i)
			}
			if !knownID || ValidateHistoryEvent(*e) != nil || e.Kind != "position" || e.RepoID != p.Proof.RepoID || e.WorktreeID != p.Proof.WorktreeID || e.BranchID != p.Proof.BranchID || e.Branch != p.Proof.Branch || e.LocalBranch != p.Proof.LocalBranch || e.GitAfter != d.GitCommit || e.Source != e.Target || !e.MemoryPinned {
				return ErrHashMismatch
			}
		} else if p.Complete || p.FinalMemory != nil || p.MemoryFinalized {
			return ErrHashMismatch
		}
	} else if p.PredecessorObservation != nil {
		return ErrHashMismatch
	}
	if (p.Version != 1 && p.Version != 2) || p.Proof.Kind != "position" || p.Proof.WorktreeID == "" ||
		!validCaptureOID(p.Proof.GitAfter) || p.Proof.Source != p.Proof.Target || len(p.Outcomes) == 0 {
		return ErrHashMismatch
	}
	if err := ValidateHistoryEvent(p.Proof); err != nil {
		return err
	}
	if err := ValidateOptionalContentHash(p.Initial); err != nil {
		return err
	}
	if p.Version == 1 && (p.FrozenPosition != nil || p.InputsReady || len(p.Settings) != 0 || p.Message != "" || p.Author != nil || p.FinalMemory != nil || p.MemoryFinalized) {
		return ErrHashMismatch
	}
	if p.Version == 2 {
		if p.FinalMemory != nil && p.FinalMemory.Validate() != nil {
			return ErrHashMismatch
		}
		f := p.FrozenPosition
		if f == nil || f.RepoID != p.Proof.RepoID || f.WorktreeID != p.Proof.WorktreeID || f.BranchID != p.Proof.BranchID || f.Branch != p.Proof.Branch || f.LocalBranch != p.Proof.LocalBranch || f.Snapshot != p.Initial || f.MemoryHash != p.Proof.MemoryHash || f.MemorySource != p.Proof.MemorySource {
			return ErrHashMismatch
		}
		for kind, hash := range p.Settings {
			if (kind != "claude" && kind != "agents" && kind != "codex") || ValidateContentHash(hash) != nil {
				return ErrHashMismatch
			}
		}
		if p.Complete && (!p.InputsReady || !p.MemoryFinalized) {
			return ErrHashMismatch
		}
	}
	member, hasTarget := p.Proof.Target == p.Initial, p.Initial != ""
	if e := p.PredecessorObservation; e != nil && e.Target != "" {
		member, hasTarget = member || p.Proof.Target == e.Target, true
	}
	seen := map[string]bool{}
	for _, o := range p.Outcomes {
		if o.MemoryPlan != nil && (p.Version != 2 || o.MemoryPlan.Validate() != nil || o.State == "absent" || (o.State == "saved" && (o.MemoryPlan.Snapshot != o.Target || o.MemoryPlan.Memory != o.MemoryHash))) {
			return ErrHashMismatch
		}
		if (o.Provider != ProviderClaude && o.Provider != ProviderCodex) || seen[o.Provider] {
			return ErrHashMismatch
		}
		seen[o.Provider] = true
		if ValidateOptionalContentHash(o.MemoryHash) != nil || ValidateOptionalContentHash(o.MemorySource) != nil || (o.MemoryHash == "" && o.MemorySource != "") || (p.Version == 1 && (o.MemoryHash != "" || o.MemorySource != "")) {
			return ErrHashMismatch
		}
		if o.Input != nil {
			if p.Version != 2 || o.Input.Provider != o.Provider || o.Input.Validate() != nil {
				return ErrHashMismatch
			}
		}
		if p.Version == 2 && ((o.State == "absent" && o.Input != nil) || (p.InputsReady && o.State != "absent" && o.Input == nil)) {
			return ErrHashMismatch
		}
		switch o.State {
		case "saved":
			if err := ValidateContentHash(o.Target); err != nil {
				return err
			}
			member = member || p.Proof.Target == o.Target
			hasTarget = true
		case "absent", "pending":
			if o.Target != "" {
				return ErrHashMismatch
			}
		case "failed":
			if err := ValidateOptionalContentHash(o.Target); err != nil {
				return err
			}
		default:
			return ErrHashMismatch
		}
		if p.Complete && o.State != "saved" && o.State != "absent" {
			return ErrHashMismatch
		}
	}
	if p.Complete && (!member || hasTarget && p.Proof.Target == "") {
		return ErrHashMismatch
	}
	if p.Observation != nil {
		if !p.MatchesObservation(p.Proof.Target, *p.Observation) {
			return ErrHashMismatch
		}
		if err := ValidateHistoryEvent(*p.Observation); err != nil {
			return err
		}
	}
	return nil
}

func validCaptureOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	if strings.Trim(s, "0") == "" {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func CaptureProviderObservationID(attempt string, index int) string {
	return strings.TrimPrefix(string(HashContent([]byte("frozen-capture-observation\x00"+attempt+"\x00"+strconv.Itoa(index)))), "sha256:")[:32]
}

func CaptureBaselineObservationID(attempt string) string {
	return strings.TrimPrefix(string(HashContent([]byte("capture-baseline\x00"+attempt))), "sha256:")[:32]
}

func CaptureFinalObservationID(attempt string) string {
	return strings.TrimPrefix(string(HashContent([]byte("capture-final-memory\x00"+attempt))), "sha256:")[:32]
}

func CaptureContinuationObservationID(attempt string) string {
	return strings.TrimPrefix(string(HashContent([]byte("capture-continuation\x00"+attempt))), "sha256:")[:32]
}

func CaptureCompletionObservationID(attempt string) string {
	return strings.TrimPrefix(string(HashContent([]byte("capture-completion\x00"+attempt))), "sha256:")[:32]
}

func CapturePublicationID(e HistoryEvent) string {
	e.ID, e.CreatedAt = "", time.Time{}
	raw, _ := json.Marshal(e)
	h := string(HashContent(append([]byte("publication\x00"), raw...)))
	return strings.TrimPrefix(h, "sha256:")[:32]
}

func (p CaptureAttempt) Publication() HistoryEvent {
	e := p.Proof
	e.Kind, e.Creation = "publish", nil
	e.MemoryHash, e.MemorySource, e.MemoryPinned = "", "", true
	e.ID = CapturePublicationID(e)
	return e
}

// SameCapturePosition includes worktree and branch generation. A matching Git
// SHA alone cannot attest to another worker's or another branch's capture.
func SameCapturePosition(a, b CaptureAttempt) bool {
	x, y := a.Proof, b.Proof
	return x.RepoID == y.RepoID && x.WorktreeID == y.WorktreeID && x.BranchID == y.BranchID &&
		x.Branch == y.Branch && x.LocalBranch == y.LocalBranch && x.GitAfter == y.GitAfter
}

type CaptureResolution struct {
	Version         int         `json:"version"`
	RepoID          string      `json:"repo_id"`
	WorktreeID      string      `json:"worktree_id"`
	AttemptID       string      `json:"attempt_id"`
	AttemptHash     ContentHash `json:"attempt_hash"`
	Kind            string      `json:"kind"` // superseded or acknowledged-gap; never capture completion
	ReplacementID   string      `json:"replacement_id,omitempty"`
	ReplacementHash ContentHash `json:"replacement_hash,omitempty"`
	PublicationID   string      `json:"publication_id,omitempty"`
	Reason          string      `json:"reason,omitempty"`
	CreatedAt       time.Time   `json:"created_at"`
}

type CaptureRetryState struct {
	Version int       `json:"version"`
	Attempt string    `json:"attempt"`
	Tries   int       `json:"tries"`
	Next    time.Time `json:"next"`
	Error   string    `json:"error,omitempty"`
}

type CaptureRecoveryStatus struct {
	Retry         *CaptureRetryState `json:"retry,omitempty"`
	ID            string             `json:"id"`
	RepoID        string             `json:"repo_id"`
	WorktreeID    string             `json:"worktree_id"`
	Branch        string             `json:"branch"`
	CodeCommit    string             `json:"code_commit"`
	Fingerprint   ContentHash        `json:"fingerprint"`
	State         string             `json:"state"`
	Outcomes      []CaptureOutcome   `json:"outcomes"`
	ReplacementID string             `json:"replacement_id,omitempty"`
	PublicationID string             `json:"publication_id,omitempty"`
	Resolution    *CaptureResolution `json:"resolution,omitempty"`
}
