package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"
)

func graphFixture(name string, parents ...ContentHash) Snapshot {
	id := HashContent([]byte(name))
	return Snapshot{ID: id, DocHash: id, Branch: "main", Parents: parents, Message: name, CreatedAt: time.Unix(int64(len(name)), 0)}
}
func graphContains(ids []ContentHash, id ContentHash) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}
func TestGraphStatePublicationPendingAndGraftClusters(t *testing.T) {
	root := graphFixture("published")
	a := graphFixture("local-a", root.ID)
	b := graphFixture("local-b")
	b.GraftParents = []ContentHash{a.ID}
	live := graphFixture("live", b.ID)
	live.Message = "hook: live"
	hidden := graphFixture("hidden", root.ID)
	hidden.Message = "hook: hidden"
	tagged := graphFixture("tag-only", root.ID)
	v := RepositoryView{Snapshots: []Snapshot{root, a, b, live, hidden, tagged}, Refs: []Ref{{Kind: RefBranch, Name: "main", Target: root.ID}, {Kind: RefTag, Name: "bookmark", Target: tagged.ID}}, Pending: []Pending{{SessionID: "live", Target: live.ID, Branch: "topic"}, {SessionID: "hidden", Target: hidden.ID, Dismissed: true, Branch: "topic"}, {SessionID: "committed", Target: root.ID, Branch: "main"}}, Unsync: []Unsync{{User: "a", Branch: "topic", Target: a.ID}, {User: "b", Branch: "topic", Target: b.ID}}}
	got, err := ProjectGraphState(v, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Hold) != 1 || len(got.Hold[0].IDs) != 2 || len(got.Hold[0].Tips) != 2 {
		t.Fatalf("graft cluster: %+v", got.Hold)
	}
	if !reflect.DeepEqual(got.OrphanSessions, []string{"live"}) || got.HoldCounts["topic"] != 3 {
		t.Fatalf("hold rows/count: %+v", got)
	}
	if !reflect.DeepEqual(got.UncommittedIDs, []ContentHash{live.ID}) || !graphContains(got.TaggedIDs, tagged.ID) || !graphContains(got.PushedIDs, root.ID) || graphContains(got.GraphIDs, hidden.ID) {
		t.Fatalf("tiers: %+v", got)
	}
	if len(got.UnpushedIDs) != 2 || graphContains(got.SharedIDs, tagged.ID) {
		t.Fatal("tag/pending changed publication", got)
	}
}
func TestGraphStateHistoricalPublicationSurvivesRewindAndStashLabel(t *testing.T) {
	root := graphFixture("root")
	past := graphFixture("past", root.ID)
	past.Branch = "(stash)"
	past.Message = "hook: captured then committed"
	v := RepositoryView{Snapshots: []Snapshot{root, past}, Refs: []Ref{{Kind: RefBranch, Name: "main", Target: root.ID}}, History: []HistoryEvent{{ID: "publish", Kind: "publish", Branch: "main", BranchID: "main", Target: past.ID}}, Pending: []Pending{{SessionID: "old", Target: past.ID}}}
	got, err := ProjectGraphState(v, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if !graphContains(got.HistoricalIDs, past.ID) || !graphContains(got.CommittedIDs, past.ID) || len(got.OrphanSessions) != 0 || len(got.UncommittedIDs) != 0 {
		t.Fatalf("lost historical publication: %+v", got)
	}
}
func TestGraphStateIdentityRenameReuseAndExactCompletion(t *testing.T) {
	repo := HashContent([]byte(t.Name()))
	root := graphFixture("root")
	old := graphFixture("old", root.ID)
	old.Branch = "topic"
	latest := graphFixture("latest", root.ID)
	latest.GraftParents = []ContentHash{old.ID}
	birth := HistoryEvent{ID: "old-birth", Kind: "birth", BranchID: "old", Branch: "topic", Source: root.ID, Target: root.ID}
	archive := HistoryEvent{ID: "old-archive", Kind: "archive", BranchID: "old", Branch: "topic", BindingParent: birth.ID, Source: old.ID}
	next := HistoryEvent{ID: "new-birth", Kind: "birth", BranchID: "new", Branch: "topic", BindingParent: archive.ID, Source: latest.ID, Target: latest.ID}
	lifecycle, err := NewBranchLifecycleRef(repo, "topic", old.ID, 1, BranchArchived)
	if err != nil {
		t.Fatal(err)
	}
	v := RepositoryView{Snapshots: []Snapshot{root, old, latest}, Refs: []Ref{{Kind: RefBranch, Name: "main", Target: latest.ID}, lifecycle}, History: []HistoryEvent{birth, archive}, Semantics: ContextSemantics{Version: 1, Merges: []ContextMergeEvidence{{EventID: "completion", Completed: true}}}}
	got, err := ProjectGraphState(v, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Markers) != 1 || got.Markers[0].Kind != "archived" {
		t.Fatal("mere inclusion was called a completed join", got.Markers)
	}
	v.History = append(v.History, HistoryEvent{ID: "completion", Kind: "pr-merge", Branch: "main", BranchID: "main", Source: old.ID, SourceBranchID: "old", PRCompleted: true})
	got, err = ProjectGraphState(v, "main", "")
	if err != nil || got.Markers[0].Kind != "joined" {
		t.Fatal("lost exact completion", got.Markers, err)
	}
	v.History = append(v.History, next)
	v.Refs = append(v.Refs, Ref{Kind: RefBranch, Name: "topic", BranchID: "new", Target: latest.ID})
	got, err = ProjectGraphState(v, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.RefScopes["topic"] != "new" || got.SnapshotScopes["topic"] != "" || len(got.Markers) != 0 {
		t.Fatalf("name reuse reattributed old capture: %+v", got)
	}
}
func TestGraphStatePreviousProgressProtectsOtherWorktrees(t *testing.T) {
	root := graphFixture("root")
	past := graphFixture("future", root.ID)
	birth := HistoryEvent{ID: "birth", Kind: "birth", Branch: "main", BranchID: "main-id", Source: root.ID, Target: root.ID}
	pos := HistoryEvent{ID: "position", Kind: "position", Branch: "main", BranchID: "main-id", Target: root.ID}
	v := RepositoryView{Snapshots: []Snapshot{root, past}, Refs: []Ref{{Kind: RefBranch, Name: "main", BranchID: "main-id", Target: past.ID}}, History: []HistoryEvent{birth, pos, {ID: "published", Kind: "publish", Branch: "main", BranchID: "main-id", Target: past.ID}}}
	got, err := ProjectGraphState(v, "main", pos.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Previous) != 1 || !graphContains(got.Previous[0].CollapsibleIDs, past.ID) {
		t.Fatal("missing retained group", got.Previous)
	}
	v.Refs = append(v.Refs, Ref{Kind: RefSession, Name: "peer-session", Target: past.ID})
	got, err = ProjectGraphState(v, "main", pos.ID)
	if err != nil || len(got.Previous) != 1 || len(got.Previous[0].CollapsibleIDs) != 0 {
		t.Fatal("hid peer's active path", got.Previous, err)
	}
	if _, err := ProjectGraphState(v, "main", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown position fell forward", err)
	}
}
func TestGraphStateInvalidGraphsAndStableWire(t *testing.T) {
	a := graphFixture("a")
	b := graphFixture("b", a.ID)
	for _, snaps := range [][]Snapshot{{a, a}, {func() Snapshot { c := a; c.Parents = []ContentHash{b.ID}; return c }(), b}} {
		if _, err := ProjectGraphState(RepositoryView{Snapshots: snaps}, "main", ""); !errors.Is(err, ErrIntegrity) {
			t.Fatal("invalid graph accepted", err)
		}
	}
	a.Parents = []ContentHash{HashContent([]byte("missing"))}
	v := RepositoryView{Snapshots: []Snapshot{a, b}, Refs: []Ref{{Kind: RefBranch, Name: "main", Target: b.ID}}}
	first, err := ProjectGraphState(v, "main", "")
	if err != nil {
		t.Fatal("missing parent should remain inspectable", err)
	}
	v.Snapshots = []Snapshot{b, a}
	second, err := ProjectGraphState(v, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	aa, _ := json.Marshal(first)
	bb, _ := json.Marshal(second)
	if string(aa) != string(bb) {
		t.Fatal("wire depends on snapshot order")
	}
}
func BenchmarkGraphStateMetadata(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			v := RepositoryView{}
			var parent ContentHash
			for i := 0; i < size; i++ {
				s := graphFixture(fmt.Sprintf("%06d", i))
				if parent != "" {
					s.Parents = []ContentHash{parent}
				}
				v.Snapshots = append(v.Snapshots, s)
				parent = s.ID
			}
			v.Refs = []Ref{{Kind: RefBranch, Name: "main", Target: parent}}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ProjectGraphState(v, "main", ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Include branch publication and completed PR history, not just a long chain.
func BenchmarkGraphStateMergedBranches(b *testing.B) {
	for _, size := range []int{1000, 10000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			root := graphFixture("base")
			v := RepositoryView{Snapshots: []Snapshot{root}}
			head := root.ID
			for branch := 0; branch < size/100; branch++ {
				name := fmt.Sprintf("topic-%d", branch)
				at := time.Unix(int64(branch*100), 0)
				v.History = append(v.History, HistoryEvent{ID: name, Kind: "birth", BranchID: name, Branch: name, Source: root.ID, Target: root.ID, CreatedAt: at})
				parent := root.ID
				for i := 0; i < 100; i++ {
					s := graphFixture(fmt.Sprintf("%s-%d", name, i), parent)
					s.Branch = name
					s.CreatedAt = at.Add(time.Duration(i) * time.Second)
					if i == 0 {
						s.GraftParents = []ContentHash{head}
					}
					v.Snapshots = append(v.Snapshots, s)
					parent = s.ID
				}
				v.History = append(v.History, HistoryEvent{ID: name + "-publish", Kind: "publish", BranchID: name, Branch: name, Target: parent, CreatedAt: at.Add(100 * time.Second)}, HistoryEvent{ID: name + "-merge", Kind: "pr-merge", BranchID: "main-id", Branch: "main", SourceBranchID: name, Source: parent, Target: parent, SharedTarget: head, PRCompleted: true, PR: &PullRequestMerge{Number: branch + 1, HeadBranch: name}, CreatedAt: at.Add(101 * time.Second)})
				v.Refs = append(v.Refs, Ref{Kind: RefBranch, Name: name, BranchID: name, Target: parent})
				head = parent
			}
			v.Refs = append(v.Refs, Ref{Kind: RefBranch, Name: "main", BranchID: "main-id", Target: head})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := ProjectGraphState(v, "main", ""); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
