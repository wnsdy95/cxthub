package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestGitContributionFailurePreservesFiniteSweep(t *testing.T) {
	now := time.Now().UTC()
	job := domain.GitContributionWork{RepoID: hh("worker repo"), AdmissionID: strings.Repeat("a", 64), Version: 1, AfterReadVersion: 3, ReadCursor: 4, ReadThrough: 8, State: "processing", Attempts: 1, NextAttempt: now, LeaseUntil: now.Add(time.Minute)}
	attempted := job
	attempted.ReadCursor = 6
	for _, scenario := range []struct {
		name          string
		err           error
		state, reason string
		cursor        int64
	}{
		{"late index", errGitContributionEvidencePending, "waiting", "evidence_pending", 6},
		{"reset range", errGitContributionRangeDiscontinuous, "waiting", "range_discontinuous", 6},
		{"absent", domain.ErrNotFound, "waiting", "source_absent", 6},
		{"already present", errGitContributionAlreadyPresent, "waiting", "already_present", 6},
		{"limit", errGitContributionEvidenceLimit, "attention", "evidence_limit", 4},
		{"integrity", domain.ErrIntegrity, "attention", "integrity_check_failed", 4},
		{"stale", domain.ErrConflict, "retrying", "authorization_or_state_changed", 4},
		{"denied", domain.ErrForbidden, "retrying", "authorization_or_state_changed", 4},
		{"canceled", context.Canceled, "retrying", "temporary_failure", 4},
		{"temporary", errors.New("offline"), "retrying", "temporary_failure", 4},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got := gitContributionFailure(job, attempted, scenario.err, now)
			if got.Validate() != nil || got.State != scenario.state || got.Reason != scenario.reason || got.ReadCursor != scenario.cursor || got.ReadThrough != job.ReadThrough || got.Version != job.Version || !got.NextAttempt.After(now) {
				t.Fatalf("invalid disposition: %+v", got)
			}
		})
	}
}
