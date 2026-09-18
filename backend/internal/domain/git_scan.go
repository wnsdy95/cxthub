package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// GitScanJob indexes one immutable commit and then discovers inverse changes.
// It never selects a shared ref or another user's working position. Cursor
// belongs to discovery, not a timestamp: late ancestors are indexed too.
type GitScanJob struct {
	ID          string      `json:"id"`
	RepoID      ContentHash `json:"repo_id"`
	GitOrigin   string      `json:"git_origin"`
	Commit      string      `json:"commit"`
	State       string      `json:"state"`
	Indexed     bool        `json:"indexed"`
	Cursor      string      `json:"cursor,omitempty"`
	Version     int64       `json:"version,string"`
	Attempts    int         `json:"attempts"`
	Reason      string      `json:"reason,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
	NextAttempt time.Time   `json:"next_attempt"`
	LeaseUntil  time.Time   `json:"lease_until"`
}

func gitEvidenceKey(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func NewGitScan(repo ContentHash, origin, commit string, now time.Time) GitScanJob {
	return GitScanJob{ID: gitEvidenceKey([]string{string(repo), origin, commit}), RepoID: repo, GitOrigin: origin, Commit: commit, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
}
func (j GitScanJob) Validate() error {
	if ValidateContentHash(j.RepoID) != nil || ValidateGitOID(j.Commit) != nil || j.GitOrigin == "" || j.ID != NewGitScan(j.RepoID, j.GitOrigin, j.Commit, j.CreatedAt).ID || j.CreatedAt.IsZero() || j.UpdatedAt.IsZero() || j.Version < 0 || j.Attempts < 0 {
		return ErrValidation
	}
	if j.Cursor != "" && (len(j.Cursor) != 128 || ValidateGitChangeID(j.Cursor[:64]) != nil || ValidateGitChangeID(j.Cursor[64:]) != nil) {
		return ErrValidation
	}
	if (!j.Indexed && j.Cursor != "") || (j.State == "completed" && !j.Indexed) {
		return ErrIntegrity
	}
	switch j.State {
	case "waiting", "running", "retrying", "attention", "completed":
		return nil
	}
	return ErrValidation
}
func (j GitScanJob) Claim(now time.Time, lease time.Duration) (GitScanJob, error) {
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
func (j GitScanJob) AcceptFinish(n GitScanJob) error {
	if j.State != "running" || j.Version != n.Version || j.ID != n.ID || j.RepoID != n.RepoID || j.GitOrigin != n.GitOrigin || j.Commit != n.Commit || j.Attempts != n.Attempts || !j.CreatedAt.Equal(n.CreatedAt) || (j.Indexed && !n.Indexed) || n.Cursor < j.Cursor {
		return ErrConflict
	}
	switch n.State {
	case "waiting", "retrying", "attention", "completed":
		return n.Validate()
	}
	return ErrValidation
}
func (j GitScanJob) Retry(now time.Time) (GitScanJob, error) {
	if j.State == "completed" {
		return j, nil
	}
	if j.State == "running" && j.LeaseUntil.After(now) {
		return j, ErrConflict
	}
	j.State = "waiting"
	j.Version++
	j.Reason = ""
	j.NextAttempt = now
	j.UpdatedAt = now
	j.LeaseUntil = time.Time{}
	return j, j.Validate()
}

// GitDeltaRecord contains provider evidence, never client-supplied source code.
// Index keys include the exact path, bytes and mode. They are candidate lookup
// keys only; domain reversal assessment and ancestry verification still run.
type GitDeltaRecord struct {
	ID          string         `json:"id"`
	RepoID      ContentHash    `json:"repo_id"`
	GitOrigin   string         `json:"git_origin"`
	Delta       GitCommitDelta `json:"delta"`
	Keys        []string       `json:"keys"`
	InverseKeys []string       `json:"inverse_keys"`
}

func NewGitDelta(repo ContentHash, origin string, d GitCommitDelta) GitDeltaRecord {
	r := GitDeltaRecord{ID: gitEvidenceKey([]string{string(repo), origin, d.Commit, d.Parent}), RepoID: repo, GitOrigin: origin, Delta: d, Keys: []string{}, InverseKeys: []string{}}
	for _, p := range d.Changes {
		r.Keys = append(r.Keys, gitEvidenceKey(p))
		r.InverseKeys = append(r.InverseKeys, gitEvidenceKey(GitPathChange{Path: p.Path, Before: p.After, After: p.Before}))
	}
	return r
}
func (r GitDeltaRecord) Validate() error {
	if ValidateContentHash(r.RepoID) != nil || r.GitOrigin == "" || !r.Delta.Complete {
		return ErrIntegrity
	}
	if err := r.Delta.Validate(); err != nil {
		return err
	}
	want := NewGitDelta(r.RepoID, r.GitOrigin, r.Delta)
	if gitEvidenceKey(want) != gitEvidenceKey(r) {
		return ErrIntegrity
	}
	return nil
}

type GitInverseCandidate struct {
	Cursor  string
	Request GitChangeRequest
}

// GitScanFinish is a bounded atomic publication: index + ancestor work, or
// candidate requests + cursor. PostgreSQL commits this with the repo revision.
type GitScanFinish struct {
	Job     GitScanJob
	Deltas  []GitDeltaRecord
	Parents []GitScanJob
	Changes []GitChangeJob
}

func (p GitScanFinish) Validate() error {
	if err := p.Job.Validate(); err != nil {
		return err
	}
	if len(p.Deltas) > 16 || len(p.Parents) > 16 || len(p.Changes) > 100 {
		return ErrValidation
	}
	for _, d := range p.Deltas {
		if d.RepoID != p.Job.RepoID || d.GitOrigin != p.Job.GitOrigin || d.Delta.Commit != p.Job.Commit {
			return ErrIntegrity
		}
		if err := d.Validate(); err != nil {
			return err
		}
	}
	for _, j := range p.Parents {
		if j.RepoID != p.Job.RepoID || j.GitOrigin != p.Job.GitOrigin || j.Commit == p.Job.Commit {
			return ErrIntegrity
		}
		if err := j.Validate(); err != nil {
			return err
		}
	}
	for _, j := range p.Changes {
		if j.RepoID != p.Job.RepoID || j.GitOrigin != p.Job.GitOrigin || (j.Request.Target != p.Job.Commit && j.Request.Commit != p.Job.Commit) {
			return ErrIntegrity
		}
		if err := j.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// GitRefObservation is an immutable delivery fact. A signed push can arrive
// out of order, so observing it does NOT move a shared context ref. Blank before
// or after represents ref creation/deletion respectively.
type GitRefObservation struct {
	Delivery  string      `json:"delivery,omitempty"`
	ID        string      `json:"id"`
	RepoID    ContentHash `json:"repo_id"`
	GitOrigin string      `json:"git_origin"`
	Source    string      `json:"source"` // github-push or reconciliation
	Ref       string      `json:"ref"`
	Before    string      `json:"before,omitempty"`
	After     string      `json:"after,omitempty"`
	Forced    bool        `json:"forced"`
}

func (o GitRefObservation) WithID() GitRefObservation {
	o.ID = ""
	if o.Source == "github-push" {
		o.ID = gitEvidenceKey([]string{string(o.RepoID), o.GitOrigin, o.Source, o.Delivery})
	} else {
		o.ID = gitEvidenceKey(o)
	}
	return o
}
func (o GitRefObservation) Validate() error {
	if ValidateContentHash(o.RepoID) != nil || o.GitOrigin == "" || o.ID != o.WithID().ID || len(o.Ref) < 12 || o.Ref[:11] != "refs/heads/" || (o.Before == "" && o.After == "") {
		return ErrValidation
	}
	if o.Source == "github-push" && (o.Delivery == "" || len(o.Delivery) > 128) {
		return ErrValidation
	}
	if err := ValidateBranchName(o.Ref[11:]); err != nil {
		return err
	}
	if o.Source != "github-push" && o.Source != "reconciliation" {
		return ErrValidation
	}
	for _, s := range []string{o.Before, o.After} {
		if s != "" {
			if err := ValidateGitOID(s); err != nil {
				return err
			}
		}
	}
	return nil
}

type GitScanPage struct {
	Reconciliation *GitHeadScan `json:"reconciliation,omitempty"`
	Items          []GitScanJob `json:"items"`
	NextCursor     string       `json:"next_cursor,omitempty"`
}

// GitHeadScan checkpoints reconciliation pagination independently per origin.
// Version fencing prevents an expired page reader from resetting newer progress.
type GitHeadScan struct {
	State       string      `json:"state"`
	Reason      string      `json:"reason,omitempty"`
	RepoID      ContentHash `json:"repo_id"`
	GitOrigin   string      `json:"git_origin"`
	Page        int         `json:"page"`
	Version     int64       `json:"version,string"`
	NextAttempt time.Time   `json:"next_attempt"`
	LeaseUntil  time.Time   `json:"lease_until"`
}

func (j GitHeadScan) Validate() error {
	switch j.State {
	case "waiting", "running", "retrying", "completed":
	default:
		return ErrValidation
	}
	if ValidateContentHash(j.RepoID) != nil || j.GitOrigin == "" || j.Page < 1 || j.Version < 0 {
		return ErrValidation
	}
	return nil
}
func (j GitHeadScan) Claim(now time.Time, lease time.Duration) (GitHeadScan, error) {
	if lease <= 0 || j.NextAttempt.After(now) || j.LeaseUntil.After(now) {
		return j, ErrNotFound
	}
	j.Version++
	j.State = "running"
	j.Reason = ""
	j.LeaseUntil = now.Add(lease)
	return j, j.Validate()
}
func (j GitHeadScan) Finish(more bool, now time.Time) GitHeadScan {
	j.State = "waiting"
	j.Reason = ""
	j.LeaseUntil = time.Time{}
	j.NextAttempt = now
	if more {
		j.Page++
	} else {
		j.Page = 1
		j.State = "completed"
		j.NextAttempt = now.Add(5 * time.Minute)
	}
	return j
}

// ValidateFor prevents publication of an indexed flag without its complete
// parent comparisons and ancestor queue. Adapters share this invariant.
func (p GitScanFinish) ValidateFor(old GitScanJob) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if err := old.AcceptFinish(p.Job); err != nil {
		return err
	}
	if old.Indexed || !p.Job.Indexed {
		if len(p.Deltas) != 0 || len(p.Parents) != 0 {
			return ErrIntegrity
		}
		return nil
	}
	if len(p.Deltas) == 0 {
		return ErrIntegrity
	}
	parents := p.Deltas[0].Delta.Parents
	want := len(parents)
	if want == 0 {
		want = 1
	}
	if len(p.Deltas) != want || len(p.Parents) != len(parents) {
		return ErrIntegrity
	}
	comparisons := map[string]bool{}
	queued := map[string]bool{}
	for _, d := range p.Deltas {
		if gitEvidenceKey(d.Delta.Parents) != gitEvidenceKey(parents) || comparisons[d.Delta.Parent] {
			return ErrIntegrity
		}
		comparisons[d.Delta.Parent] = true
	}
	for _, j := range p.Parents {
		if queued[j.Commit] {
			return ErrIntegrity
		}
		queued[j.Commit] = true
	}
	for _, parent := range parents {
		if !queued[parent] || !comparisons[parent] {
			return ErrIntegrity
		}
	}
	return nil
}

func (j GitHeadScan) Defer(now time.Time) GitHeadScan {
	j.State = "retrying"
	j.Reason = "temporary_provider_or_storage_failure"
	j.LeaseUntil = time.Time{}
	j.NextAttempt = now.Add(2 * time.Minute)
	return j
}
