package domain

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func legacyPRParticipationVerifier(checkpoint PRHistoryCheckpoint, history []HistoryEvent) (*PRParticipationVerifier, error) {
	if checkpoint.Validate() != nil || checkpoint.PullRequestID == 0 {
		return nil, ErrIntegrity
	}
	proofs := make(map[publicationObservationKey][]HistoryEvent)
	attachments := make(map[string][]HistoryEvent)
	for _, event := range history {
		if event.RepoID != string(checkpoint.RepoID) || ValidateHistoryEvent(event) != nil {
			return nil, ErrIntegrity
		}
		if event.Kind != "publish" && event.Kind != "pr-merge" && !event.CreatedAt.After(checkpoint.MergedAt) {
			key := publicationKey(event)
			proofs[key] = append(proofs[key], event)
		}
		if event.Kind == "attach" && !event.CreatedAt.After(checkpoint.MergedAt) {
			attachments[event.BranchID] = append(attachments[event.BranchID], event)
		}
	}
	verifier := &PRParticipationVerifier{checkpoint: checkpoint, publications: make(map[string]HistoryEvent), branches: make(map[string]map[string]bool)}
	for _, observations := range proofs {
		for _, observation := range observations {
			if observation.Target == "" || ValidateGitOID(observation.GitAfter) != nil {
				continue
			}
			candidate := observation
			candidate.Kind, candidate.Source = "publish", candidate.Target
			matching := append([]HistoryEvent{observation}, attachments[observation.BranchID]...)
			if !MatchesPRSourcePublication(candidate, checkpoint.PR.HeadBranch, matching) {
				continue
			}
			if verifier.branches[observation.GitAfter] == nil {
				verifier.branches[observation.GitAfter] = make(map[string]bool)
			}
			verifier.branches[observation.GitAfter][observation.BranchID] = true
		}
	}
	for _, publication := range history {
		if publication.Kind != "publish" {
			continue
		}
		observations := proofs[publicationKey(publication)]
		if len(observations) == 0 {
			continue
		}
		matching := append([]HistoryEvent{}, observations...)
		matching = append(matching, attachments[publication.BranchID]...)
		if MatchesPRSourcePublication(publication, checkpoint.PR.HeadBranch, matching) {
			verifier.publications[publication.ID] = publication
		}
	}
	return verifier, nil
}

func assertPRParticipationIndexParity(t *testing.T, index *PRParticipationHistoryIndex, checkpoint PRHistoryCheckpoint, observation GitRefObservation, history []HistoryEvent) {
	t.Helper()
	want, err := legacyPRParticipationVerifier(checkpoint, history)
	if err != nil {
		t.Fatal("invalid legacy fixture", err)
	}
	actual, err := index.Verifier(context.Background(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := NewPRParticipationVerifier(checkpoint, history)
	if err != nil {
		t.Fatal(err)
	}
	for _, got := range []*PRParticipationVerifier{actual, wrapped} {
		if !reflect.DeepEqual(got.branches, want.branches) || !reflect.DeepEqual(got.publications, want.publications) {
			t.Fatalf("indexed checkpoint changed candidates or ownership at %v: branches=%v want=%v publications=%v want=%v", checkpoint.MergedAt, got.branches, want.branches, got.publications, want.publications)
		}
		for _, publication := range history {
			if publication.Kind != "publish" {
				continue
			}
			expected, expectedErr := want.Verify(observation, publication)
			result, resultErr := got.Verify(observation, publication)
			if result != expected || !errors.Is(resultErr, expectedErr) {
				t.Fatal("participation result differs from full-history verifier", result, resultErr, expected, expectedErr)
			}
		}
	}
}

func TestPRParticipationHistoryIndexLegacyParity(t *testing.T) {
	for _, scenario := range []string{"canonical", "canonical-native-same", "archive-proof", "rename-proof", "missing-proof", "proof-at-merge", "proof-after-merge", "publication-clock-rollback", "alias-missing", "alias-shared-worktree", "alias-cross-worktree", "alias-renamed-same-worktree", "alias-unrelated-worktree", "alias-without-worktree", "alias-foreign-identity", "alias-at-proof", "alias-after-proof", "alias-after-merge", "foreign-owner-capture-worktree", "foreign-owner-after-merge", "alias-foreign-owner", "nonmatching-head-owner", "duplicate-publication-ID"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint, observation, _, history := participationFixture()
			switch scenario {
			case "canonical-native-same":
				for position := range history {
					history[position].LocalBranch, history[position].WorktreeID = checkpoint.PR.HeadBranch, strings.Repeat("a", 32)
				}
			case "archive-proof":
				history[0].Kind = "archive"
			case "rename-proof":
				history[0].Kind, history[0].PreviousBranch = "rename", "old-name"
			case "missing-proof":
				history = history[1:]
			case "proof-at-merge":
				history[0].CreatedAt = checkpoint.MergedAt
			case "proof-after-merge":
				history[0].CreatedAt = checkpoint.MergedAt.Add(time.Nanosecond)
			case "publication-clock-rollback":
				history[1].CreatedAt = history[0].CreatedAt.Add(-time.Minute)
			case "foreign-owner-capture-worktree", "foreign-owner-after-merge", "nonmatching-head-owner":
				foreign := history[0]
				foreign.ID, foreign.BranchID, foreign.WorktreeID = strings.Repeat("3", 32), "foreign-identity", strings.Repeat("c", 32)
				foreign.Target, foreign.LocalBranch = HashContent([]byte("foreign capture")), "other-native"
				if scenario == "foreign-owner-after-merge" {
					foreign.CreatedAt = checkpoint.MergedAt.Add(time.Nanosecond)
				}
				if scenario == "nonmatching-head-owner" {
					foreign.Branch = "other-context"
				}
				history = append(history, foreign)
			case "duplicate-publication-ID":
				later := history[1]
				later.CreatedAt = later.CreatedAt.Add(time.Hour)
				history = append(history, later)
			}
			if strings.HasPrefix(scenario, "alias-") {
				for position := range history {
					history[position].Branch, history[position].LocalBranch = "context-alias", checkpoint.PR.HeadBranch
					history[position].WorktreeID = strings.Repeat("a", 32)
				}
				attached := history[0]
				attached.ID, attached.Kind, attached.Branch = strings.Repeat("3", 32), "attach", "previous-context-name"
				attached.Target, attached.GitAfter = HashContent([]byte("attachment capture")), strings.Repeat("e", 40)
				attached.CreatedAt = history[0].CreatedAt.Add(-time.Second)
				switch scenario {
				case "alias-cross-worktree":
					attached.WorktreeID = strings.Repeat("b", 32)
				case "alias-renamed-same-worktree":
					attached.LocalBranch = "old-native-name"
				case "alias-unrelated-worktree":
					attached.WorktreeID, attached.LocalBranch = strings.Repeat("b", 32), "other-native"
				case "alias-without-worktree":
					attached.WorktreeID = ""
				case "alias-foreign-identity":
					attached.BranchID = "foreign-identity"
				case "alias-at-proof":
					attached.CreatedAt = history[0].CreatedAt
				case "alias-after-proof":
					attached.CreatedAt = history[0].CreatedAt.Add(time.Nanosecond)
				case "alias-after-merge":
					attached.CreatedAt = checkpoint.MergedAt.Add(time.Nanosecond)
				}
				if scenario != "alias-missing" {
					history = append(history, attached)
				}
				if scenario == "alias-foreign-owner" {
					foreign := history[0]
					foreign.ID, foreign.BranchID, foreign.WorktreeID = strings.Repeat("4", 32), "foreign-identity", strings.Repeat("b", 32)
					foreign.Target = HashContent([]byte("foreign alias capture"))
					binding := foreign
					binding.ID, binding.Kind, binding.LocalBranch = strings.Repeat("5", 32), "attach", "renamed-foreign-alias"
					binding.CreatedAt = foreign.CreatedAt.Add(-time.Second)
					history = append(history, foreign, binding)
				}
			}
			for _, reverse := range []bool{false, true} {
				if reverse {
					slices.Reverse(history)
				}
				index, err := NewPRParticipationHistoryIndex(context.Background(), checkpoint.RepoID, history)
				if err != nil {
					t.Fatal(err)
				}
				for _, delta := range []time.Duration{0, -201 * time.Second, 201 * time.Second, 0} {
					current := checkpoint
					current.MergedAt = current.MergedAt.Add(delta)
					assertPRParticipationIndexParity(t, index, current, observation, history)
				}
			}
		})
	}
}

func prParticipationIndexHistory(requests, unrelated, witnesses, publications int, alias bool) (ContentHash, []HistoryEvent, []PRHistoryCheckpoint) {
	base, _, _, seed := participationFixture()
	history := make([]HistoryEvent, 0, unrelated+requests*(witnesses+publications))
	appendEvent := func(event HistoryEvent) {
		event.ID = fmt.Sprintf("%032x", len(history)+1)
		history = append(history, event)
	}
	checkpoints := make([]PRHistoryCheckpoint, 0, requests)
	for request := 0; request < requests; request++ {
		checkpoint := base
		checkpoint.PR.Number, checkpoint.PullRequestID = request+1, int64(request+1000)
		checkpoint.PR.HeadBranch = fmt.Sprintf("feature/%d", request)
		checkpoints = append(checkpoints, checkpoint)
		ordinary := seed[0]
		ordinary.Branch, ordinary.BranchID, ordinary.GitAfter = checkpoint.PR.HeadBranch, fmt.Sprintf("identity-%d", request), fmt.Sprintf("%040x", request+1)
		if alias {
			ordinary.Branch, ordinary.LocalBranch, ordinary.WorktreeID = fmt.Sprintf("context/%d", request), checkpoint.PR.HeadBranch, strings.Repeat("a", 32)
			attached := ordinary
			attached.Kind, attached.WorktreeID, attached.CreatedAt = "attach", strings.Repeat("b", 32), ordinary.CreatedAt.Add(-time.Second)
			appendEvent(attached)
		}
		for witness := 0; witness < witnesses; witness++ {
			appendEvent(ordinary)
		}
		publication := ordinary
		publication.Kind, publication.CreatedAt = "publish", checkpoint.MergedAt.Add(time.Hour)
		for published := 0; published < publications; published++ {
			appendEvent(publication)
		}
	}
	for position := 0; position < unrelated; position++ {
		event := seed[0]
		event.Branch, event.BranchID = fmt.Sprintf("unrelated/%d", position), fmt.Sprintf("unrelated-identity-%d", position)
		appendEvent(event)
	}
	return base.RepoID, history, checkpoints
}

func TestPRParticipationHistoryIndexUntruncatedAndImmutable(t *testing.T) {
	repo, history, checkpoints := prParticipationIndexHistory(3, 257, 257, 257, false)
	index, err := NewPRParticipationHistoryIndex(context.Background(), repo, history)
	if err != nil {
		t.Fatal(err)
	}
	before := make([]*PRParticipationVerifier, len(checkpoints))
	for position, checkpoint := range checkpoints {
		before[position], err = index.Verifier(context.Background(), checkpoint)
		if err != nil || len(before[position].publications) != 257 {
			t.Fatal("repeated key publications truncated", err)
		}
		_, observation, _, _ := participationFixture()
		assertPRParticipationIndexParity(t, index, checkpoint, observation, history)
	}
	for position := range history {
		history[position] = HistoryEvent{}
	}
	var readers sync.WaitGroup
	for range 16 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for position, checkpoint := range checkpoints {
				verifier, err := index.Verifier(context.Background(), checkpoint)
				if err != nil || !reflect.DeepEqual(verifier.publications, before[position].publications) || !reflect.DeepEqual(verifier.branches, before[position].branches) {
					t.Error("caller mutation or concurrent checkpoint changed index", err)
					return
				}
				clear(verifier.publications)
				clear(verifier.branches)
			}
		}()
	}
	readers.Wait()
}

func TestPRParticipationHistoryIndexRetainsLateCandidatesAndAttachments(t *testing.T) {
	checkpoint, observation, publication, history := participationFixture()
	for position := 0; position < 257; position++ {
		event := history[0]
		event.ID = fmt.Sprintf("%032x", position+1)
		history = append(history, event)
	}
	foreign := history[0]
	foreign.ID, foreign.BranchID, foreign.Target, foreign.WorktreeID = strings.Repeat("f", 32), "late-foreign-owner", HashContent([]byte("another capture")), strings.Repeat("c", 32)
	history = append(history, foreign)
	index, err := NewPRParticipationHistoryIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := index.Verifier(context.Background(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(observation, publication); !errors.Is(err, ErrConflict) {
		t.Fatal("late foreign owner was truncated or capture/worktree scoped", err)
	}
	_, _, _, history = participationFixture()
	for position := range history {
		history[position].Branch, history[position].LocalBranch, history[position].WorktreeID = "context", checkpoint.PR.HeadBranch, strings.Repeat("a", 32)
	}
	publication = history[1]
	for position := 0; position < 257; position++ {
		attached := history[0]
		attached.ID, attached.Kind, attached.LocalBranch, attached.WorktreeID = fmt.Sprintf("%032x", position+1), "attach", "unrelated-alias", strings.Repeat("b", 32)
		attached.CreatedAt = attached.CreatedAt.Add(-time.Second)
		if position == 256 {
			attached.LocalBranch = checkpoint.PR.HeadBranch
		}
		history = append(history, attached)
	}
	index, err = NewPRParticipationHistoryIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err = index.Verifier(context.Background(), checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verifier.Verify(observation, publication); err != nil {
		t.Fatal("last cross-worktree attachment was truncated", err)
	}
}

func TestPRParticipationHistoryIndexValidatesUnrelatedHistory(t *testing.T) {
	for _, scenario := range []string{"foreign-repo", "invalid-id", "invalid-unrelated-publication", "invalid-creation", "invalid-unrelated-PR"} {
		t.Run(scenario, func(t *testing.T) {
			checkpoint, _, _, history := participationFixture()
			event := history[0]
			event.ID, event.Branch, event.BranchID = strings.Repeat("3", 32), "unrelated", "unrelated-identity"
			switch scenario {
			case "foreign-repo":
				event.RepoID = string(HashContent([]byte("foreign")))
			case "invalid-id":
				event.ID = "invalid"
			case "invalid-unrelated-publication":
				event.Kind, event.Source = "publish", ""
			case "invalid-creation":
				event.Creation = &GitCreation{Evidence: "invented"}
			case "invalid-unrelated-PR":
				event.Kind = "pr-merge"
			}
			history = append(history, event)
			if index, err := NewPRParticipationHistoryIndex(context.Background(), checkpoint.RepoID, history); !errors.Is(err, ErrIntegrity) || index != nil {
				t.Fatal("unrelated corrupt history skipped", index, err)
			}
			if verifier, err := NewPRParticipationVerifier(checkpoint, history); !errors.Is(err, ErrIntegrity) || verifier != nil {
				t.Fatal("wrapper skipped unrelated corrupt history", verifier, err)
			}
		})
	}
}

func TestPRParticipationHistoryIndexScopeAndCancellation(t *testing.T) {
	checkpoint, observation, _, history := participationFixture()
	history[0].Creation = &GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature", "HEAD"}, StartRef: "HEAD", StartCommit: history[0].GitAfter}
	index, err := NewPRParticipationHistoryIndex(context.Background(), checkpoint.RepoID, history)
	if err != nil {
		t.Fatal(err)
	}
	history[0].Creation.StartCommit = "invalid"
	history[0].Creation.Command[0] = "invalid"
	if index.history[0].Creation.StartCommit != observation.Before || index.history[0].Creation.Command[0] != "git" {
		t.Fatal("mutable creation metadata aliases caller history")
	}
	for _, invalid := range []PRHistoryCheckpoint{{}, checkpoint} {
		invalid.RepoID = HashContent([]byte("foreign checkpoint"))
		if verifier, err := index.Verifier(context.Background(), invalid); !errors.Is(err, ErrIntegrity) || verifier != nil {
			t.Fatal("foreign or invalid checkpoint accepted", verifier, err)
		}
	}
	var missing *PRParticipationHistoryIndex
	if _, err := missing.Verifier(context.Background(), checkpoint); !errors.Is(err, ErrIntegrity) {
		t.Fatal("nil history index accepted", err)
	}
	if _, err := NewPRParticipationHistoryIndex(context.Background(), "invalid", nil); !errors.Is(err, ErrValidation) {
		t.Fatal("invalid index repo accepted", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if verifier, err := index.Verifier(canceled, checkpoint); !errors.Is(err, context.Canceled) || verifier != nil {
		t.Fatal("canceled checkpoint accepted", verifier, err)
	}
	if index, err := NewPRParticipationHistoryIndex(canceled, checkpoint.RepoID, nil); !errors.Is(err, context.Canceled) || index != nil {
		t.Fatal("canceled construction accepted", index, err)
	}
	repo, many, checkpoints := prParticipationIndexHistory(1, 10, 257, 257, false)
	parent, stop := context.WithCancel(context.Background())
	defer stop()
	duringBuild := &gitPublicationIndexCancelContext{Context: parent, cancel: stop, after: 10}
	if index, err := NewPRParticipationHistoryIndex(duringBuild, repo, many); !errors.Is(err, context.Canceled) || index != nil {
		t.Fatal("in-progress construction cancellation ignored", index, err)
	}
	index, err = NewPRParticipationHistoryIndex(context.Background(), repo, many)
	if err != nil {
		t.Fatal(err)
	}
	parent, stop = context.WithCancel(context.Background())
	defer stop()
	duringVerify := &gitPublicationIndexCancelContext{Context: parent, cancel: stop, after: 10}
	if verifier, err := index.Verifier(duringVerify, checkpoints[0]); !errors.Is(err, context.Canceled) || verifier != nil {
		t.Fatal("in-progress checkpoint cancellation ignored", verifier, err)
	}
}

var benchmarkPRParticipationVerifier *PRParticipationVerifier

func BenchmarkPRParticipationHistoryIndex(benchmark *testing.B) {
	for _, size := range []struct {
		requests, unrelated, witnesses, publications int
		alias                                        bool
	}{{8, 2048, 1, 1, false}, {64, 4096, 1, 1, false}, {1, 0, 257, 257, false}, {1, 0, 257, 257, true}} {
		name := fmt.Sprintf("PRs=%d/unrelated=%d/witnesses=%d/publications=%d/alias=%t", size.requests, size.unrelated, size.witnesses, size.publications, size.alias)
		benchmark.Run(name, func(benchmark *testing.B) {
			repo, history, checkpoints := prParticipationIndexHistory(size.requests, size.unrelated, size.witnesses, size.publications, size.alias)
			for _, indexed := range []bool{false, true} {
				benchmark.Run(fmt.Sprintf("indexed=%t", indexed), func(benchmark *testing.B) {
					benchmark.ReportAllocs()
					benchmark.ResetTimer()
					for iteration := 0; iteration < benchmark.N; iteration++ {
						var index *PRParticipationHistoryIndex
						if indexed {
							var err error
							index, err = NewPRParticipationHistoryIndex(context.Background(), repo, history)
							if err != nil {
								benchmark.Fatal(err)
							}
						}
						for _, checkpoint := range checkpoints {
							var verifier *PRParticipationVerifier
							var err error
							if indexed {
								verifier, err = index.Verifier(context.Background(), checkpoint)
							} else {
								verifier, err = legacyPRParticipationVerifier(checkpoint, history)
							}
							if err != nil {
								benchmark.Fatal(err)
							}
							benchmarkPRParticipationVerifier = verifier
						}
					}
				})
			}
		})
	}
	benchmarkPRParticipationVerifier = nil
}
