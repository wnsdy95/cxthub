package domain

import (
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestPublicationHistoryBranchesUsesCausalLocalNames(t *testing.T) {
	f := newPublicationFixture()
	rename := f.event("rename", "R", "renamed", f.a)
	rename.PreviousBranch = "feature"
	rename.BindingParent = f.in.History[0].ID
	rename.CreatedAt = time.Unix(1, 0).UTC()
	archive := f.event("archive", "R", "renamed", "")
	archive.Source = f.a
	archive.BindingParent = rename.ID
	archive.CreatedAt = time.Unix(2, 0).UTC()
	reuse := f.event("birth", "X", "renamed", f.b)
	reuse.BindingParent = archive.ID
	legacy := f.event("position", LegacyContextBranchID(f.in.RepoID, "legacy"), "legacy", f.a)
	events := append(f.in.History, rename, archive, reuse, legacy)
	got, err := PublicationHistoryBranches(f.in.RepoID, events)
	if err != nil {
		t.Fatal(err)
	}
	want := []PublicationBranch{{"renamed", "R"}, {"renamed", "X"}, {"legacy", legacy.BranchID}}
	slices.SortFunc(want, func(a, b PublicationBranch) int {
		if a.BranchID < b.BranchID {
			return -1
		}
		if a.BranchID > b.BranchID {
			return 1
		}
		return 0
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonical identities=%v want=%v", got, want)
	}
	slices.Reverse(events)
	again, err := PublicationHistoryBranches(f.in.RepoID, events)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("input order chose name: %v %v", again, err)
	}
	empty, err := PublicationHistoryBranches(f.in.RepoID, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty history invented all: %v %v", empty, err)
	}
}
func TestPublicationHistoryBranchesRejectsUnprovenNames(t *testing.T) {
	for _, scenario := range []string{"modern-completion", "collision", "invalid-identity"} {
		t.Run(scenario, func(t *testing.T) {
			f := newPublicationFixture()
			events := f.in.History
			switch scenario {
			case "modern-completion":
				events = []HistoryEvent{f.event("publish", "R", "feature", f.a)}
			case "collision":
				e := events[0]
				e.Target = f.b
				events = append(events, e)
			case "invalid-identity":
				events[0].BranchID = "R\nX"
			}
			got, err := PublicationHistoryBranches(f.in.RepoID, events)
			if err == nil || len(got) != 0 {
				t.Fatalf("unproven names authorized: %v %v", got, err)
			}
			if scenario == "collision" && !errors.Is(err, ErrHashMismatch) {
				t.Fatal(err)
			}
		})
	}
}

func TestPublicationHistoryBranchesKeepsForeignWitnessesWithoutSelectingThem(t *testing.T) {
	f := newPublicationFixture()
	f.origin("X", "source")
	witness := f.event("position", "X", "source", f.b)
	f.in.History = append(f.in.History, witness)
	branches, err := PublicationHistoryBranches(f.in.RepoID, f.in.History)
	if err != nil || !reflect.DeepEqual(branches, []PublicationBranch{{"feature", "R"}}) {
		t.Fatalf("foreign witness blocked local drain: %v %v", branches, err)
	}
	f.in.Scope = PublicationScope{Branches: branches, HistoryOnly: true}
	f.in.Refs = nil
	p := planMust(t, f.in)
	if !slices.Contains(sentIDs(p), witness.ID) {
		t.Fatal("omitted identity lost required ordinary proof")
	}
}
