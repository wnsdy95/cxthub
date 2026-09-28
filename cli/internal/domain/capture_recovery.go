package domain

import (
	"encoding/json"
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
}

type CaptureOutcome struct {
	Provider    string      `json:"provider"`
	State       string      `json:"state"`
	SessionPath string      `json:"session_path,omitempty"`
	Target      ContentHash `json:"target,omitempty"`
	Error       string      `json:"error,omitempty"`
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
	if p.Version != 1 || p.Proof.Kind != "position" || p.Proof.WorktreeID == "" ||
		!validCaptureOID(p.Proof.GitAfter) || p.Proof.Source != p.Proof.Target || len(p.Outcomes) == 0 {
		return ErrHashMismatch
	}
	if err := ValidateHistoryEvent(p.Proof); err != nil {
		return err
	}
	if err := ValidateOptionalContentHash(p.Initial); err != nil {
		return err
	}
	member, hasTarget := p.Proof.Target == p.Initial, p.Initial != ""
	seen := map[string]bool{}
	for _, o := range p.Outcomes {
		if (o.Provider != ProviderClaude && o.Provider != ProviderCodex) || seen[o.Provider] {
			return ErrHashMismatch
		}
		seen[o.Provider] = true
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

type CaptureRecoveryStatus struct {
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
