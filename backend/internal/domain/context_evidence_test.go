package domain

import (
	"fmt"
	"testing"
)

func TestContextEvidenceCompletionSurvivesPlacementChange(t *testing.T) {
	base, main, source := HashContent([]byte("base")), HashContent([]byte("main")), HashContent([]byte("source"))
	snaps := []Snapshot{{ID: base}, {ID: main, Parents: []ContentHash{base}}, {ID: source, Parents: []ContentHash{base}, GraftParents: []ContentHash{main}}}
	birth := HistoryEvent{ID: "birth", BranchID: "topic-id", Kind: "birth", Source: base}
	merge := HistoryEvent{ID: "receipt", Kind: "pr-merge", PRCompleted: true, PR: &PullRequestMerge{}, SourceBranchID: birth.BranchID, Source: source, Target: source, SharedTarget: main}
	history := []HistoryEvent{birth, merge}
	before := ProjectContextSemantics(snaps, history).Merges
	if len(before) != 1 || !before[0].Completed || !before[0].PlacementIntact || before[0].Lineage != "natural" {
		t.Fatalf("initial evidence: %+v", before)
	}
	// A same-branch Join can replace the overlay while retaining its receipt.
	snaps[2].GraftParents = nil
	snaps[1].GraftParents = []ContentHash{source}
	after := ProjectContextSemantics(snaps, history).Merges
	if len(after) != 1 || !after[0].Completed || after[0].PlacementIntact || after[0].BirthID != birth.ID || after[0].Lineage != "natural" {
		t.Fatalf("completion was revoked: %+v", after)
	}
	missing := ProjectContextSemantics(snaps[:2], history).Merges
	if len(missing) != 1 || !missing[0].Completed || missing[0].SourceAvailable || missing[0].Lineage != "missing" {
		t.Fatalf("missing endpoints hid receipt: %+v", missing)
	}
}

func TestContextEvidenceDistinguishesConversationFromInclusion(t *testing.T) {
	a, b := HashContent([]byte("a")), HashContent([]byte("b"))
	birth := HistoryEvent{ID: "birth", BranchID: "topic", Kind: "birth", Source: a}
	merge := HistoryEvent{ID: "merge", Kind: "pr-merge", PRCompleted: true, PR: &PullRequestMerge{}, SourceBranchID: "topic", Source: b, Target: b, SharedTarget: a}
	for _, tc := range []struct {
		snap Snapshot
		want string
	}{
		{Snapshot{ID: b, Parents: []ContentHash{a}}, "natural"},
		{Snapshot{ID: b, GraftParents: []ContentHash{a}}, "graft"},
		{Snapshot{ID: b, Parents: []ContentHash{a}, Grafted: true}, "graft"},
		{Snapshot{ID: b}, "disconnected"},
		{Snapshot{ID: b, Parents: []ContentHash{HashContent([]byte("missing"))}}, "missing"},
	} {
		got := ProjectContextSemantics([]Snapshot{{ID: a}, tc.snap}, []HistoryEvent{merge, birth}).Merges[0]
		if got.Lineage != tc.want {
			t.Fatalf("%+v: got %s want %s", tc.snap, got.Lineage, tc.want)
		}
	}
	merge.PRCompleted = false
	if len(ProjectContextSemantics(nil, []HistoryEvent{merge}).Merges) != 0 {
		t.Fatal("intent was reported as completed")
	}
}

func TestContextAncestryIndexDoesNotRejectCrossEdges(t *testing.T) {
	nodes := map[ContentHash]Snapshot{}
	var ids []ContentHash
	for i := 0; i < 80; i++ {
		id := HashContent([]byte(fmt.Sprint(i)))
		s := Snapshot{ID: id}
		if i > 0 {
			s.Parents = []ContentHash{ids[(i-1)/2]}
		}
		if i > 5 {
			s.GraftParents = []ContentHash{ids[i-4]}
		}
		nodes[id] = s
		ids = append(ids, id)
	}
	index := newContextAncestry(nodes, false)
	for _, root := range ids {
		want := ContextClosure(nodes, []ContentHash{root})
		for _, target := range ids {
			if index.reaches(root, target) != want[target] {
				t.Fatalf("cross-edge ancestry differs: %s -> %s", root, target)
			}
		}
	}
}

func BenchmarkContextSemanticsLargeHistory(b *testing.B) {
	snaps := make([]Snapshot, 10_000)
	for i := range snaps {
		snaps[i].ID = HashContent([]byte(fmt.Sprint(i)))
		if i > 0 {
			snaps[i].Parents = []ContentHash{snaps[i-1].ID}
		}
	}
	var events []HistoryEvent
	for i := 0; i < 1_000; i++ {
		identity := fmt.Sprint(i)
		events = append(events, HistoryEvent{ID: "birth" + identity, BranchID: identity, Kind: "birth", Source: snaps[0].ID},
			HistoryEvent{ID: "merge" + identity, Kind: "pr-merge", PRCompleted: true, PR: &PullRequestMerge{}, SourceBranchID: identity, Source: snaps[9000+i].ID, Target: snaps[9000+i].ID, SharedTarget: snaps[0].ID})
	}
	b.ResetTimer()
	for b.Loop() {
		ProjectContextSemantics(snaps, events)
	}
}
