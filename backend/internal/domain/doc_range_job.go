package domain

import (
	"errors"
	"fmt"
	"time"
)

var ErrDocRangeIndexPending = errors.New("document range index is being prepared")
var ErrDocByteRangeBackfillRequired = fmt.Errorf("%w: legacy byte-range backfill is required", ErrDocRangeIndexPending)

const MaxPendingDocRangeJobs = 32

type DocRangeIndexJob struct {
	RepositoryEpoch string `json:"-"`
	RepoID          ContentHash
	DocHash         ContentHash
	BaseHash        ContentHash
	State           string
	Reason          string
	Attempts        int
	Version         int64
	CreatedAt       time.Time
	NextAttempt     time.Time
	LeaseUntil      time.Time
}

func (job DocRangeIndexJob) Validate() error {
	if err := ValidateContentHash(job.RepoID); err != nil {
		return err
	}
	if err := ValidateContentHash(job.DocHash); err != nil {
		return err
	}
	if job.BaseHash != "" {
		if err := ValidateContentHash(job.BaseHash); err != nil {
			return err
		}
	}
	if job.Attempts < 0 || job.Version < 0 || job.CreatedAt.IsZero() || job.NextAttempt.IsZero() {
		return ErrValidation
	}
	switch job.State {
	case "waiting", "running", "retrying", "completed", "rejected":
	default:
		return ErrValidation
	}
	return nil
}
