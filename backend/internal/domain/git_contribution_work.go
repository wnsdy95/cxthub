package domain

import "time"

type GitContributionWork struct {
	RepoID           ContentHash `json:"repo_id"`
	AdmissionID      string      `json:"admission_id"`
	Version          int64       `json:"version,string"`
	AfterReadVersion int64       `json:"after_read_version,string"`
	ReadCursor       int64       `json:"read_cursor,string"`
	ReadThrough      int64       `json:"read_through,string"`
	State            string      `json:"state"`
	Reason           string      `json:"reason,omitempty"`
	Attempts         int         `json:"attempts"`
	NextAttempt      time.Time   `json:"next_attempt"`
	LeaseUntil       time.Time   `json:"lease_until"`
}

func (work GitContributionWork) ValidateRecorded() error {
	if work.Version == 0 {
		if work.Attempts != 0 || (work.State != "waiting" && work.State != "completed") || work.Reason != "" || work.ReadCursor != work.AfterReadVersion || work.ReadThrough != 0 {
			return ErrValidation
		}
		work.Version, work.Attempts = 1, 1
	}
	return work.Validate()
}

func (work GitContributionWork) Validate() error {
	if ValidateContentHash(work.RepoID) != nil || ValidateContentHash(ContentHash("sha256:"+work.AdmissionID)) != nil || work.Version < 1 || work.AfterReadVersion < 1 || work.ReadCursor < work.AfterReadVersion || (work.ReadThrough == 0 && work.ReadCursor != work.AfterReadVersion) || (work.ReadThrough != 0 && work.ReadThrough < work.ReadCursor) || work.Attempts < 1 || work.NextAttempt.IsZero() {
		return ErrValidation
	}
	switch work.State {
	case "processing":
		if work.LeaseUntil.IsZero() {
			return ErrValidation
		}
	case "waiting", "retrying", "completed", "attention":
		if !work.LeaseUntil.IsZero() {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	switch work.Reason {
	case "", "awaiting_bound_head", "evidence_pending", "range_discontinuous", "source_absent", "already_present", "evidence_limit", "integrity_check_failed", "authorization_or_state_changed", "provider_scope_changed", "temporary_failure":
	default:
		return ErrValidation
	}
	return nil
}
