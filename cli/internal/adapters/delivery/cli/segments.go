package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
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
//
// With a tool and a session identifier it uploads that session alone; a
// session the tools' records on this computer do not hold makes it fail.
func RunSegmentsPush(ctx context.Context, uploader inbound.SegmentUploader, cwd string, session []string, asJSON bool, w io.Writer) error {
	if uploader == nil {
		return fmt.Errorf("session report upload unavailable")
	}
	var result domain.SegmentPushResult
	var err error
	if len(session) == 2 {
		result, err = uploader.PushSegmentsOf(ctx, cwd, []domain.SessionUploadKey{{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}})
	} else {
		result, err = uploader.PushSegments(ctx, cwd)
	}
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
	if len(result.NotFound) > 0 {
		return fmt.Errorf("%s %s is not among the sessions the tools recorded for this repository on this computer", result.NotFound[0].Provider, result.NotFound[0].SessionID)
	}
	return nil
}

func writeSegmentPushResult(w io.Writer, result domain.SegmentPushResult) {
	removed := ""
	if result.Removed > 0 {
		removed = fmt.Sprintf(", %d removed from the server", result.Removed)
	}
	fmt.Fprintf(w, "Session reports: %d stored, %d unchanged, %d refused%s (%d sessions)\n", result.Stored, result.Unchanged, result.Refused, removed, len(result.Sessions))
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
	}
	if records := result.LocalRecords; records != nil {
		fmt.Fprintf(w, "Copies: what was sent for %d sessions is kept under .cxt/sessions, each with a log of its uploads\n", records.Kept)
		if records.NotKept != "" {
			fmt.Fprintf(w, "  a copy was not written in this run: %s\n", records.NotKept)
		}
	}
	fmt.Fprintln(w, "On the server a report is read by you alone until what it explains is pushed; a pushed part is read by whoever may view the repository.")
	switch {
	case result.Branches != nil && result.LocalRecords != nil:
		fmt.Fprintln(w, "On this computer, branch IDs were written into Git's branch configuration and the copies into .cxt/sessions.")
	case result.Branches != nil:
		fmt.Fprintln(w, "On this computer only branch IDs in Git's branch configuration were written.")
	case result.LocalRecords != nil:
		fmt.Fprintln(w, "On this computer only the copies in .cxt/sessions were written.")
	default:
		fmt.Fprintln(w, "Nothing was written on this computer.")
	}
	if result.Refused > 0 {
		fmt.Fprintf(w, "\nRefused (%d) — these sessions are not on the server\n", result.Refused)
		for _, session := range result.Sessions {
			if session.State == domain.SegmentPushRefused {
				fmt.Fprintf(w, "  %s %s: %s\n", session.Provider, session.ID, session.Reason)
			}
		}
	}
	if result.Removed > 0 {
		fmt.Fprintf(w, "\nRemoved (%d) — these sessions were removed from the server and nothing of them was sent\n", result.Removed)
		for _, session := range result.Sessions {
			if session.State == domain.SegmentPushRemoved {
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

// RunSegmentsLog prints what the repository's .cxt folder keeps about the
// uploads: every session with its last log line, or, when a tool and a
// session identifier are given, that session's whole log. It reads only.
func RunSegmentsLog(ctx context.Context, care inbound.SegmentCare, cwd string, session []string, asJSON bool, w io.Writer) error {
	if care == nil {
		return fmt.Errorf("upload records unavailable")
	}
	var key *domain.SessionUploadKey
	if len(session) == 2 {
		key = &domain.SessionUploadKey{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}
	}
	listings, err := care.UploadLog(ctx, cwd, key)
	if err != nil {
		return err
	}
	if asJSON {
		if listings == nil {
			listings = []domain.SessionUploadListing{}
		}
		return json.NewEncoder(w).Encode(struct {
			Version  int                           `json:"version"`
			Sessions []domain.SessionUploadListing `json:"sessions"`
		}{1, listings})
	}
	if key == nil {
		fmt.Fprintf(w, "Upload records kept under .cxt/sessions (%d sessions)\n", len(listings))
		if len(listings) == 0 {
			fmt.Fprintln(w, "  none yet: cxt segments push, or the collector, writes them when it uploads")
		}
	}
	for _, listing := range listings {
		fmt.Fprintf(w, "%s %s\n", listing.Provider, listing.SessionID)
		if listing.ReportKept {
			fmt.Fprintf(w, "  report copy        %d segments, %d records\n", listing.Segments, listing.Records)
		} else {
			fmt.Fprintln(w, "  report copy        none")
		}
		switch {
		case listing.ConversationKept && listing.Doc != "":
			fmt.Fprintf(w, "  conversation copy  kept; the report names %d events in %d documents, the last %s\n", listing.Events, listing.Parts, listing.Doc)
		case listing.ConversationKept:
			fmt.Fprintln(w, "  conversation copy  kept; the report names no conversation")
		default:
			fmt.Fprintln(w, "  conversation copy  none")
		}
		if key == nil && listing.Lines > len(listing.Log) {
			fmt.Fprintf(w, "  last of %d log lines:\n", listing.Lines)
		}
		for _, entry := range listing.Log {
			fmt.Fprintf(w, "  %s  %s\n", entry.At, segmentLogLine(entry))
		}
	}
	return nil
}

// RunSegmentsRemove removes a session from the server for every reader, with
// the sessions it spawned. Without confirmed it removes nothing and fails,
// saying what it would do, so that nothing is removed by a command that was
// only tried.
func RunSegmentsRemove(ctx context.Context, care inbound.SegmentCare, cwd string, session []string, confirmed, asJSON bool, w io.Writer) error {
	if care == nil {
		return fmt.Errorf("the repository's server is unavailable")
	}
	if len(session) != 2 {
		return fmt.Errorf("name the session by its tool and its identifier")
	}
	key := domain.SessionUploadKey{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}
	if !confirmed {
		return fmt.Errorf("nothing was removed. cxt segments remove %s %s --yes removes this session from the server for every reader: its report, its summaries, its conversation, and the sessions it spawned. "+
			"Nothing on this computer is removed. It is uploaded again only after cxt segments allow %s %s", key.Provider, key.SessionID, key.Provider, key.SessionID)
	}
	result, err := care.RemoveSession(ctx, cwd, key)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(w).Encode(result)
	}
	fmt.Fprintf(w, "Removed from the server (%d sessions): the report, the summaries and the conversation of each\n", len(result.Removed))
	for _, removed := range result.Removed {
		fmt.Fprintf(w, "  %s %s\n", removed.Provider, removed.SessionID)
	}
	fmt.Fprintln(w, "Nothing on this computer was removed: the tool's records and the copies under .cxt/sessions stay.")
	fmt.Fprintln(w, "The server takes none of these sessions again until it is allowed again, one session at a time:")
	for _, removed := range result.Removed {
		fmt.Fprintf(w, "  cxt segments allow %s %s\n", removed.Provider, removed.SessionID)
	}
	return nil
}

// RunSegmentsAllow takes back the removal of a session and uploads it again.
func RunSegmentsAllow(ctx context.Context, care inbound.SegmentCare, cwd string, session []string, asJSON bool, w io.Writer) error {
	if care == nil {
		return fmt.Errorf("the repository's server is unavailable")
	}
	if len(session) != 2 {
		return fmt.Errorf("name the session by its tool and its identifier")
	}
	key := domain.SessionUploadKey{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}
	result, err := care.AllowSession(ctx, cwd, key)
	if err != nil {
		return err
	}
	if asJSON {
		if err := json.NewEncoder(w).Encode(result); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(w, "%s %s is allowed on the server again.\n", key.Provider, key.SessionID)
		switch {
		case len(result.Upload.NotFound) > 0:
			fmt.Fprintln(w, "It was not uploaded: the tool's records of this session are no longer on this computer.")
		case result.Upload.Refused > 0:
			for _, session := range result.Upload.Sessions {
				fmt.Fprintf(w, "It was not uploaded: %s\n", session.Reason)
			}
		default:
			for _, session := range result.Upload.Sessions {
				fmt.Fprintf(w, "Uploaded again: report %s", session.State)
				if session.Revision > 0 {
					fmt.Fprintf(w, " (revision %d)", session.Revision)
				}
				if session.Content == domain.SegmentContentStored {
					fmt.Fprint(w, ", with its conversation")
				}
				if session.Summaries > 0 {
					fmt.Fprintf(w, ", %d summaries", session.Summaries)
				}
				fmt.Fprintln(w)
			}
		}
	}
	if len(result.Upload.NotFound) > 0 || result.Upload.Refused > 0 {
		return fmt.Errorf("%s %s is allowed again but was not uploaded", key.Provider, key.SessionID)
	}
	return nil
}

// RunSegmentsDoctor reads this computer and the server and prints what does
// not fit, each with why it matters and the command that puts it right. It
// changes nothing, and it does not fail because it found something: a
// finding is its answer.
func RunSegmentsDoctor(ctx context.Context, care inbound.SegmentCare, cwd string, asJSON bool, w io.Writer) error {
	if care == nil {
		return fmt.Errorf("diagnosis unavailable")
	}
	diagnosis, err := care.Diagnose(ctx, cwd)
	if err != nil {
		return err
	}
	if asJSON {
		return json.NewEncoder(w).Encode(diagnosis)
	}
	notes := len(diagnosis.Findings) - diagnosis.Problems
	fmt.Fprintf(w, "Session upload check: %d problems, %d things to know\n", diagnosis.Problems, notes)
	fmt.Fprintf(w, "Looked at: %s\n", strings.Join(diagnosis.Checked, ", "))
	if len(diagnosis.Findings) == 0 {
		fmt.Fprintln(w, "Everything that was looked at fits.")
		return nil
	}
	for _, problem := range []bool{true, false} {
		title := "\nTo know"
		if problem {
			title = "\nProblems"
		}
		printed := false
		for _, finding := range diagnosis.Findings {
			if finding.Problem != problem {
				continue
			}
			if !printed {
				fmt.Fprintln(w, title)
				printed = true
			}
			fmt.Fprintf(w, "  [%s] %s\n", finding.Code, finding.What)
			if finding.Why != "" {
				fmt.Fprintf(w, "      why: %s\n", finding.Why)
			}
			if finding.Fix != "" {
				fmt.Fprintf(w, "      fix: %s\n", finding.Fix)
			}
		}
	}
	return nil
}

// RunSegmentsRefresh asks the server to read the repository from GitHub
// again soon.
func RunSegmentsRefresh(ctx context.Context, care inbound.SegmentCare, cwd string, w io.Writer) error {
	if care == nil {
		return fmt.Errorf("the repository's server is unavailable")
	}
	if err := care.RefreshRemote(ctx, cwd); err != nil {
		return err
	}
	fmt.Fprintln(w, "The server was asked to read the repository from GitHub again. It does so within about a minute; a large repository takes several passes.")
	fmt.Fprintln(w, "cxt segments doctor says afterwards whether a push is seen.")
	return nil
}

// RunSegmentsArchive archives a session on the server or restores it.
func RunSegmentsArchive(ctx context.Context, care inbound.SegmentCare, cwd string, session []string, archived bool, w io.Writer) error {
	if care == nil {
		return fmt.Errorf("the repository's server is unavailable")
	}
	if len(session) != 2 {
		return fmt.Errorf("name the session by its tool and its identifier")
	}
	key := domain.SessionUploadKey{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}
	if err := care.SetArchived(ctx, cwd, key, archived); err != nil {
		return err
	}
	if archived {
		fmt.Fprintf(w, "%s %s is archived on the server: it is left out of commits, the search and the summaries for every reader, and nothing of it is removed.\n", key.Provider, key.SessionID)
		fmt.Fprintf(w, "cxt segments unarchive %s %s brings it back.\n", key.Provider, key.SessionID)
		return nil
	}
	fmt.Fprintf(w, "%s %s is restored from the archive on the server.\n", key.Provider, key.SessionID)
	return nil
}

// RunSegmentsResend sends the copy kept under .cxt/sessions for a session
// again. It is for a session the tool no longer holds.
func RunSegmentsResend(ctx context.Context, uploader inbound.SegmentUploader, cwd string, session []string, asJSON bool, w io.Writer) error {
	if uploader == nil {
		return fmt.Errorf("session report upload unavailable")
	}
	if len(session) != 2 {
		return fmt.Errorf("name the session by its tool and its identifier")
	}
	key := domain.SessionUploadKey{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}
	result, err := uploader.ResendSession(ctx, cwd, key)
	if err != nil {
		return err
	}
	if asJSON {
		if err := json.NewEncoder(w).Encode(result); err != nil {
			return err
		}
	}
	for _, sent := range result.Sessions {
		switch sent.State {
		case domain.SegmentPushRemoved:
			if !asJSON {
				fmt.Fprintf(w, "Nothing was sent: %s\n", sent.Reason)
			}
			return fmt.Errorf("%s %s is removed from the server", key.Provider, key.SessionID)
		case domain.SegmentPushRefused:
			return fmt.Errorf("the server refused the copy of %s %s: %s", key.Provider, key.SessionID, sent.Reason)
		}
		if !asJSON {
			fmt.Fprintf(w, "Sent again from the copy under .cxt/sessions: report %s (revision %d)", sent.State, sent.Revision)
			if sent.Content == domain.SegmentContentStored {
				fmt.Fprint(w, ", with its conversation")
			}
			if sent.Summaries > 0 {
				fmt.Fprintf(w, ", %d summaries written again", sent.Summaries)
			}
			fmt.Fprintln(w)
			if sent.SummaryReason != "" {
				fmt.Fprintf(w, "A summary was not stored: %s\n", sent.SummaryReason)
			}
		}
	}
	return nil
}

// RunSegmentsFork starts new work from a commit. Without a session it makes
// a Git branch at the commit. With a session it continues that session's
// conversation, up to the commit when one is given, as a new session of a
// tool in the folder, and also makes the branch when one is named.
func RunSegmentsFork(ctx context.Context, forker inbound.SessionForker, cwd string, session []string, at, branch, to string, asJSON bool, w io.Writer) error {
	if forker == nil {
		return fmt.Errorf("fork unavailable")
	}
	in := inbound.SessionForkInput{Cwd: cwd, At: at, Branch: branch, To: domain.ProviderKind(to)}
	if len(session) == 2 {
		in.Session = &domain.SessionUploadKey{Provider: domain.ProviderKind(session[0]), SessionID: session[1]}
	}
	result, err := forker.Fork(ctx, in)
	if asJSON && (err == nil || result.ResumeCommand != "") {
		if encodeErr := json.NewEncoder(w).Encode(result); encodeErr != nil {
			return encodeErr
		}
		return err
	}
	if result.ResumeCommand != "" {
		fmt.Fprintf(w, "A new %s session continues %s %s", result.Tool, result.Session.Provider, result.Session.SessionID)
		if result.Commit != "" {
			fmt.Fprintf(w, " up to commit %s", result.Commit)
		}
		fmt.Fprintf(w, ": %d events of its conversation.\n", result.Events)
		if !result.SameTool {
			fmt.Fprintf(w, "The conversation was recorded by %s and is rebuilt for %s; what only %s can read of it is not carried over.\n", result.Session.Provider, result.Tool, result.Session.Provider)
		}
		switch result.OmittedAs {
		case "summary":
			fmt.Fprintf(w, "Its first %d events did not fit the tool's context and are left out; the summary the session has at the commit stands in for them.\n", result.Omitted)
		case "note":
			fmt.Fprintf(w, "Its first %d events did not fit the tool's context and are left out; no summary of them was found, and a line in the new session says so.\n", result.Omitted)
		}
		if result.NotReadable > 0 {
			fmt.Fprintf(w, "%d events up to there are not open to this account and are not in the new session.\n", result.NotReadable)
		}
		fmt.Fprintf(w, "Start it in this folder with:\n  %s\n", result.ResumeCommand)
	}
	if result.Branch != "" {
		fmt.Fprintf(w, "The Git branch %s was made at commit %s. No folder was switched to it:\n  git switch %s\n", result.Branch, result.Commit, result.Branch)
		if result.ResumeCommand == "" {
			fmt.Fprintln(w, "A session started on it is given the most recent summary along the branch.")
		}
	}
	return err
}

// segmentLogLine says one log entry in words.
func segmentLogLine(entry domain.SessionUploadLogEntry) string {
	run := "requested"
	if entry.Automatic {
		run = "automatic"
	}
	if entry.Action != "" {
		line := entry.Action
		if entry.Reason != "" {
			line += ": " + entry.Reason
		}
		return line
	}
	parts := []string{}
	switch {
	case entry.Report == domain.SegmentPushRefused:
		parts = append(parts, "report refused: "+entry.Reason)
	case entry.Report != "" && entry.Revision > 0:
		parts = append(parts, fmt.Sprintf("report %s (revision %d)", entry.Report, entry.Revision))
	case entry.Report != "":
		parts = append(parts, "report "+string(entry.Report))
	}
	switch entry.Content {
	case domain.SegmentContentStored:
		parts = append(parts, fmt.Sprintf("conversation of %d events", entry.Events))
	case domain.SegmentContentNotUploaded:
		parts = append(parts, "conversation not uploaded: "+entry.ContentReason)
	}
	if entry.Summaries > 0 {
		parts = append(parts, fmt.Sprintf("%d summaries stored", entry.Summaries))
	}
	if entry.SummaryReason != "" {
		parts = append(parts, "a summary was not stored: "+entry.SummaryReason)
	}
	return run + "  " + strings.Join(parts, " · ")
}

// segmentCollectorInterval is the least time between the starts of two
// automatic runs in one repository. A run that took long is followed by a
// longer pause: segmentCollectorShare times what it took, so the collector
// works for at most about one part in segmentCollectorShare of the time. A
// run reads a session that went on from its beginning, which was measured at
// about a second for a 6 MB session file and ten seconds for an 80 MB one.
// After a run that failed for a reason that holds for every session, and at
// most in any case, the next one waits segmentCollectorBackoff.
//
// A push of this clone is not held back by any of this. What was pushed is
// what the team is shown, and pushes are few: when one has ended, the pause
// ends too, and while a collector waits it looks for that every
// segmentCollectorPushPoll.
var (
	segmentCollectorInterval = 10 * time.Second
	segmentCollectorBackoff  = 10 * time.Minute
	segmentCollectorPushPoll = 500 * time.Millisecond
)

const segmentCollectorShare = 6

// SpawnSegmentCollector starts a detached collector for the repository
// holding cwd and returns at once. Hooks call it after their own work: a hook
// is only what wakes the collector, never what it reads.
func SpawnSegmentCollector(cwd string) {
	if !segmentCollectorWanted(cwd) {
		return
	}
	// A collector already running takes the request and runs once more after
	// its pause; no new process is needed.
	if gitctx.NewCollectorFiles().RequestRunIfBusy(context.Background(), cwd) {
		return
	}
	spawnDetached(cwd, "git-hook", "segment-collect")
}

// segmentCollectorWanted reports whether automatic reading is on in the
// repository holding cwd and there is a server to upload to.
func segmentCollectorWanted(cwd string) bool {
	root := cxtRepoRoot(context.Background(), cwd)
	if !remotecfg.SegmentsAuto(root) {
		return false
	}
	_, ok := remotecfg.Origin(root)
	return ok || os.Getenv("CXT_REMOTE") != ""
}

// spawnDetached starts this program again with args, in a session of its
// own, and does not wait for it: what a hook starts must outlive the hook.
func spawnDetached(cwd string, args ...string) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}

// wakeAfterPush is the command of the wait for a push to end.
const wakeAfterPush = "after-push"

// A push is watched until it ends, and not for ever: one that is still going
// after wakeAfterPushLimit is left to the next wake.
var (
	wakeAfterPushPoll  = 200 * time.Millisecond
	wakeAfterPushLimit = 30 * time.Minute
)

// SpawnWakeAfterPush starts the detached wait for the process pushing to
// end. The pre-push hook calls it with the Git process that runs the hook:
// Git runs that hook before the push and has no hook after it, and what a
// push changes in this clone, origin's branches, is there to read only when
// that process has ended.
func SpawnWakeAfterPush(cwd string, pushing int) {
	if !segmentCollectorWanted(cwd) {
		return
	}
	spawnDetached(cwd, "git-hook", wakeAfterPush, strconv.Itoa(pushing))
}

// processRuns reports whether a process with this number exists. A process
// of another user exists too: asking about it is refused, not unanswered.
func processRuns(process int) bool {
	err := syscall.Kill(process, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// runWakeAfterPush is the detached wait. It waits until the process that
// pushes has ended and then wakes the collector like any hook: it reads
// nothing and decides nothing, and whether the push changed anything is for
// the collector to find. It holds no lock, so collectors other hooks wake run
// while it waits.
func runWakeAfterPush(ctx context.Context, c *Container, cwd, pushing string) error {
	process, err := strconv.Atoi(pushing)
	if err != nil || process <= 1 || c == nil || c.WakeSegmentCollector == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, wakeAfterPushLimit)
	defer cancel()
	for processRuns(process) && ctx.Err() == nil {
		select {
		case <-ctx.Done():
		case <-time.After(wakeAfterPushPoll):
		}
	}
	// The push is noted before the collector is woken, so that the run this
	// wake leads to finds it.
	if c.NoteSegmentCollectorPush != nil {
		c.NoteSegmentCollectorPush(context.WithoutCancel(ctx), cwd)
	}
	c.WakeSegmentCollector(cwd)
	return nil
}

// segmentCollectorPause waits out the pause before a run, and ends it when a
// push of this clone has ended. It takes the note of a push in either case:
// the run that follows answers it.
func segmentCollectorPause(ctx context.Context, wait time.Duration, pushed func() bool) error {
	for arrived := pushed(); wait > 0 && !arrived; arrived = pushed() {
		step := min(wait, segmentCollectorPushPoll)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(step):
		}
		wait -= step
	}
	return nil
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
	if took := time.Duration(state.LastRun.Millis) * time.Millisecond; took > 0 && took*segmentCollectorShare > interval {
		interval = took * segmentCollectorShare
	}
	if state.LastRun.Error != "" || interval > segmentCollectorBackoff {
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
	// The collector, like the hooks that wake it, acts only where cxt was set up.
	if state := gitctx.InspectContextRoot(ctx, cwd); !state.GitRepository || !state.Initialized {
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
	pushed := func() bool { return c.TakeSegmentCollectorPush != nil && c.TakeSegmentCollectorPush(ctx, cwd) }
	_, _ = c.WithSegmentCollectorLock(ctx, cwd, func(another func() bool) error {
		for {
			wait := time.Duration(0)
			if c.SegmentCollectorState != nil {
				if state, err := c.SegmentCollectorState(ctx, cwd); err == nil {
					wait = segmentCollectorWait(state, time.Now())
				}
			}
			if err := segmentCollectorPause(ctx, wait, pushed); err != nil {
				return err
			}
			_, _ = c.SegmentCollector.CollectSegments(ctx, cwd)
			if ctx.Err() != nil {
				return nil
			}
			if !another() {
				return nil
			}
		}
	})
	return nil
}
