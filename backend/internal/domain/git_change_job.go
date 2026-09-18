package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

// GitChangeRequest asks for verification, never asserts that a reversal happened.
// It is repository-scoped immutable evidence; it does not move a shared branch
// or a worktree's position. Merge comparison parents must be explicit.
type GitChangeRequest struct {
	Target       string `json:"target"`
	TargetParent string `json:"target_parent,omitempty"`
	Commit       string `json:"commit"`
	Parent       string `json:"parent,omitempty"`
}

func (r GitChangeRequest) Validate() error {
	for _, oid := range []string{r.Target, r.Commit} {
		if err := ValidateGitOID(oid); err != nil {
			return err
		}
	}
	for _, oid := range []string{r.TargetParent, r.Parent} {
		if oid != "" {
			if err := ValidateGitOID(oid); err != nil {
				return err
			}
		}
	}
	if r.Target == r.Commit || r.Target == r.TargetParent || r.Commit == r.Parent {
		return fmt.Errorf("%w: self-referencing Git change", ErrValidation)
	}
	return nil
}
func GitChangeID(repo ContentHash, origin string, request GitChangeRequest) string {
	b, _ := json.Marshal(struct {
		Repo    ContentHash
		Origin  string
		Request GitChangeRequest
	}{repo, origin, request})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func ValidateGitChangeID(id string) error {
	if len(id) != 64 {
		return ErrValidation
	}
	_, err := hex.DecodeString(id)
	if err != nil {
		return ErrValidation
	}
	return nil
}

// Result is written exactly once with the completed state. Completion means
// verification finished, not that every change was proved or is currently active.
// Version fences a worker after lease recovery; progress belongs to this queue,
// not the immutable PR-completion history stream.
type GitChangeJob struct {
	ID          string               `json:"id"`
	RepoID      ContentHash          `json:"repo_id"`
	GitOrigin   string               `json:"git_origin"`
	Request     GitChangeRequest     `json:"request"`
	State       string               `json:"state"`
	Attempts    int                  `json:"attempts"`
	Version     int64                `json:"version,string"`
	Reason      string               `json:"reason,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
	UpdatedAt   time.Time            `json:"updated_at"`
	NextAttempt time.Time            `json:"next_attempt"`
	LeaseUntil  time.Time            `json:"lease_until"`
	Result      *GitReversalEvidence `json:"result,omitempty"`
}

func (j GitChangeJob) Validate() error {
	if err := ValidateContentHash(j.RepoID); err != nil {
		return err
	}
	if err := j.Request.Validate(); err != nil {
		return err
	}
	if j.GitOrigin == "" || j.ID != GitChangeID(j.RepoID, j.GitOrigin, j.Request) || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() || j.Version < 0 || j.Attempts < 0 {
		return ErrValidation
	}
	switch j.State {
	case "waiting", "running", "retrying", "attention":
		if j.Result != nil {
			return ErrIntegrity
		}
	case "completed":
		if j.Result == nil || j.Result.Commit != j.Request.Commit || j.Result.Target != j.Request.Target {
			return ErrIntegrity
		}
		if (j.Request.Parent != "" && j.Result.Parent != j.Request.Parent) || (j.Request.TargetParent != "" && j.Result.TargetParent != j.Request.TargetParent) {
			return ErrIntegrity
		}
		if err := j.Result.Validate(); err != nil {
			return err
		}
	default:
		return ErrValidation
	}
	return nil
}
func (j GitChangeJob) Claim(now time.Time, lease time.Duration) (GitChangeJob, error) {
	if lease <= 0 || j.NextAttempt.After(now) || (j.State != "waiting" && j.State != "retrying" && j.State != "running") || (j.State == "running" && j.LeaseUntil.After(now)) {
		return j, ErrNotFound
	}
	j.State = "running"
	j.Version++
	j.Attempts++
	j.UpdatedAt = now
	j.LeaseUntil = now.Add(lease)
	return j, j.Validate()
}
func (j GitChangeJob) AcceptFinish(next GitChangeJob) error {
	if j.State != "running" || j.Version != next.Version || j.ID != next.ID || j.Request != next.Request || j.RepoID != next.RepoID || j.GitOrigin != next.GitOrigin || !j.CreatedAt.Equal(next.CreatedAt) || j.Attempts != next.Attempts {
		return ErrConflict
	}
	if next.State != "completed" && next.State != "retrying" && next.State != "attention" {
		return ErrValidation
	}
	return next.Validate()
}
func (j GitChangeJob) Retry(now time.Time) (GitChangeJob, error) {
	if j.State == "completed" {
		return j, nil
	}
	if j.State == "running" && j.LeaseUntil.After(now) {
		return j, ErrConflict
	}
	j.State = "waiting"
	j.Version++
	j.Reason = ""
	j.UpdatedAt = now
	j.NextAttempt = now
	j.LeaseUntil = time.Time{}
	return j, j.Validate()
}

// GitChangeSummary excludes potentially large path evidence. Lists remain cheap;
// callers explicitly fetch one result when they need its complete proof.
type GitChangeSummary struct {
	ID              string           `json:"id"`
	Request         GitChangeRequest `json:"request"`
	State           string           `json:"state"`
	Version         int64            `json:"version,string"`
	Reason          string           `json:"reason,omitempty"`
	UpdatedAt       time.Time        `json:"updated_at"`
	Coverage        string           `json:"coverage,omitempty"`
	VerifiedPaths   int              `json:"verified_paths"`
	UnverifiedPaths int              `json:"unverified_paths"`
}

func (j GitChangeJob) Summary() GitChangeSummary {
	out := GitChangeSummary{ID: j.ID, Request: j.Request, State: j.State, Version: j.Version, Reason: j.Reason, UpdatedAt: j.UpdatedAt}
	if j.Result != nil {
		out.Coverage = j.Result.Coverage
		out.VerifiedPaths = len(j.Result.Paths)
		out.UnverifiedPaths = len(j.Result.UnverifiedPaths)
	}
	return out
}

type GitChangePage struct {
	Items      []GitChangeSummary `json:"items"`
	NextCursor string             `json:"next_cursor,omitempty"`
}
