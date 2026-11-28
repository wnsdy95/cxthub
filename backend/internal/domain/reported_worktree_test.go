package domain

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWorktreeReportTakesNamesNotPaths(t *testing.T) {
	good := WorktreeReport{Machine: "work laptop", Worktrees: []WorktreeReportEntry{{Name: "app", Branch: "main"}, {Name: "app-detached"}}}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := (WorktreeReport{}).Validate(); err != nil {
		t.Fatalf("an empty list from an unlabeled machine: %v", err)
	}
	many := WorktreeReport{}
	for index := 0; index <= ReportedWorktreeLimit; index++ {
		many.Worktrees = append(many.Worktrees, WorktreeReportEntry{Name: "w" + strings.Repeat("x", index%7) + string(rune('a'+index%26)) + strings.Repeat("y", index/26)})
	}
	for name, report := range map[string]WorktreeReport{
		"a path":             {Worktrees: []WorktreeReportEntry{{Name: "work/app"}}},
		"a Windows path":     {Worktrees: []WorktreeReportEntry{{Name: `work\app`}}},
		"no name":            {Worktrees: []WorktreeReportEntry{{Branch: "main"}}},
		"a name twice":       {Worktrees: []WorktreeReportEntry{{Name: "app"}, {Name: "app", Branch: "main"}}},
		"a label with a tab": {Machine: "work\tlaptop"},
		"too many":           many,
	} {
		if err := report.Validate(); !errors.Is(err, ErrValidation) {
			t.Errorf("%s was accepted: %v", name, err)
		}
	}
}

func TestMergeWorktreeReportKeepsTheNameOfARemovedWorktree(t *testing.T) {
	first, second := time.Date(2031, 1, 2, 3, 0, 0, 0, time.UTC), time.Date(2031, 1, 3, 3, 0, 0, 0, time.UTC)
	stored := MergeWorktreeReport(nil, WorktreeReport{Machine: "laptop", Worktrees: []WorktreeReportEntry{{Name: "app", Branch: "main"}, {Name: "app-fix", Branch: "fix/typo"}}}, first)
	stored = MergeWorktreeReport(stored, WorktreeReport{Machine: "desktop", Worktrees: []WorktreeReportEntry{{Name: "app", Branch: "main"}}}, first)
	want := []ReportedWorktree{
		{Machine: "desktop", Name: "app", Branch: "main", FirstSeenAt: first, LastSeenAt: first},
		{Machine: "laptop", Name: "app", Branch: "main", FirstSeenAt: first, LastSeenAt: first},
		{Machine: "laptop", Name: "app-fix", Branch: "fix/typo", FirstSeenAt: first, LastSeenAt: first},
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("stored = %+v", stored)
	}
	// The laptop removed one worktree, moved another to a new branch and
	// added one. The desktop's list is not touched by the laptop's report.
	stored = MergeWorktreeReport(stored, WorktreeReport{Machine: "laptop", Worktrees: []WorktreeReportEntry{{Name: "app", Branch: "feature"}, {Name: "app-new"}}}, second)
	want = []ReportedWorktree{
		{Machine: "desktop", Name: "app", Branch: "main", FirstSeenAt: first, LastSeenAt: first},
		{Machine: "laptop", Name: "app", Branch: "feature", FirstSeenAt: first, LastSeenAt: second},
		{Machine: "laptop", Name: "app-fix", Branch: "fix/typo", Removed: true, FirstSeenAt: first, LastSeenAt: first},
		{Machine: "laptop", Name: "app-new", FirstSeenAt: second, LastSeenAt: second},
	}
	if !reflect.DeepEqual(stored, want) {
		t.Fatalf("after the second list = %+v", stored)
	}
	// A folder made again under a removed name is present again and keeps
	// when it was first seen.
	stored = MergeWorktreeReport(stored, WorktreeReport{Machine: "laptop", Worktrees: []WorktreeReportEntry{{Name: "app-fix"}}}, second.Add(time.Hour))
	for _, worktree := range stored {
		if err := worktree.Validate(); err != nil {
			t.Errorf("%s/%s: %v", worktree.Machine, worktree.Name, err)
		}
		if worktree.Machine == "laptop" && worktree.Name == "app-fix" && (worktree.Removed || !worktree.FirstSeenAt.Equal(first) || worktree.Branch != "") {
			t.Errorf("a worktree made again: %+v", worktree)
		}
		if worktree.Machine == "laptop" && worktree.Name != "app-fix" && !worktree.Removed {
			t.Errorf("a worktree no longer listed is still present: %+v", worktree)
		}
	}
}
