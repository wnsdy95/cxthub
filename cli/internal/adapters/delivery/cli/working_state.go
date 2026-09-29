package cli

import (
	"fmt"
	"strings"

	"github.com/wnsdy95/cxthub/cli/internal/domain"
)

// Render only the versioned query result. These summaries never inspect provider
// files or interpret an empty capture list as proof of a clean working session.
func printWorkingState(s domain.WorkingState) {
	fmt.Printf("repository %s · worktree %s\n", s.Selection.RepoID, s.Selection.WorktreeID)
	fmt.Printf("Git: %s @ %s\n", s.Selection.GitBranch, s.Selection.GitCommit)
	fmt.Printf("context: %s %s @ %s\n", s.Selection.ContextMode, s.Selection.ContextBranch, shortHash(s.Selection.ContextSnapshot))
	if !s.Selection.CodeMatchesSelection {
		fmt.Println("selection differs from the current code; no position was changed")
	}
	fmt.Printf("memory: pinned=%t applied=%s observed=%s\n", s.Memory.Pinned, shortHash(s.Memory.AppliedHash), shortHash(s.Memory.ObservedAttachment))
	if s.Memory.AttachmentChanged {
		fmt.Println("a newer memory attachment is observed; the applied memory remains pinned")
	}
	if p := s.AppliedProjection; p != nil {
		fmt.Printf("applied projection: %s at Git %s; matches selection=%t (stored receipt, current server access unchecked)\n", p.ReceiptID, p.GitCommit, p.MatchesSelection)
	}
	fmt.Printf("staged: %d source(s), revision %s\n", len(s.Staged), s.IndexRevision)
	for _, e := range s.Staged {
		fmt.Printf("  %s %s %s events [%d,%d)\n", e.Key, e.Provider, e.SessionID, e.StartEvent, e.Events)
	}
	fmt.Printf("stored pending: %d repository observation(s); live activity unknown\n", len(s.Pending))
	fmt.Printf("local finalizations: %d; server acknowledgement unknown\n", len(s.LocalCommits))
	fmt.Printf("coverage: %s\n", s.Coverage)
	if len(s.Gaps) > 0 {
		fmt.Printf("gaps: %s\n", strings.Join(s.Gaps, ", "))
	}
}

func printContextDiff(d domain.ContextDiff) {
	fmt.Printf("diff: %s · %s\n", d.Mode, d.Coverage)
	for _, e := range d.Changes {
		fmt.Printf("%s %s: %s (%s)", e.Provider, e.SessionID, e.State, e.Baseline)
		if e.CountsKnown {
			fmt.Printf(" +[%d,%d) -[%d,%d)", e.Added.Start, e.Added.End, e.Removed.Start, e.Removed.End)
		}
		fmt.Println()
	}
	if len(d.Changes) == 0 {
		fmt.Println("(no changes in the stored observations; live provider activity was not checked)")
	}
	if len(d.Gaps) > 0 {
		fmt.Printf("gaps: %s\n", strings.Join(d.Gaps, ", "))
	}
}
