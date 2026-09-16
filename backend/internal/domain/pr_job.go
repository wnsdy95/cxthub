package domain

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

var ErrPRSourcePending = errors.New("PR source context has not arrived")

// PRPromotionJob is delivery state; source/completion history remains immutable.
// Version fences workers whose lease expired while they were executing.
type PRPromotionJob struct {
	ID           string           `json:"id"`
	RepoID       ContentHash      `json:"repo_id"`
	PR           PullRequestMerge `json:"pr"`
	BaseBranchID string           `json:"base_branch_id"`
	GitOrigin    string           `json:"git_origin"`
	State        string           `json:"state"`
	Attempts     int              `json:"attempts"`
	Version      int64            `json:"version"`
	Reason       string           `json:"reason,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	NextAttempt  time.Time        `json:"next_attempt"`
	LeaseUntil   time.Time        `json:"lease_until"`
}

func PRPromotionID(repo ContentHash, number int) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s:pr:%d", repo, number)))
	return fmt.Sprintf("%x", h[:16])
}
func (j PRPromotionJob) Validate() error {
	if err := ValidateContentHash(j.RepoID); err != nil {
		return err
	}
	if err := j.PR.Validate(); err != nil {
		return err
	}
	if j.ID != PRPromotionID(j.RepoID, j.PR.Number) || j.CreatedAt.IsZero() {
		return ErrValidation
	}
	switch j.State {
	case "waiting", "retrying", "running", "completed", "attention":
	default:
		return ErrValidation
	}
	return nil
}
