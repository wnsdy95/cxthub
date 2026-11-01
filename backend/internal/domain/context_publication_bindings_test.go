package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func contextBindingFixture() (ContentHash, HistoryEvent, HistoryEvent) {
	repo, snapshot := HashContent([]byte("binding repository")), HashContent([]byte("binding snapshot"))
	observation := HistoryEvent{ID: strings.Repeat("a", 32), RepoID: string(repo), BranchID: "identity", Branch: "main", LocalBranch: "native", WorktreeID: strings.Repeat("b", 32), Kind: "position", Source: snapshot, Target: snapshot, GitAfter: strings.Repeat("c", 40), CreatedAt: time.Unix(20, 0).UTC()}
	publication := observation
	publication.ID, publication.Kind, publication.CreatedAt = strings.Repeat("d", 32), "publish", time.Unix(10, 0).UTC()
	return repo, observation, publication
}

func TestContextPublicationBindingsRequireExactAcceptedObservation(test *testing.T) {
	repo, observation, publication := contextBindingFixture()
	for _, scenario := range []struct {
		name   string
		change func(*HistoryEvent)
	}{
		{"repository", func(event *HistoryEvent) { event.RepoID = string(HashContent([]byte("foreign"))) }},
		{"branch identity", func(event *HistoryEvent) { event.BranchID = "different" }},
		{"branch label", func(event *HistoryEvent) { event.Branch = "other" }},
		{"local branch", func(event *HistoryEvent) { event.LocalBranch = "other-native" }},
		{"worktree", func(event *HistoryEvent) { event.WorktreeID = strings.Repeat("e", 32) }},
		{"commit", func(event *HistoryEvent) { event.GitAfter = strings.Repeat("f", 40) }},
		{"snapshot", func(event *HistoryEvent) { event.Target = HashContent([]byte("other capture")) }},
		{"PR receipt is not capture observation", func(event *HistoryEvent) {
			event.Kind, event.SourceBranchID = "pr-merge", "source-identity"
			event.PR = &PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: event.GitAfter, MergeSHA: strings.Repeat("f", 40)}
		}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			other := observation
			scenario.change(&other)
			if err := ValidateHistoryEvent(other); err != nil {
				test.Fatal("mismatch fixture must remain a valid observation", err)
			}
			if got := ContextPublicationBindings(repo, []HistoryEvent{other, publication}); len(got) != 0 {
				test.Fatal("mismatched observation invented a finalized binding", got)
			}
		})
	}
	want := map[ContentHash][]CommitContextBinding{observation.Target: {{EventID: publication.ID, GitCommit: publication.GitAfter, BranchID: publication.BranchID, WorktreeID: publication.WorktreeID, SegmentIDs: []ContentHash{}}}}
	for _, history := range [][]HistoryEvent{{observation, publication}, {publication, observation}} {
		before := append([]HistoryEvent{}, history...)
		if got := ContextPublicationBindings(repo, history); !reflect.DeepEqual(got, want) {
			test.Fatalf("exact proof or wall-clock rollback changed bindings: %+v", got)
		}
		if !reflect.DeepEqual(history, before) {
			test.Fatal("matching mutated accepted history")
		}
	}
}

func TestContextPublicationBindingsDoNotInferFinalization(test *testing.T) {
	repo, observation, publication := contextBindingFixture()
	merge := observation
	merge.ID, merge.Kind, merge.SourceBranchID = strings.Repeat("e", 32), "pr-merge", "source-identity"
	merge.PR = &PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: observation.GitAfter, MergeSHA: strings.Repeat("f", 40)}
	if err := ValidateHistoryEvent(merge); err != nil {
		test.Fatal(err)
	}
	for _, scenario := range []struct {
		name    string
		history []HistoryEvent
	}{
		{"raw observation", []HistoryEvent{observation}},
		{"publication alone", []HistoryEvent{publication}},
		{"publication cannot prove itself", []HistoryEvent{publication, publication}},
		{"PR receipt is not publication", []HistoryEvent{observation, merge}},
		{"no evidence", nil},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			if got := ContextPublicationBindings(repo, scenario.history); got == nil || len(got) != 0 {
				test.Fatal("non-publication evidence produced bindings", got)
			}
		})
	}
	for _, damage := range []string{"observation id", "observation time", "publication id", "publication source", "publication commit"} {
		test.Run(damage, func(test *testing.T) {
			observed, finalized := observation, publication
			switch damage {
			case "observation id":
				observed.ID = "invalid"
			case "observation time":
				observed.CreatedAt = time.Time{}
			case "publication id":
				finalized.ID = "invalid"
			case "publication source":
				finalized.Source = HashContent([]byte("another source"))
			case "publication commit":
				finalized.GitAfter = strings.Repeat("0", 40)
			}
			if got := ContextPublicationBindings(repo, []HistoryEvent{observed, finalized}); len(got) != 0 {
				test.Fatal("invalid evidence was accepted", got)
			}
		})
	}
}

func TestContextPublicationBindingsSortReceiptsWithoutInventingSegments(test *testing.T) {
	repo, observation, later := contextBindingFixture()
	earlier := later
	earlier.ID = strings.Repeat("1", 32)
	other := observation
	other.ID, other.Target = strings.Repeat("2", 32), HashContent([]byte("second capture"))
	other.Source = other.Target
	otherPublication := other
	otherPublication.ID, otherPublication.Kind = strings.Repeat("3", 32), "publish"
	history := []HistoryEvent{later, otherPublication, observation, earlier, other}
	want := map[ContentHash][]CommitContextBinding{
		observation.Target: {
			{EventID: earlier.ID, GitCommit: earlier.GitAfter, BranchID: earlier.BranchID, WorktreeID: earlier.WorktreeID, SegmentIDs: []ContentHash{}},
			{EventID: later.ID, GitCommit: later.GitAfter, BranchID: later.BranchID, WorktreeID: later.WorktreeID, SegmentIDs: []ContentHash{}},
		},
		other.Target: {{EventID: otherPublication.ID, GitCommit: otherPublication.GitAfter, BranchID: otherPublication.BranchID, WorktreeID: otherPublication.WorktreeID, SegmentIDs: []ContentHash{}}},
	}
	if got := ContextPublicationBindings(repo, history); !reflect.DeepEqual(got, want) {
		test.Fatalf("receipt order or unassigned segment identity changed: %+v", got)
	}
	observation.LocalBranch, observation.WorktreeID = "", ""
	later.LocalBranch, later.WorktreeID = "", ""
	if got := ContextPublicationBindings(repo, []HistoryEvent{observation, later}); len(got[observation.Target]) != 1 {
		test.Fatal("exact legacy proof with absent optional coordinates was rejected", got)
	}
}
