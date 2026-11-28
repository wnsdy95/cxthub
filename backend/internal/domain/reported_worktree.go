package domain

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A reported worktree is a working folder of the repository on one of a
// member's machines, as that member's CLI listed it. Only the folder's name,
// the branch checked out in it and the machine's label are reported, never a
// path, and only the member who reported them sees them. A worktree that is
// gone from a later list keeps its name and is marked removed.

// ReportedWorktreeLimit bounds one machine's list.
const ReportedWorktreeLimit = 2000

// WorktreeReportEntry is one worktree in a machine's list.
type WorktreeReportEntry struct {
	Name   string `json:"name"`
	Branch string `json:"branch,omitempty"`
}

// WorktreeReport is the whole list of one machine.
type WorktreeReport struct {
	Machine   string                `json:"machine,omitempty"`
	Worktrees []WorktreeReportEntry `json:"worktrees"`
}

// ReportedWorktree is a stored worktree of one member.
type ReportedWorktree struct {
	Machine     string    `json:"machine,omitempty"`
	Name        string    `json:"name"`
	Branch      string    `json:"branch,omitempty"`
	Removed     bool      `json:"removed"`
	FirstSeenAt time.Time `json:"first_seen_at"`
	LastSeenAt  time.Time `json:"last_seen_at"`
}

func validWorktreeName(name string) bool {
	return anchoredText(name, anchoredNameLimit, true) && !strings.ContainsAny(name, `/\`)
}

// Validate checks a list a CLI sent: plain names, no path, each name once.
func (r WorktreeReport) Validate() error {
	invalid := func(what string) error { return fmt.Errorf("%w: worktree list: %s", ErrValidation, what) }
	if !anchoredText(r.Machine, anchoredNameLimit, false) {
		return invalid("machine label is not plain text")
	}
	if len(r.Worktrees) > ReportedWorktreeLimit {
		return invalid("too many worktrees")
	}
	seen := map[string]struct{}{}
	for _, worktree := range r.Worktrees {
		if !validWorktreeName(worktree.Name) || !anchoredText(worktree.Branch, anchoredNameLimit, false) {
			return invalid("a worktree needs a name, not a path")
		}
		if _, dup := seen[worktree.Name]; dup {
			return invalid("a name is listed twice")
		}
		seen[worktree.Name] = struct{}{}
	}
	return nil
}

// Validate checks a stored worktree.
func (w ReportedWorktree) Validate() error {
	if !validWorktreeName(w.Name) || !anchoredText(w.Machine, anchoredNameLimit, false) || !anchoredText(w.Branch, anchoredNameLimit, false) ||
		w.FirstSeenAt.IsZero() || w.LastSeenAt.Before(w.FirstSeenAt) {
		return fmt.Errorf("%w: stored worktree", ErrValidation)
	}
	return nil
}

// MergeWorktreeReport applies one machine's list to what a member has stored.
// A listed worktree is present with its branch as of now; a stored worktree of
// that machine that is no longer listed is marked removed and keeps what was
// known; worktrees of other machines are left as they are.
func MergeWorktreeReport(stored []ReportedWorktree, report WorktreeReport, at time.Time) []ReportedWorktree {
	at = at.UTC()
	listed := make(map[string]WorktreeReportEntry, len(report.Worktrees))
	for _, worktree := range report.Worktrees {
		listed[worktree.Name] = worktree
	}
	merged := make([]ReportedWorktree, 0, len(stored)+len(report.Worktrees))
	for _, worktree := range stored {
		if worktree.Machine == report.Machine {
			if entry, present := listed[worktree.Name]; present {
				worktree.Branch, worktree.Removed, worktree.LastSeenAt = entry.Branch, false, at
				delete(listed, worktree.Name)
			} else {
				worktree.Removed = true
			}
		}
		merged = append(merged, worktree)
	}
	for _, entry := range listed {
		merged = append(merged, ReportedWorktree{Machine: report.Machine, Name: entry.Name, Branch: entry.Branch, FirstSeenAt: at, LastSeenAt: at})
	}
	SortReportedWorktrees(merged)
	return merged
}

// SortReportedWorktrees orders a member's worktrees by machine, then name.
func SortReportedWorktrees(worktrees []ReportedWorktree) {
	sort.Slice(worktrees, func(left, right int) bool {
		if worktrees[left].Machine != worktrees[right].Machine {
			return worktrees[left].Machine < worktrees[right].Machine
		}
		return worktrees[left].Name < worktrees[right].Name
	})
}
