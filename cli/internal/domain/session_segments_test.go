package domain

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// All records below are synthetic. Times are milliseconds on an arbitrary day.
const segmentDay = int64(1_800_000_000_000)

func segmentSHA(letter string) string { return strings.Repeat(letter, 40) }

func segmentAt(seconds int64) int64 { return segmentDay + seconds*1000 }

// segmentCommit is a commit object created at the given second.
func segmentCommit(letter string, second int64) RecordedCommit {
	return RecordedCommit{SHA: segmentSHA(letter), CreatedSec: segmentAt(second) / 1000, AuthoredSec: segmentAt(second) / 1000, AuthorEmail: "dev@example.test", Subject: "change " + letter}
}

// segmentCommand is an item that completes a shell command running from start
// to end, printing the given text.
func segmentCommand(id, turn string, start, end int64, folder, output string) RecordedItem {
	return RecordedItem{ID: id, Turn: turn, AtMs: segmentAt(end), Folder: folder, Commands: []RecordedCommand{{
		StartedMs: segmentAt(start), EndedMs: segmentAt(end), Folder: folder, Tokens: CommitNumberTokens(output),
	}}}
}

func segmentMessage(id, turn string, second int64, folder string) RecordedItem {
	return RecordedItem{ID: id, Turn: turn, AtMs: segmentAt(second), Folder: folder}
}

func segmentSession(t *testing.T, report SegmentReport, id string) SegmentedSession {
	t.Helper()
	for _, session := range report.Sessions {
		if session.ID == id {
			return session
		}
	}
	t.Fatalf("session %q is not in the report", id)
	return SegmentedSession{}
}

func segmentCommitEntry(t *testing.T, report SegmentReport, sha string) SegmentCommit {
	t.Helper()
	for _, commit := range report.Commits {
		if commit.SHA == sha {
			return commit
		}
	}
	t.Fatalf("commit %s is not in the report", sha[:7])
	return SegmentCommit{}
}

func segmentRanges(session SegmentedSession) [][2]string {
	var out [][2]string
	for _, segment := range session.Segments {
		out = append(out, [2]string{segment.First, segment.Last})
	}
	return out
}

var segmentMain = RecordedWorktree{Root: "/work/app", Main: true, Current: "main"}

func TestCommitNumberTokens(t *testing.T) {
	text := "[main abc1234] change\n" +
		"def5678 at line start\tabc1234 again\n" +
		"too short abc123, upper ABCDEF1, mixed abc12g4, glued xabc1234 abc1234_\n" +
		strings.Repeat("a", 40) + " " + strings.Repeat("b", 41) + "\n" +
		"range 1111111..2222222 é3333333 …4444444"
	got := CommitNumberTokens(text)
	want := []string{"abc1234", "def5678", strings.Repeat("a", 40), "1111111", "2222222", "4444444"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tokens = %q, want %q", got, want)
	}
	if got := CommitNumberTokens(""); got != nil {
		t.Fatalf("empty text gave %q", got)
	}
}

func TestSegmentLinksAPrintedCommitNumberWithinTheCommandTime(t *testing.T) {
	for _, tc := range []struct {
		name    string
		created int64
		linked  bool
	}{
		{"two seconds before the command started", 98, true},
		{"earlier than the slack before", 97, false},
		{"while the command ran", 105, true},
		{"three seconds after the command ended", 113, true},
		{"later than the slack after", 114, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report := BuildSegmentReport(SegmentInputs{
				Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", tc.created)}},
				Sessions: []RecordedSession{{Provider: ProviderClaude, ID: "s1", Scope: SessionScopeFolder, Items: []RecordedItem{
					segmentMessage("m1", "t1", 90, "/work/app"),
					segmentCommand("m2", "t1", 100, 110, "/work/app", "[main aaaaaaa] change a"),
				}}},
			})
			session := segmentSession(t, report, "s1")
			if tc.linked != (len(session.Segments) == 1) {
				t.Fatalf("segments = %+v, want linked=%v", session.Segments, tc.linked)
			}
			if !tc.linked {
				if session.Uncommitted == nil || session.Uncommitted.Records != 2 {
					t.Fatalf("an unlinked session keeps every record uncommitted: %+v", session.Uncommitted)
				}
				return
			}
			want := []SegmentCommitLink{{SHA: segmentSHA("a"), Rule: SegmentLinkPrintedNumber}}
			if !reflect.DeepEqual(session.Segments[0].Commits, want) {
				t.Fatalf("commit links = %+v", session.Segments[0].Commits)
			}
		})
	}
}

func TestSegmentPrintedNumberMustBeAPrefixOfTheCommit(t *testing.T) {
	// Same seven leading digits, different commit: the longer printed number
	// decides.
	near := RecordedCommit{SHA: "aaaaaaa" + strings.Repeat("0", 33), CreatedSec: segmentAt(105) / 1000}
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 105), near}},
		Sessions: []RecordedSession{{Provider: ProviderClaude, ID: "s1", Items: []RecordedItem{
			segmentCommand("m1", "t1", 100, 110, "/work/app", "created aaaaaaa0"),
		}}},
	})
	links := segmentSession(t, report, "s1").Segments[0].Commits
	if len(links) != 1 || links[0].SHA != near.SHA {
		t.Fatalf("links = %+v", links)
	}
}

func TestSegmentFallbackLinksACommitNobodyPrintedByFolderAndTime(t *testing.T) {
	linked := RecordedWorktree{Root: "/work/app-feature", Current: "feature", Made: []MadeCommit{{AtSec: segmentAt(105) / 1000, SHA: segmentSHA("a")}}}
	sessions := []RecordedSession{
		{Provider: ProviderCodex, ID: "in-folder", Items: []RecordedItem{segmentCommand("i1", "t1", 100, 110, "/work/app-feature/pkg", "ok")}},
		{Provider: ProviderCodex, ID: "other-folder", Items: []RecordedItem{segmentCommand("o1", "t1", 100, 110, "/work/app", "ok")}},
		{Provider: ProviderCodex, ID: "other-time", Items: []RecordedItem{segmentCommand("x1", "t1", 200, 210, "/work/app-feature", "ok")}},
	}
	report := BuildSegmentReport(SegmentInputs{
		Git:      GitRecords{Worktrees: []RecordedWorktree{segmentMain, linked}, Commits: []RecordedCommit{segmentCommit("a", 105)}},
		Sessions: sessions,
	})
	got := segmentCommitEntry(t, report, segmentSHA("a")).Sessions
	want := []SegmentCommitSession{{Provider: ProviderCodex, ID: "in-folder", Rule: SegmentLinkFolderAndTime}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fallback links = %+v", got)
	}
	if report.Summary.LinkedByFolderAndTime != 1 || report.Summary.LinkedByPrintedNumber != 0 || report.Summary.HeadLogCommits != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentFallbackIsNotUsedForACommitASessionPrinted(t *testing.T) {
	worktree := segmentMain
	worktree.Made = []MadeCommit{{AtSec: segmentAt(105) / 1000, SHA: segmentSHA("a")}}
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{worktree}, Commits: []RecordedCommit{segmentCommit("a", 105)}},
		Sessions: []RecordedSession{
			{Provider: ProviderClaude, ID: "printed", Items: []RecordedItem{segmentCommand("p1", "t1", 100, 110, "/elsewhere", "aaaaaaa")}},
			{Provider: ProviderClaude, ID: "only-running", Items: []RecordedItem{segmentCommand("r1", "t1", 100, 110, "/work/app", "tests passed")}},
		},
	})
	got := segmentCommitEntry(t, report, segmentSHA("a")).Sessions
	want := []SegmentCommitSession{{Provider: ProviderClaude, ID: "printed", Rule: SegmentLinkPrintedNumber}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("links = %+v", got)
	}
	if len(segmentSession(t, report, "only-running").Segments) != 0 {
		t.Fatal("a session that only ran in the folder must not be linked once another session printed the commit")
	}
}

func TestSegmentFallbackLinksACommitWhoseObjectIsGone(t *testing.T) {
	worktree := segmentMain
	worktree.Made = []MadeCommit{{AtSec: segmentAt(105) / 1000, SHA: segmentSHA("a")}}
	report := BuildSegmentReport(SegmentInputs{
		Git:      GitRecords{Worktrees: []RecordedWorktree{worktree}},
		Sessions: []RecordedSession{{Provider: ProviderCodex, ID: "s1", Items: []RecordedItem{segmentCommand("m1", "t1", 100, 110, "/work/app", "")}}},
	})
	commit := segmentCommitEntry(t, report, segmentSHA("a"))
	if len(commit.Sessions) != 1 || commit.Local.State != LocalCommitNoObject || commit.CreatedAt == "" {
		t.Fatalf("commit = %+v", commit)
	}
}

func TestSegmentSeveralCommitsInOneTurn(t *testing.T) {
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 20), segmentCommit("b", 40), segmentCommit("c", 80)}},
		Sessions: []RecordedSession{{Provider: ProviderClaude, ID: "s1", Items: []RecordedItem{
			segmentMessage("m1", "t1", 10, "/work/app"),
			segmentCommand("m2", "t1", 19, 21, "/work/app", "[main aaaaaaa] first"),
			segmentMessage("m3", "t1", 30, "/work/app"),
			segmentCommand("m4", "t1", 39, 41, "/work/app", "[main bbbbbbb] second"),
			segmentMessage("m5", "t1", 50, "/work/app"),
			segmentMessage("m6", "t1", 60, "/work/app"),
			segmentMessage("m7", "t2", 70, "/work/app"),
			segmentCommand("m8", "t2", 79, 81, "/work/app", "[main ccccccc] third"),
			segmentMessage("m9", "t2", 90, "/work/app"),
			segmentMessage("m10", "t3", 100, "/work/app"),
		}}},
	})
	session := segmentSession(t, report, "s1")
	// The first commit of turn t1 closes right after its command; the last one
	// keeps the rest of the turn. t2 has one commit, so it keeps its closing
	// message too. t3 made no commit.
	if got, want := segmentRanges(session), [][2]string{{"m1", "m2"}, {"m3", "m6"}, {"m7", "m9"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("segments = %v, want %v", got, want)
	}
	if session.Segments[0].ClosesTurn || !session.Segments[1].ClosesTurn || !session.Segments[2].ClosesTurn {
		t.Fatalf("closes-turn flags = %+v", session.Segments)
	}
	if session.Uncommitted == nil || session.Uncommitted.First != "m10" || session.Uncommitted.Last != "m10" {
		t.Fatalf("uncommitted = %+v", session.Uncommitted)
	}
	if report.Summary.Segments.Total != 3 || report.Summary.SegmentsWithOneCommit != 3 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentWithoutARecordedTurnClosesAtTheCommit(t *testing.T) {
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 20)}},
		Sessions: []RecordedSession{{Provider: ProviderClaude, ID: "s1", Items: []RecordedItem{
			segmentCommand("m1", "", 19, 21, "/work/app", "aaaaaaa"),
			segmentMessage("m2", "", 30, "/work/app"),
		}}},
	})
	session := segmentSession(t, report, "s1")
	if got, want := segmentRanges(session), [][2]string{{"m1", "m1"}}; !reflect.DeepEqual(got, want) || session.Segments[0].ClosesTurn {
		t.Fatalf("segments = %+v", session.Segments)
	}
}

func TestSegmentOneCommandMakingSeveralCommits(t *testing.T) {
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 20), segmentCommit("b", 21), segmentCommit("c", 22)}},
		Sessions: []RecordedSession{{Provider: ProviderCodex, ID: "s1", Items: []RecordedItem{
			segmentMessage("m1", "t1", 10, ""),
			segmentCommand("m2", "t1", 19, 23, "/work/app", "[main bbbbbbb] two\n[main aaaaaaa] one\n[main ccccccc] three"),
			segmentMessage("m3", "t1", 30, ""),
		}}},
	})
	session := segmentSession(t, report, "s1")
	if len(session.Segments) != 1 {
		t.Fatalf("one command gives one segment, got %+v", session.Segments)
	}
	want := []SegmentCommitLink{
		{SHA: segmentSHA("a"), Rule: SegmentLinkPrintedNumber},
		{SHA: segmentSHA("b"), Rule: SegmentLinkPrintedNumber},
		{SHA: segmentSHA("c"), Rule: SegmentLinkPrintedNumber},
	}
	if !reflect.DeepEqual(session.Segments[0].Commits, want) || session.Segments[0].Last != "m3" {
		t.Fatalf("segment = %+v", session.Segments[0])
	}
	if report.Summary.SegmentsWithMoreCommits != 1 || report.Summary.LinkedCommits != 3 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentItemCompletingTwoCommandsChecksEachCommandTime(t *testing.T) {
	item := RecordedItem{ID: "m1", Turn: "t1", AtMs: segmentAt(300), Folder: "/work/app", Commands: []RecordedCommand{
		{StartedMs: segmentAt(100), EndedMs: segmentAt(110), Folder: "/work/app", Tokens: []string{"aaaaaaa", "bbbbbbb"}},
		{StartedMs: segmentAt(200), EndedMs: segmentAt(210), Folder: "/work/app", Tokens: []string{"ccccccc"}},
	}}
	report := BuildSegmentReport(SegmentInputs{
		// b was created during the second command, which did not print it.
		Git:      GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 105), segmentCommit("b", 205), segmentCommit("c", 205)}},
		Sessions: []RecordedSession{{Provider: ProviderClaude, ID: "s1", Items: []RecordedItem{item}}},
	})
	session := segmentSession(t, report, "s1")
	want := []SegmentCommitLink{{SHA: segmentSHA("a"), Rule: SegmentLinkPrintedNumber}, {SHA: segmentSHA("c"), Rule: SegmentLinkPrintedNumber}}
	if len(session.Segments) != 1 || !reflect.DeepEqual(session.Segments[0].Commits, want) || session.Commands != 2 {
		t.Fatalf("session = %+v", session)
	}
}

func TestSegmentTwoSessionsOnTheSameCommit(t *testing.T) {
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 105)}},
		Sessions: []RecordedSession{
			{Provider: ProviderCodex, ID: "worker", Items: []RecordedItem{segmentCommand("w1", "t1", 100, 106, "/work/app", "[main aaaaaaa] change")}},
			{Provider: ProviderCodex, ID: "lead", Items: []RecordedItem{segmentCommand("l1", "t9", 90, 120, "/work/app", "worker finished aaaaaaa")}},
		},
	})
	for _, id := range []string{"worker", "lead"} {
		if session := segmentSession(t, report, id); len(session.Segments) != 1 || session.Segments[0].Commits[0].SHA != segmentSHA("a") {
			t.Fatalf("%s segments = %+v", id, session.Segments)
		}
	}
	if got := segmentCommitEntry(t, report, segmentSHA("a")).Sessions; len(got) != 2 || got[0].ID != "lead" || got[1].ID != "worker" {
		t.Fatalf("commit sessions = %+v", got)
	}
	if report.Summary.CommitsWithMoreSessions != 1 || report.Summary.CommitsWithOneSession != 0 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentForkedSessionCountsCopiedRecordsOnce(t *testing.T) {
	shared := []RecordedItem{
		segmentMessage("m1", "t1", 10, "/work/app"),
		segmentCommand("m2", "t1", 19, 21, "/work/app", "[main aaaaaaa] before the fork"),
		segmentMessage("m3", "t1", 30, "/work/app"),
	}
	original := append(append([]RecordedItem{}, shared...), segmentMessage("m4", "t2", 40, "/work/app"))
	fork := append(append([]RecordedItem{}, shared...),
		segmentMessage("f1", "t5", 200, "/work/app"),
		segmentCommand("f2", "t5", 219, 221, "/work/app", "[main bbbbbbb] after the fork"),
	)
	inputs := SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 20), segmentCommit("b", 220)}},
		Sessions: []RecordedSession{
			{Provider: ProviderClaude, ID: "fork", Items: fork},
			{Provider: ProviderClaude, ID: "original", Items: original},
		},
	}
	report := BuildSegmentReport(inputs)
	originalSession, forkSession := segmentSession(t, report, "original"), segmentSession(t, report, "fork")
	if originalSession.Records != 4 || originalSession.CopiedRecords != 0 {
		t.Fatalf("original = %+v", originalSession)
	}
	if forkSession.Records != 2 || forkSession.CopiedRecords != 3 || !reflect.DeepEqual(forkSession.CopiedFrom, []string{"original"}) {
		t.Fatalf("fork = %+v", forkSession)
	}
	// The copied part's commit links only to the session it belongs to, and the
	// fork's first segment starts at its own first record.
	if got := segmentCommitEntry(t, report, segmentSHA("a")).Sessions; len(got) != 1 || got[0].ID != "original" {
		t.Fatalf("copied commit sessions = %+v", got)
	}
	if got, want := segmentRanges(forkSession), [][2]string{{"f1", "f2"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("fork segments = %v", got)
	}
	if report.Summary.CopiedRecords != 3 || report.Summary.CommitsWithMoreSessions != 0 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentResumedChainGivesEachRecordToItsFirstSession(t *testing.T) {
	// Each later file repeats everything before it and adds its own records.
	first := []RecordedItem{segmentMessage("a1", "t1", 10, ""), segmentMessage("a2", "t1", 20, "")}
	second := append(append([]RecordedItem{}, first...), segmentMessage("b1", "t2", 30, ""))
	third := append(append([]RecordedItem{}, second...), segmentMessage("c1", "t3", 40, ""))
	report := BuildSegmentReport(SegmentInputs{Sessions: []RecordedSession{
		{Provider: ProviderClaude, ID: "third", Items: third},
		{Provider: ProviderClaude, ID: "first", Items: first},
		{Provider: ProviderClaude, ID: "second", Items: second},
	}})
	for id, want := range map[string][2]int{"first": {2, 0}, "second": {1, 2}, "third": {1, 3}} {
		session := segmentSession(t, report, id)
		if session.Records != want[0] || session.CopiedRecords != want[1] {
			t.Fatalf("%s: records=%d copied=%d, want %v", id, session.Records, session.CopiedRecords, want)
		}
	}
	if got := segmentSession(t, report, "third").CopiedFrom; !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("third copied from %v", got)
	}
}

func TestSegmentSessionWithoutCommitsIsPlacedWhereItWorked(t *testing.T) {
	feature := RecordedWorktree{Root: "/work/app-feature", Current: "feature"}
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain, feature}},
		Sessions: []RecordedSession{{Provider: ProviderClaude, ID: "s1", Items: []RecordedItem{
			{ID: "m1", AtMs: segmentAt(10), Folder: "/work/app", Branch: "main"},
			{ID: "m2", AtMs: segmentAt(20), Folder: "/work/app/pkg", Branch: "main"},
			{ID: "m3", AtMs: segmentAt(30), Folder: "/work/app-feature", Branch: "feature"},
			{ID: "m4", AtMs: segmentAt(40), Folder: "/somewhere/else", Branch: "other"},
		}}},
	})
	session := segmentSession(t, report, "s1")
	if len(session.Segments) != 0 || session.Uncommitted == nil || session.Uncommitted.Records != 4 {
		t.Fatalf("session = %+v", session)
	}
	want := []SessionPlace{
		{Worktree: "/work/app", Branch: "main", Rule: SessionPlaceRecorded},
		{Worktree: "/work/app-feature", Branch: "feature", Rule: SessionPlaceRecorded},
	}
	if !reflect.DeepEqual(session.Uncommitted.Places, want) || session.Uncommitted.Unplaced {
		t.Fatalf("places = %+v", session.Uncommitted.Places)
	}
	if report.Summary.SessionsWithoutCommits.Claude != 1 || report.Summary.SessionsWithCommits.Total != 0 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentWorkWithoutABranchIsPlacedOnItsWorktree(t *testing.T) {
	detached := RecordedWorktree{Root: "/work/app-detached"}
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain, detached}},
		Sessions: []RecordedSession{
			{Provider: ProviderClaude, ID: "claude", Items: []RecordedItem{{ID: "c1", AtMs: segmentAt(10), Folder: "/work/app-detached", Detached: true}}},
			{Provider: ProviderCodex, ID: "codex", Items: []RecordedItem{segmentCommand("x1", "t1", 10, 11, "/work/app-detached", "")}},
		},
	})
	if got, want := segmentSession(t, report, "claude").Uncommitted.Places, []SessionPlace{{Worktree: "/work/app-detached", Rule: SessionPlaceRecorded}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("claude places = %+v", got)
	}
	if got, want := segmentSession(t, report, "codex").Uncommitted.Places, []SessionPlace{{Worktree: "/work/app-detached", Rule: SessionPlaceCurrentBranch}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("codex places = %+v", got)
	}
}

func TestSegmentPlaceReadsTheBranchFromTheHeadLog(t *testing.T) {
	worktree := RecordedWorktree{Root: "/work/app", Main: true, Current: "later", Before: "main", Moves: []HeadMove{
		{AtSec: segmentAt(100) / 1000, Branch: "feature"},
		{AtSec: segmentAt(200) / 1000, Branch: ""},
		{AtSec: segmentAt(300) / 1000, Branch: "later"},
	}}
	for second, want := range map[int64]string{50: "main", 100: "feature", 150: "feature", 250: "", 400: "later"} {
		if branch, rule := worktree.BranchAt(segmentAt(second) / 1000); branch != want || rule != SessionPlaceHeadLog {
			t.Fatalf("branch at %ds = %q (%s), want %q", second, branch, rule, want)
		}
	}
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{worktree}},
		Sessions: []RecordedSession{{Provider: ProviderCodex, ID: "s1", Items: []RecordedItem{
			segmentCommand("m1", "t1", 50, 51, "/work/app", ""),
			segmentCommand("m2", "t1", 150, 151, "/work/app", ""),
			segmentCommand("m3", "t1", 250, 251, "/work/app", ""),
		}}},
	})
	want := []SessionPlace{
		{Worktree: "/work/app", Branch: "main", Rule: SessionPlaceHeadLog},
		{Worktree: "/work/app", Branch: "feature", Rule: SessionPlaceHeadLog},
		{Worktree: "/work/app", Rule: SessionPlaceHeadLog},
	}
	if got := segmentSession(t, report, "s1").Uncommitted.Places; !reflect.DeepEqual(got, want) {
		t.Fatalf("places = %+v", got)
	}
}

func TestSegmentPlaceIsNeverGuessed(t *testing.T) {
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}},
		Sessions: []RecordedSession{
			{Provider: ProviderCodex, ID: "no-folder", Scope: SessionScopeFolder, Items: []RecordedItem{segmentMessage("n1", "t1", 10, "")}},
			{Provider: ProviderCodex, ID: "start-folder", Scope: SessionScopeFolder, StartFolder: "/work/app/pkg", Items: []RecordedItem{segmentMessage("s1", "t1", 10, "")}},
			{Provider: ProviderCodex, ID: "removed", Scope: SessionScopeOriginURL, StartFolder: "/gone/app-old", Items: []RecordedItem{
				segmentCommand("r1", "t1", 10, 11, "/gone/app-old/pkg", ""),
				segmentCommand("r2", "t1", 20, 21, "/tmp/scratch", ""),
			}},
		},
	})
	if part := segmentSession(t, report, "no-folder").Uncommitted; !part.Unplaced || len(part.Places) != 0 {
		t.Fatalf("no-folder = %+v", part)
	}
	if got, want := segmentSession(t, report, "start-folder").Uncommitted.Places, []SessionPlace{{Worktree: "/work/app", Branch: "main", Rule: SessionPlaceCurrentBranch}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("start-folder places = %+v", got)
	}
	if got, want := segmentSession(t, report, "removed").Uncommitted.Places, []SessionPlace{{Worktree: "/gone/app-old", Removed: true, Rule: SessionPlaceRemovedFolder}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("removed places = %+v", got)
	}
	if report.Summary.UnplacedUncommittedParts != 1 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentDeepestWorktreeHoldsANestedFolder(t *testing.T) {
	nested := RecordedWorktree{Root: "/work/app/.trees/fix", Current: "fix", Made: []MadeCommit{{AtSec: segmentAt(105) / 1000, SHA: segmentSHA("a")}}}
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain, nested}},
		Sessions: []RecordedSession{
			{Provider: ProviderCodex, ID: "nested", Items: []RecordedItem{segmentCommand("n1", "t1", 100, 110, "/work/app/.trees/fix/pkg", "")}},
			{Provider: ProviderCodex, ID: "outer", Items: []RecordedItem{segmentCommand("o1", "t1", 100, 110, "/work/app/pkg", "")}},
		},
	})
	if got := segmentCommitEntry(t, report, segmentSHA("a")).Sessions; len(got) != 1 || got[0].ID != "nested" {
		t.Fatalf("links = %+v", got)
	}
}

func TestSegmentSpawnPoints(t *testing.T) {
	parent := RecordedSession{Provider: ProviderClaude, ID: "parent", Items: []RecordedItem{
		segmentMessage("p1", "t1", 10, "/work/app"),
		segmentCommand("p2", "t1", 19, 21, "/work/app", "[main aaaaaaa] change"),
		segmentMessage("p3", "t2", 30, "/work/app"),
		segmentMessage("p4", "t2", 40, "/work/app"),
	}}
	byCall := RecordedSession{Provider: ProviderClaude, ID: "by-call", Sub: true, Parent: "parent", SpawnItem: "p1", Items: []RecordedItem{segmentMessage("c1", "", 35, "/work/app")}}
	byTime := RecordedSession{Provider: ProviderClaude, ID: "by-time", Sub: true, Parent: "parent", Items: []RecordedItem{segmentMessage("d1", "", 35, "/work/app")}}
	orphan := RecordedSession{Provider: ProviderClaude, ID: "orphan", Sub: true, Parent: "not-read", Items: []RecordedItem{segmentMessage("o1", "", 35, "/work/app")}}
	report := BuildSegmentReport(SegmentInputs{
		Git:      GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{segmentCommit("a", 20)}},
		Sessions: []RecordedSession{orphan, byTime, parent, byCall},
	})
	want := []SessionSpawn{
		{Provider: ProviderClaude, Parent: "not-read", Child: "orphan"},
		{Provider: ProviderClaude, Parent: "parent", Child: "by-call", ParentInScope: true, AtRecord: "p1", ParentSegment: 1, Rule: SpawnPointToolCall},
		{Provider: ProviderClaude, Parent: "parent", Child: "by-time", ParentInScope: true, AtRecord: "p3", ParentUncommitted: true, Rule: SpawnPointStartTime},
	}
	if !reflect.DeepEqual(report.Spawns, want) {
		t.Fatalf("spawns = %+v", report.Spawns)
	}
	if report.Summary.Sessions.ClaudeSub != 3 || report.Summary.Sessions.Claude != 1 || report.Summary.Spawns != 3 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentAgentCallsAreReportedAsRecorded(t *testing.T) {
	report := BuildSegmentReport(SegmentInputs{
		Sessions: []RecordedSession{{Provider: ProviderCodex, ID: "lead"}, {Provider: ProviderCodex, ID: "worker", Sub: true, Parent: "lead"}},
		Calls: []RecordedAgentCall{
			{Provider: ProviderCodex, Tool: "sendInput", From: "lead", To: []string{"worker"}, Status: "completed", AtMs: segmentAt(20), Item: "t1/i2"},
			{Provider: ProviderCodex, Tool: "spawnAgent", From: "lead", To: []string{"worker"}, Status: "completed", AtMs: segmentAt(10), Item: "t1/i1"},
			{Provider: ProviderCodex, Tool: "sendInput", From: "stranger", To: []string{"nobody"}, AtMs: segmentAt(30), Item: "t9/i1"},
		},
	})
	if len(report.AgentCalls) != 2 || report.AgentCalls[0].Tool != "spawnAgent" || report.AgentCalls[1].Tool != "sendInput" {
		t.Fatalf("agent calls = %+v", report.AgentCalls)
	}
}

func TestSegmentLocalCommitPositionIsOnlyALocalView(t *testing.T) {
	onBranch := segmentCommit("a", 20)
	onBranch.OnBranch = true
	rewritten := segmentCommit("b", 40)
	itsCopy := segmentCommit("c", 400)
	itsCopy.AuthoredSec, itsCopy.Subject, itsCopy.OnBranch = rewritten.AuthoredSec, rewritten.Subject, true
	dangling := segmentCommit("d", 60)
	report := BuildSegmentReport(SegmentInputs{
		Git: GitRecords{Worktrees: []RecordedWorktree{segmentMain}, Commits: []RecordedCommit{onBranch, rewritten, itsCopy, dangling}},
		Sessions: []RecordedSession{{Provider: ProviderCodex, ID: "s1", Items: []RecordedItem{
			segmentCommand("m1", "t1", 19, 21, "/work/app", "aaaaaaa"),
			segmentCommand("m2", "t2", 39, 41, "/work/app", "bbbbbbb"),
			segmentCommand("m3", "t3", 59, 61, "/work/app", "ddddddd"),
		}}},
	})
	if got := segmentCommitEntry(t, report, segmentSHA("a")).Local; got.State != LocalCommitOnBranch {
		t.Fatalf("on-branch commit = %+v", got)
	}
	if got := segmentCommitEntry(t, report, segmentSHA("b")).Local; got.State != LocalCommitThroughCopy || !reflect.DeepEqual(got.Copies, []string{segmentSHA("c")}) {
		t.Fatalf("rewritten commit = %+v", got)
	}
	if got := segmentCommitEntry(t, report, segmentSHA("d")).Local; got.State != LocalCommitNotOnBranch {
		t.Fatalf("dangling commit = %+v", got)
	}
}

func TestSegmentHeadLogCommitWithoutASessionStaysVisible(t *testing.T) {
	worktree := segmentMain
	worktree.Made = []MadeCommit{{AtSec: segmentAt(500) / 1000, SHA: segmentSHA("a")}}
	report := BuildSegmentReport(SegmentInputs{Git: GitRecords{Worktrees: []RecordedWorktree{worktree}, Commits: []RecordedCommit{segmentCommit("a", 500)}}})
	commit := segmentCommitEntry(t, report, segmentSHA("a"))
	if len(commit.Sessions) != 0 || !reflect.DeepEqual(commit.MadeIn, []string{"/work/app"}) {
		t.Fatalf("commit = %+v", commit)
	}
	if report.Summary.HeadLogCommitsNoSession != 1 || report.Summary.LinkedCommits != 0 {
		t.Fatalf("summary = %+v", report.Summary)
	}
}

func TestSegmentReportDoesNotDependOnInputOrder(t *testing.T) {
	worktree := segmentMain
	worktree.Made = []MadeCommit{{AtSec: segmentAt(305) / 1000, SHA: segmentSHA("c")}}
	sessions := []RecordedSession{
		{Provider: ProviderClaude, ID: "one", Scope: SessionScopeFolder, Items: []RecordedItem{
			segmentMessage("a1", "t1", 10, "/work/app"),
			segmentCommand("a2", "t1", 19, 21, "/work/app", "aaaaaaa"),
		}},
		{Provider: ProviderClaude, ID: "two", Scope: SessionScopeFolder, Items: []RecordedItem{
			segmentMessage("a1", "t1", 10, "/work/app"),
			segmentCommand("a2", "t1", 19, 21, "/work/app", "aaaaaaa"),
			segmentCommand("b1", "t2", 119, 121, "/work/app", "bbbbbbb aaaaaaa"),
		}},
		{Provider: ProviderCodex, ID: "three", Sub: true, Parent: "four", Scope: SessionScopeFolder, Items: []RecordedItem{segmentCommand("c1", "t3", 300, 310, "/work/app", "")}},
		{Provider: ProviderCodex, ID: "four", Scope: SessionScopeOriginURL, StartedMs: segmentAt(5)},
	}
	commits := []RecordedCommit{segmentCommit("a", 20), segmentCommit("b", 120), segmentCommit("c", 305)}
	unread := []UnreadSessionRecord{{Provider: ProviderCodex, Source: "b", Reason: "r"}, {Provider: ProviderClaude, Source: "a", Reason: "r"}}
	encode := func(report SegmentReport) string {
		raw, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	want := encode(BuildSegmentReport(SegmentInputs{Git: GitRecords{Worktrees: []RecordedWorktree{worktree}, Commits: commits}, Sessions: sessions, Unread: unread}))
	reversed := func() SegmentInputs {
		in := SegmentInputs{Git: GitRecords{Worktrees: []RecordedWorktree{worktree}}}
		for index := len(sessions) - 1; index >= 0; index-- {
			in.Sessions = append(in.Sessions, sessions[index])
		}
		for index := len(commits) - 1; index >= 0; index-- {
			in.Git.Commits = append(in.Git.Commits, commits[index])
		}
		in.Unread = []UnreadSessionRecord{unread[1], unread[0]}
		return in
	}
	for run := 0; run < 5; run++ {
		if got := encode(BuildSegmentReport(reversed())); got != want {
			t.Fatalf("report changed with input order:\n got %s\nwant %s", got, want)
		}
	}
	if !strings.Contains(want, `"version":1`) || !strings.Contains(want, `"unread":[{"provider":"claude"`) {
		t.Fatalf("report layout = %s", want)
	}
}

func TestSessionRecordScopeHoldsOnlyFoldersInsideAWorktree(t *testing.T) {
	scope := GitRecords{Origin: "example.test/team/app", Worktrees: []RecordedWorktree{{Root: "/work/app/", Aliases: []string{"/private/work/app"}}}}.ScopeOf()
	for folder, want := range map[string]bool{"/work/app": true, "/work/app/pkg": true, "/private/work/app/x": true, "/work/app-other": false, "/work": false, "": false} {
		if got := scope.Holds(folder); got != want {
			t.Fatalf("Holds(%q) = %v, want %v", folder, got, want)
		}
	}
	if scope.Origin != "example.test/team/app" {
		t.Fatalf("origin = %q", scope.Origin)
	}
}
