package domain

import "time"

type GitContributionSupplement struct {
	ID             string      `json:"id"`
	RepoID         ContentHash `json:"repo_id"`
	GitOrigin      string      `json:"git_origin"`
	ContributionID string      `json:"contribution_id"`
	PublicationID  string      `json:"publication_id"`
	ProofID        string      `json:"proof_id"`
	Version        int64       `json:"version,string"`
	State          string      `json:"state"`
	Reason         string      `json:"reason,omitempty"`
	Attempts       int         `json:"attempts"`
	NextAttempt    time.Time   `json:"next_attempt"`
	LeaseUntil     time.Time   `json:"lease_until"`
}

func (job GitContributionSupplement) WithID() GitContributionSupplement {
	job.ID = gitEvidenceKey(struct {
		Version        int         `json:"version"`
		RepoID         ContentHash `json:"repo_id"`
		ContributionID string      `json:"contribution_id"`
		PublicationID  string      `json:"publication_id"`
	}{1, job.RepoID, job.ContributionID, job.PublicationID})
	return job
}

func (job GitContributionSupplement) Validate() error {
	if job.ID != job.WithID().ID || ValidateContentHash(job.RepoID) != nil || !validPRContributionIdentity(job.GitOrigin) || ValidateContentHash(ContentHash("sha256:"+job.ContributionID)) != nil || ValidateContentHash(ContentHash("sha256:"+job.ProofID)) != nil || !validAdmissionEventID(job.PublicationID) || job.Version < 0 || job.Attempts < 0 || job.NextAttempt.IsZero() {
		return ErrValidation
	}
	if (job.Version == 0) != (job.Attempts == 0) {
		return ErrValidation
	}
	switch job.State {
	case "processing":
		if job.Version < 1 || job.LeaseUntil.IsZero() {
			return ErrValidation
		}
	case "waiting", "retrying", "completed", "attention":
		if !job.LeaseUntil.IsZero() {
			return ErrValidation
		}
	default:
		return ErrValidation
	}
	if job.Version == 0 && (job.State != "waiting" || job.Reason != "") {
		return ErrValidation
	}
	switch job.Reason {
	case "", "evidence_pending", "authorization_or_state_changed", "integrity_check_failed", "projection_limit", "temporary_failure":
	default:
		return ErrValidation
	}
	return nil
}
