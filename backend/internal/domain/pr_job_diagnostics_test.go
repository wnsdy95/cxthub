package domain

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func diagnosticJob() PRPromotionJob {
	repo := HashContent([]byte("PR diagnostics"))
	pr := PullRequestMerge{Number: 352, BaseBranch: "main", HeadBranch: "feature", HeadSHA: strings.Repeat("a", 40), MergeSHA: strings.Repeat("b", 40)}
	now := time.Unix(1000, 0).UTC()
	return PRPromotionJob{ID: PRPromotionID(repo, pr.Number), RepoID: repo, PR: pr, State: "waiting", CreatedAt: now, UpdatedAt: now, NextAttempt: now}
}

func TestPRJobDiagnosticsLegacyStartsAtFirstObservedTransition(t *testing.T) {
	legacy := diagnosticJob()
	legacy.Attempts, legacy.Version, legacy.Reason = 19, 23, "integrity_check_failed"
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "diagnostics") || strings.Contains(string(raw), "failure_class") {
		t.Fatal("optional fields present on legacy job")
	}
	var read PRPromotionJob
	if err := json.Unmarshal(raw, &read); err != nil {
		t.Fatal(err)
	}
	if err := read.Validate(); err != nil || read.Diagnostics != nil {
		t.Fatalf("legacy validation: %v", err)
	}
	now := legacy.CreatedAt.Add(time.Hour)
	claimed := read
	claimed.State, claimed.Attempts, claimed.Version = "running", read.Attempts+1, read.Version+1
	claimed.UpdatedAt = now
	claimed = AppendPRJobDiagnostic(read, claimed, "claimed", now)
	d := claimed.Diagnostics
	if !d.Since.Equal(now) || d.TotalClaims != 1 || d.Dropped != 0 || len(d.Events) != 1 || d.Events[0].Reason != "" || d.Events[0].Attempt != 20 {
		t.Fatal("legacy history or lifetime claims were inferred")
	}
}

func TestPRJobDiagnosticAppendOwnsStoredTrail(t *testing.T) {
	current := diagnosticJob()
	current.State, current.Version, current.Attempts = "running", 1, 1
	elapsed := int64(12)
	current.Diagnostics = &PRJobDiagnostics{Since: current.CreatedAt, TotalClaims: 1, Events: []PRJobDiagnostic{
		{Kind: "finished", At: current.CreatedAt, Version: 0, Attempt: 1, State: "retrying", Reason: "temporary_failure", FailureClass: "canceled", ElapsedMS: &elapsed},
	}}
	next := current
	next.State, next.Reason, next.FailureClass = "completed", "", ""
	next.Diagnostics = &PRJobDiagnostics{TotalClaims: 9999}
	next = AppendPRJobDiagnostic(current, next, "finished", current.UpdatedAt.Add(time.Second))
	if next.Diagnostics == current.Diagnostics || next.Diagnostics.TotalClaims != 1 || len(next.Diagnostics.Events) != 2 || next.Diagnostics.Events[0].Reason != "temporary_failure" {
		t.Fatal("append accepted replacement diagnostics or lost stored history")
	}
	*next.Diagnostics.Events[0].ElapsedMS = 400
	next.Diagnostics.Events[0].Reason = "invalid_request"
	next.Diagnostics.TotalClaims = 500
	if elapsed != 12 || current.Diagnostics.Events[0].Reason != "temporary_failure" || current.Diagnostics.TotalClaims != 1 {
		t.Fatal("append aliases old diagnostics")
	}
}

func TestPRJobDiagnosticsBoundAndObservedClaims(t *testing.T) {
	j := diagnosticJob()
	j = AppendPRJobDiagnostic(PRPromotionJob{}, j, "queued", j.CreatedAt)
	for i := 0; i < 80; i++ {
		old := j
		j.State, j.Attempts, j.Version = "running", j.Attempts+1, j.Version+1
		j.UpdatedAt = j.UpdatedAt.Add(time.Second)
		j = AppendPRJobDiagnostic(old, j, "claimed", j.UpdatedAt)
		old = j
		j.State, j.Reason = "retrying", "temporary_failure"
		j.UpdatedAt = j.UpdatedAt.Add(time.Second)
		j = AppendPRJobDiagnostic(old, j, "finished", j.UpdatedAt)
		if err := j.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	d := j.Diagnostics
	if len(d.Events) != 32 || d.TotalClaims != 80 || d.Dropped != 129 || !d.Since.Equal(j.CreatedAt) || d.Events[0].Attempt != 65 || d.Events[31].Attempt != 80 {
		t.Fatal("bounded trail lost observed counters, start time, or newest events")
	}
}

func TestPRJobDiagnosticsElapsedClampsAndJSONNames(t *testing.T) {
	current := diagnosticJob()
	current.State, current.Version, current.Attempts = "running", 1, 1
	finish := current
	finish.State, finish.Reason, finish.FailureClass = "retrying", "temporary_failure", "deadline_exceeded"
	finish = AppendPRJobDiagnostic(current, finish, "finished", current.UpdatedAt.Add(-time.Minute))
	if err := finish.Validate(); err != nil {
		t.Fatal(err)
	}
	if finish.Diagnostics.Events[0].ElapsedMS == nil || *finish.Diagnostics.Events[0].ElapsedMS != 0 {
		t.Fatal("negative elapsed was not recorded as zero")
	}
	raw, err := json.Marshal(finish.Diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields) != 4 || fields["since"] == nil || fields["total_claims"] == nil || fields["dropped"] == nil || fields["events"] == nil {
		t.Fatal("diagnostics JSON field names differ from contract")
	}
	var events []map[string]json.RawMessage
	if err := json.Unmarshal(fields["events"], &events); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"kind", "at", "version", "attempt", "state", "reason", "failure_class", "elapsed_ms"} {
		if events[0][key] == nil {
			t.Fatalf("missing event field %s", key)
		}
	}
	if string(events[0]["elapsed_ms"]) != "0" || len(events[0]) != 8 {
		t.Fatal("event JSON contract differs")
	}
}

func TestPRJobDiagnosticsRejectUnsafeValuesAndBounds(t *testing.T) {
	base := diagnosticJob()
	base = AppendPRJobDiagnostic(PRPromotionJob{}, base, "queued", base.CreatedAt)
	for _, class := range []string{"", "deadline_exceeded", "canceled", "unspecified"} {
		j := base
		j.FailureClass = class
		if err := j.Validate(); err != nil {
			t.Fatalf("safe failure class rejected: %v", err)
		}
	}
	for _, reason := range []string{"", "source_context_pending", "source_finalization_required", "integrity_check_failed", "policy_changed", "invalid_request", "identity_or_history_conflict", "repository_or_base_missing", "temporary_failure", "retry_limit_reached"} {
		j := base
		j.Reason = reason
		if err := j.Validate(); err != nil {
			t.Fatalf("known reason rejected: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*PRPromotionJob)
	}{
		{"top failure", func(j *PRPromotionJob) { j.FailureClass = "raw-secret-error" }},
		{"top reason", func(j *PRPromotionJob) { j.Reason = "raw-secret-error" }},
		{"diagnostic only reason", func(j *PRPromotionJob) { j.Reason = "lease_expired" }},
		{"event kind", func(j *PRPromotionJob) { j.Diagnostics.Events[0].Kind = "raw-secret-error" }},
		{"event state", func(j *PRPromotionJob) { j.Diagnostics.Events[0].State = "raw-secret-error" }},
		{"event reason", func(j *PRPromotionJob) { j.Diagnostics.Events[0].Reason = "raw-secret-error" }},
		{"event failure", func(j *PRPromotionJob) { j.Diagnostics.Events[0].FailureClass = "raw-secret-error" }},
		{"negative elapsed", func(j *PRPromotionJob) {
			n := int64(-1)
			j.Diagnostics.Events[0].Kind, j.Diagnostics.Events[0].ElapsedMS = "finished", &n
		}},
		{"non finish elapsed", func(j *PRPromotionJob) { n := int64(0); j.Diagnostics.Events[0].ElapsedMS = &n }},
		{"zero since", func(j *PRPromotionJob) { j.Diagnostics.Since = time.Time{} }},
		{"zero at", func(j *PRPromotionJob) { j.Diagnostics.Events[0].At = time.Time{} }},
		{"negative version", func(j *PRPromotionJob) { j.Diagnostics.Events[0].Version = -1 }},
		{"negative attempt", func(j *PRPromotionJob) { j.Diagnostics.Events[0].Attempt = -1 }},
		{"empty trail", func(j *PRPromotionJob) { j.Diagnostics.Events = nil }},
		{"too many events", func(j *PRPromotionJob) { j.Diagnostics.Events = make([]PRJobDiagnostic, 33) }},
		{"missing claim count", func(j *PRPromotionJob) { j.Diagnostics.Events[0].Kind = "claimed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := base
			d := *base.Diagnostics
			d.Events = append([]PRJobDiagnostic(nil), d.Events...)
			j.Diagnostics = &d
			tc.edit(&j)
			if err := j.Validate(); !errors.Is(err, ErrValidation) {
				t.Fatal("unsafe diagnostics accepted")
			} else if strings.Contains(err.Error(), "raw-secret-error") {
				t.Fatal("validation echoed unsafe input")
			}
		})
	}
	if base.Diagnostics.Events[0].Kind != "queued" {
		t.Fatal("validation fixture mutated")
	}
}

func TestPRJobDiagnosticsCountersSaturate(t *testing.T) {
	j := diagnosticJob()
	j = AppendPRJobDiagnostic(PRPromotionJob{}, j, "queued", j.CreatedAt)
	for i := 1; i < MaxPRJobDiagnosticEvents; i++ {
		j = AppendPRJobDiagnostic(j, j, "queued", j.CreatedAt)
	}
	j.Diagnostics.TotalClaims, j.Diagnostics.Dropped = ^uint64(0), ^uint64(0)
	next := j
	next.State, next.Attempts, next.Version = "running", 1, 1
	next = AppendPRJobDiagnostic(j, next, "claimed", j.CreatedAt.Add(time.Second))
	if next.Diagnostics.TotalClaims != ^uint64(0) || next.Diagnostics.Dropped != ^uint64(0) || len(next.Diagnostics.Events) != 32 {
		t.Fatal("diagnostic counters wrapped")
	}
	if err := next.Validate(); err != nil {
		t.Fatal(err)
	}
}
