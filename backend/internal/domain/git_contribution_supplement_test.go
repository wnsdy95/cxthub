package domain

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func gitContributionSupplementFixture() GitContributionSupplement {
	contribution := gitContributionFixture()
	return (GitContributionSupplement{RepoID: contribution.RepoID, GitOrigin: contribution.GitOrigin,
		ContributionID: contribution.ID, PublicationID: strings.Repeat("a", 32), ProofID: strings.Repeat("b", 64),
		State: "waiting", NextAttempt: time.Unix(100, 0).UTC()}).WithID()
}

func TestGitContributionSupplementIdentityUsesOnlyPublicationPair(t *testing.T) {
	initial := gitContributionSupplementFixture()
	if initial.Validate() != nil || initial.WithID() != initial || ValidateContentHash(ContentHash("sha256:"+initial.ID)) != nil {
		t.Fatal("invalid or unstable initial job", initial)
	}
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContributionSupplement)
	}{
		{"repo", func(job *GitContributionSupplement) { job.RepoID = HashContent([]byte("other supplement repository")) }},
		{"contribution", func(job *GitContributionSupplement) { job.ContributionID = strings.Repeat("c", 64) }},
		{"publication", func(job *GitContributionSupplement) { job.PublicationID = strings.Repeat("c", 32) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			changed := initial
			scenario.mutate(&changed)
			if !errors.Is(changed.Validate(), ErrValidation) {
				t.Fatal("changed immutable pair retained its canonical ID")
			}
			changed = changed.WithID()
			if changed.ID == initial.ID || changed.Validate() != nil {
				t.Fatal("different pair did not receive its own identity", changed)
			}
		})
	}
	claimed := initial
	claimed.Version, claimed.Attempts, claimed.State = 7, 3, "processing"
	claimed.LeaseUntil = initial.NextAttempt.Add(time.Minute)
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContributionSupplement)
	}{
		{"proof", func(job *GitContributionSupplement) { job.ProofID = strings.Repeat("c", 64) }},
		{"origin", func(job *GitContributionSupplement) { job.GitOrigin = "https://github.com/example/another" }},
		{"version", func(job *GitContributionSupplement) { job.Version++ }},
		{"attempts", func(job *GitContributionSupplement) { job.Attempts++ }},
		{"lease", func(job *GitContributionSupplement) { job.LeaseUntil = job.LeaseUntil.Add(time.Minute) }},
		{"schedule", func(job *GitContributionSupplement) { job.NextAttempt = job.NextAttempt.Add(time.Hour) }},
		{"reason", func(job *GitContributionSupplement) { job.Reason = "temporary_failure" }},
		{"completed", func(job *GitContributionSupplement) { job.State, job.LeaseUntil = "completed", time.Time{} }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			changed := claimed
			scenario.mutate(&changed)
			if changed.ID != initial.ID || changed.WithID().ID != initial.ID || changed.Validate() != nil {
				t.Fatal("operational metadata changed pair identity", changed)
			}
		})
	}
	tampered := initial
	tampered.ID = "untrusted prior key"
	if tampered.WithID() != initial || tampered.ID != "untrusted prior key" {
		t.Fatal("WithID used its prior key or mutated its receiver")
	}
}

func TestGitContributionSupplementValidLifecycle(t *testing.T) {
	initial := gitContributionSupplementFixture()
	if initial.Version != 0 || initial.Attempts != 0 || initial.Validate() != nil {
		t.Fatal("initial unclaimed waiting job must be valid")
	}
	for _, state := range []string{"processing", "waiting", "retrying", "completed", "attention"} {
		t.Run(state, func(t *testing.T) {
			job := initial
			job.Version, job.Attempts, job.State = 1, 1, state
			if state == "processing" {
				job.LeaseUntil = job.NextAttempt.Add(time.Minute)
			}
			if job.Validate() != nil || job.WithID().ID != initial.ID {
				t.Fatal("valid lifecycle state rejected", job)
			}
		})
	}
	for _, reason := range []string{"", "evidence_pending", "authorization_or_state_changed", "integrity_check_failed", "projection_limit", "temporary_failure"} {
		job := initial
		job.Version, job.Attempts, job.State, job.Reason = 1, 1, "retrying", reason
		if err := job.Validate(); err != nil {
			t.Fatalf("known reason %q rejected: %v", reason, err)
		}
	}
}

func TestGitContributionSupplementRejectsInvalidReferences(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContributionSupplement)
	}{
		{"missing repo", func(job *GitContributionSupplement) { job.RepoID = "" }},
		{"malformed repo", func(job *GitContributionSupplement) { job.RepoID = "sha256:../repo" }},
		{"uppercase repo", func(job *GitContributionSupplement) { job.RepoID = ContentHash("sha256:" + strings.Repeat("A", 64)) }},
		{"missing origin", func(job *GitContributionSupplement) { job.GitOrigin = "" }},
		{"blank origin", func(job *GitContributionSupplement) { job.GitOrigin = " \t" }},
		{"padded origin", func(job *GitContributionSupplement) { job.GitOrigin += " " }},
		{"control origin", func(job *GitContributionSupplement) { job.GitOrigin += "\x00" }},
		{"newline origin", func(job *GitContributionSupplement) { job.GitOrigin += "\n" }},
		{"nonutf8 origin", func(job *GitContributionSupplement) { job.GitOrigin = string([]byte{0xff}) }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			job := gitContributionSupplementFixture()
			scenario.mutate(&job)
			if err := job.WithID().Validate(); !errors.Is(err, ErrValidation) {
				t.Fatal("invalid source scope accepted after rehash", err)
			}
		})
	}
	for _, field := range []string{"contribution", "proof", "publication"} {
		t.Run(field, func(t *testing.T) {
			width := 64
			if field == "publication" {
				width = 32
			}
			for _, invalid := range []string{"", strings.Repeat("a", width-1), strings.Repeat("a", width+1), strings.Repeat("A", width), strings.Repeat("g", width), "sha256:" + strings.Repeat("a", width), "../outside"} {
				job := gitContributionSupplementFixture()
				switch field {
				case "contribution":
					job.ContributionID = invalid
				case "proof":
					job.ProofID = invalid
				case "publication":
					job.PublicationID = invalid
				}
				if err := job.WithID().Validate(); !errors.Is(err, ErrValidation) {
					t.Fatalf("noncanonical %s %q accepted: %v", field, invalid, err)
				}
			}
		})
	}
	initial := gitContributionSupplementFixture()
	for _, invalid := range []string{"", strings.Repeat("f", 64), strings.ToUpper(initial.ID), "sha256:" + initial.ID, "../outside"} {
		job := initial
		job.ID = invalid
		if !errors.Is(job.Validate(), ErrValidation) {
			t.Fatalf("tampered canonical job ID %q accepted", invalid)
		}
	}
}

func TestGitContributionSupplementRejectsInvalidLifecycle(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*GitContributionSupplement)
	}{
		{"negative version", func(job *GitContributionSupplement) { job.Version = -1 }},
		{"negative attempts", func(job *GitContributionSupplement) { job.Attempts = -1 }},
		{"version without attempts", func(job *GitContributionSupplement) { job.Version = 1 }},
		{"attempts without version", func(job *GitContributionSupplement) { job.Attempts = 1 }},
		{"missing next attempt", func(job *GitContributionSupplement) { job.NextAttempt = time.Time{} }},
		{"initial processing", func(job *GitContributionSupplement) {
			job.State, job.LeaseUntil = "processing", job.NextAttempt.Add(time.Minute)
		}},
		{"initial retrying", func(job *GitContributionSupplement) { job.State = "retrying" }},
		{"initial completed", func(job *GitContributionSupplement) { job.State = "completed" }},
		{"initial attention", func(job *GitContributionSupplement) { job.State = "attention" }},
		{"initial reason", func(job *GitContributionSupplement) { job.Reason = "evidence_pending" }},
		{"initial lease", func(job *GitContributionSupplement) { job.LeaseUntil = job.NextAttempt.Add(time.Minute) }},
		{"missing state", func(job *GitContributionSupplement) { job.State = "" }},
		{"unknown state", func(job *GitContributionSupplement) { job.State = "running" }},
		{"unknown reason", func(job *GitContributionSupplement) {
			job.Version, job.Attempts, job.Reason = 1, 1, "provider_scope_changed"
		}},
		{"processing without lease", func(job *GitContributionSupplement) { job.Version, job.Attempts, job.State = 1, 1, "processing" }},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			job := gitContributionSupplementFixture()
			scenario.mutate(&job)
			if !errors.Is(job.Validate(), ErrValidation) {
				t.Fatal("invalid lifecycle accepted", job)
			}
		})
	}
	for _, state := range []string{"waiting", "retrying", "completed", "attention"} {
		job := gitContributionSupplementFixture()
		job.Version, job.Attempts, job.State = 1, 1, state
		job.LeaseUntil = job.NextAttempt.Add(time.Minute)
		if !errors.Is(job.Validate(), ErrValidation) {
			t.Fatalf("settled state %q retained processing lease", state)
		}
	}
}

func TestGitContributionSupplementMaxCountersRoundTrip(t *testing.T) {
	job := gitContributionSupplementFixture()
	job.Version, job.Attempts, job.State = math.MaxInt64, math.MaxInt, "processing"
	job.LeaseUntil = job.NextAttempt.Add(time.Minute)
	if err := job.Validate(); err != nil {
		t.Fatal("representable counter boundary rejected", err)
	}
	raw, err := json.Marshal(job)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"version":"9223372036854775807"`) {
		t.Fatal("version lost string-encoded precision", string(raw))
	}
	var decoded GitContributionSupplement
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != job || decoded.Validate() != nil {
		t.Fatal("job round trip lost canonical identity or counter precision", decoded, err)
	}
}
