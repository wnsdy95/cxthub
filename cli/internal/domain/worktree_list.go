package domain

import (
	"path/filepath"
	"sort"
)

// WorktreeRegistration is one worktree of this computer as the server is
// told about it: the folder's name and the branch checked out in it. The
// folder's path is never sent.
type WorktreeRegistration struct {
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
}

// WorktreeRegistrations lists the repository's worktrees by folder name. Two
// folders with one name cannot be told apart by name alone, so the name is
// listed once: for the main worktree if it is one of them, otherwise for the
// folder whose path sorts first.
func WorktreeRegistrations(worktrees []RecordedWorktree) []WorktreeRegistration {
	ordered := append([]RecordedWorktree(nil), worktrees...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].Main != ordered[j].Main {
			return ordered[i].Main
		}
		return ordered[i].Root < ordered[j].Root
	})
	seen := map[string]struct{}{}
	out := []WorktreeRegistration{}
	for _, worktree := range ordered {
		name := filepath.Base(worktree.Root)
		if name == "" || name == "." || name == string(filepath.Separator) {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, WorktreeRegistration{Name: name, Branch: worktree.Current})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// SegmentPushWorktrees says what a run did about the worktree list. Like the
// branch IDs it never stops the sessions: a failure is said here and the
// next run tries again.
type SegmentPushWorktrees struct {
	// Listed counts the worktrees of this computer; Reported says the server
	// now holds that list.
	Listed   int  `json:"listed"`
	Reported bool `json:"reported"`
	// NotReported says why the list was not sent.
	NotReported string `json:"not_reported,omitempty"`
}
