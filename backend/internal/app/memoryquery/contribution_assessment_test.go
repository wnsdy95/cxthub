package memoryquery

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/wnsdy95/cxthub/backend/internal/domain"
)

func TestPRContributionMemoryAssessmentUsesExactOriginalAndFileEvidence(test *testing.T) {
	source, config, _ := liveFixture(test)
	source.scoped = false
	source.origin = "https://github.com/example/effective-memory"
	contributionAssessmentCodeFixture(test, source)
	ctx := context.Background()
	capture, otherCapture := domain.HashContent([]byte("A")), domain.HashContent([]byte("B"))
	var publication domain.HistoryEvent
	for index, input := range []struct {
		snapshot domain.ContentHash
		code     int
		branch   string
	}{{capture, 2, "branch-a"}, {otherCapture, 3, "branch-b"}, {capture, 8, "rewritten-source"}} {
		observation := domain.HistoryEvent{ID: fmt.Sprintf("%032x", index+1), RepoID: string(source.repo),
			BranchID: input.branch, Branch: input.branch, Kind: "position", Target: input.snapshot,
			GitAfter: contributionAssessmentOID(input.code), CreatedAt: time.Date(2026, 1, 1, 0, 0, index+1, 0, time.UTC)}
		key := sha256.Sum256([]byte(observation.ID + ":publish"))
		publication = observation
		publication.ID, publication.Kind, publication.Source = fmt.Sprintf("%x", key[:16]), "publish", observation.Target
		publication.MemoryPinned, publication.CreatedAt = true, observation.CreatedAt.Add(time.Second)
		source.history = append(source.history, observation, publication)
	}
	sort.Slice(source.history, func(left, right int) bool {
		if source.history[left].CreatedAt.Equal(source.history[right].CreatedAt) {
			return source.history[left].ID < source.history[right].ID
		}
		return source.history[left].CreatedAt.Before(source.history[right].CreatedAt)
	})
	history := source.history
	queries := newLiveTestQueries(test, config)
	record := (domain.PRContribution{RepoID: source.repo, GitOrigin: source.origin, ProviderRepositoryID: 1, PullRequestID: 2,
		PR:                  domain.PullRequestMerge{Number: 1, HeadBranch: "rewritten-source", BaseBranch: "main", HeadSHA: contributionAssessmentOID(9), MergeSHA: contributionAssessmentOID(2)},
		DestinationBranchID: "main", SourceBranchID: publication.BranchID, Capture: capture, OriginalHead: publication.GitAfter}).WithID()
	for _, scenario := range []struct {
		name     string
		code     int
		valid    bool
		source   domain.ContentHash
		original string
		want     string
	}{
		{"applied", 3, true, capture, publication.GitAfter, "applied"},
		{"reverted", 4, true, capture, publication.GitAfter, "inactive"},
		{"partial revert", 6, true, capture, publication.GitAfter, "review"},
		{"missing evidence", 99, true, capture, publication.GitAfter, "review"},
		{"unselected merge", 1, true, capture, publication.GitAfter, "inactive"},
		{"late ambiguity", 3, false, capture, publication.GitAfter, "review"},
		{"different capture", 3, true, otherCapture, publication.GitAfter, "review"},
		{"different head", 3, true, capture, contributionAssessmentOID(9), "review"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			candidate := record
			candidate.Capture, candidate.OriginalHead = scenario.source, scenario.original
			cache := &liveContributionCache{repo: source.repo, history: history, loaded: true,
				records: []VerifiedContribution{{Record: ContributionRecord{ID: candidate.ID, RepoID: candidate.RepoID,
					DestinationBranchID: candidate.DestinationBranchID, SourceBranchID: candidate.SourceBranchID,
					Capture: candidate.Capture, OriginalHead: candidate.OriginalHead, IntegrationSHA: candidate.PR.MergeSHA},
					Valid: scenario.valid, Publications: map[string]domain.HistoryEvent{publication.ID: publication}}},
				facts: liveContributionFacts{{ID: candidate.ID, DestinationBranchID: candidate.DestinationBranchID,
					Capture: candidate.Capture, IntegrationSHA: candidate.PR.MergeSHA, Valid: scenario.valid,
					SourceBranch: candidate.PR.HeadBranch, DestinationBranch: candidate.PR.BaseBranch, PRNumber: candidate.PR.Number}},
			}
			evidence, err := queries.newLiveEvidence(ctx, source.repo, cache)
			if err != nil {
				test.Fatal(err)
			}
			resolver, err := newMemoryIntegration(queries, evidence, source.repo, history)
			if err != nil {
				test.Fatal(err)
			}
			item := domain.EffectiveMemoryItem{SourceSnapshot: capture, Kind: "code", Text: "rewritten feature", Code: &domain.MemoryCodeScope{Commit: publication.GitAfter, Paths: []string{"a", "b"}}}
			got, err := resolver.assess(ctx, item, contributionAssessmentOID(scenario.code), domain.RepositoryRevision{})
			if err != nil || got.State != scenario.want {
				test.Fatalf("assessment: %+v %v", got, err)
			}
			if scenario.name == "applied" && (got.IntegrationReceipt != record.ID || len(got.Paths) != 2) {
				test.Fatal("missing contribution receipt or skipped file checks")
			}
		})
	}
}

func contributionAssessmentOID(ordinal int) string { return fmt.Sprintf("%040x", ordinal) }

func contributionAssessmentCodeFixture(test *testing.T, source *liveTestSource) {
	test.Helper()
	old := domain.GitEntry{OID: contributionAssessmentOID(101), Mode: "100644"}
	fresh := domain.GitEntry{OID: contributionAssessmentOID(102), Mode: "100755"}
	other := domain.GitEntry{OID: contributionAssessmentOID(103), Mode: "100644"}
	change := func(path string, before, after domain.GitEntry) domain.GitPathChange {
		return domain.GitPathChange{Path: path, Before: before, After: after}
	}
	delta := func(ordinal, parent int, changes ...domain.GitPathChange) domain.GitCommitDelta {
		value := domain.GitCommitDelta{Commit: contributionAssessmentOID(ordinal), Parents: []string{}, Complete: true, Changes: changes}
		if parent != 0 {
			value.Parent = contributionAssessmentOID(parent)
			value.Parents = []string{value.Parent}
		}
		return value
	}
	source.deltas = map[string]domain.GitCommitDelta{
		contributionAssessmentOID(1): delta(1, 0, change("a", domain.GitEntry{}, old), change("b", domain.GitEntry{}, old)),
		contributionAssessmentOID(2): delta(2, 1, change("a", old, fresh), change("b", old, fresh)),
		contributionAssessmentOID(3): delta(3, 2, change("x", domain.GitEntry{}, other)),
		contributionAssessmentOID(4): delta(4, 3, change("a", fresh, old), change("b", fresh, old)),
		contributionAssessmentOID(5): delta(5, 4, change("a", old, fresh), change("b", old, fresh)),
		contributionAssessmentOID(6): delta(6, 3, change("a", fresh, old)),
		contributionAssessmentOID(7): delta(7, 3, change("a", fresh, other)),
		contributionAssessmentOID(8): delta(8, 1, change("a", old, fresh), change("b", old, fresh)),
		contributionAssessmentOID(9): delta(9, 1, change("a", old, fresh), change("b", old, fresh)),
	}
	for _, input := range []struct {
		code    int
		entries map[string]domain.GitEntry
	}{
		{1, map[string]domain.GitEntry{"a": old, "b": old}},
		{2, map[string]domain.GitEntry{"a": fresh, "b": fresh}},
		{3, map[string]domain.GitEntry{"a": fresh, "b": fresh, "x": other}},
		{4, map[string]domain.GitEntry{"a": old, "b": old, "x": other}},
		{5, map[string]domain.GitEntry{"a": fresh, "b": fresh, "x": other}},
		{6, map[string]domain.GitEntry{"a": old, "b": fresh, "x": other}},
		{7, map[string]domain.GitEntry{"a": other, "b": fresh, "x": other}},
		{8, map[string]domain.GitEntry{"a": fresh, "b": fresh}},
		{9, map[string]domain.GitEntry{"a": fresh, "b": fresh}},
	} {
		node, err := domain.NewGitTreeNode(input.entries, 40)
		if err != nil {
			test.Fatal(err)
		}
		commit := contributionAssessmentOID(input.code)
		source.commits[commit] = domain.GitCommitTree{Commit: commit, Tree: node.OID, Parents: source.deltas[commit].Parents}
		source.nodes[node.OID] = node
	}
}
