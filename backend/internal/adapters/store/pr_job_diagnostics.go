package store

import (
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

// finishPRJob accepts only outcome fields from a worker. Identity, attempt and
// diagnostics always come from the fenced current record.
func finishPRJob(current, outcome domain.PRPromotionJob) (domain.PRPromotionJob, error) {
	if current.RepoID != outcome.RepoID || current.ID != outcome.ID || current.Version != outcome.Version ||
		current.State != "running" || current.PR != outcome.PR || current.GitOrigin != outcome.GitOrigin {
		return domain.PRPromotionJob{}, domain.ErrConflict
	}
	if outcome.State == "running" {
		return domain.PRPromotionJob{}, domain.ErrValidation
	}
	next := current
	next.State, next.Reason, next.FailureClass = outcome.State, outcome.Reason, outcome.FailureClass
	next.UpdatedAt, next.NextAttempt, next.LeaseUntil = outcome.UpdatedAt, outcome.NextAttempt, outcome.LeaseUntil
	// Validate before clearing success so arbitrary error/secret strings are
	// rejected even when a worker mistakenly sends them with a completed result.
	if err := next.Validate(); err != nil {
		return domain.PRPromotionJob{}, err
	}
	if next.State == "completed" {
		next.Reason = ""
		next.FailureClass = ""
	}
	next = domain.AppendPRJobDiagnostic(current, next, "finished", next.UpdatedAt)
	return next, next.Validate()
}

func retryPRJobUnchanged(j domain.PRPromotionJob, now time.Time) bool {
	return j.State == "completed" || (j.State == "waiting" && j.Attempts == 0 && j.Reason == "" &&
		j.FailureClass == "" && !j.NextAttempt.After(now) && j.LeaseUntil.IsZero())
}
