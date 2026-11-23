package domain

import (
	"strings"
	"testing"
	"time"
)

func diagnosisCodes(diagnosis SegmentDiagnosis) []string {
	codes := make([]string, 0, len(diagnosis.Findings))
	for _, finding := range diagnosis.Findings {
		codes = append(codes, finding.Code)
	}
	return codes
}

func diagnosisFinding(t *testing.T, diagnosis SegmentDiagnosis, code string) SegmentFinding {
	t.Helper()
	for _, finding := range diagnosis.Findings {
		if finding.Code == code {
			return finding
		}
	}
	t.Fatalf("no finding %s among %v", code, diagnosisCodes(diagnosis))
	return SegmentFinding{}
}

func diagnosisHealthy() SegmentDiagnosisInput {
	sha := strings.Repeat("a", 40)
	onRemote := true
	listed := AnchoredSessionListed{Provider: ProviderClaude, SessionID: "session-1", Revision: 2, BodyHash: "sha256:server", Records: 5, Segments: 1}
	listed.Commits = append(listed.Commits, struct {
		SHA      string   `json:"sha"`
		OnRemote *bool    `json:"on_remote,omitempty"`
		Copies   []string `json:"copies,omitempty"`
	}{SHA: sha, OnRemote: &onRemote})
	return SegmentDiagnosisInput{
		Now: time.Date(2031, 1, 2, 4, 0, 0, 0, time.UTC), SetUp: true, AutoOn: true, Connected: true,
		Collector: &CollectorState{EnabledAt: "2031-01-01T00:00:00Z", LastRun: &CollectorRun{At: "2031-01-02T03:59:00Z", Stored: 1}},
		Report: SegmentReport{
			Sessions: []SegmentedSession{{Provider: ProviderClaude, ID: "session-1", Records: 5, EndedAt: "2031-01-02T03:58:00Z",
				Segments: []SessionSegment{{Commits: []SegmentCommitLink{{SHA: sha, Rule: SegmentLinkPrintedNumber}}}}}},
			Commits: []SegmentCommit{{SHA: sha, Sessions: []SegmentCommitSession{{Provider: ProviderClaude, ID: "session-1", Rule: SegmentLinkPrintedNumber}}}},
		},
		Kept:     []SessionUploadListing{{Provider: ProviderClaude, SessionID: "session-1", ReportKept: true, Log: []SessionUploadLogEntry{{At: "2031-01-02T03:59:00Z", Report: SegmentPushStored, ServerHash: "sha256:server"}}}},
		OnOrigin: map[string]bool{sha: true},
		Server: &SegmentDiagnosisServer{Sessions: []AnchoredSessionListed{listed}, RemoteSyncedAt: "2031-01-02T03:59:30Z",
			Summarized: map[string]map[string]bool{sha: {"claude/session-1": true}}},
	}
}

func TestDiagnosisOfAStateWhereEverythingFitsFindsNothing(t *testing.T) {
	diagnosis := DiagnoseSegments(diagnosisHealthy())
	if len(diagnosis.Findings) != 0 || diagnosis.Problems != 0 {
		t.Fatalf("findings: %+v", diagnosis.Findings)
	}
	// What was looked at is said, so that nothing found means something.
	for _, want := range []string{"set-up", "collector", "unread-records", "links", "copies", "server-sessions", "pushed-commits", "summaries"} {
		found := false
		for _, checked := range diagnosis.Checked {
			found = found || checked == want
		}
		if !found {
			t.Fatalf("%s is not among what was checked: %v", want, diagnosis.Checked)
		}
	}
}

func TestDiagnosisNamesEachThingThatDoesNotFitWithItsFix(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for name, test := range map[string]struct {
		change  func(*SegmentDiagnosisInput)
		code    string
		problem bool
		fix     string
		says    string
	}{
		"cxt is not set up":               {func(in *SegmentDiagnosisInput) { in.SetUp = false; in.Kept = nil }, "not-set-up", true, "cxt setup", "not set up"},
		"no server":                       {func(in *SegmentDiagnosisInput) { in.Connected = false }, "not-connected", true, "cxt setup <server-url>", "not connected"},
		"the collector is turned off":     {func(in *SegmentDiagnosisInput) { in.AutoOn = false }, "collector-off", true, "cxt config segments.auto on", "turned off"},
		"the collector never ran":         {func(in *SegmentDiagnosisInput) { in.Collector = nil }, "collector-never-ran", false, "cxt segments push", "has not run"},
		"the collector's last run failed": {func(in *SegmentDiagnosisInput) { in.Collector.LastRun.Error = "synthetic status 500" }, "collector-failing", true, "cxt segments push", "failed: synthetic status 500"},
		"the collector is not signed in":  {func(in *SegmentDiagnosisInput) { in.Collector.LastRun.Error = "server answered 401" }, "collector-failing", true, "cxt login", "401"},
		"the collector did not run since a session went on": {func(in *SegmentDiagnosisInput) {
			in.Collector.LastRun.At = "2031-01-02T03:00:00Z"
			in.Report.Sessions[0].EndedAt = "2031-01-02T03:30:00Z"
		}, "collector-behind", true, "cxt segments push", "1 sessions have records newer"},
		"records that cannot be read for want of sqlite3": {func(in *SegmentDiagnosisInput) {
			in.Report.Unread = []UnreadSessionRecord{{Provider: ProviderCodex, Source: "state_5.sqlite", Reason: "the sqlite3 program is not installed, so the thread database was not read"}}
		}, "unread-records", true, "install the sqlite3 program", "[codex] state_5.sqlite"},
		"a Codex layout this cxt does not read": {func(in *SegmentDiagnosisInput) {
			in.Report.Unread = []UnreadSessionRecord{{Provider: ProviderCodex, Source: "thread_history_2.sqlite", Reason: "Codex keeps its threads another way than this cxt reads them (x). A newer cxt is needed; the threads stay"}}
		}, "unread-records", true, "update cxt", "A newer cxt is needed"},
		"a commit linked by the weaker rule": {func(in *SegmentDiagnosisInput) { in.Report.Commits[0].Sessions[0].Rule = SegmentLinkFolderAndTime }, "weak-link", false, "", "same folder"},
		"the tool removed its files": {func(in *SegmentDiagnosisInput) {
			in.Kept = append(in.Kept, SessionUploadListing{Provider: ProviderClaude, SessionID: "old", ReportKept: true})
			listed := in.Server.Sessions[0]
			listed.SessionID = "old"
			in.Server.Sessions = append(in.Server.Sessions, listed)
		}, "records-gone", false, "", "no longer hold the records of 1 sessions"},
		"the server cannot be read": {func(in *SegmentDiagnosisInput) { in.Server, in.ServerError = nil, "server answered 401: token expired" }, "server-not-read", true, "cxt login", "could not be read"},
		"a removed session": {func(in *SegmentDiagnosisInput) {
			in.Server.Sessions = nil
			in.Server.Removed = []AnchoredSessionRemoval{{Provider: ProviderClaude, SessionID: "session-1", RemovedAt: "2031-01-02T03:00:00Z", ByYou: true}}
		}, "removed", false, "cxt segments allow claude session-1", "you removed it on 2031-01-02T03:00:00Z"},
		"a session that is not on the server":           {func(in *SegmentDiagnosisInput) { in.Server.Sessions = nil }, "not-on-server", true, "cxt segments push claude session-1", "claude session-1 is not on the server"},
		"an archived session":                           {func(in *SegmentDiagnosisInput) { in.Server.Sessions[0].ArchivedAt = "2031-01-02T03:00:00Z" }, "archived", false, "cxt segments unarchive claude session-1", "archived on the server"},
		"the server holds fewer records":                {func(in *SegmentDiagnosisInput) { in.Server.Sessions[0].Records = 3 }, "server-behind", true, "cxt segments push claude session-1", "holds 3 of the 5 records"},
		"the server holds another report than was sent": {func(in *SegmentDiagnosisInput) { in.Server.Sessions[0].BodyHash = "sha256:other" }, "server-differs", true, "cxt segments push claude session-1", "(revision 2) is not the one this computer last sent"},
		"no copy is kept":                               {func(in *SegmentDiagnosisInput) { in.Kept = nil }, "no-copy", false, "cxt segments push", "no copy is kept under .cxt/sessions for 1 sessions"},
		"a pushed commit the server has not seen":       {func(in *SegmentDiagnosisInput) { in.Server.Sessions[0].Commits[0].OnRemote = nil }, "push-not-seen", true, "cxt segments refresh", "is on origin here, but the server has not seen it there: its last read of GitHub was at 2031-01-02T03:59:30Z"},
		"a commit without a summary":                    {func(in *SegmentDiagnosisInput) { in.Server.Summarized[sha] = map[string]bool{} }, "summary-missing", false, "cxt segments push claude session-1", "has no summary on the server at commit aaaaaaaaaa"},
		"a session left only in its copy": {func(in *SegmentDiagnosisInput) {
			in.Kept = append(in.Kept, SessionUploadListing{Provider: ProviderCodex, SessionID: "thread-9", ReportKept: true})
		}, "only-in-copy", true, "cxt segments resend codex thread-9", "only the copy under .cxt/sessions is left"},
	} {
		in := diagnosisHealthy()
		test.change(&in)
		diagnosis := DiagnoseSegments(in)
		finding := diagnosisFinding(t, diagnosis, test.code)
		if finding.Problem != test.problem || finding.Fix != test.fix || !strings.Contains(finding.What, test.says) {
			t.Errorf("%s: %+v", name, finding)
		}
		if test.problem && diagnosis.Problems == 0 {
			t.Errorf("%s: a problem was not counted", name)
		}
		if finding.Why == "" {
			t.Errorf("%s: the finding does not say why it matters", name)
		}
	}
}

func TestDiagnosisCountsManySessionsAndPutsProblemsFirst(t *testing.T) {
	in := diagnosisHealthy()
	in.Report.Commits[0].Sessions[0].Rule = SegmentLinkFolderAndTime // something to know
	for index := 0; index < segmentDiagnosisListLimit+2; index++ {
		in.Report.Sessions = append(in.Report.Sessions, SegmentedSession{Provider: ProviderClaude, ID: "early-" + string(rune('a'+index)), Records: 1, EndedAt: "2030-12-01T00:00:00Z"})
	}
	diagnosis := DiagnoseSegments(in)
	absent := diagnosisFinding(t, diagnosis, "not-on-server")
	if absent.Session != nil || absent.Fix != "cxt segments push" || !strings.Contains(absent.What, "12 sessions") || !strings.Contains(absent.Why, "2031-01-01T00:00:00Z") {
		t.Fatalf("many absent sessions: %+v", absent)
	}
	if diagnosis.Findings[0].Code != "not-on-server" || diagnosis.Findings[len(diagnosis.Findings)-1].Problem {
		t.Fatalf("order: %v", diagnosisCodes(diagnosis))
	}
	// A commit the server holds through its copy counts as seen, and a
	// session that belongs to someone else is not compared.
	in = diagnosisHealthy()
	in.Server.Sessions[0].Commits[0].OnRemote, in.Server.Sessions[0].Commits[0].Copies = nil, []string{strings.Repeat("b", 40)}
	shared := in.Server.Sessions[0]
	shared.SessionID, shared.Shared, shared.Records = "theirs", true, 0
	in.Server.Sessions = append(in.Server.Sessions, shared)
	if diagnosis := DiagnoseSegments(in); len(diagnosis.Findings) != 0 {
		t.Fatalf("a copy on the remote, and another member's session: %+v", diagnosis.Findings)
	}
}
