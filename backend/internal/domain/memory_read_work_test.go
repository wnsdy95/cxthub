package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func memoryReadWorkTargetFixture() MemoryReadTarget {
	return MemoryReadTarget{
		Version: MemoryReadWorkVersion,
		Rules:   MemoryReadRules,
		RepoID:  ContentHash("sha256:" + strings.Repeat("a", 64)),
		Selection: EffectiveMemorySelection{
			SnapshotID: ContentHash("sha256:" + strings.Repeat("b", 64)),
			CodeCommit: strings.Repeat("c", 40),
		},
	}
}

func memoryReadWorkFixture(test *testing.T) MemoryReadWork {
	test.Helper()
	target := memoryReadWorkTargetFixture()
	identifier, err := MemoryReadTargetID(context.Background(), target)
	if err != nil {
		test.Fatal(err)
	}
	created := time.Date(2026, time.October, 19, 1, 0, 0, 0, time.UTC)
	return MemoryReadWork{
		ID: identifier, Target: target,
		Source: RepositoryMemorySourceGeneration{
			Version: RepositoryMemorySourceVersion,
			Graph: RepositoryViewSourceCheckpoint{
				Version: RepositoryViewSourceVersion, Scope: RepositoryGraphSourceScope,
				RepoID: target.RepoID, Epoch: "12345678-1234-4321-9876-123456789abc", Sequence: 7,
			},
			Epoch: "23456789-1234-4321-9876-123456789abc", Sequence: 11,
		},
		State: "waiting", Version: 1,
		CreatedAt: created, UpdatedAt: created, NextAttempt: created,
	}
}

func TestMemoryReadTargetCanonicalIdentity(test *testing.T) {
	target := memoryReadWorkTargetFixture()
	want := `{"version":1,"rules":1,"repo_id":"sha256:` + strings.Repeat("a", 64) +
		`","selection":{"snapshot_id":"sha256:` + strings.Repeat("b", 64) +
		`","code_commit":"` + strings.Repeat("c", 40) + `"},"content":""}`
	raw, err := MemoryReadTargetBytes(context.Background(), target)
	if err != nil || string(raw) != want {
		test.Fatalf("canonical target bytes = %s, %v; want %s", raw, err, want)
	}
	identifier, err := MemoryReadTargetID(context.Background(), target)
	if err != nil {
		test.Fatal(err)
	}
	wantID := HashContent(append([]byte("cxthub:memory-read-target:v1\x00"), []byte(want)...))
	if identifier != wantID || identifier == HashContent(raw) {
		test.Fatalf("target identity lost canonical bytes or domain separation: %s", identifier)
	}
	var decoded MemoryReadTarget
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded != target {
		test.Fatalf("target round trip changed selection: %+v, %v", decoded, err)
	}
	raw[0] = '!'
	repeated, err := MemoryReadTargetBytes(context.Background(), decoded)
	if err != nil || string(repeated) != want {
		test.Fatalf("caller mutated subsequent target encoding: %s, %v", repeated, err)
	}
	repeatedID, err := MemoryReadTargetID(context.Background(), decoded)
	if err != nil || repeatedID != identifier {
		test.Fatalf("repeated target identity changed: %s, %v", repeatedID, err)
	}
}

func TestMemoryReadTargetIdentityBindsEverySelectionDimension(test *testing.T) {
	baseline := memoryReadWorkTargetFixture()
	baselineID, err := MemoryReadTargetID(context.Background(), baseline)
	if err != nil {
		test.Fatal(err)
	}
	seen := map[ContentHash]string{baselineID: "baseline"}
	for _, scenario := range []struct {
		name   string
		mutate func(*MemoryReadTarget)
	}{
		{name: "repository", mutate: func(target *MemoryReadTarget) { target.RepoID = HashContent([]byte("other repository")) }},
		{name: "snapshot", mutate: func(target *MemoryReadTarget) { target.Selection.SnapshotID = HashContent([]byte("other snapshot")) }},
		{name: "code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = strings.Repeat("d", 40) }},
		{name: "sha256 code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = strings.Repeat("c", 64) }},
		{name: "branch", mutate: func(target *MemoryReadTarget) { target.Selection.Branch = "main" }},
		{name: "other branch", mutate: func(target *MemoryReadTarget) { target.Selection.Branch = "release/main" }},
		{name: "branch case", mutate: func(target *MemoryReadTarget) { target.Selection.Branch = "Main" }},
		{name: "original memory", mutate: func(target *MemoryReadTarget) { target.Selection.MemoryHash = HashContent([]byte("original memory")) }},
		{name: "other original memory", mutate: func(target *MemoryReadTarget) {
			target.Selection.MemoryHash = HashContent([]byte("other original memory"))
		}},
		{name: "claims", mutate: func(target *MemoryReadTarget) { target.Content = "claims" }},
		{name: "prompt", mutate: func(target *MemoryReadTarget) { target.Content = "prompt" }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			target := baseline
			scenario.mutate(&target)
			identifier, err := MemoryReadTargetID(context.Background(), target)
			if err != nil {
				test.Fatal(err)
			}
			if previous, exists := seen[identifier]; exists {
				test.Fatalf("distinct selection aliases %s: %s", previous, identifier)
			}
			seen[identifier] = scenario.name
		})
	}
}

func TestMemoryReadTargetRejectsInvalidContracts(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*MemoryReadTarget)
	}{
		{name: "zero version", mutate: func(target *MemoryReadTarget) { target.Version = 0 }},
		{name: "future version", mutate: func(target *MemoryReadTarget) { target.Version++ }},
		{name: "zero rules", mutate: func(target *MemoryReadTarget) { target.Rules = 0 }},
		{name: "future rules", mutate: func(target *MemoryReadTarget) { target.Rules++ }},
		{name: "missing repository", mutate: func(target *MemoryReadTarget) { target.RepoID = "" }},
		{name: "malformed repository", mutate: func(target *MemoryReadTarget) { target.RepoID = "foreign" }},
		{name: "uppercase hash", mutate: func(target *MemoryReadTarget) { target.RepoID = ContentHash("sha256:" + strings.Repeat("A", 64)) }},
		{name: "missing snapshot", mutate: func(target *MemoryReadTarget) { target.Selection.SnapshotID = "" }},
		{name: "malformed snapshot", mutate: func(target *MemoryReadTarget) { target.Selection.SnapshotID = "snapshot" }},
		{name: "missing code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = "" }},
		{name: "abbreviated code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = "abcdef0" }},
		{name: "zero code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = strings.Repeat("0", 40) }},
		{name: "uppercase code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = strings.Repeat("A", 40) }},
		{name: "nonhex code commit", mutate: func(target *MemoryReadTarget) { target.Selection.CodeCommit = strings.Repeat("g", 40) }},
		{name: "malformed memory", mutate: func(target *MemoryReadTarget) { target.Selection.MemoryHash = "memory" }},
		{name: "branch plus original memory", mutate: func(target *MemoryReadTarget) {
			target.Selection.Branch = "main"
			target.Selection.MemoryHash = HashContent([]byte("original memory"))
		}},
		{name: "branch traversal", mutate: func(target *MemoryReadTarget) { target.Selection.Branch = "refs/../main" }},
		{name: "branch control character", mutate: func(target *MemoryReadTarget) { target.Selection.Branch = "main\x00" }},
		{name: "content all alias", mutate: func(target *MemoryReadTarget) { target.Content = "all" }},
		{name: "content whitespace", mutate: func(target *MemoryReadTarget) { target.Content = " claims" }},
		{name: "content case", mutate: func(target *MemoryReadTarget) { target.Content = "Claims" }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			target := memoryReadWorkTargetFixture()
			scenario.mutate(&target)
			if err := target.Validate(); !errors.Is(err, ErrValidation) {
				test.Fatalf("target validation = %v; want validation error", err)
			}
			if raw, err := MemoryReadTargetBytes(context.Background(), target); !errors.Is(err, ErrValidation) || len(raw) != 0 {
				test.Fatalf("invalid target serialized: %s, %v", raw, err)
			}
			if identifier, err := MemoryReadTargetID(context.Background(), target); !errors.Is(err, ErrValidation) || identifier != "" {
				test.Fatalf("invalid target received identity: %s, %v", identifier, err)
			}
		})
	}
}

func TestMemoryReadTargetRejectsLossyUTF8(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		branch string
	}{
		{name: "invalid leading byte", branch: "feature/\xff"},
		{name: "colliding invalid byte", branch: "feature/\xfe"},
		{name: "overlong encoding", branch: "feature/\xc0\xaf"},
		{name: "surrogate encoding", branch: "feature/\xed\xa0\x80"},
		{name: "truncated encoding", branch: "feature/\xe2\x82"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			target := memoryReadWorkTargetFixture()
			target.Selection.Branch = scenario.branch
			if err := target.Validate(); err == nil {
				test.Errorf("accepted invalid UTF-8 branch %q", scenario.branch)
			}
			if raw, err := MemoryReadTargetBytes(context.Background(), target); err == nil || len(raw) != 0 {
				test.Errorf("lossy canonical target bytes for %q: %s, %v", scenario.branch, raw, err)
			}
			if identifier, err := MemoryReadTargetID(context.Background(), target); err == nil || identifier != "" {
				test.Errorf("lossy target received an identity for %q: %s, %v", scenario.branch, identifier, err)
			}
			raw, err := json.Marshal(target)
			if err != nil {
				test.Fatal(err)
			}
			work := memoryReadWorkFixture(test)
			work.Target = target
			work.ID = HashContent(append([]byte("cxthub:memory-read-target:v1\x00"), raw...))
			if err := work.Validate(); !errors.Is(err, ErrIntegrity) {
				test.Errorf("lossy target passed work validation: %v", err)
			}
		})
	}
}

func TestMemoryReadTargetBranchByteBoundsAndEscaping(test *testing.T) {
	for _, branch := range []string{strings.Repeat("a", 1024), strings.Repeat("é", 512), strings.Repeat("&", 1024), "feature/한글-😀-\ufffd", "feature/\"quoted\""} {
		target := memoryReadWorkTargetFixture()
		target.Selection.Branch = branch
		raw, err := MemoryReadTargetBytes(context.Background(), target)
		if err != nil || len(raw) > MaxMemoryReadTargetBytes || !json.Valid(raw) {
			test.Fatalf("valid bounded target failed encoding (%d branch bytes): %v", len(branch), err)
		}
		var decoded MemoryReadTarget
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded != target {
			test.Fatalf("escaping lost exact branch bytes: %+v, %v", decoded, err)
		}
		canonical, err := MemoryReadTargetBytes(context.Background(), decoded)
		if err != nil || !bytes.Equal(raw, canonical) {
			test.Fatalf("escaped target is not stable: %v", err)
		}
	}
	for _, branch := range []string{strings.Repeat("a", 1025), strings.Repeat("é", 512) + "a", strings.Repeat("a", MaxRepositoryViewSourceKeyBytes+1), strings.Repeat("&", MaxMemoryReadTargetBytes)} {
		target := memoryReadWorkTargetFixture()
		target.Selection.Branch = branch
		if err := target.Validate(); err == nil {
			test.Errorf("accepted oversized branch: %d bytes", len(branch))
		}
		if raw, err := MemoryReadTargetBytes(context.Background(), target); err == nil || len(raw) != 0 {
			test.Errorf("oversized branch produced target bytes: %d, %v", len(raw), err)
		}
		if identifier, err := MemoryReadTargetID(context.Background(), target); err == nil || identifier != "" {
			test.Errorf("oversized branch received identity: %s, %v", identifier, err)
		}
	}
}

func TestMemoryReadTargetCancellation(test *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, release := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer release()
	for _, current := range []context.Context{canceled, expired} {
		for _, target := range []MemoryReadTarget{memoryReadWorkTargetFixture(), {}} {
			if raw, err := MemoryReadTargetBytes(current, target); !errors.Is(err, current.Err()) || len(raw) != 0 {
				test.Fatalf("canceled target serialization = %s, %v; want %v", raw, err, current.Err())
			}
			if identifier, err := MemoryReadTargetID(current, target); !errors.Is(err, current.Err()) || identifier != "" {
				test.Fatalf("canceled target identity = %s, %v; want %v", identifier, err, current.Err())
			}
		}
	}
}

func TestMemoryReadWorkValidStatesAndOlderOutput(test *testing.T) {
	for _, scenario := range []struct {
		state    string
		reason   string
		attempts int
	}{
		{state: "waiting"},
		{state: "running", attempts: 1},
		{state: "retrying", reason: "temporary_failure", attempts: 1},
		{state: "completed", attempts: 1},
		{state: "rejected", reason: "attempts_exhausted", attempts: MaxMemoryReadWorkAttempts},
	} {
		test.Run(scenario.state, func(test *testing.T) {
			work := memoryReadWorkFixture(test)
			work.State, work.Reason, work.Attempts = scenario.state, scenario.reason, scenario.attempts
			if work.State == "running" {
				work.LeaseUntil = work.UpdatedAt.Add(time.Minute)
			}
			qualification := HashContent([]byte("previously qualified output"))
			if work.State == "completed" {
				work.QualificationID = qualification
			}
			if err := work.Validate(); err != nil {
				test.Fatalf("valid %s work rejected: %v", work.State, err)
			}
			work.QualificationID = qualification
			if err := work.Validate(); err != nil {
				test.Fatalf("work state must remain separate from older qualified output: %v", err)
			}
		})
	}
}

func TestMemoryReadWorkRejectsBindingAndFenceCorruption(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		mutate func(*MemoryReadWork)
	}{
		{name: "missing target identity", mutate: func(work *MemoryReadWork) { work.ID = "" }},
		{name: "malformed target identity", mutate: func(work *MemoryReadWork) { work.ID = "invalid" }},
		{name: "other target identity", mutate: func(work *MemoryReadWork) { work.ID = HashContent([]byte("other target")) }},
		{name: "mutated content", mutate: func(work *MemoryReadWork) { work.Target.Content = "claims" }},
		{name: "mutated snapshot", mutate: func(work *MemoryReadWork) { work.Target.Selection.SnapshotID = HashContent([]byte("other snapshot")) }},
		{name: "mutated branch", mutate: func(work *MemoryReadWork) { work.Target.Selection.Branch = "main" }},
		{name: "mutated commit", mutate: func(work *MemoryReadWork) { work.Target.Selection.CodeCommit = strings.Repeat("e", 40) }},
		{name: "mutated memory pin", mutate: func(work *MemoryReadWork) { work.Target.Selection.MemoryHash = HashContent([]byte("other memory")) }},
		{name: "invalid target rules", mutate: func(work *MemoryReadWork) { work.Target.Rules++ }},
		{name: "invalid target version", mutate: func(work *MemoryReadWork) { work.Target.Version++ }},
		{name: "foreign repository source", mutate: func(work *MemoryReadWork) { work.Source.Graph.RepoID = HashContent([]byte("other repository")) }},
		{name: "invalid source version", mutate: func(work *MemoryReadWork) { work.Source.Version++ }},
		{name: "invalid graph version", mutate: func(work *MemoryReadWork) { work.Source.Graph.Version++ }},
		{name: "view source instead of graph", mutate: func(work *MemoryReadWork) { work.Source.Graph.Scope = RepositoryViewSourceScope }},
		{name: "empty graph epoch", mutate: func(work *MemoryReadWork) { work.Source.Graph.Epoch = "" }},
		{name: "zero graph epoch", mutate: func(work *MemoryReadWork) { work.Source.Graph.Epoch = "00000000-0000-0000-0000-000000000000" }},
		{name: "negative graph sequence", mutate: func(work *MemoryReadWork) { work.Source.Graph.Sequence = -1 }},
		{name: "empty memory epoch", mutate: func(work *MemoryReadWork) { work.Source.Epoch = "" }},
		{name: "zero memory epoch", mutate: func(work *MemoryReadWork) { work.Source.Epoch = "00000000-0000-0000-0000-000000000000" }},
		{name: "negative memory sequence", mutate: func(work *MemoryReadWork) { work.Source.Sequence = -1 }},
		{name: "negative attempts", mutate: func(work *MemoryReadWork) { work.Attempts = -1 }},
		{name: "excessive attempts", mutate: func(work *MemoryReadWork) { work.Attempts = MaxMemoryReadWorkAttempts + 1 }},
		{name: "zero fence", mutate: func(work *MemoryReadWork) { work.Version = 0 }},
		{name: "negative fence", mutate: func(work *MemoryReadWork) { work.Version = -1 }},
		{name: "invalid qualification", mutate: func(work *MemoryReadWork) { work.QualificationID = "qualification" }},
		{name: "missing created time", mutate: func(work *MemoryReadWork) { work.CreatedAt = time.Time{} }},
		{name: "missing updated time", mutate: func(work *MemoryReadWork) { work.UpdatedAt = time.Time{} }},
		{name: "missing next attempt", mutate: func(work *MemoryReadWork) { work.NextAttempt = time.Time{} }},
		{name: "missing state", mutate: func(work *MemoryReadWork) { work.State = "" }},
		{name: "unknown state", mutate: func(work *MemoryReadWork) { work.State = "published" }},
		{name: "state case", mutate: func(work *MemoryReadWork) { work.State = "Running" }},
		{name: "unknown reason", mutate: func(work *MemoryReadWork) { work.Reason = "raw backend error" }},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			work := memoryReadWorkFixture(test)
			scenario.mutate(&work)
			if err := work.Validate(); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("accepted corrupt work or wrong error: %v", err)
			}
		})
	}
}

func TestMemoryReadWorkLeaseStateInvariants(test *testing.T) {
	for _, state := range []string{"waiting", "retrying", "rejected", "completed"} {
		test.Run(state, func(test *testing.T) {
			work := memoryReadWorkFixture(test)
			work.State, work.Attempts = state, 1
			if state == "retrying" {
				work.Reason = "temporary_failure"
			}
			if state == "rejected" {
				work.Reason = "invalid_source"
			}
			work.QualificationID = HashContent([]byte("previously qualified output"))
			work.LeaseUntil = work.UpdatedAt.Add(time.Minute)
			if err := work.Validate(); !errors.Is(err, ErrIntegrity) {
				test.Fatalf("non-running work retained a lease: %v", err)
			}
		})
	}
	test.Run("running without lease", func(test *testing.T) {
		work := memoryReadWorkFixture(test)
		work.State, work.Attempts = "running", 1
		if err := work.Validate(); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("running work without lease accepted: %v", err)
		}
	})
	test.Run("running without claim attempt", func(test *testing.T) {
		work := memoryReadWorkFixture(test)
		work.State = "running"
		work.LeaseUntil = work.UpdatedAt.Add(time.Minute)
		if err := work.Validate(); !errors.Is(err, ErrIntegrity) {
			test.Fatalf("unclaimed running work accepted: %v", err)
		}
	})
}

func TestMemoryReadWorkIntegerBoundariesAndTargetStability(test *testing.T) {
	for _, fence := range []int64{1, 9007199254740993, math.MaxInt64} {
		work := memoryReadWorkFixture(test)
		work.State, work.Attempts, work.Version = "running", MaxMemoryReadWorkAttempts, fence
		work.Source.Sequence, work.Source.Graph.Sequence = math.MaxInt64, math.MaxInt64
		work.LeaseUntil = work.UpdatedAt.Add(time.Minute)
		if err := work.Validate(); err != nil {
			test.Fatalf("valid integer boundary rejected: fence %d, %v", fence, err)
		}
		identifier, err := MemoryReadTargetID(context.Background(), work.Target)
		if err != nil || identifier != work.ID {
			test.Fatalf("source/fence leaked into coalesced target identity: %s, %v", identifier, err)
		}
	}
	work := memoryReadWorkFixture(test)
	work.Source.Sequence, work.Source.Graph.Sequence = 0, 0
	if err := work.Validate(); err != nil {
		test.Fatalf("initial source generation rejected: %v", err)
	}
}

func TestMemoryReadWorkReasonAllowlist(test *testing.T) {
	for _, reason := range []string{"", "temporary_failure", "source_changed", "source_reset", "resource_limit", "invalid_source", "access_denied", "identity_upgrade_required", "retention_expired", "retention_cleanup", "attempts_exhausted", "comparison_mismatch"} {
		if !MemoryReadWorkReasonValid(reason) {
			test.Errorf("documented bounded reason rejected: %q", reason)
		}
	}
	for _, reason := range []string{"unknown", " temporary_failure", "temporary_failure ", "SOURCE_CHANGED", "source_changed\x00", "source_changed\n", strings.Repeat("failure", 4096)} {
		if MemoryReadWorkReasonValid(reason) {
			test.Errorf("unbounded or noncanonical reason accepted: %q", reason)
		}
	}
	if errors.Is(ErrMemoryReadNotReady, ErrMemoryReadNeedsAttention) || errors.Is(ErrMemoryReadNeedsAttention, ErrMemoryReadNotReady) {
		test.Fatal("pending and terminal preparation states must remain distinguishable")
	}
}
