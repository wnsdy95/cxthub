package domain

import (
	"reflect"
	"testing"
	"time"
)

func TestStagingObservationVersions(t *testing.T) {
	final := HistoryEvent{ID: "final", RepoID: "repo", BranchID: "branch-id", Branch: "main", LocalBranch: "alias", WorktreeID: "worktree", Kind: "publish", Source: "target", Target: "target", GitAfter: "commit", MemoryHash: "inherited", MemorySource: "owner", MemoryPinned: true, CreatedAt: time.Unix(1, 0)}
	contribution := final
	contribution.ID, contribution.MemoryHash, contribution.MemorySource = "contribution", "", ""
	sibling := contribution
	sibling.ID, sibling.Source, sibling.Target = "sibling", "sibling-target", "sibling-target"
	clone := func(e HistoryEvent) HistoryEvent {
		e.ID = string(HashContent([]byte("staging-observation/v1/" + e.ID)))[7:39]
		e.Kind = "position"
		return e
	}
	for _, version := range []int{1, StagingCommitVersion} {
		op := StagingCommit{Version: version, Publications: []HistoryEvent{sibling, contribution}, Position: WorkingPosition{Selection: &final}}
		want := []HistoryEvent{clone(sibling), clone(final)}
		if version == 1 {
			want = []HistoryEvent{clone(sibling), clone(contribution), clone(final)}
		}
		if got := StagingObservations(op); !reflect.DeepEqual(got, want) {
			t.Fatalf("version %d observations: got %+v want %+v", version, got, want)
		}
		if !reflect.DeepEqual(op.Publications, []HistoryEvent{sibling, contribution}) || !reflect.DeepEqual(*op.Position.Selection, final) {
			t.Fatal("mutated frozen publications")
		}
	}
	// The same snapshot alone is insufficient to replace an observation. Other
	// identities, aliases, code positions and worktrees remain separate facts.
	for name, mutate := range map[string]func(*HistoryEvent){
		"repository":      func(e *HistoryEvent) { e.RepoID = "other" },
		"branch identity": func(e *HistoryEvent) { e.BranchID = "other" },
		"branch":          func(e *HistoryEvent) { e.Branch = "other" },
		"alias":           func(e *HistoryEvent) { e.LocalBranch = "other" },
		"worktree":        func(e *HistoryEvent) { e.WorktreeID = "other" },
		"code":            func(e *HistoryEvent) { e.GitAfter = "other" },
		"target":          func(e *HistoryEvent) { e.Target = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			other := contribution
			mutate(&other)
			op := StagingCommit{Version: StagingCommitVersion, Publications: []HistoryEvent{other}, Position: WorkingPosition{Selection: &final}}
			if got := StagingObservations(op); !reflect.DeepEqual(got, []HistoryEvent{clone(other), clone(final)}) {
				t.Fatalf("unrelated observation replaced: %+v", got)
			}
		})
	}
}
