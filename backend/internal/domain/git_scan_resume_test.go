package domain

import (
	"strings"
	"testing"
	"time"
)

func TestGitScanResumesOnlyProviderSideStops(t *testing.T) {
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	base := NewGitScan(HashContent([]byte(t.Name())), "https://github.com/example/project", strings.Repeat("a", 40), now.Add(-59*24*time.Hour))
	base.Attempts, base.Version = 5401, 5401
	for _, scenario := range []struct {
		state, reason string
		resumed       bool
	}{
		{"attention", GitScanCommitUnavailable, true},
		{"attention", GitScanRetryLimit, true},
		{"attention", "integrity_check_failed", false},
		{"attention", "invalid_or_ambiguous_git_evidence", false},
		{"attention", "origin_or_lease_changed", false},
		{"retrying", GitScanCommitPending, false},
		{"retrying", "temporary_provider_or_storage_failure", false},
		{"waiting", "", false},
	} {
		job := base
		job.State, job.Reason = scenario.state, scenario.reason
		job.NextAttempt = now.Add(time.Hour)
		next, resumed, err := job.ResumeOnProviderEvidence(now)
		if err != nil || resumed != scenario.resumed {
			t.Fatal(scenario, resumed, err)
		}
		if !resumed {
			if next != job {
				t.Fatal("unresumed scan changed", scenario, next)
			}
			continue
		}
		if next.State != "waiting" || next.Reason != "" || next.Version != job.Version+1 || next.Attempts != job.Attempts || !next.NextAttempt.Equal(now) || !next.UpdatedAt.Equal(now) || !next.CreatedAt.Equal(job.CreatedAt) {
			t.Fatal("resumed scan lost its fence or history", scenario, next)
		}
	}
	running := base
	running.State, running.LeaseUntil = "running", now.Add(time.Minute)
	if next, resumed, err := running.ResumeOnProviderEvidence(now); err != nil || resumed || next != running {
		t.Fatal("provider evidence touched a live lease", next, resumed, err)
	}
	completed := base
	completed.State, completed.Indexed, completed.TreeIndexed = "completed", true, true
	if next, resumed, err := completed.ResumeOnProviderEvidence(now); err != nil || resumed || next != completed {
		t.Fatal("provider evidence reopened a completed scan", next, resumed, err)
	}
}
