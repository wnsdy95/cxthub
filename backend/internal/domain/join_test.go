package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

func joinFixture() (JoinGraph, JoinRequest, map[string]ContentHash) {
	ids := map[string]ContentHash{}
	for _, name := range []string{"repo", "P", "H", "X", "T", "Y"} {
		ids[name] = HashContent([]byte("join-domain-" + name))
	}
	graph := JoinGraph{Memberships: map[ContentHash]map[string]bool{}}
	for _, name := range []string{"P", "H", "X", "T"} {
		snap := Snapshot{ID: ids[name], RepoID: ids["repo"], DocHash: ids[name], Branch: "main"}
		switch name {
		case "H", "X":
			snap.Parents = []ContentHash{ids["P"]}
		case "T":
			snap.Parents = []ContentHash{ids["X"]}
		}
		if name == "H" {
			snap.GraftParents = []ContentHash{ids["T"]}
			snap.GraftSeq = 2
		}
		graph.Snapshots = append(graph.Snapshots, snap)
		graph.Memberships[snap.ID] = map[string]bool{"main": true}
	}
	graph.Refs = []Ref{{RepoID: ids["repo"], Kind: RefBranch, Name: "main", Target: ids["H"]}}
	return graph, JoinRequest{RepoID: ids["repo"], Branch: "main", Source: ids["X"]}, ids
}

func TestPlanJoinPreservesHistoryAndInput(t *testing.T) {
	for _, all := range []bool{false, true} {
		graph, request, ids := joinFixture()
		request.IncludeDescendants = all
		before, _ := json.Marshal(graph)
		plan, err := PlanJoin(graph, request)
		if err != nil {
			t.Fatal(err)
		}
		after, _ := json.Marshal(graph)
		if string(before) != string(after) {
			t.Fatal("planning changed repository graph")
		}
		if !reflect.DeepEqual(plan.Segment, []ContentHash{ids["X"], ids["T"]}) {
			t.Fatalf("segment %v", plan.Segment)
		}
		fork := ""
		wantHead := ids["T"]
		if !all {
			fork = SessionRefPrefix("main") + "remaining"
			wantHead = ids["X"]
		}
		m, err := plan.Mutation(fork)
		if err != nil {
			t.Fatal(err)
		}
		if m.NewHead != wantHead || m.ExpectedHead != ids["H"] {
			t.Fatalf("plan %+v", m)
		}
		byID := map[ContentHash]Snapshot{}
		for _, snap := range graph.Snapshots {
			byID[snap.ID] = snap
		}
		if err := ValidateJoinGraphScope(m, byID, graph.Refs); err != nil {
			t.Fatal(err)
		}
		original := snapshotReachableSet(byID, ids["H"])
		for _, patch := range m.Grafts {
			snap := byID[patch.SnapshotID]
			snap.GraftParents = patch.Parents
			byID[snap.ID] = snap
		}
		retained := snapshotReachableSet(byID, m.NewHead)
		if m.ForkTip != "" {
			for id := range snapshotReachableSet(byID, m.ForkTip) {
				retained[id] = true
			}
		}
		if !reflect.DeepEqual(original, retained) {
			t.Fatalf("lost history: %v -> %v", original, retained)
		}
		for _, snap := range graph.Snapshots {
			if !reflect.DeepEqual(snap.Parents, byID[snap.ID].Parents) {
				t.Fatal("natural parent changed")
			}
		}
	}
}

func TestPlanJoinClassification(t *testing.T) {
	cases := []struct {
		name   string
		change func(*JoinGraph, *JoinRequest, map[string]ContentHash)
		want   error
	}{
		{"stale pending is committed", func(g *JoinGraph, _ *JoinRequest, n map[string]ContentHash) {
			g.Pendings = []Pending{{Target: n["X"]}}
			g.Snapshots[2].Message = HookMessagePrefix + "committed"
		}, nil},
		{"unattached capture", func(g *JoinGraph, _ *JoinRequest, n map[string]ContentHash) {
			g.Snapshots[1].GraftParents = nil
			g.Pendings = []Pending{{Target: n["X"]}}
		}, ErrValidation},
		{"foreign ref", func(g *JoinGraph, _ *JoinRequest, n map[string]ContentHash) {
			g.Refs = append(g.Refs, Ref{Kind: RefBranch, Name: "other", Target: n["T"]})
		}, ErrConflict},
		{"branch fork", func(g *JoinGraph, _ *JoinRequest, n map[string]ContentHash) {
			g.Snapshots = append(g.Snapshots, Snapshot{ID: n["Y"], RepoID: n["repo"], Parents: []ContentHash{n["X"]}})
			g.Memberships[n["Y"]] = map[string]bool{"main": true}
		}, ErrConflict},
		{"natural ancestor", func(_ *JoinGraph, r *JoinRequest, n map[string]ContentHash) { r.Source = n["P"] }, ErrConflict},
		{"duplicate snapshot", func(g *JoinGraph, _ *JoinRequest, _ map[string]ContentHash) {
			g.Snapshots = append(g.Snapshots, g.Snapshots[2])
		}, ErrIntegrity},
		{"foreign repository", func(g *JoinGraph, _ *JoinRequest, n map[string]ContentHash) { g.Snapshots[2].RepoID = n["Y"] }, ErrIntegrity},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, r, n := joinFixture()
			tc.change(&g, &r, n)
			_, err := PlanJoin(g, r)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestJoinDomainRechecksChangedGraph(t *testing.T) {
	graph, request, ids := joinFixture()
	request.IncludeDescendants = true
	plan, err := PlanJoin(graph, request)
	if err != nil {
		t.Fatal(err)
	}
	m, err := plan.Mutation("")
	if err != nil {
		t.Fatal(err)
	}
	byID := map[ContentHash]Snapshot{}
	for _, snap := range graph.Snapshots {
		byID[snap.ID] = snap
	}
	refs := append([]Ref{}, graph.Refs...)
	refs = append(refs, Ref{Kind: RefSession, Name: SessionRefPrefix("other") + "new", Target: ids["X"]})
	if err := ValidateJoinGraphScope(m, byID, refs); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign attachment accepted: %v", err)
	}
	byID[ids["Y"]] = Snapshot{ID: ids["Y"], Parents: []ContentHash{ids["T"]}}
	refs = append(graph.Refs, Ref{Kind: RefSession, Name: SessionRefPrefix("main") + "new", Target: ids["Y"]})
	if err := ValidateJoinGraphScope(m, byID, refs); !errors.Is(err, ErrConflict) {
		t.Fatalf("new child accepted: %v", err)
	}
	delete(byID, ids["T"])
	if err := ValidateJoinGraphScope(m, byID, graph.Refs); !errors.Is(err, ErrConflict) {
		t.Fatalf("missing segment accepted: %v", err)
	}
}

func BenchmarkPlanJoinSharedRoots(b *testing.B) {
	graph, request, ids := joinFixture()
	previous := ContentHash("")
	for i := 0; i < 10000; i++ {
		id := HashContent([]byte(fmt.Sprint("large-join-", i)))
		snap := Snapshot{ID: id, RepoID: request.RepoID}
		if previous != "" {
			snap.Parents = []ContentHash{previous}
		}
		graph.Snapshots = append(graph.Snapshots, snap)
		previous = id
		graph.Refs = append(graph.Refs, Ref{Kind: RefSession, Name: SessionRefPrefix("main") + fmt.Sprint(i), Target: ids["H"]})
	}
	graph.Snapshots[0].Parents = []ContentHash{previous}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := PlanJoin(graph, request); err != nil {
			b.Fatal(err)
		}
	}
}
