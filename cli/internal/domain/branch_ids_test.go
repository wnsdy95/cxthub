package domain

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestPlanBranchIDs(t *testing.T) {
	id := func(letter string) string { return strings.Repeat(letter, 32) }
	next := 0
	newID := func() string { next++; return fmt.Sprintf("%032x", next) }
	plan := func(branches ...LocalBranch) []BranchIDAssignment {
		next = 0
		return PlanBranchIDs(branches, newID)
	}

	// A branch seen for the first time gets an ID; one that has its own keeps it.
	got := plan(LocalBranch{Name: "main", ID: id("a"), IssuedFor: "main"}, LocalBranch{Name: "feature"})
	if want := []BranchIDAssignment{{Name: "feature", ID: fmt.Sprintf("%032x", 1)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("first sight: %+v", got)
	}
	// Git moved the setting with a rename: the only holder keeps its ID.
	if got := plan(LocalBranch{Name: "renamed", ID: id("a"), IssuedFor: "old-name"}); got != nil {
		t.Fatalf("a renamed branch: %+v", got)
	}
	// git branch -c copied the setting: the holder under the issued name keeps
	// the ID, the copy gets a new one.
	got = plan(LocalBranch{Name: "copy", ID: id("a"), IssuedFor: "main"}, LocalBranch{Name: "main", ID: id("a"), IssuedFor: "main"})
	if want := []BranchIDAssignment{{Name: "copy", ID: fmt.Sprintf("%032x", 1)}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("a copy: %+v", got)
	}
	// A renamed branch keeps the name its ID was issued for. Copied later,
	// neither holder has that name, so the one rule gives both a new ID.
	got = plan(LocalBranch{Name: "renamed", ID: id("a"), IssuedFor: "old-name"}, LocalBranch{Name: "copy", ID: id("a"), IssuedFor: "old-name"})
	if len(got) != 2 || got[0].Name != "renamed" || got[1].Name != "copy" || got[0].ID == got[1].ID {
		t.Fatalf("renamed and copied: %+v", got)
	}
	// A value this CLI did not issue is not an ID.
	for _, value := range []string{"hand-written", strings.Repeat("A", 32), strings.Repeat("a", 31), strings.Repeat("a", 33)} {
		if got := plan(LocalBranch{Name: "main", ID: value, IssuedFor: "main"}); len(got) != 1 || got[0].Name != "main" {
			t.Fatalf("%q kept as an ID: %+v", value, got)
		}
	}
	if got := plan(LocalBranch{ID: id("a")}); got != nil {
		t.Fatalf("a branch without a name: %+v", got)
	}
	if !ValidBranchID(id("0")) || !ValidBranchID("0123456789abcdef0123456789abcdef") || ValidBranchID("") {
		t.Fatal("ID format")
	}
}

func TestBranchRegistrationsNameOnlyWhatGitShowsOnOrigin(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	got := BranchRegistrations([]LocalBranch{
		{Name: "main", ID: a, Remote: "main"},
		{Name: "local-name", ID: b, Remote: "name-on-origin"},
		{Name: "second-view", ID: b, Remote: "name-on-origin"},
		{Name: "never-pushed", ID: strings.Repeat("c", 32)},
		{Name: "no-id-yet", Remote: "no-id-yet"},
	})
	want := []BranchRegistration{{Name: "main", LocalID: a}, {Name: "name-on-origin", LocalID: b}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registrations: %+v", got)
	}
	if got := BranchRegistrations(nil); got == nil || len(got) != 0 {
		t.Fatalf("no branches: %#v", got)
	}
}
