package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestRepositorySourceSnapshotCloneIsolation(test *testing.T) {
	fixture := func() Snapshot {
		return Snapshot{
			ID: HashContent([]byte("snapshot")), RepoID: HashContent([]byte("repo")), Branch: "main",
			Branches: []string{"main", "feature"}, Parents: []ContentHash{HashContent([]byte("parent"))},
			GraftParents: []ContentHash{HashContent([]byte("graft"))}, Models: []string{"model"},
			MemoryHash: HashContent([]byte("memory")), Grafted: true, GraftSeq: 7,
			Author: TeamIdentity{Name: "author"}, CreatedAt: time.Now(),
		}
	}
	original := fixture()
	expected := fixture()
	expected.CreatedAt = original.CreatedAt
	cloned := original.Clone()
	if !reflect.DeepEqual(cloned, original) {
		test.Fatal("clone changed snapshot fields")
	}
	cloned.Branches[0] = "changed"
	cloned.Parents[0] = HashContent([]byte("changed parent"))
	cloned.GraftParents[0] = HashContent([]byte("changed graft"))
	cloned.Models[0] = "changed model"
	cloned.MemoryHash = HashContent([]byte("changed memory"))
	if !reflect.DeepEqual(original, expected) {
		test.Fatal("clone mutation changed source snapshot")
	}
	sibling := original.Clone()
	original.Branches[0] = "source changed"
	original.Parents[0] = HashContent([]byte("source parent"))
	original.GraftParents[0] = HashContent([]byte("source graft"))
	original.Models[0] = "source model"
	if !reflect.DeepEqual(sibling, expected) {
		test.Fatal("source mutation changed sibling clone")
	}
}

func TestRepositorySourceHistoryEventCloneIsolation(test *testing.T) {
	fixture := func() HistoryEvent {
		return HistoryEvent{
			ID: "history", Branch: "feature", BranchID: "branch", Kind: "birth",
			Creation:  &GitCreation{Evidence: "process-argv", Command: []string{"git", "switch", "-c", "feature"}, StartRef: "HEAD"},
			PR:        &PullRequestMerge{Number: 1, BaseBranch: "main", HeadBranch: "feature", HeadSHA: "head", MergeSHA: "merge"},
			CreatedAt: time.Date(2026, time.October, 19, 0, 0, 0, 0, time.UTC),
		}
	}
	original, expected := fixture(), fixture()
	cloned := original.Clone()
	if !reflect.DeepEqual(cloned, original) {
		test.Fatal("clone changed history fields")
	}
	cloned.Creation.Command[0] = "changed"
	cloned.Creation.StartRef = "changed ref"
	cloned.PR.BaseBranch = "changed base"
	cloned.PR.Number++
	if !reflect.DeepEqual(original, expected) {
		test.Fatal("clone mutation changed source history")
	}
	sibling := original.Clone()
	original.Creation.Command[0] = "source changed"
	original.Creation.Evidence = "unavailable"
	original.PR.HeadBranch = "source branch"
	if !reflect.DeepEqual(sibling, expected) {
		test.Fatal("source mutation changed sibling clone")
	}
}

func TestRepositorySourceClonesPreserveNilAndEmpty(test *testing.T) {
	for _, snapshot := range []Snapshot{
		{},
		{Branches: []string{}, Parents: []ContentHash{}, GraftParents: []ContentHash{}, Models: []string{}},
		{Branches: make([]string, 0, 4), Parents: make([]ContentHash, 0, 4), GraftParents: make([]ContentHash, 0, 4), Models: make([]string, 0, 4)},
	} {
		if cloned := snapshot.Clone(); !reflect.DeepEqual(cloned, snapshot) {
			test.Fatalf("snapshot clone changed nil/empty fields: %#v", cloned)
		}
	}
	for _, event := range []HistoryEvent{
		{},
		{Creation: &GitCreation{}, PR: &PullRequestMerge{}},
		{Creation: &GitCreation{Command: []string{}}},
		{Creation: &GitCreation{Command: make([]string, 0, 4)}},
	} {
		cloned := event.Clone()
		if !reflect.DeepEqual(cloned, event) {
			test.Fatalf("history clone changed nil/empty fields: %#v", cloned)
		}
		if event.Creation != nil && cloned.Creation == event.Creation || event.PR != nil && cloned.PR == event.PR {
			test.Fatal("history clone retained mutable pointer")
		}
	}
}
