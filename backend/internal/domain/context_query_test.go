package domain

import (
	"errors"
	"reflect"
	"testing"
)

func TestContextSelectionSeparatesPositionFromRetainedHistory(t *testing.T) {
	a, b, c, p := HashContent([]byte("a")), HashContent([]byte("b")), HashContent([]byte("c")), HashContent([]byte("pending"))
	v := RepositoryView{Revision: RepositoryRevision{Graph: 7}, Snapshots: []Snapshot{
		{ID: a}, {ID: b, Parents: []ContentHash{a}}, {ID: c, GraftParents: []ContentHash{b}}, {ID: p, Parents: []ContentHash{c}},
	}, Refs: []Ref{{Kind: RefBranch, Name: "main", Target: c}}}
	for _, tc := range []struct {
		scope string
		want  []ContentHash
	}{
		{"all", []ContentHash{a, b, c, p}}, {"current", []ContentHash{a, b}}, {"previous", []ContentHash{c}},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			out, err := SelectContext(v, ContextSelection{Position: string(b), Scope: tc.scope}, "main")
			if err != nil {
				t.Fatal(err)
			}
			got := map[ContentHash]bool{}
			for _, s := range out.Snapshots {
				got[s.ID] = true
			}
			want := map[ContentHash]bool{}
			for _, id := range tc.want {
				want[id] = true
			}
			if !reflect.DeepEqual(got, want) || out.Revision != v.Revision || out.Position != b {
				t.Fatalf("selection: %+v", out)
			}
		})
	}
	if v.Refs[0].Target != c || v.Snapshots[2].GraftParents[0] != b {
		t.Fatal("selection mutated shared history")
	}
	for _, scope := range []string{"current", "previous"} {
		if _, err := SelectContext(v, ContextSelection{Scope: scope}, "main"); !errors.Is(err, ErrValidation) {
			t.Fatalf("implicit position: %v", err)
		}
	}
	if _, err := SelectContext(v, ContextSelection{Scope: "current", Position: "unknown"}, "main"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing position: %v", err)
	}
}

func TestContextSelectionArchivedIdentityDoesNotIncludeSameNameLabels(t *testing.T) {
	a, b := HashContent([]byte("archived")), HashContent([]byte("unproven label"))
	birth := HistoryEvent{ID: "birth", BranchID: "work", Branch: "topic", Kind: "birth", Source: a, Target: a}
	archive := HistoryEvent{ID: "archive", BranchID: "work", Branch: "topic", Kind: "archive", BindingParent: birth.ID, Source: a, Target: a}
	v := RepositoryView{Snapshots: []Snapshot{{ID: a}, {ID: b, Branch: "topic"}}, History: []HistoryEvent{archive, birth}}
	out, err := SelectContext(v, ContextSelection{Branch: "topic", Scope: "archived"}, "main")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Snapshots) != 1 || out.Snapshots[0].ID != a || len(out.History) != 2 {
		t.Fatalf("archived identity mixed: %+v", out)
	}
}

func TestContextSelectionRejectsDuplicateIDs(t *testing.T) {
	id := HashContent([]byte("duplicate"))
	_, err := SelectContext(RepositoryView{Snapshots: []Snapshot{{ID: id}, {ID: id}}}, ContextSelection{}, "main")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("duplicate accepted: %v", err)
	}
}

func TestContextSelectionAllKeepsAmbiguousLegacyHistoryInspectable(t *testing.T) {
	v := RepositoryView{History: []HistoryEvent{
		{ID: "a", Kind: "birth", BranchID: "first", Branch: "reused"},
		{ID: "b", Kind: "birth", BranchID: "second", Branch: "reused"},
	}}
	out, err := SelectContext(v, ContextSelection{}, "main")
	if err != nil || len(out.History) != 2 {
		t.Fatalf("legacy evidence hidden: %+v %v", out, err)
	}
	if _, err = SelectContext(v, ContextSelection{Branch: "reused"}, "main"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("ambiguous selection guessed: %v", err)
	}
}
