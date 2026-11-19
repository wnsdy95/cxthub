package domain

import (
	"reflect"
	"testing"
)

func TestWorktreeRegistrationsSendNamesNeverPaths(t *testing.T) {
	got := WorktreeRegistrations([]RecordedWorktree{
		{Root: "/work/app/.worktrees/fix", Current: "fix/typo"},
		{Root: "/work/app", Main: true, Current: "main"},
		{Root: "/elsewhere/detached"},
		// Another folder with the main worktree's name: the name is listed
		// once, for the main worktree.
		{Root: "/copies/app", Current: "experiment"},
		// Two folders with one name and no main worktree among them: the
		// path that sorts first.
		{Root: "/b/twin", Current: "second"},
		{Root: "/a/twin", Current: "first"},
	})
	want := []WorktreeRegistration{
		{Name: "app", Branch: "main"},
		{Name: "detached"},
		{Name: "fix", Branch: "fix/typo"},
		{Name: "twin", Branch: "first"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registrations = %+v", got)
	}
	if empty := WorktreeRegistrations(nil); empty == nil || len(empty) != 0 {
		t.Fatalf("no worktrees = %#v", empty)
	}
}
