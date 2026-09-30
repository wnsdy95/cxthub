package domain

import "time"

const MaxPRJobDiagnosticEvents = 32

// PRJobDiagnostics records only transitions observed since Since. TotalClaims
// is not a lifetime attempt count, especially for jobs predating diagnostics.
type PRJobDiagnostics struct {
	Since       time.Time         `json:"since"`
	TotalClaims uint64            `json:"total_claims"`
	Dropped     uint64            `json:"dropped"`
	Events      []PRJobDiagnostic `json:"events"`
}

type PRJobDiagnostic struct {
	Kind         string    `json:"kind"`
	At           time.Time `json:"at"`
	Version      int64     `json:"version"`
	Attempt      int       `json:"attempt"`
	State        string    `json:"state"`
	Reason       string    `json:"reason"`
	FailureClass string    `json:"failure_class,omitempty"`
	// ElapsedMS measures this fenced claim only; it is not a latency SLA.
	ElapsedMS *int64 `json:"elapsed_ms,omitempty"`
}

func validPRJobState(state string) bool {
	switch state {
	case "waiting", "retrying", "running", "completed", "attention":
		return true
	}
	return false
}

func validPRJobFailureClass(class string) bool {
	switch class {
	case "", "deadline_exceeded", "canceled", "unspecified":
		return true
	}
	return false
}

func validPRJobReason(reason string, diagnostic bool) bool {
	switch reason {
	case "", "source_context_pending", "source_finalization_required", "integrity_check_failed",
		"policy_changed", "invalid_request", "identity_or_history_conflict", "repository_or_base_missing",
		"temporary_failure", "retry_limit_reached":
		return true
	case "lease_expired":
		return diagnostic
	}
	return false
}

func (d *PRJobDiagnostics) Validate() error {
	if d == nil {
		return nil // legacy jobs remain readable without invented history
	}
	if d.Since.IsZero() || len(d.Events) == 0 || len(d.Events) > MaxPRJobDiagnosticEvents {
		return ErrValidation
	}
	var claims uint64
	for _, e := range d.Events {
		switch e.Kind {
		case "queued", "finished", "retry_requested", "source_available":
		case "claimed":
			claims++
		default:
			return ErrValidation
		}
		if e.At.IsZero() || e.Version < 0 || e.Attempt < 0 || !validPRJobState(e.State) ||
			!validPRJobReason(e.Reason, true) || !validPRJobFailureClass(e.FailureClass) ||
			(e.ElapsedMS != nil && (e.Kind != "finished" || *e.ElapsedMS < 0)) {
			return ErrValidation
		}
	}
	if d.TotalClaims < claims {
		return ErrValidation
	}
	return nil
}

// AppendPRJobDiagnostic ignores next.Diagnostics and owns a copy of the current
// stored trail, including elapsed pointers. The store must fence current before
// accepting an outcome. No events or counters are inferred from old Attempts.
func AppendPRJobDiagnostic(current, next PRPromotionJob, kind string, at time.Time) PRPromotionJob {
	d := PRJobDiagnostics{Since: at}
	var events []PRJobDiagnostic
	if current.Diagnostics != nil {
		d = *current.Diagnostics
		events = current.Diagnostics.Events
	}
	start := max(0, len(events)+1-MaxPRJobDiagnosticEvents)
	d.Dropped = addPRJobDiagnosticCount(d.Dropped, uint64(start))
	d.Events = make([]PRJobDiagnostic, 0, MaxPRJobDiagnosticEvents)
	for _, e := range events[start:] {
		if e.ElapsedMS != nil {
			elapsed := *e.ElapsedMS
			e.ElapsedMS = &elapsed
		}
		d.Events = append(d.Events, e)
	}
	e := PRJobDiagnostic{Kind: kind, At: at, Version: next.Version, Attempt: next.Attempts,
		State: next.State, Reason: next.Reason, FailureClass: next.FailureClass}
	if kind == "claimed" {
		d.TotalClaims = addPRJobDiagnosticCount(d.TotalClaims, 1)
		e.Reason = ""
		if current.State == "running" {
			e.Reason = "lease_expired"
		}
	}
	if kind == "finished" {
		elapsed := max(int64(0), at.Sub(current.UpdatedAt).Milliseconds())
		e.ElapsedMS = &elapsed
	}
	d.Events = append(d.Events, e)
	next.Diagnostics = &d
	return next
}

func addPRJobDiagnosticCount(count, delta uint64) uint64 {
	if delta > ^uint64(0)-count {
		return ^uint64(0)
	}
	return count + delta
}
