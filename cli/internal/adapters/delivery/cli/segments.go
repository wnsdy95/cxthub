package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/wnsdy95/cxthub-cli/internal/adapters/gitctx"
	"github.com/wnsdy95/cxthub-cli/internal/adapters/remotecfg"

	"github.com/wnsdy95/cxthub-cli/internal/domain"
	"github.com/wnsdy95/cxthub-cli/internal/ports/inbound"
)

// RunSegments prints the read-only session segment report for the repository
// holding cwd. It only reads the records coding agents and Git left behind.
func RunSegments(ctx context.Context, reporter inbound.SegmentReporter, cwd string, asJSON bool, w io.Writer) error {
	if reporter == nil {
		return fmt.Errorf("session record reader unavailable")
	}
	report, err := reporter.SegmentReport(ctx, cwd)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(w).Encode(report)
	}
	writeSegmentReport(w, report)
	return nil
}

func segmentCounts(c domain.SegmentSessionCounts) string {
	return fmt.Sprintf("%d (claude %d, claude sub %d, codex %d, codex sub %d)", c.Total, c.Claude, c.ClaudeSub, c.Codex, c.CodexSub)
}

func segmentRange(r domain.SegmentRange) string {
	return fmt.Sprintf("%s .. %s  %d records, %d commands", r.First, r.Last, r.Records, r.Commands)
}

func writeSegmentReport(w io.Writer, report domain.SegmentReport) {
	summary := report.Summary
	fmt.Fprintf(w, "Session segments for %s", report.Repository.Root)
	if report.Repository.Origin != "" {
		fmt.Fprintf(w, " (origin %s)", report.Repository.Origin)
	}
	fmt.Fprintf(w, " · worktrees: %d\n", report.Repository.Worktrees)
	fmt.Fprintln(w, "Read from the records coding agents and Git left on this computer. Nothing was written or uploaded.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Summary")
	fmt.Fprintf(w, "  sessions          %s\n", segmentCounts(summary.Sessions))
	fmt.Fprintf(w, "  with commits      %s\n", segmentCounts(summary.SessionsWithCommits))
	fmt.Fprintf(w, "  without commits   %s\n", segmentCounts(summary.SessionsWithoutCommits))
	fmt.Fprintf(w, "  segments          %s\n", segmentCounts(summary.Segments))
	fmt.Fprintf(w, "  segment endings   %d at one commit, %d at more than one\n", summary.SegmentsWithOneCommit, summary.SegmentsWithMoreCommits)
	fmt.Fprintf(w, "  linked commits    %d (%d by %s, %d by %s) · %d with one session, %d with more\n", summary.LinkedCommits,
		summary.LinkedByPrintedNumber, domain.SegmentLinkPrintedNumber, summary.LinkedByFolderAndTime, domain.SegmentLinkFolderAndTime,
		summary.CommitsWithOneSession, summary.CommitsWithMoreSessions)
	fmt.Fprintf(w, "  HEAD-log commits  %d made in a worktree, %d of them without a session\n", summary.HeadLogCommits, summary.HeadLogCommitsNoSession)
	fmt.Fprintf(w, "  copied records    %d counted once for their first session · %d copied sub-session files read once\n", summary.CopiedRecords, summary.SessionFileCopies)
	fmt.Fprintf(w, "  relations         %d spawns, %d agent calls\n", summary.Spawns, summary.AgentCalls)
	fmt.Fprintf(w, "  unread records    %d · %d uncommitted parts without a readable place\n", summary.UnreadRecords, summary.UnplacedUncommittedParts)

	if len(report.Unread) > 0 {
		fmt.Fprintf(w, "\nUnread session records (%d) — these exist but were not read; the report does not cover them\n", len(report.Unread))
		for _, unread := range report.Unread {
			tool := ""
			if unread.Provider != "" {
				tool = "[" + string(unread.Provider) + "] "
			}
			fmt.Fprintf(w, "  %s%s: %s\n", tool, unread.Source, unread.Reason)
		}
	}

	fmt.Fprintf(w, "\nSessions (%d)\n", len(report.Sessions))
	for _, session := range report.Sessions {
		kind := string(session.Provider)
		if session.Sub {
			kind += " sub"
		}
		fmt.Fprintf(w, "%s %s", kind, session.ID)
		if session.Parent != "" {
			fmt.Fprintf(w, "  spawned by %s", session.Parent)
		}
		fmt.Fprintf(w, "  %d records, %d commands", session.Records, session.Commands)
		if session.StartedAt != "" {
			fmt.Fprintf(w, "  %s", session.StartedAt)
			if session.EndedAt != "" {
				fmt.Fprintf(w, " .. %s", session.EndedAt)
			}
		}
		if session.Scope != "" {
			fmt.Fprintf(w, "  in this repository by %s", session.Scope)
		}
		fmt.Fprintln(w)
		if session.CopiedRecords > 0 {
			fmt.Fprintf(w, "  %d copied records belong to %s\n", session.CopiedRecords, strings.Join(session.CopiedFrom, ", "))
		}
		if session.FileCopies > 0 {
			fmt.Fprintf(w, "  %d identical copies of this session's file were read once\n", session.FileCopies)
		}
		for index, segment := range session.Segments {
			closing := ""
			if segment.ClosesTurn {
				closing = "  to the end of its turn"
			}
			fmt.Fprintf(w, "  segment %d  %s%s\n", index+1, segmentRange(segment.SegmentRange), closing)
			for _, commit := range segment.Commits {
				fmt.Fprintf(w, "    commit %s  linked by %s\n", commit.SHA, commit.Rule)
			}
		}
		if part := session.Uncommitted; part != nil {
			fmt.Fprintf(w, "  uncommitted  %s\n", segmentRange(part.SegmentRange))
			for _, place := range part.Places {
				branch := place.Branch
				if branch == "" {
					branch = "(no branch)"
				}
				removed := ""
				if place.Removed {
					removed = " (no longer a worktree)"
				}
				fmt.Fprintf(w, "    place %s%s @ %s  read from %s\n", place.Worktree, removed, branch, place.Rule)
			}
			if part.Unplaced {
				fmt.Fprintln(w, "    place unknown: no record names a folder of this repository")
			}
		}
	}

	fmt.Fprintf(w, "\nCommits (%d) — \"local\" is what this clone alone shows; pull-request commit lists are not read here\n", len(report.Commits))
	for _, commit := range report.Commits {
		fmt.Fprintf(w, "%s", commit.SHA)
		if commit.CreatedAt != "" {
			fmt.Fprintf(w, "  %s", commit.CreatedAt)
		}
		fmt.Fprintf(w, "  local: %s", commit.Local.State)
		if len(commit.Local.Copies) > 0 {
			fmt.Fprintf(w, " %s", strings.Join(commit.Local.Copies, ", "))
		}
		fmt.Fprintln(w)
		for _, root := range commit.MadeIn {
			fmt.Fprintf(w, "  made in %s\n", root)
		}
		for _, session := range commit.Sessions {
			fmt.Fprintf(w, "  session %s %s  linked by %s\n", session.Provider, session.ID, session.Rule)
		}
		if len(commit.Sessions) == 0 {
			fmt.Fprintln(w, "  no session")
		}
	}

	fmt.Fprintf(w, "\nSpawns (%d)\n", len(report.Spawns))
	for _, spawn := range report.Spawns {
		fmt.Fprintf(w, "%s %s -> %s", spawn.Provider, spawn.Parent, spawn.Child)
		switch {
		case !spawn.ParentInScope:
			fmt.Fprint(w, "  parent is not a session of this repository")
		case spawn.AtRecord == "":
			fmt.Fprint(w, "  point in the parent unknown")
		default:
			where := "uncommitted part"
			if spawn.ParentSegment > 0 {
				where = fmt.Sprintf("segment %d", spawn.ParentSegment)
			}
			fmt.Fprintf(w, "  at %s in the parent's %s, found by %s", spawn.AtRecord, where, spawn.Rule)
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintf(w, "\nAgent calls (%d)\n", len(report.AgentCalls))
	for _, call := range report.AgentCalls {
		fmt.Fprintf(w, "%s %s  %s -> %s", call.Provider, call.Tool, call.From, strings.Join(call.To, ", "))
		if call.Status != "" {
			fmt.Fprintf(w, "  %s", call.Status)
		}
		if call.At != "" {
			fmt.Fprintf(w, "  %s", call.At)
		}
		fmt.Fprintln(w)
	}
}

// RunSegmentsPush computes the same report and uploads one report per session
// to the repository's server. Every session is listed with what happened to
// it; a refused report makes the command fail after the others were stored.
func RunSegmentsPush(ctx context.Context, uploader inbound.SegmentUploader, cwd string, asJSON bool, w io.Writer) error {
	if uploader == nil {
		return fmt.Errorf("session report upload unavailable")
	}
	result, err := uploader.PushSegments(ctx, cwd)
	if err != nil {
		return err
	}
	if asJSON {
		if err := json.NewEncoder(w).Encode(result); err != nil {
			return err
		}
	} else {
		writeSegmentPushResult(w, result)
	}
	if result.Refused > 0 {
		return fmt.Errorf("%d of %d session reports were refused", result.Refused, len(result.Sessions))
	}
	return nil
}

func writeSegmentPushResult(w io.Writer, result domain.SegmentPushResult) {
	fmt.Fprintf(w, "Session reports: %d stored, %d unchanged, %d refused (%d sessions)\n", result.Stored, result.Unchanged, result.Refused, len(result.Sessions))
	withContent := 0
	for _, session := range result.Sessions {
		if session.Content == domain.SegmentContentStored {
			withContent++
		}
	}
	if withContent > 0 || result.ContentNotUploaded > 0 {
		fmt.Fprintf(w, "Conversation: uploaded for %d sessions, not uploaded for %d\n", withContent, result.ContentNotUploaded)
	}
	if worktrees := result.Worktrees; worktrees != nil {
		if worktrees.Reported {
			fmt.Fprintf(w, "Worktrees: %d listed by name, with the branch checked out in each\n", worktrees.Listed)
		} else {
			fmt.Fprintf(w, "Worktrees: the list of %d was not sent in this run: %s\n", worktrees.Listed, worktrees.NotReported)
		}
	}
	if branches := result.Branches; branches != nil {
		fmt.Fprintf(w, "Branch IDs: %d branches have one (%d new) · %d of %d on origin registered\n", branches.WithID, branches.Assigned, branches.Registered, branches.OnOrigin)
		if branches.NotAssigned != "" {
			fmt.Fprintf(w, "  IDs were not assigned in this run: %s\n", branches.NotAssigned)
		}
		if branches.NotRegistered != "" {
			fmt.Fprintf(w, "  IDs were not registered in this run: %s\n", branches.NotRegistered)
		}
		fmt.Fprintln(w, "Only you can read these reports on the server. On this computer only branch IDs in Git's branch configuration were written.")
	} else {
		fmt.Fprintln(w, "Only you can read these reports on the server. Nothing was written on this computer.")
	}
	if result.Refused > 0 {
		fmt.Fprintf(w, "\nRefused (%d) — these sessions are not on the server\n", result.Refused)
		for _, session := range result.Sessions {
			if session.State == domain.SegmentPushRefused {
				fmt.Fprintf(w, "  %s %s: %s\n", session.Provider, session.ID, session.Reason)
			}
		}
	}
	if result.ContentNotUploaded > 0 {
		fmt.Fprintf(w, "\nWithout conversation (%d) — these reports are on the server, their conversation is not\n", result.ContentNotUploaded)
		for _, session := range result.Sessions {
			if session.Content == domain.SegmentContentNotUploaded {
				fmt.Fprintf(w, "  %s %s: %s\n", session.Provider, session.ID, session.ContentReason)
			}
		}
	}
	if len(result.Unread) > 0 {
		fmt.Fprintf(w, "\nUnread session records (%d) — these exist but were not read, so no report covers them\n", len(result.Unread))
		for _, unread := range result.Unread {
			tool := ""
			if unread.Provider != "" {
				tool = "[" + string(unread.Provider) + "] "
			}
			fmt.Fprintf(w, "  %s%s: %s\n", tool, unread.Source, unread.Reason)
		}
	}
	if result.AgentCallsWithoutSender > 0 {
		fmt.Fprintf(w, "\n%d agent calls were recorded by a session outside this repository and were not uploaded\n", result.AgentCallsWithoutSender)
	}
}

// segmentCollectorInterval is the least time between the starts of two
// automatic runs in one repository. After a run that failed for a reason that
// holds for every session, the next one waits segmentCollectorBackoff.
var (
	segmentCollectorInterval = 60 * time.Second
	segmentCollectorBackoff  = 10 * time.Minute
)

// SpawnSegmentCollector starts a detached collector for the repository
// holding cwd and returns at once. Hooks call it after their own work: a hook
// is only what wakes the collector, never what it reads.
func SpawnSegmentCollector(cwd string) {
	root := cxtRepoRoot(context.Background(), cwd)
	if !remotecfg.SegmentsAuto(root) {
		return
	}
	if _, ok := remotecfg.Origin(root); !ok && os.Getenv("CXT_REMOTE") == "" {
		return
	}
	// A collector already running takes the request; no new process is needed.
	if gitctx.NewCollectorFiles().RequestRunIfBusy(context.Background(), cwd) {
		return
	}
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, "git-hook", "segment-collect")
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// segmentCollectorWait is how long a collector waits before it may start a
// run, given how the last one ended.
func segmentCollectorWait(state domain.CollectorState, now time.Time) time.Duration {
	if state.LastRun == nil {
		return 0
	}
	last, err := time.Parse(time.RFC3339Nano, state.LastRun.At)
	if err != nil || last.After(now) {
		return 0
	}
	interval := segmentCollectorInterval
	if state.LastRun.Error != "" {
		interval = segmentCollectorBackoff
	}
	if wait := interval - now.Sub(last); wait > 0 {
		return wait
	}
	return 0
}

// runSegmentCollector is the detached collector. One runs per repository; a
// collector woken while another runs leaves a request, and the running one
// answers it with one more run. It never fails its caller: how a run ended is
// kept in the collector's state.
func runSegmentCollector(ctx context.Context, c *Container, cwd string) error {
	if c == nil || c.SegmentCollector == nil || c.WithSegmentCollectorLock == nil {
		return nil
	}
	root := cxtRepoRoot(ctx, cwd)
	if !remotecfg.SegmentsAuto(root) {
		return nil
	}
	if _, ok := remotecfg.Origin(root); !ok && os.Getenv("CXT_REMOTE") == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	_, _ = c.WithSegmentCollectorLock(ctx, cwd, func(another func() bool) error {
		for {
			if c.SegmentCollectorState != nil {
				if state, err := c.SegmentCollectorState(ctx, cwd); err == nil {
					if wait := segmentCollectorWait(state, time.Now()); wait > 0 {
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(wait):
						}
					}
				}
			}
			_, _ = c.SegmentCollector.CollectSegments(ctx, cwd)
			if ctx.Err() != nil || !another() {
				return nil
			}
		}
	})
	return nil
}
